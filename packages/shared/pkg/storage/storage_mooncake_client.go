//go:build mooncake && cgo

package storage

/*
#cgo LDFLAGS: -lnuma
#define _GNU_SOURCE
#include <stdlib.h>
#include <string.h>
#include <sched.h>
#include <numa.h>
#include <pthread.h>

static inline int numa_available_wrapper() {
    return numa_available();
}

static inline int numa_num_configured_nodes_wrapper() {
    return numa_num_configured_nodes();
}

static inline int numa_current_node_wrapper() {
    int cpu = sched_getcpu();
    if (cpu < 0) {
        return -1;
    }
    return numa_node_of_cpu(cpu);
}

static inline long current_thread_id_wrapper() {
    return (long)pthread_self();
}

static inline void* numa_alloc_onnode_wrapper(size_t size, int node) {
    void* ptr = numa_alloc_onnode(size, node);
    if (ptr) {
        memset(ptr, 0, size);
    }
    return ptr;
}

static inline void numa_free_wrapper(void* ptr, size_t size) {
    numa_free(ptr, size);
}
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/kvcache-ai/Mooncake/mooncake-store/go/mooncakestore"
	"go.uber.org/zap"
	"golang.org/x/sync/semaphore"

	"github.com/e2b-dev/infra/packages/shared/pkg/env"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

// Reads go through 4 MiB buffers allocated on the reader's NUMA node and
// registered with Mooncake (UB mode cannot read more than 4 MiB at once).
// Each OS thread keeps one buffer once it has read; a reserve pool, refilled
// in the background, serves the first read of a thread.
const (
	numaBufferSize = mooncakeChunkSize

	defaultNumaPoolSize           = 512
	maxNumaPoolSize               = 4096
	defaultNumaPoolRefillInterval = time.Second
	defaultNumaPoolRefillBatch    = 8
	numaPoolRefillBackoffInitial  = 250 * time.Millisecond
	numaPoolRefillBackoffMax      = 30 * time.Second
	defaultGetIntoMaxConcurrency  = 128
	maxGetIntoMaxConcurrency      = 4096
	mooncakeReadMaxAttempts       = 3
	mooncakeReadRetryBaseDelay    = time.Millisecond
)

var (
	globalNumaPool       atomic.Pointer[numaBufferPool]
	globalGetIntoLimiter atomic.Pointer[getIntoLimiter]
	threadBuffers        sync.Map

	errNumaBufferUnavailable     = errors.New("mooncake NUMA thread buffer is unavailable")
	errGetIntoLimiterUnavailable = errors.New("mooncake GetInto limiter is not initialized")
)

var nrSockets int

func init() {
	if C.numa_available_wrapper() == 0 {
		nrSockets = int(C.numa_num_configured_nodes_wrapper())
	}
	if nrSockets < 1 {
		nrSockets = 1
	}
}

// mooncakeClient implements mooncakeKV on the Mooncake store client.
type mooncakeClient struct {
	store *mooncakestore.Store
	// Put copies the value into the client's local buffer
	// (MOONCAKE_LOCAL_BUFFER_SIZE) before sending it, and fails ("Failed to
	// allocate buffer for put operation") when the puts in flight need more.
	// Twenty sandboxes pausing at once start hundreds of 4 MiB chunk puts, so
	// the bytes in flight are bounded by the buffer size.
	putBytes *semaphore.Weighted
	putMax   int64
}

var _ mooncakeKV = (*mooncakeClient)(nil)

var (
	mooncakeClientOnce sync.Once
	mooncakeClientInst *mooncakeClient
	mooncakeClientErr  error
)

// newMooncakeClient sets up the process-wide Mooncake client on first use.
// The template and build cache providers share it.
func newMooncakeClient() (mooncakeKV, error) {
	mooncakeClientOnce.Do(func() {
		store, localBufSize, err := setupMooncakeStore()
		if err != nil {
			mooncakeClientErr = err

			return
		}
		// Leave a quarter of the buffer for the client's own use.
		putMax := max(localBufSize/4*3, mooncakeChunkSize)
		mooncakeClientInst = &mooncakeClient{store: store, putBytes: semaphore.NewWeighted(putMax), putMax: putMax}
	})

	if mooncakeClientErr != nil {
		return nil, mooncakeClientErr
	}

	return mooncakeClientInst, nil
}

func (c *mooncakeClient) Put(key string, value []byte) error {
	if len(value) == 0 {
		return fmt.Errorf("mooncake put %s: empty value", key)
	}

	weight := min(int64(len(value)), c.putMax)
	if err := c.putBytes.Acquire(context.Background(), weight); err != nil {
		return fmt.Errorf("mooncake put %s: %w", key, err)
	}
	defer c.putBytes.Release(weight)

	cValue := C.CBytes(value)
	defer C.free(cValue)

	if err := c.store.Put(key, unsafe.Slice((*byte)(cValue), len(value)), nil); err != nil {
		return fmt.Errorf("mooncake put %s: %w", key, err)
	}

	return nil
}

func (c *mooncakeClient) GetInto(ctx context.Context, key string, dst []byte) (int, error) {
	if len(dst) == 0 || len(dst) > numaBufferSize {
		return 0, fmt.Errorf("mooncake get %s: buffer size %d out of range", key, len(dst))
	}

	nb, n, release, err := getIntoNumaBuffer(ctx, c.store, key, uint64(len(dst)))
	if err != nil {
		if exists, existsErr := c.store.Exists(key); existsErr == nil && !exists {
			return 0, ErrObjectNotExist
		}

		return 0, fmt.Errorf("mooncake get %s: %w", key, err)
	}
	defer release()

	if n > int64(len(dst)) {
		return 0, fmt.Errorf("mooncake get %s: got %d bytes into a %d byte buffer", key, n, len(dst))
	}

	return copy(dst, nb.buf[:n]), nil
}

func (c *mooncakeClient) GetSize(key string) (int64, error) {
	size, err := c.store.GetSize(key)
	if err != nil {
		if exists, existsErr := c.store.Exists(key); existsErr == nil && !exists {
			return 0, ErrObjectNotExist
		}

		return 0, fmt.Errorf("mooncake get size %s: %w", key, err)
	}

	return size, nil
}

func (c *mooncakeClient) Exists(key string) (bool, error) {
	return c.store.Exists(key)
}

func (c *mooncakeClient) Remove(key string) error {
	if err := c.store.Remove(key, true); err != nil {
		if exists, existsErr := c.store.Exists(key); existsErr == nil && !exists {
			return nil
		}

		return fmt.Errorf("mooncake remove %s: %w", key, err)
	}

	return nil
}

func (c *mooncakeClient) RemoveByRegex(pattern string) (int64, error) {
	return c.store.RemoveByRegex(pattern, true)
}

// setupMooncakeStore connects to Mooncake with the MOONCAKE_* settings, the
// same variables as the KASandbox backend, and prewarms the read buffers.
func setupMooncakeStore() (*mooncakestore.Store, int64, error) {
	store, err := mooncakestore.New()
	if err != nil {
		return nil, 0, fmt.Errorf("failed to create mooncake store: %w", err)
	}
	ready := false
	defer func() {
		if !ready {
			store.Close()
		}
	}()

	localHostname := env.GetEnv("MOONCAKE_LOCAL_HOSTNAME", "localhost")
	metadataServer := env.GetEnv("MOONCAKE_METADATA_SERVER", "http://localhost:8080/metadata")
	globalSegSize, err := env.GetEnvAsInt("MOONCAKE_GLOBAL_SEGMENT_SIZE", 1073741824)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid MOONCAKE_GLOBAL_SEGMENT_SIZE: %w", err)
	}
	localBufSize, err := env.GetEnvAsInt("MOONCAKE_LOCAL_BUFFER_SIZE", 134217728)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid MOONCAKE_LOCAL_BUFFER_SIZE: %w", err)
	}
	protocol := env.GetEnv("MOONCAKE_PROTOCOL", "tcp")
	deviceName := env.GetEnv("MOONCAKE_DEVICE_NAME", "")
	masterAddr := env.GetEnv("MOONCAKE_MASTER_ADDR", "localhost:50051")

	if err := store.Setup(localHostname, metadataServer, uint64(globalSegSize), uint64(localBufSize),
		protocol, deviceName, masterAddr); err != nil {
		return nil, 0, fmt.Errorf("failed to setup mooncake store: %w", err)
	}

	mountSize, err := env.GetEnvAsInt("MOONCAKE_MOUNT_SEGMENT_SIZE", 0)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid MOONCAKE_MOUNT_SEGMENT_SIZE: %w", err)
	}
	if mountSize > 0 {
		if err := store.InitAll(protocol, deviceName, uint64(mountSize)); err != nil {
			return nil, 0, fmt.Errorf("failed to init mooncake segments: %w", err)
		}
	}

	getMaxConcurrency, err := env.GetEnvAsInt("MOONCAKE_GET_MAX_CONCURRENCY", defaultGetIntoMaxConcurrency)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid MOONCAKE_GET_MAX_CONCURRENCY: %w", err)
	}
	if getMaxConcurrency < 1 || getMaxConcurrency > maxGetIntoMaxConcurrency {
		return nil, 0, fmt.Errorf("MOONCAKE_GET_MAX_CONCURRENCY must be between 1 and %d, got %d",
			maxGetIntoMaxConcurrency, getMaxConcurrency)
	}

	poolSize, err := env.GetEnvAsInt("MOONCAKE_NUMA_POOL_SIZE", defaultNumaPoolSize)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid MOONCAKE_NUMA_POOL_SIZE: %w", err)
	}
	if poolSize < nrSockets || poolSize > maxNumaPoolSize {
		return nil, 0, fmt.Errorf("MOONCAKE_NUMA_POOL_SIZE must be between %d and %d, got %d",
			nrSockets, maxNumaPoolSize, poolSize)
	}

	ctx := context.Background()
	lowWatermark := envIntInRange(ctx, "MOONCAKE_NUMA_POOL_LOW_WATERMARK", poolSize/4, 0, poolSize-1)
	refillMs := envIntInRange(ctx, "MOONCAKE_NUMA_POOL_REFILL_INTERVAL_MS", int(defaultNumaPoolRefillInterval/time.Millisecond), 1, 1<<31-1)
	refillBatch := envIntInRange(ctx, "MOONCAKE_NUMA_POOL_REFILL_BATCH", defaultNumaPoolRefillBatch, 1, 64)

	pool, err := newNumaBufferPool(store, numaBufferPoolConfig{
		total:          poolSize,
		lowWatermark:   lowWatermark,
		refillInterval: time.Duration(refillMs) * time.Millisecond,
		refillBatch:    refillBatch,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("failed to prewarm NUMA reserve pool: %w", err)
	}

	globalNumaPool.Store(pool)
	globalGetIntoLimiter.Store(newGetIntoLimiter(getMaxConcurrency))
	pool.startReconciler(ctx, store)
	ready = true

	logger.L().Info(ctx, "Mooncake storage client ready",
		zap.String("master", masterAddr),
		zap.String("protocol", protocol),
		zap.Int("local_buffer_bytes", localBufSize),
		zap.Int("numa_nodes", nrSockets),
		zap.Int("numa_pool_size", poolSize))

	return store, int64(localBufSize), nil
}

// envIntInRange reads an optional tuning knob; an invalid value falls back
// to the default with a warning.
func envIntInRange(ctx context.Context, name string, def, lo, hi int) int {
	val, err := env.GetEnvAsInt(name, def)
	if err != nil || val < lo || val > hi {
		logger.L().Warn(ctx, "invalid Mooncake setting, using default",
			zap.String("name", name), zap.Int("default", def), zap.Int("min", lo), zap.Int("max", hi), zap.Error(err))

		return def
	}

	return val
}

type getIntoLimiter struct {
	slots    chan struct{}
	inFlight atomic.Int64
}

func newGetIntoLimiter(maxConcurrency int) *getIntoLimiter {
	return &getIntoLimiter{slots: make(chan struct{}, maxConcurrency)}
}

func (l *getIntoLimiter) acquire(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	select {
	case l.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	l.inFlight.Add(1)

	return func() {
		<-l.slots
		l.inFlight.Add(-1)
	}, nil
}

func waitForMooncakeReadRetry(ctx context.Context, attempt int) error {
	timer := time.NewTimer(time.Duration(attempt) * mooncakeReadRetryBaseDelay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// getIntoWithRetry retries reads that fail under high concurrency.
func getIntoWithRetry(ctx context.Context, store *mooncakestore.Store, key string, ptr uintptr, size uint64) (int64, error) {
	limiter := globalGetIntoLimiter.Load()
	if limiter == nil {
		return 0, errGetIntoLimiterUnavailable
	}

	var (
		n       int64
		lastErr error
	)
	for attempt := 1; attempt <= mooncakeReadMaxAttempts; attempt++ {
		release, err := limiter.acquire(ctx)
		if err != nil {
			return 0, err
		}
		n, lastErr = store.GetInto(key, ptr, size)
		release()
		if lastErr == nil {
			return n, nil
		}
		if !errors.Is(lastErr, mooncakestore.ErrGet) || attempt == mooncakeReadMaxAttempts {
			break
		}
		if err := waitForMooncakeReadRetry(ctx, attempt); err != nil {
			return 0, err
		}
	}

	return n, lastErr
}

func getIntoNumaBuffer(ctx context.Context, store *mooncakestore.Store, key string, size uint64) (*numaBuffer, int64, func(), error) {
	nb, release, err := getOrCreateNumaBuffer(ctx, store)
	if err != nil {
		return nil, 0, nil, err
	}

	n, err := getIntoWithRetry(ctx, store, key, uintptr(unsafe.Pointer(&nb.buf[0])), size)
	if err != nil {
		release()

		return nil, n, nil, err
	}

	return nb, n, release, nil
}

type numaBufferPool struct {
	ready []chan *numaBuffer

	total          int
	lowWatermark   int
	refillInterval time.Duration
	refillBatch    int
	refillWake     chan struct{}
	// Only the reconciler goroutine touches these.
	refillFailures []int
	refillRetryAt  []time.Time
}

type numaBufferPoolConfig struct {
	total          int
	lowWatermark   int
	refillInterval time.Duration
	refillBatch    int
}

// newNumaBufferPool allocates and registers the complete pool before
// publishing it.
func newNumaBufferPool(store *mooncakestore.Store, cfg numaBufferPoolConfig) (*numaBufferPool, error) {
	pool := &numaBufferPool{
		ready:          make([]chan *numaBuffer, nrSockets),
		total:          cfg.total,
		lowWatermark:   cfg.lowWatermark,
		refillInterval: cfg.refillInterval,
		refillBatch:    cfg.refillBatch,
		refillWake:     make(chan struct{}, 1),
		refillFailures: make([]int, nrSockets),
		refillRetryAt:  make([]time.Time, nrSockets),
	}

	for node := range nrSockets {
		nodeCapacity := cfg.total / nrSockets
		if node < cfg.total%nrSockets {
			nodeCapacity++
		}
		pool.ready[node] = make(chan *numaBuffer, nodeCapacity)
	}

	// Round-robin so prewarming progresses evenly across NUMA nodes.
	for i := range cfg.total {
		node := i % nrSockets
		nb, err := allocAndRegisterOnNode(store, node)
		if err != nil {
			pool.freeIdleBuffers()

			return nil, fmt.Errorf("node=%d buffer=%d/%d: %w", node, i+1, cfg.total, err)
		}
		pool.ready[node] <- nb
	}

	return pool, nil
}

func (p *numaBufferPool) takeLocal(node int) (*numaBuffer, bool) {
	if p == nil || node < 0 || node >= len(p.ready) {
		return nil, false
	}

	select {
	case nb := <-p.ready[node]:
		if p.lowWatermark > 0 && p.readyCount() <= p.lowWatermark {
			p.requestRefill()
		}

		return nb, true
	default:
		return nil, false
	}
}

func (p *numaBufferPool) readyCount() int {
	total := 0
	for node := range p.ready {
		total += len(p.ready[node])
	}

	return total
}

func (p *numaBufferPool) putBack(nb *numaBuffer) {
	if nb == nil || nb.cptr == nil {
		return
	}
	if p == nil || nb.node < 0 || nb.node >= len(p.ready) {
		nb.free()

		return
	}

	select {
	case p.ready[nb.node] <- nb:
	default:
		nb.free()
	}
}

func (p *numaBufferPool) requestRefill() {
	select {
	case p.refillWake <- struct{}{}:
	default:
	}
}

// startReconciler starts the background refill; it runs for the life of
// the process, like the client.
func (p *numaBufferPool) startReconciler(ctx context.Context, store *mooncakestore.Store) {
	if p.refillBatch <= 0 {
		return
	}

	go p.reconcileLoop(ctx, store)
}

func (p *numaBufferPool) reconcileLoop(ctx context.Context, store *mooncakestore.Store) {
	ticker := time.NewTicker(p.refillInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.refillWake:
			p.reconcileOnce(ctx, store)
		case <-ticker.C:
			p.reconcileOnce(ctx, store)
		}
	}
}

// reconcileOnce refills up to refillBatch buffers per pass.
func (p *numaBufferPool) reconcileOnce(ctx context.Context, store *mooncakestore.Store) {
	for range p.refillBatch {
		if ctx.Err() != nil {
			return
		}
		// Pause while reads are in flight: allocating and registering shares
		// the Mooncake client and pinned memory with the read path, and
		// competing with it shows up as tail latency.
		if limiter := globalGetIntoLimiter.Load(); limiter != nil && limiter.inFlight.Load() > 0 {
			return
		}
		if p.readyCount() >= p.total {
			return
		}

		node := p.pickRefillNode()
		if node < 0 {
			return
		}

		nb, err := allocAndRegisterOnNode(store, node)
		if err != nil {
			p.recordRefillFailure(ctx, node, err)

			return
		}
		p.refillFailures[node] = 0
		p.refillRetryAt[node] = time.Time{}

		select {
		case p.ready[node] <- nb:
		default:
			nb.free()
		}
	}
}

// pickRefillNode returns the NUMA node with the fewest ready buffers,
// skipping nodes in failure backoff.
func (p *numaBufferPool) pickRefillNode() int {
	now := time.Now()
	best, bestReady := -1, 0
	for node := range p.ready {
		if !p.refillRetryAt[node].IsZero() && now.Before(p.refillRetryAt[node]) {
			continue
		}
		if ready := len(p.ready[node]); best < 0 || ready < bestReady {
			best, bestReady = node, ready
		}
	}

	return best
}

func (p *numaBufferPool) recordRefillFailure(ctx context.Context, node int, err error) {
	p.refillFailures[node]++
	delay := min(numaPoolRefillBackoffInitial<<min(p.refillFailures[node]-1, 7), numaPoolRefillBackoffMax)
	p.refillRetryAt[node] = time.Now().Add(delay)

	logger.L().Warn(ctx, "Mooncake NUMA buffer refill failed",
		zap.Int("node", node), zap.Int("failures", p.refillFailures[node]), zap.Duration("retry_in", delay), zap.Error(err))
}

func (p *numaBufferPool) freeIdleBuffers() {
	if p == nil {
		return
	}

	for node := range p.ready {
	drain:
		for {
			select {
			case nb := <-p.ready[node]:
				nb.free()
			default:
				break drain
			}
		}
	}
}

func allocAndRegisterOnNode(store *mooncakestore.Store, node int) (*numaBuffer, error) {
	if node < 0 || node >= nrSockets {
		node = 0
	}

	cptr := C.numa_alloc_onnode_wrapper(C.size_t(numaBufferSize), C.int(node))
	if cptr == nil {
		return nil, fmt.Errorf("numa_alloc_onnode failed: size=%d, node=%d", numaBufferSize, node)
	}

	if err := store.RegisterBuffer(uintptr(cptr), uint64(numaBufferSize)); err != nil {
		C.numa_free_wrapper(cptr, C.size_t(numaBufferSize))

		return nil, fmt.Errorf("failed to register buffer on node %d: %w", node, err)
	}

	return &numaBuffer{
		buf:   unsafe.Slice((*byte)(cptr), numaBufferSize),
		cptr:  cptr,
		size:  numaBufferSize,
		node:  node,
		store: store,
	}, nil
}

type threadLocalNumaBuffer struct {
	buffer      *numaBuffer
	fromReserve bool
}

// getOrCreateNumaBuffer pins the goroutine to its OS thread and returns the
// thread's registered buffer, taking one from the local reserve (or
// allocating one) on the thread's first read.
func getOrCreateNumaBuffer(ctx context.Context, store *mooncakestore.Store) (*numaBuffer, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	runtime.LockOSThread()

	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(runtime.UnlockOSThread)
	}

	threadID := int64(C.current_thread_id_wrapper())
	if value, ok := threadBuffers.Load(threadID); ok {
		if tlb := value.(*threadLocalNumaBuffer); tlb.buffer != nil {
			return tlb.buffer, release, nil
		}
	}

	node := int(C.numa_current_node_wrapper())
	if node < 0 || node >= nrSockets {
		node = 0
	}

	pool := globalNumaPool.Load()
	nb, fromReserve := pool.takeLocal(node)
	if !fromReserve {
		var err error
		if nb, err = allocAndRegisterOnNode(store, node); err != nil {
			release()

			return nil, nil, err
		}
	}

	actual, loaded := threadBuffers.LoadOrStore(threadID, &threadLocalNumaBuffer{buffer: nb, fromReserve: fromReserve})
	if loaded {
		if fromReserve {
			pool.putBack(nb)
		} else {
			nb.free()
		}

		tlb := actual.(*threadLocalNumaBuffer)
		if tlb.buffer == nil {
			release()

			return nil, nil, errNumaBufferUnavailable
		}

		return tlb.buffer, release, nil
	}

	return nb, release, nil
}

type numaBuffer struct {
	buf   []byte
	cptr  unsafe.Pointer
	size  int
	node  int
	store *mooncakestore.Store
}

func (nb *numaBuffer) free() {
	if nb.cptr == nil {
		return
	}

	if nb.store != nil {
		if err := nb.store.UnregisterBuffer(uintptr(nb.cptr)); err != nil {
			logger.L().Warn(context.Background(), "failed to unregister Mooncake NUMA buffer",
				zap.Int("node", nb.node), zap.Int("size", nb.size), zap.Error(err))
		}
	}

	C.numa_free_wrapper(nb.cptr, C.size_t(nb.size))
	nb.cptr = nil
	nb.buf = nil
	nb.store = nil
}
