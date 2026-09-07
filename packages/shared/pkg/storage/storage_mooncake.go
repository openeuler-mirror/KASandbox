//go:build mooncake

package storage

/*
#cgo LDFLAGS: -lnuma
#define _GNU_SOURCE
#include <stdio.h>
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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/e2b-dev/infra/packages/shared/pkg/env"
	"github.com/kvcache-ai/Mooncake/mooncake-store/go/mooncakestore"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

const (
	mooncakeOperationTimeout  = 30 * time.Second
	mooncakeUploadConcurrency = 16
	numaBufferSize            = 4 * 1024 * 1024 // 4 MB default buffer size

	// MOONCAKE_NUMA_POOL_SIZE controls the per-NUMA reserve total. A reserve
	// buffer is assigned to an OS thread once and stays thread-local afterward.
	// The pool is bounded only by its ready capacity (the pool total): a
	// background reconciler refills the reserve when the ready count drops
	// below the configured low watermark, pausing while foreground GetInto
	// reads are in flight.
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
	mooncakeInstances    = make(map[string]*mooncakeStorage)
	mooncakeInstancesMu  sync.RWMutex
	globalNumaPool       atomic.Pointer[numaBufferPool]
	globalGetIntoLimiter atomic.Pointer[getIntoLimiter]
	globalMetadataCache  sync.Map
	threadBuffers        sync.Map
	threadBufferGate     sync.Mutex
	threadBufferUsers    sync.WaitGroup
	mooncakeShuttingDown atomic.Bool
)

var chunkBufferPool = sync.Pool{
	New: func() any {
		buf := make([]byte, MemoryChunkSize)
		return &buf
	},
}

func getChunkBuffer(ctx context.Context) ([]byte, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	ptr := chunkBufferPool.Get().(*[]byte)
	buf := (*ptr)[:MemoryChunkSize]
	var once sync.Once
	return buf, func() {
		once.Do(func() {
			*ptr = buf[:MemoryChunkSize]
			chunkBufferPool.Put(ptr)
		})
	}, nil
}

type getIntoLimiter struct {
	slots    chan struct{}
	inFlight atomic.Int64
}

func newGetIntoLimiter(maxConcurrency int) *getIntoLimiter {
	return &getIntoLimiter{
		slots: make(chan struct{}, maxConcurrency),
	}
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

func acquireGetIntoSlot(ctx context.Context) (func(), error) {
	limiter := globalGetIntoLimiter.Load()
	if limiter == nil {
		return nil, errGetIntoLimiterUnavailable
	}
	return limiter.acquire(ctx)
}

func waitForMooncakeReadRetry(ctx context.Context, attempt int) error {
	delay := time.Duration(attempt) * mooncakeReadRetryBaseDelay
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func getIntoWithRetry(
	ctx context.Context,
	store *mooncakestore.Store,
	key string,
	ptr uintptr,
	size uint64,
) (int64, error) {
	var (
		n        int64
		lastErr  error
		attempts int
	)
	for attempt := 1; attempt <= mooncakeReadMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		attempts = attempt

		releaseSlot, err := acquireGetIntoSlot(ctx)
		if err != nil {
			return n, err
		}
		n, lastErr = store.GetInto(key, ptr, size)
		releaseSlot()
		if lastErr == nil {
			return n, nil
		}
		if !errors.Is(lastErr, mooncakestore.ErrGet) || attempt == mooncakeReadMaxAttempts {
			break
		}
		if err := waitForMooncakeReadRetry(ctx, attempt); err != nil {
			return n, err
		}
	}

	zap.L().Sugar().Warnf(
		"[MooncakeStorage] GetInto failed after %d attempts: key=%s size=%d err=%v",
		attempts,
		key,
		size,
		lastErr,
	)
	return n, lastErr
}

func getIntoNumaBuffer(
	ctx context.Context,
	store *mooncakestore.Store,
	key string,
	size uint64,
) (*numaBuffer, int64, func(), error) {
	numaBuf, releaseBuffer, err := getOrCreateNumaBuffer(ctx, store)
	if err != nil {
		return nil, 0, nil, err
	}

	n, getErr := getIntoWithRetry(
		ctx,
		store,
		key,
		uintptr(unsafe.Pointer(&numaBuf.buf[0])),
		size,
	)
	if getErr != nil {
		releaseBuffer()
		return nil, n, nil, getErr
	}
	return numaBuf, n, releaseBuffer, nil
}

func batchGetInto(
	ctx context.Context,
	store *mooncakestore.Store,
	keys []string,
	ptrs []uintptr,
	sizes []uint64,
) ([]int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var (
		lengths  []int64
		lastErr  error
		attempts int
	)
	for attempt := 1; attempt <= mooncakeReadMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		attempts = attempt

		releaseSlot, err := acquireGetIntoSlot(ctx)
		if err != nil {
			return nil, err
		}
		lengths, lastErr = store.BatchGetInto(keys, ptrs, sizes)
		releaseSlot()
		if lastErr == nil {
			break
		}
		if !errors.Is(lastErr, mooncakestore.ErrBatchOp) || attempt == mooncakeReadMaxAttempts {
			break
		}
		if err := waitForMooncakeReadRetry(ctx, attempt); err != nil {
			return nil, err
		}
	}
	if lastErr != nil {
		zap.L().Sugar().Warnf(
			"[MooncakeStorage] BatchGetInto failed after %d attempts: keys=%d err=%v",
			attempts,
			len(keys),
			lastErr,
		)
		return nil, lastErr
	}

	for i, n := range lengths {
		if n >= 0 {
			continue
		}
		n, err := getIntoWithRetry(ctx, store, keys[i], ptrs[i], sizes[i])
		if err != nil {
			return nil, fmt.Errorf("failed to recover batch item %s: %w", keys[i], err)
		}
		lengths[i] = n
	}
	return lengths, nil
}

type storeFactory struct {
	store     *mooncakestore.Store
	pool      *numaBufferPool
	once      sync.Once
	closeOnce sync.Once
	err       error
}

var globalStoreFactory = &storeFactory{}

func getStoreFactory() (*storeFactory, error) {
	globalStoreFactory.once.Do(func() {
		store, err := mooncakestore.New()
		if err != nil {
			globalStoreFactory.err = fmt.Errorf("failed to create mooncake store: %w", err)
			return
		}
		ready := false
		defer func() {
			if !ready {
				store.Close()
			}
		}()

		localHostname := env.GetEnv("MOONCAKE_LOCAL_HOSTNAME", "localhost")
		metadataServer := env.GetEnv("MOONCAKE_METADATA_SERVER", "http://localhost:8080/metadata")
		val, err := env.GetEnvAsInt("MOONCAKE_GLOBAL_SEGMENT_SIZE", 1073741824)
		if err != nil {
			globalStoreFactory.err = fmt.Errorf("invalid MOONCAKE_GLOBAL_SEGMENT_SIZE: %w", err)
			return
		}
		globalSegSize := uint64(val)
		valBufferSize, err := env.GetEnvAsInt("MOONCAKE_LOCAL_BUFFER_SIZE", 134217728)
		if err != nil {
			globalStoreFactory.err = fmt.Errorf("invalid MOONCAKE_LOCAL_BUFFER_SIZE: %w", err)
			return
		}
		localBufSize := uint64(valBufferSize)
		protocol := env.GetEnv("MOONCAKE_PROTOCOL", "tcp")
		deviceName := env.GetEnv("MOONCAKE_DEVICE_NAME", "")
		masterAddr := env.GetEnv("MOONCAKE_MASTER_ADDR", "localhost:50051")

		if err := store.Setup(localHostname, metadataServer, globalSegSize, localBufSize,
			protocol, deviceName, masterAddr); err != nil {
			globalStoreFactory.err = fmt.Errorf("failed to setup mooncake store: %w", err)
			return
		}

		valSegSize, err := env.GetEnvAsInt("MOONCAKE_MOUNT_SEGMENT_SIZE", 0)
		if err != nil {
			globalStoreFactory.err = fmt.Errorf("invalid MOONCAKE_MOUNT_SEGMENT_SIZE: %w", err)
			return
		}
		mountSize := uint64(valSegSize)
		if mountSize > 0 {
			if err := store.InitAll(protocol, deviceName, mountSize); err != nil {
				globalStoreFactory.err = fmt.Errorf("failed to init mooncake segments: %w", err)
				return
			}
		}

		getMaxConcurrency, err := env.GetEnvAsInt(
			"MOONCAKE_GET_MAX_CONCURRENCY",
			defaultGetIntoMaxConcurrency,
		)
		if err != nil {
			globalStoreFactory.err = fmt.Errorf("invalid MOONCAKE_GET_MAX_CONCURRENCY: %w", err)
			return
		}
		if getMaxConcurrency < 1 || getMaxConcurrency > maxGetIntoMaxConcurrency {
			globalStoreFactory.err = fmt.Errorf(
				"MOONCAKE_GET_MAX_CONCURRENCY must be between 1 and %d, got %d",
				maxGetIntoMaxConcurrency,
				getMaxConcurrency,
			)
			return
		}
		getLimiter := newGetIntoLimiter(getMaxConcurrency)

		poolSize, err := env.GetEnvAsInt("MOONCAKE_NUMA_POOL_SIZE", defaultNumaPoolSize)
		if err != nil {
			globalStoreFactory.err = fmt.Errorf("invalid MOONCAKE_NUMA_POOL_SIZE: %w", err)
			return
		}
		if poolSize < nrSockets || poolSize > maxNumaPoolSize {
			globalStoreFactory.err = fmt.Errorf(
				"MOONCAKE_NUMA_POOL_SIZE must be between %d and %d, got %d",
				nrSockets,
				maxNumaPoolSize,
				poolSize,
			)
			return
		}

		lowWatermark := poolSize / 4
		if val, err := env.GetEnvAsInt("MOONCAKE_NUMA_POOL_LOW_WATERMARK", lowWatermark); err != nil {
			zap.L().Sugar().Warnf(
				"[MooncakeStorage] [NUMA.reserve] invalid MOONCAKE_NUMA_POOL_LOW_WATERMARK, using default %d: %v",
				lowWatermark,
				err,
			)
		} else if val < 0 || val >= poolSize {
			zap.L().Sugar().Warnf(
				"[MooncakeStorage] [NUMA.reserve] MOONCAKE_NUMA_POOL_LOW_WATERMARK must be between 0 and %d, got %d; using default %d",
				poolSize-1,
				val,
				lowWatermark,
			)
		} else {
			lowWatermark = val
		}

		refillInterval := defaultNumaPoolRefillInterval
		if val, err := env.GetEnvAsInt("MOONCAKE_NUMA_POOL_REFILL_INTERVAL_MS", int(defaultNumaPoolRefillInterval/time.Millisecond)); err != nil {
			zap.L().Sugar().Warnf(
				"[MooncakeStorage] [NUMA.reserve] invalid MOONCAKE_NUMA_POOL_REFILL_INTERVAL_MS, using default %s: %v",
				refillInterval,
				err,
			)
		} else if val < 1 {
			zap.L().Sugar().Warnf(
				"[MooncakeStorage] [NUMA.reserve] MOONCAKE_NUMA_POOL_REFILL_INTERVAL_MS must be >= 1, got %d; using default %s",
				val,
				refillInterval,
			)
		} else {
			refillInterval = time.Duration(val) * time.Millisecond
		}

		refillBatch := defaultNumaPoolRefillBatch
		if val, err := env.GetEnvAsInt("MOONCAKE_NUMA_POOL_REFILL_BATCH", defaultNumaPoolRefillBatch); err != nil {
			zap.L().Sugar().Warnf(
				"[MooncakeStorage] [NUMA.reserve] invalid MOONCAKE_NUMA_POOL_REFILL_BATCH, using default %d: %v",
				refillBatch,
				err,
			)
		} else if val < 1 || val > 64 {
			zap.L().Sugar().Warnf(
				"[MooncakeStorage] [NUMA.reserve] MOONCAKE_NUMA_POOL_REFILL_BATCH must be between 1 and 64, got %d; using default %d",
				val,
				refillBatch,
			)
		} else {
			refillBatch = val
		}

		pool, err := newNumaBufferPool(store, numaBufferPoolConfig{
			total:          poolSize,
			lowWatermark:   lowWatermark,
			refillInterval: refillInterval,
			refillBatch:    refillBatch,
		})
		if err != nil {
			globalStoreFactory.err = fmt.Errorf("failed to prewarm NUMA reserve pool: %w", err)
			return
		}

		globalNumaPool.Store(pool)
		globalGetIntoLimiter.Store(getLimiter)

		// Reserve buffers are assigned once to OS threads. Steady-state reads
		// retain the thread-local fast path and do not borrow from the pool.
		globalStoreFactory.store = store
		globalStoreFactory.pool = pool
		pool.startReconciler(context.Background(), store)
		ready = true
	})
	return globalStoreFactory, globalStoreFactory.err
}

func (f *storeFactory) close() {
	f.closeOnce.Do(func() {
		store := f.store
		f.store = nil
		if store == nil {
			return
		}
		if f.pool != nil {
			f.pool.stopReconciler()
		}
		releaseAllThreadBuffers(f.pool)
		if pool := f.pool; pool != nil {
			globalNumaPool.CompareAndSwap(pool, nil)
			pool.close()
			f.pool = nil
		}
		globalGetIntoLimiter.Store(nil)
		store.Close()
	})
}

type numaBufferPool struct {
	ready     []chan *numaBuffer
	closeOnce sync.Once
	closing   atomic.Bool

	total          int
	lowWatermark   int
	refillInterval time.Duration
	refillBatch    int
	refillWake     chan struct{}
	refillCancel   context.CancelFunc
	refillWG       sync.WaitGroup
	// 以下两个切片只由 reconciler 单 goroutine 访问
	refillFailures []int
	refillRetryAt  []time.Time
}

type numaBufferPoolConfig struct {
	total          int
	lowWatermark   int
	refillInterval time.Duration
	refillBatch    int
}

// newNumaBufferPool allocates and registers the complete pool before publishing it.
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

	for node := 0; node < nrSockets; node++ {
		nodeCapacity := cfg.total / nrSockets
		if node < cfg.total%nrSockets {
			nodeCapacity++
		}
		pool.ready[node] = make(chan *numaBuffer, nodeCapacity)
	}

	// Allocate round-robin so prewarming progresses evenly across NUMA nodes.
	for i := 0; i < cfg.total; i++ {
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
	if p == nil || p.closing.Load() || node < 0 || node >= len(p.ready) {
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
	if p == nil || p.closing.Load() || nb.node < 0 || nb.node >= len(p.ready) {
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

// startReconciler launches the single background refill goroutine.
func (p *numaBufferPool) startReconciler(ctx context.Context, store *mooncakestore.Store) {
	if p.refillBatch <= 0 {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	p.refillCancel = cancel
	p.refillWG.Add(1)
	go p.reconcileLoop(ctx, store)
}

// stopReconciler cancels the refill loop and waits for any in-flight
// allocation so no allocAndRegisterOnNode races store.Close.
func (p *numaBufferPool) stopReconciler() {
	if p.refillCancel != nil {
		p.refillCancel()
	}
	p.refillWG.Wait()
}

func (p *numaBufferPool) reconcileLoop(ctx context.Context, store *mooncakestore.Store) {
	defer p.refillWG.Done()
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

// reconcileOnce refills up to refillBatch buffers per pass. The pool is
// bounded only by its ready capacity: the high watermark equals the pool
// total because ready channels are sized to total/nrSockets.
func (p *numaBufferPool) reconcileOnce(ctx context.Context, store *mooncakestore.Store) {
	for i := 0; i < p.refillBatch; i++ {
		if p.closing.Load() || ctx.Err() != nil {
			return
		}
		// 前台有 GetInto 在途时暂停补池：alloc+register 与读路径共享
		// mooncake client 和内存 pin 资源，补池争抢会造成长尾延迟。
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
			p.recordRefillFailure(node, err)
			return
		}
		p.refillFailures[node] = 0
		p.refillRetryAt[node] = time.Time{}
		select {
		case p.ready[node] <- nb:
		default:
			// Buffers never return to the pool, so a full ready channel is
			// unreachable; free defensively.
			nb.free()
		}
	}
}

// pickRefillNode returns the NUMA node with the fewest ready buffers,
// skipping nodes in failure backoff. Only the reconciler goroutine calls it.
func (p *numaBufferPool) pickRefillNode() int {
	now := time.Now()
	best := -1
	bestReady := 0
	for node := range p.ready {
		if !p.refillRetryAt[node].IsZero() && now.Before(p.refillRetryAt[node]) {
			continue
		}
		ready := len(p.ready[node])
		if best < 0 || ready < bestReady {
			best = node
			bestReady = ready
		}
	}
	return best
}

func (p *numaBufferPool) recordRefillFailure(node int, err error) {
	p.refillFailures[node]++
	shift := p.refillFailures[node] - 1
	if shift > 7 {
		shift = 7
	}
	delay := numaPoolRefillBackoffInitial << shift
	if delay > numaPoolRefillBackoffMax {
		delay = numaPoolRefillBackoffMax
	}
	p.refillRetryAt[node] = time.Now().Add(delay)
	zap.L().Sugar().Warnf(
		"[MooncakeStorage] [NUMA.reserve] event=refill_fail node=%d failures=%d retry_in=%s err=%v",
		node,
		p.refillFailures[node],
		delay,
		err,
	)
}

func (p *numaBufferPool) freeIdleBuffers() {
	if p == nil {
		return
	}
	for node := range p.ready {
		for {
			select {
			case nb := <-p.ready[node]:
				nb.free()
			default:
				goto nextNode
			}
		}
	nextNode:
	}
}

func (p *numaBufferPool) close() {
	if p == nil {
		return
	}
	p.closeOnce.Do(func() {
		p.closing.Store(true)
		p.freeIdleBuffers()
	})
}

func allocAndRegisterOnNode(store *mooncakestore.Store, node int) (*numaBuffer, error) {
	if node < 0 || node >= nrSockets {
		node = 0
	}

	cptr := C.numa_alloc_onnode_wrapper(C.size_t(numaBufferSize), C.int(node))
	if cptr == nil {
		return nil, fmt.Errorf("numa_alloc_onnode failed: size=%d, node=%d", numaBufferSize, node)
	}

	nb := &numaBuffer{
		buf:   unsafe.Slice((*byte)(cptr), numaBufferSize),
		cptr:  cptr,
		size:  numaBufferSize,
		node:  node,
		store: store,
	}

	if err := store.RegisterBuffer(uintptr(cptr), uint64(numaBufferSize)); err != nil {
		C.numa_free_wrapper(cptr, C.size_t(numaBufferSize))
		nb.cptr = nil
		nb.buf = nil
		nb.store = nil
		return nil, fmt.Errorf("failed to register buffer on node %d: %w", node, err)
	}
	return nb, nil
}

type threadLocalNumaBuffer struct {
	buffer      *numaBuffer
	fromReserve bool
}

// getOrCreateNumaBuffer pins the goroutine and reuses one registered buffer
// associated with the current OS thread. The first read takes a local reserve
// buffer when available and only allocates/registers on a local reserve miss.
func getOrCreateNumaBuffer(
	ctx context.Context,
	store *mooncakestore.Store,
) (*numaBuffer, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	runtime.LockOSThread()

	threadBufferGate.Lock()
	if mooncakeShuttingDown.Load() {
		threadBufferGate.Unlock()
		runtime.UnlockOSThread()
		return nil, nil, errNumaBufferShutdown
	}
	threadBufferUsers.Add(1)
	threadBufferGate.Unlock()

	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			threadBufferUsers.Done()
			runtime.UnlockOSThread()
		})
	}

	threadID := int64(C.current_thread_id_wrapper())
	if value, ok := threadBuffers.Load(threadID); ok {
		tlb := value.(*threadLocalNumaBuffer)
		if tlb.buffer != nil {
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
		nb, err = allocAndRegisterOnNode(store, node)
		if err != nil {
			release()
			return nil, nil, err
		}
	}

	actual, loaded := threadBuffers.LoadOrStore(
		threadID,
		&threadLocalNumaBuffer{
			buffer:      nb,
			fromReserve: fromReserve,
		},
	)
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
	if nb.cptr != nil {
		if nb.store != nil {
			if err := nb.store.UnregisterBuffer(uintptr(nb.cptr)); err != nil {
				zap.L().Sugar().Warnf(
					"[MooncakeStorage] [NUMA] failed to unregister buffer ptr=%x node=%d size=%d: %v",
					uintptr(nb.cptr),
					nb.node,
					nb.size,
					err,
				)
			}
		}
		C.numa_free_wrapper(nb.cptr, C.size_t(nb.size))
		nb.cptr = nil
		nb.buf = nil
		nb.store = nil
	}
}

var nrSockets int

func init() {
	if C.numa_available_wrapper() == 0 {
		nrSockets = int(C.numa_num_configured_nodes_wrapper())
	}
	if nrSockets < 1 {
		nrSockets = 1
	}
}

var (
	errNumaBufferShutdown        = errors.New("mooncake storage is shutting down")
	errNumaBufferUnavailable     = errors.New("mooncake NUMA thread buffer is unavailable")
	errGetIntoLimiterUnavailable = errors.New("mooncake GetInto limiter is not initialized")
)

func makeChunkKey(prefix string, offset int64) string {
	buf := make([]byte, 0, len(prefix)+20)
	buf = append(buf, prefix...)
	buf = strconv.AppendInt(buf, offset, 10)
	return string(buf)
}

func loadCachedMetadata(path string) (*mooncakeObjectMeta, bool) {
	value, ok := globalMetadataCache.Load(path)
	if !ok {
		return nil, false
	}
	meta := value.(mooncakeObjectMeta)
	return &meta, true
}

func storeCachedMetadata(path string, meta *mooncakeObjectMeta) {
	if meta == nil {
		return
	}
	globalMetadataCache.Store(path, *meta)
}

func invalidateCachedMetadata(path string) {
	globalMetadataCache.Delete(path)
}

func invalidateCachedMetadataPrefix(prefix string) {
	globalMetadataCache.Range(func(key, _ any) bool {
		path, ok := key.(string)
		if ok && strings.HasPrefix(path, prefix) {
			globalMetadataCache.Delete(path)
		}
		return true
	})
}

func releaseAllThreadBuffers(pool *numaBufferPool) {
	threadBufferGate.Lock()
	mooncakeShuttingDown.Store(true)
	threadBufferGate.Unlock()

	threadBufferUsers.Wait()
	threadBuffers.Range(func(key, value any) bool {
		tlb := value.(*threadLocalNumaBuffer)
		if tlb.buffer != nil {
			if tlb.fromReserve {
				pool.putBack(tlb.buffer)
			} else {
				tlb.buffer.free()
			}
			tlb.buffer = nil
		}
		threadBuffers.Delete(key)
		return true
	})
}

// Shutdown releases all NUMA buffers and closes all mooncake storage instances.
// Call this before process exit to avoid resource leaks in long-running processes.
func Shutdown() {
	mooncakeInstancesMu.Lock()
	clear(mooncakeInstances)
	mooncakeInstancesMu.Unlock()
	globalMetadataCache.Range(func(key, _ any) bool {
		globalMetadataCache.Delete(key)
		return true
	})

	globalStoreFactory.close()
}

// -----------------------------------------------------------------------------
// StorageProvider implementation
// -----------------------------------------------------------------------------

type mooncakeStorage struct {
	store      *mooncakestore.Store
	bucketName string // used as a key-prefix / namespace
}

var _ StorageProvider = (*mooncakeStorage)(nil)

// NewMooncakeStorageProvider creates a Mooncake-backed storage provider.
// Configuration is read from environment variables (see Setup call below).
func NewMooncakeStorageProvider(_ context.Context, bucketName string) (*mooncakeStorage, error) {
	mooncakeInstancesMu.Lock()
	defer mooncakeInstancesMu.Unlock()

	if instance, exists := mooncakeInstances[bucketName]; exists {
		return instance, nil
	}

	factory, err := getStoreFactory()
	if err != nil {
		return nil, err
	}

	instance := &mooncakeStorage{
		store:      factory.store,
		bucketName: bucketName,
	}
	mooncakeInstances[bucketName] = instance
	return instance, nil
}

func (s *mooncakeStorage) key(path string) string {
	if s.bucketName == "" {
		return path
	}
	return s.bucketName + "/" + path
}

func (s *mooncakeStorage) DeleteObjectsWithPrefix(ctx context.Context, prefix string) error {
	ctx, cancel := context.WithTimeout(ctx, mooncakeOperationTimeout)
	defer cancel()

	// Mooncake has flat keys, so use regex matching over the prefix.
	// WARNING: This may scan all keys; avoid frequent use with large buckets.
	pattern := "^" + regexp.QuoteMeta(s.key(prefix))
	if !strings.HasSuffix(prefix, "/") {
		pattern += ".*"
	}

	_, err := s.store.RemoveByRegex(pattern, true)
	if err != nil {
		return fmt.Errorf("failed to delete objects with prefix %s: %w", prefix, err)
	}
	invalidateCachedMetadataPrefix(s.key(prefix))
	return nil
}

func (s *mooncakeStorage) UploadSignedURL(_ context.Context, _ string, _ time.Duration) (string, error) {
	return "", fmt.Errorf("mooncake storage does not support signed URLs")
}

func (s *mooncakeStorage) OpenSeekable(_ context.Context, path string, _ SeekableObjectType) (Seekable, error) {
	return newMooncakeSeekable(s.store, s.key(path)), nil
}

func (s *mooncakeStorage) OpenBlob(_ context.Context, path string, _ ObjectType) (Blob, error) {
	key := s.key(path)
	return &mooncakeBlob{
		store:          s.store,
		path:           key,
		chunkKeyPrefix: key + "#c#",
	}, nil
}

func (s *mooncakeStorage) GetDetails() string {
	return fmt.Sprintf("[Mooncake Storage, prefix set to %s]", s.bucketName)
}

// -----------------------------------------------------------------------------
// Blob implementation (small objects: headers, metadata, etc.)
// -----------------------------------------------------------------------------

type mooncakeBlob struct {
	store          *mooncakestore.Store
	path           string
	chunkKeyPrefix string
}

var _ Blob = (*mooncakeBlob)(nil)

var (
	errNotChunkedBlob     = errors.New("not a chunked Mooncake blob")
	errLegacyBlobTooLarge = errors.New("legacy single-object Mooncake blob exceeds the 4 MiB registered buffer limit")
)

func (o *mooncakeBlob) Put(ctx context.Context, data []byte) error {
	invalidateCachedMetadata(o.path)
	if len(data) <= MemoryChunkSize {
		return o.store.Put(o.path, data, nil)
	}

	if err := o.deleteObjectAndChunks(); err != nil {
		return fmt.Errorf("failed to clean up old object %s: %w", o.path, err)
	}

	g, groupCtx := errgroup.WithContext(ctx)
	g.SetLimit(mooncakeUploadConcurrency)

	size := int64(len(data))
	for offset := int64(0); offset < size; offset += MemoryChunkSize {
		off := offset
		g.Go(func() error {
			if err := groupCtx.Err(); err != nil {
				return err
			}

			end := min(off+MemoryChunkSize, size)
			chunk := data[off:end]
			cChunk := C.CBytes(chunk)
			defer C.free(cChunk)

			return o.store.Put(o.chunkKey(off), unsafe.Slice((*byte)(cChunk), len(chunk)), nil)
		})
	}

	if err := g.Wait(); err != nil {
		return fmt.Errorf("failed to upload chunks: %w", err)
	}

	meta := &mooncakeObjectMeta{Size: size, ChunkSize: MemoryChunkSize}
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}
	if err := o.store.Put(o.path, metaBytes, nil); err != nil {
		return fmt.Errorf("failed to write metadata: %w", err)
	}
	storeCachedMetadata(o.path, meta)
	return nil
}

func (o *mooncakeBlob) WriteTo(ctx context.Context, dst io.Writer) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, mooncakeOperationTimeout)
	defer cancel()

	meta, objectSize, err := o.readMeta(ctx)
	if err == nil {
		return o.writeChunkedTo(ctx, dst, meta)
	}
	if !errors.Is(err, errNotChunkedBlob) {
		return 0, err
	}

	return o.writeSingleTo(ctx, dst, objectSize)
}

func (o *mooncakeBlob) Exists(_ context.Context) (bool, error) {
	return o.store.Exists(o.path)
}

func (o *mooncakeBlob) chunkKey(offset int64) string {
	return makeChunkKey(o.chunkKeyPrefix, offset)
}

func (o *mooncakeBlob) deleteObjectAndChunks() error {
	invalidateCachedMetadata(o.path)
	if _, err := o.store.RemoveByRegex("^"+regexp.QuoteMeta(o.path)+"$", true); err != nil {
		return err
	}
	if _, err := o.store.RemoveByRegex("^"+regexp.QuoteMeta(o.chunkKeyPrefix), true); err != nil {
		return err
	}
	return nil
}

func (o *mooncakeBlob) readMeta(ctx context.Context) (*mooncakeObjectMeta, int64, error) {
	if meta, ok := loadCachedMetadata(o.path); ok {
		return meta, 0, nil
	}

	size, err := o.store.GetSize(o.path)
	if err != nil {
		exists, existsErr := o.store.Exists(o.path)
		if existsErr == nil && !exists {
			return nil, 0, ErrObjectNotExist
		}
		return nil, 0, fmt.Errorf("failed to get object size for %s: %w", o.path, err)
	}
	if size > 4096 {
		return nil, size, errNotChunkedBlob
	}
	if size == 0 {
		return nil, size, errNotChunkedBlob
	}

	numaBuf, n, release, err := getIntoNumaBuffer(ctx, o.store, o.path, uint64(size))
	if err != nil {
		return nil, size, fmt.Errorf("failed to read potential metadata for %s: %w", o.path, err)
	}
	defer release()

	var meta mooncakeObjectMeta
	if err := json.Unmarshal(numaBuf.buf[:n], &meta); err != nil {
		return nil, size, errNotChunkedBlob
	}
	if meta.ChunkSize <= 0 || meta.ChunkSize > numaBufferSize || meta.Size < 0 {
		return nil, size, errNotChunkedBlob
	}
	storeCachedMetadata(o.path, &meta)
	return &meta, size, nil
}

func (o *mooncakeBlob) writeSingleTo(ctx context.Context, dst io.Writer, size int64) (int64, error) {
	if size == 0 {
		return 0, nil
	}
	if size > numaBufferSize {
		return 0, fmt.Errorf(
			"%w: path=%s size=%d; re-upload the object so it uses chunked storage",
			errLegacyBlobTooLarge,
			o.path,
			size,
		)
	}

	stagingBuf, releaseStaging, err := getChunkBuffer(ctx)
	if err != nil {
		return 0, err
	}
	defer releaseStaging()

	numaBuf, n, releaseNuma, err := getIntoNumaBuffer(ctx, o.store, o.path, uint64(size))
	if err != nil {
		return int64(n), fmt.Errorf("failed to get object: %w", err)
	}
	if int64(n) > size || int64(n) > int64(len(stagingBuf)) {
		releaseNuma()
		return 0, fmt.Errorf("invalid object length for %s: got=%d expected_at_most=%d", o.path, n, size)
	}

	copy(stagingBuf[:int(n)], numaBuf.buf[:n])
	releaseNuma()

	written, err := dst.Write(stagingBuf[:int(n)])
	if err != nil {
		return int64(written), err
	}
	if written != int(n) {
		return int64(written), io.ErrShortWrite
	}
	return int64(written), nil
}

func (o *mooncakeBlob) writeChunkedTo(ctx context.Context, dst io.Writer, meta *mooncakeObjectMeta) (int64, error) {
	var total int64
	for offset := int64(0); offset < meta.Size; offset += meta.ChunkSize {
		if err := ctx.Err(); err != nil {
			return total, err
		}

		chunkEnd := min(offset+meta.ChunkSize, meta.Size)
		n, err := o.readChunk(ctx, offset, chunkEnd-offset, dst)
		total += int64(n)
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func (o *mooncakeBlob) readChunk(ctx context.Context, chunkOffset, chunkSize int64, dst io.Writer) (int, error) {
	stagingBuf, releaseStaging, err := getChunkBuffer(ctx)
	if err != nil {
		return 0, err
	}
	defer releaseStaging()

	key := o.chunkKey(chunkOffset)
	numaBuf, n, releaseNuma, err := getIntoNumaBuffer(ctx, o.store, key, uint64(chunkSize))
	if err != nil {
		return int(n), fmt.Errorf("failed to read chunk %s: %w", key, err)
	}
	if int64(n) > chunkSize || int64(n) > int64(len(stagingBuf)) {
		releaseNuma()
		return 0, fmt.Errorf("invalid chunk length for %s: got=%d expected_at_most=%d", key, n, chunkSize)
	}

	copy(stagingBuf[:int(n)], numaBuf.buf[:n])
	releaseNuma()

	written, err := dst.Write(stagingBuf[:int(n)])
	if err != nil {
		return written, fmt.Errorf("failed to write chunk %s: %w", key, err)
	}
	if written != int(n) {
		return written, io.ErrShortWrite
	}
	return written, nil
}

// -----------------------------------------------------------------------------
// Seekable implementation (large objects split into 4 MB chunks)
// -----------------------------------------------------------------------------

type mooncakeObjectMeta struct {
	Size      int64 `json:"size"`
	ChunkSize int64 `json:"chunk_size"`
}

type mooncakeSeekable struct {
	store          *mooncakestore.Store
	path           string
	chunkKeyPrefix string
	metaMu         sync.Mutex
	meta           *mooncakeObjectMeta
	metaErr        error
	metaLoaded     bool
	metaLoading    bool
	metaWait       chan struct{}
}

var (
	_ Seekable        = (*mooncakeSeekable)(nil)
	_ StreamingReader = (*mooncakeSeekable)(nil)
	//_ BufferRegistrar = (*mooncakeSeekable)(nil)
	//_ BatchReader     = (*mooncakeSeekable)(nil)
)

func newMooncakeSeekable(store *mooncakestore.Store, path string) *mooncakeSeekable {
	return &mooncakeSeekable{
		store:          store,
		path:           path,
		chunkKeyPrefix: path + "#c#",
	}
}

func (o *mooncakeSeekable) RegisterBuffer(ptr uintptr, size uint64) error {
	err := o.store.RegisterBuffer(ptr, size)
	if err != nil {
		return fmt.Errorf("mooncake seekable register buffer failed: ptr=%x, size=%d: %w", ptr, size, err)
	}
	return nil
}

func (o *mooncakeSeekable) UnregisterBuffer(ptr uintptr) error {
	err := o.store.UnregisterBuffer(ptr)
	if err != nil {
		return fmt.Errorf("mooncake seekable unregister buffer failed: ptr=%x: %w", ptr, err)
	}
	return nil
}

func (o *mooncakeSeekable) chunkKey(offset int64) string {
	return makeChunkKey(o.chunkKeyPrefix, offset)
}

func (o *mooncakeSeekable) getMeta(ctx context.Context) (*mooncakeObjectMeta, error) {
	if meta, ok := loadCachedMetadata(o.path); ok {
		return meta, nil
	}

	for {
		o.metaMu.Lock()
		if o.metaLoaded {
			meta, err := o.meta, o.metaErr
			o.metaMu.Unlock()
			return meta, err
		}
		if o.metaLoading {
			wait := o.metaWait
			o.metaMu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		o.metaLoading = true
		o.metaWait = make(chan struct{})
		wait := o.metaWait
		o.metaMu.Unlock()

		meta, err, cache := o.loadMeta(ctx)

		o.metaMu.Lock()
		if cache {
			o.meta = meta
			o.metaErr = err
			o.metaLoaded = true
			if err == nil {
				storeCachedMetadata(o.path, meta)
			}
		}
		o.metaLoading = false
		o.metaWait = nil
		close(wait)
		o.metaMu.Unlock()
		return meta, err
	}
}

func (o *mooncakeSeekable) loadMeta(ctx context.Context) (*mooncakeObjectMeta, error, bool) {
	numaBuf, n, release, err := getIntoNumaBuffer(ctx, o.store, o.path, uint64(4096))
	if err != nil {
		exists, existsErr := o.store.Exists(o.path)
		if existsErr == nil && !exists {
			// A high-concurrency Mooncake lookup can transiently report a
			// missing object. Return the state to this caller, but do not poison
			// the seekable instance's metadata cache permanently.
			return nil, ErrObjectNotExist, false
		}
		return nil, fmt.Errorf("failed to read metadata for %s: %w", o.path, err), false
	}
	defer release()

	var meta mooncakeObjectMeta
	if err := json.Unmarshal(numaBuf.buf[:n], &meta); err != nil {
		return nil, fmt.Errorf("failed to parse metadata for %s: %w", o.path, err), true
	}
	if meta.ChunkSize <= 0 || meta.ChunkSize > numaBufferSize || meta.Size < 0 {
		return nil, fmt.Errorf("invalid metadata for %s: %+v", o.path, meta), true
	}
	return &meta, nil, true
}

func (o *mooncakeSeekable) Size(ctx context.Context) (int64, error) {
	meta, err := o.getMeta(ctx)
	if err != nil {
		return 0, err
	}
	return meta.Size, nil
}

func (o *mooncakeSeekable) ReadAt(ctx context.Context, buff []byte, off int64) (int, error) {
	meta, err := o.getMeta(ctx)
	if err != nil {
		return 0, err
	}
	if off >= meta.Size {
		return 0, io.EOF
	}
	if off < 0 {
		return 0, errors.New("negative offset")
	}

	remaining := min(int64(len(buff)), meta.Size-off)

	return o.readAtSingleChunk(ctx, buff, off, remaining, meta)
}

func (o *mooncakeSeekable) readAtSingleChunk(ctx context.Context, buff []byte, off int64, remaining int64, meta *mooncakeObjectMeta) (int, error) {
	chunkOffset := (off / meta.ChunkSize) * meta.ChunkSize
	chunkEnd := min(chunkOffset+meta.ChunkSize, meta.Size)
	chunkSize := chunkEnd - chunkOffset

	startInChunk := off - chunkOffset
	data, err := o.readChunk(ctx, chunkOffset, chunkSize, buff, startInChunk)
	if err != nil {
		return 0, fmt.Errorf("failed to read chunk at %d: %w", chunkOffset, err)
	}

	copied := min(len(data), int(remaining))

	if copied < len(buff) && off+int64(copied) >= meta.Size {
		return copied, io.EOF
	}
	return copied, nil
}

func (o *mooncakeSeekable) readChunk(ctx context.Context, chunkOffset, chunkSize int64, buff []byte, startInChunk int64) ([]byte, error) {
	key := o.chunkKey(chunkOffset)

	numaBuf, n, release, err := getIntoNumaBuffer(ctx, o.store, key, uint64(chunkSize))
	if err != nil {
		return nil, err
	}
	if startInChunk >= int64(n) {
		release()
		return nil, io.EOF
	}

	endInChunk := min(int64(n), startInChunk+int64(len(buff)))
	copied := copy(buff, numaBuf.buf[startInChunk:endInChunk])
	release()
	return buff[:copied], nil
}

// BatchReadInto implements the BatchReader interface for Mooncake backend.
// It reads multiple chunks in a single BatchGetInto call, writing data directly
// into the caller-provided memory regions.
func (o *mooncakeSeekable) BatchReadInto(ctx context.Context, offsets []int64, sizes []int64, ptrs []uintptr) ([]int, error) {
	if len(offsets) == 0 {
		return []int{}, nil
	}

	// Get metadata to know the object size for boundary checks
	meta, err := o.getMeta(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get object meta: %w", err)
	}

	// Build chunk keys for each offset
	keys := make([]string, len(offsets))
	sizeArray := make([]uint64, len(sizes))
	for i, offset := range offsets {
		keys[i] = o.chunkKey(offset)
		// Adjust size if it exceeds object boundary
		chunkEnd := min(offset+sizes[i], meta.Size)
		actualSize := chunkEnd - offset
		sizeArray[i] = uint64(actualSize)
	}

	lengths, err := batchGetInto(ctx, o.store, keys, ptrs, sizeArray)
	if err != nil {
		return nil, fmt.Errorf("failed to batch get chunks: %w", err)
	}

	// Convert uint64 lengths to int
	result := make([]int, len(lengths))
	for i, l := range lengths {
		result[i] = int(l)
	}

	return result, nil
}

// mooncakeRangeReader provides a streaming io.ReadCloser for range requests.
// It reads chunks on-demand instead of loading the entire range into memory.
type mooncakeRangeReader struct {
	ctx        context.Context
	o          *mooncakeSeekable
	off        int64 // current read offset within the object
	end        int64 // exclusive end offset
	meta       *mooncakeObjectMeta
	chunkBuf   []byte
	releaseBuf func()
	chunkOff   int64
}

func (r *mooncakeRangeReader) Read(p []byte) (int, error) {
	if r.off >= r.end {
		r.releaseChunkBuffer()
		return 0, io.EOF
	}

	want := int64(len(p))
	remaining := r.end - r.off
	if want > remaining {
		want = remaining
	}
	if want == 0 {
		return 0, io.EOF
	}

	var total int
	for want > 0 && r.off < r.end {
		// Load the chunk containing the current offset
		chunkOffset := (r.off / r.meta.ChunkSize) * r.meta.ChunkSize
		if r.chunkBuf == nil || r.chunkOff != chunkOffset {
			r.releaseChunkBuffer()

			chunkEnd := min(chunkOffset+r.meta.ChunkSize, r.meta.Size)
			chunkSize := chunkEnd - chunkOffset

			buf, releaseBuf, err := getChunkBuffer(r.ctx)
			if err != nil {
				if total > 0 {
					return total, nil
				}
				return 0, err
			}
			buf = buf[:int(chunkSize)]
			data, err := r.o.readChunk(r.ctx, chunkOffset, chunkSize, buf, 0)
			if err != nil {
				releaseBuf()
				if total > 0 {
					return total, nil
				}
				return 0, err
			}
			r.chunkBuf = data
			r.releaseBuf = releaseBuf
			r.chunkOff = chunkOffset
		}

		startInChunk := r.off - r.chunkOff
		if startInChunk >= int64(len(r.chunkBuf)) {
			if total > 0 {
				return total, nil
			}
			return 0, io.EOF
		}
		endInChunk := min(int64(len(r.chunkBuf)), startInChunk+want)
		copied := copy(p[total:], r.chunkBuf[startInChunk:endInChunk])
		total += copied
		r.off += int64(copied)
		want -= int64(copied)

		if endInChunk >= int64(len(r.chunkBuf)) {
			r.releaseChunkBuffer()
		}
	}

	if r.off >= r.end {
		r.releaseChunkBuffer()
		return total, io.EOF
	}
	return total, nil
}

func (r *mooncakeRangeReader) Close() error {
	r.releaseChunkBuffer()
	return nil
}

func (r *mooncakeRangeReader) releaseChunkBuffer() {
	r.chunkBuf = nil
	r.chunkOff = 0
	if r.releaseBuf != nil {
		r.releaseBuf()
		r.releaseBuf = nil
	}
}

func (o *mooncakeSeekable) OpenRangeReader(ctx context.Context, off, length int64) (io.ReadCloser, error) {
	meta, err := o.getMeta(ctx)
	if err != nil {
		return nil, err
	}
	if off >= meta.Size {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}

	actualLength := min(length, meta.Size-off)
	return &mooncakeRangeReader{
		ctx:  ctx,
		o:    o,
		off:  off,
		end:  off + actualLength,
		meta: meta,
	}, nil
}

func (o *mooncakeSeekable) StoreFile(ctx context.Context, localPath string) error {
	invalidateCachedMetadata(o.path)
	o.metaMu.Lock()
	o.meta = nil
	o.metaErr = nil
	o.metaLoaded = false
	o.metaMu.Unlock()

	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("failed to open file %s: %w", localPath, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat file %s: %w", localPath, err)
	}
	size := info.Size()

	// Upload chunks in parallel.
	g, groupCtx := errgroup.WithContext(ctx)
	g.SetLimit(mooncakeUploadConcurrency)

	for offset := int64(0); offset < size; offset += MemoryChunkSize {
		off := offset
		g.Go(func() error {
			if err := groupCtx.Err(); err != nil {
				return err
			}

			end := min(off+MemoryChunkSize, size)
			buf, releaseBuf, err := getChunkBuffer(groupCtx)
			if err != nil {
				return err
			}
			defer releaseBuf()
			chunk := buf[:int(end-off)]

			if _, err := file.ReadAt(chunk, off); err != nil && !errors.Is(err, io.EOF) {
				return fmt.Errorf("failed to read chunk at %d from %s: %w", off, localPath, err)
			}

			cChunk := C.CBytes(chunk)
			defer C.free(cChunk)
			return o.store.Put(o.chunkKey(off), unsafe.Slice((*byte)(cChunk), len(chunk)), nil)
		})
	}

	if err := g.Wait(); err != nil {
		return fmt.Errorf("failed to upload chunks: %w", err)
	}

	// Write metadata last so that readers see a consistent object only when fully written.
	meta := &mooncakeObjectMeta{Size: size, ChunkSize: MemoryChunkSize}
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}

	if err := o.store.Put(o.path, metaBytes, nil); err != nil {
		return fmt.Errorf("failed to write metadata: %w", err)
	}
	storeCachedMetadata(o.path, meta)

	return nil
}
