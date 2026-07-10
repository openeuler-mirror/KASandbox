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

static inline long get_current_thread_id() {
    return (long)pthread_self();
}

static inline int get_current_numa_node() {
    if (numa_available() < 0) {
        return 0;
    }
    int cpu = sched_getcpu();
    if (cpu < 0) {
        return 0;
    }
    int node = numa_node_of_cpu(cpu);
    if (node < 0 || node >= numa_num_configured_nodes()) {
        return 0;
    }
    return node;
}

static inline int bind_to_socket(int socket_id) {
    if (numa_available() < 0) {
        fprintf(stderr, "NUMA not available, skip binding\n");
        return 0;
    }

    int nr_nodes = numa_num_configured_nodes();
    if (socket_id < 0 || socket_id >= nr_nodes) {
        socket_id = 0;
    }

    cpu_set_t cpu_set;
    CPU_ZERO(&cpu_set);

    struct bitmask* cpu_list = numa_allocate_cpumask();
    if (!cpu_list) {
        fprintf(stderr, "numa_allocate_cpumask failed\n");
        return -1;
    }

    if (numa_node_to_cpus(socket_id, cpu_list) < 0) {
        fprintf(stderr, "numa_node_to_cpus failed for node %d\n", socket_id);
        numa_free_cpumask(cpu_list);
        return -1;
    }

    int nr_possible_cpus = numa_num_possible_cpus();
    int nr_cpus = 0;
    for (int cpu = 0; cpu < nr_possible_cpus; ++cpu) {
        if (numa_bitmask_isbitset(cpu_list, cpu)) {
            CPU_SET(cpu, &cpu_set);
            ++nr_cpus;
        }
    }
    numa_free_cpumask(cpu_list);

    if (nr_cpus > 0) {
        if (sched_setaffinity(0, sizeof(cpu_set_t), &cpu_set) != 0) {
            perror("sched_setaffinity failed");
            return -1;
        }
    }
    return nr_cpus;
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
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sync/errgroup"
	"go.uber.org/zap"
	"github.com/e2b-dev/infra/packages/shared/pkg/env"
	"github.com/kvcache-ai/Mooncake/mooncake-store/go/mooncakestore"
)

const (
	mooncakeOperationTimeout  = 30 * time.Second
	mooncakeUploadConcurrency = 16
	numaBufferSize            = 4 * 1024 * 1024 // 4 MB default buffer size
)

var (
	mooncakeInstances   = make(map[string]*mooncakeStorage)
	mooncakeInstancesMu sync.RWMutex
)

// chunkBufPool is used to reuse chunk buffers for StoreFile and range reads
// to reduce GC pressure.
var chunkBufPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, MemoryChunkSize)
		return &b
	},
}

type storeFactory struct {
	store *mooncakestore.Store
	once  sync.Once
	err   error
}

var globalStoreFactory *storeFactory

func getStoreFactory() (*storeFactory, error) {
	if globalStoreFactory == nil {
		globalStoreFactory = &storeFactory{}
	}
	if globalStoreFactory.err != nil {
		return nil, globalStoreFactory.err
	}
	globalStoreFactory.once.Do(func() {
		store, err := mooncakestore.New()
		if err != nil {
			globalStoreFactory.err = fmt.Errorf("failed to create mooncake store: %w", err)
			return
		}

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
			store.Close()
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
				store.Close()
				globalStoreFactory.err = fmt.Errorf("failed to init mooncake segments: %w", err)
				return
			}
		}

		globalStoreFactory.store = store
	})
	return globalStoreFactory, globalStoreFactory.err
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
			t := time.Now()
			_ = nb.store.UnregisterBuffer(uintptr(nb.cptr))
			zap.L().Sugar().Infof("[MooncakeStorage] [NUMA] UnregisterBuffer node=%d size=%d cost_ms=%.3f",
				nb.node, nb.size, time.Since(t).Seconds()*1000)
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
	} else {
		nrSockets = 1
	}
}

type threadLocalNumaBuffer struct {
	mu     sync.Mutex
	buffer *numaBuffer
	node   int
}

var threadBuffers sync.Map
var isShutdown atomic.Bool // Global shutdown flag to prevent buffer use during shutdown

// getOrCreateNumaBuffer returns a NUMA-local buffer for the current OS thread.
// The caller MUST call the returned release function (usually with defer) to
// unlock the OS thread. The buffer itself is kept in thread-local storage for
// reuse and is only freed at process shutdown.
// Returns error if shutdown has been initiated to prevent memory corruption.
func getOrCreateNumaBuffer(store *mooncakestore.Store) (*numaBuffer, func(), error) {
	// Check shutdown flag first to prevent using buffers during shutdown
	if isShutdown.Load() {
		return nil, nil, fmt.Errorf("mooncake storage is shutting down, cannot acquire NUMA buffer")
	}

	runtime.LockOSThread()
	threadID := C.get_current_thread_id()

	if val, ok := threadBuffers.Load(threadID); ok {
		tlb := val.(*threadLocalNumaBuffer)
		tlb.mu.Lock()
		defer tlb.mu.Unlock()
		if tlb.buffer != nil {
			// Double-check shutdown flag after acquiring lock
			if isShutdown.Load() {
				return nil, nil, fmt.Errorf("mooncake storage is shutting down")
			}
			return tlb.buffer, runtime.UnlockOSThread, nil
		}
	}

	// Use the actual NUMA node where the current thread is running instead of
	// hashing by thread ID. Falls back to thread ID modulo if detection fails.
	node := int(C.get_current_numa_node())
	if node < 0 || node >= nrSockets {
		node = int(int64(threadID) % int64(nrSockets))
	}

	C.bind_to_socket(C.int(node))

	cptr := C.numa_alloc_onnode_wrapper(C.size_t(numaBufferSize), C.int(node))
	if cptr == nil {
		runtime.UnlockOSThread()
		return nil, nil, fmt.Errorf("numa_alloc_onnode failed: size=%d, node=%d", numaBufferSize, node)
	}

	nb := &numaBuffer{
		buf:   unsafe.Slice((*byte)(cptr), numaBufferSize),
		cptr:  cptr,
		size:  numaBufferSize,
		node:  node,
		store: store,
	}

	t := time.Now()
	if err := store.RegisterBuffer(uintptr(cptr), uint64(numaBufferSize)); err != nil {
		nb.free()
		runtime.UnlockOSThread()
		return nil, nil, fmt.Errorf("failed to register buffer: %w", err)
	}
	zap.L().Sugar().Infof("[MooncakeStorage] [NUMA] RegisterBuffer thread_id=%d node=%d size=%d cost_ms=%.3f",
		threadID, node, numaBufferSize, time.Since(t).Seconds()*1000)

	tlb := &threadLocalNumaBuffer{
		buffer: nb,
		node:   node,
	}
	threadBuffers.Store(threadID, tlb)

	return nb, runtime.UnlockOSThread, nil
}

// releaseAllThreadBuffers releases all NUMA buffers and clears thread-local storage.
// Should be called at process shutdown.
// Sets shutdown flag first to prevent new buffer acquisitions during release.
func releaseAllThreadBuffers() {
	// Set shutdown flag first to prevent new buffer acquisitions
	isShutdown.Store(true)

	threadBuffers.Range(func(key, value interface{}) bool {
		tlb := value.(*threadLocalNumaBuffer)
		tlb.mu.Lock()
		if tlb.buffer != nil {
			tlb.buffer.free()
			tlb.buffer = nil
		}
		tlb.mu.Unlock()
		threadBuffers.Delete(key)
		return true
	})
}

// Shutdown releases all NUMA buffers and closes all mooncake storage instances.
// Call this before process exit to avoid resource leaks in long-running processes.
func Shutdown() {
	releaseAllThreadBuffers()

	mooncakeInstancesMu.Lock()
	defer mooncakeInstancesMu.Unlock()
	for _, instance := range mooncakeInstances {
		if instance.store != nil {
			instance.store.Close()
		}
	}
	clear(mooncakeInstances)
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
	zap.L().Sugar().Infof("[MooncakeStorage] [NewMooncakeStorageProvider] pid=%d thread_id=%d bucket=%s",
		os.Getpid(), C.get_current_thread_id(), bucketName)
	return instance, nil
}

func (s *mooncakeStorage) key(path string) string {
	if s.bucketName == "" {
		return path
	}
	return s.bucketName + "/" + path
}

func (s *mooncakeStorage) DeleteObjectsWithPrefix(ctx context.Context, prefix string) error {
	start := time.Now()
	defer func() {
		zap.L().Sugar().Infof("[MooncakeStorage] [DeleteObjectsWithPrefix] prefix=%s total_cost_ms=%.3f",
			prefix, time.Since(start).Seconds()*1000)
	}()

	ctx, cancel := context.WithTimeout(ctx, mooncakeOperationTimeout)
	defer cancel()

	// Mooncake has flat keys — use regex matching over the prefix.
	// WARNING: This may scan all keys; avoid frequent use with large buckets.
	pattern := "^" + regexp.QuoteMeta(s.key(prefix))
	if !strings.HasSuffix(prefix, "/") {
		pattern += ".*"
	}

	t := time.Now()
	_, err := s.store.RemoveByRegex(pattern, true)
	zap.L().Sugar().Infof("[MooncakeStorage] [DeleteObjectsWithPrefix] phase=remove_by_regex prefix=%s cost_ms=%.3f",
		prefix, time.Since(t).Seconds()*1000)
	if err != nil {
		return fmt.Errorf("failed to delete objects with prefix %s: %w", prefix, err)
	}
	return nil
}

func (s *mooncakeStorage) UploadSignedURL(_ context.Context, _ string, _ time.Duration) (string, error) {
	return "", fmt.Errorf("mooncake storage does not support signed URLs")
}

func (s *mooncakeStorage) OpenSeekable(_ context.Context, path string, _ SeekableObjectType) (Seekable, error) {
	zap.L().Sugar().Infof("[MooncakeStorage] [OpenSeekable] path=%s", path)
	return newMooncakeSeekable(s.store, s.key(path)), nil
}

func (s *mooncakeStorage) OpenBlob(_ context.Context, path string, _ ObjectType) (Blob, error) {
	zap.L().Sugar().Infof("[MooncakeStorage] [OpenBlob] path=%s", path)
	return &mooncakeBlob{
		store: s.store,
		path:  s.key(path),
	}, nil
}

func (s *mooncakeStorage) GetDetails() string {
	return fmt.Sprintf("[Mooncake Storage, prefix set to %s]", s.bucketName)
}

// -----------------------------------------------------------------------------
// Blob implementation (small objects: headers, metadata, etc.)
// -----------------------------------------------------------------------------

type mooncakeBlob struct {
	store *mooncakestore.Store
	path  string
}

var _ Blob = (*mooncakeBlob)(nil)

func (o *mooncakeBlob) Put(_ context.Context, data []byte) error {
	start := time.Now()
	defer func() {
		zap.L().Sugar().Infof("[MooncakeStorage] [Blob.Put] path=%s size=%d total_cost_ms=%.3f",
			o.path, len(data), time.Since(start).Seconds()*1000)
	}()

	t := time.Now()
	err := o.store.Put(o.path, data, nil)
	zap.L().Sugar().Infof("[MooncakeStorage] [Blob.Put] phase=store_put path=%s size=%d cost_ms=%.3f",
		o.path, len(data), time.Since(t).Seconds()*1000)
	return err
}

func (o *mooncakeBlob) WriteTo(ctx context.Context, dst io.Writer) (int64, error) {
	start := time.Now()
	defer func() {
		zap.L().Sugar().Infof("[MooncakeStorage] [Blob.WriteTo] path=%s total_cost_ms=%.3f",
			o.path, time.Since(start).Seconds()*1000)
	}()

	ctx, cancel := context.WithTimeout(ctx, mooncakeOperationTimeout)
	defer cancel()

	t := time.Now()
	size, err := o.store.GetSize(o.path)
	zap.L().Sugar().Infof("[MooncakeStorage] [Blob.WriteTo] phase=get_size path=%s cost_ms=%.3f",
		o.path, time.Since(t).Seconds()*1000)
	if err != nil {
		exists, _ := o.store.Exists(o.path)
		if !exists {
			return 0, ErrObjectNotExist
		}
		return 0, fmt.Errorf("failed to get object size: %w", err)
	}
	if size == 0 {
		return 0, nil
	}

	numaBuf, release, err := getOrCreateNumaBuffer(o.store)
	if err != nil {
		return 0, fmt.Errorf("failed to get NUMA buffer: %w", err)
	}
	defer release()

	if int(size) > len(numaBuf.buf) {
		buf := make([]byte, size)
		t := time.Now()
		n, err := o.store.GetInto(o.path, uintptr(unsafe.Pointer(&buf[0])), uint64(len(buf)))
		zap.L().Sugar().Infof("[MooncakeStorage] [Blob.WriteTo] phase=get_into_large path=%s size=%d cost_ms=%.3f",
			o.path, len(buf), time.Since(t).Seconds()*1000)
		if err != nil {
			return int64(n), fmt.Errorf("failed to get object: %w", err)
		}
		t = time.Now()
		written, err := dst.Write(buf[:n])
		zap.L().Sugar().Infof("[MooncakeStorage] [Blob.WriteTo] phase=dst_write path=%s size=%d cost_ms=%.3f",
			o.path, written, time.Since(t).Seconds()*1000)
		return int64(written), err
	}

	t = time.Now()
	n, err := o.store.GetInto(o.path, uintptr(unsafe.Pointer(&numaBuf.buf[0])), uint64(size))
	zap.L().Sugar().Infof("[MooncakeStorage] [Blob.WriteTo] phase=get_into_numa path=%s size=%d cost_ms=%.3f",
		o.path, size, time.Since(t).Seconds()*1000)
	if err != nil {
		return int64(n), fmt.Errorf("failed to get object: %w", err)
	}
	t = time.Now()
	written, err := dst.Write(numaBuf.buf[:n])
	zap.L().Sugar().Infof("[MooncakeStorage] [Blob.WriteTo] phase=dst_write path=%s size=%d cost_ms=%.3f",
		o.path, written, time.Since(t).Seconds()*1000)
	return int64(written), err
}

func (o *mooncakeBlob) Exists(_ context.Context) (bool, error) {
	return o.store.Exists(o.path)
}

// -----------------------------------------------------------------------------
// Seekable implementation (large objects split into 4 MB chunks)
// -----------------------------------------------------------------------------

type mooncakeObjectMeta struct {
	Size      int64 `json:"size"`
	ChunkSize int64 `json:"chunk_size"`
}

type mooncakeSeekable struct {
	store   *mooncakestore.Store
	path    string
	meta    *mooncakeObjectMeta
	metaErr error
	once    sync.Once
}

var (
	_ Seekable        = (*mooncakeSeekable)(nil)
	_ StreamingReader = (*mooncakeSeekable)(nil)
	//_ BufferRegistrar = (*mooncakeSeekable)(nil)
	//_ BatchReader     = (*mooncakeSeekable)(nil)
)

func newMooncakeSeekable(store *mooncakestore.Store, path string) *mooncakeSeekable {
	return &mooncakeSeekable{
		store: store,
		path:  path,
	}
}

func (o *mooncakeSeekable) RegisterBuffer(ptr uintptr, size uint64) error {
	t := time.Now()
	err := o.store.RegisterBuffer(ptr, size)
	if err != nil {
		return fmt.Errorf("mooncake seekable register buffer failed: ptr=%x, size=%d: %w", ptr, size, err)
	}
	zap.L().Sugar().Infof("[MooncakeSeekable] RegisterBuffer ptr=%x, size=%d, cost=%.3f ms", ptr, size, time.Since(t).Seconds()*1000)
	return nil
}

func (o *mooncakeSeekable) UnregisterBuffer(ptr uintptr) error {
	t := time.Now()
	err := o.store.UnregisterBuffer(ptr)
	if err != nil {
		return fmt.Errorf("mooncake seekable unregister buffer failed: ptr=%x: %w", ptr, err)
	}
	zap.L().Sugar().Infof("[MooncakeSeekable] UnregisterBuffer ptr=%x, cost=%.3f ms", ptr, time.Since(t).Seconds()*1000)
	return nil
}

func (o *mooncakeSeekable) chunkKey(offset int64) string {
	return fmt.Sprintf("%s#c#%d", o.path, offset)
}

func (o *mooncakeSeekable) getMeta() (*mooncakeObjectMeta, error) {
	o.once.Do(func() {
		numaBuf, release, err := getOrCreateNumaBuffer(o.store)
		if err != nil {
			o.metaErr = fmt.Errorf("failed to get NUMA buffer: %w", err)
			return
		}
		defer release()

		t := time.Now()
		n, err := o.store.GetInto(o.path, uintptr(unsafe.Pointer(&numaBuf.buf[0])), uint64(4096))
		zap.L().Sugar().Infof("[MooncakeStorage] [Seekable.getMeta] phase=get_into path=%s cost_ms=%.3f",
			o.path, time.Since(t).Seconds()*1000)
		if err != nil {
			exists, _ := o.store.Exists(o.path)
			if !exists {
				o.metaErr = ErrObjectNotExist
				return
			}
			o.metaErr = fmt.Errorf("failed to read metadata for %s: %w", o.path, err)
			return
		}
		var meta mooncakeObjectMeta
		if err := json.Unmarshal(numaBuf.buf[:n], &meta); err != nil {
			o.metaErr = fmt.Errorf("failed to parse metadata for %s: %w", o.path, err)
			return
		}
		o.meta = &meta
	})
	return o.meta, o.metaErr
}

func (o *mooncakeSeekable) Size(_ context.Context) (int64, error) {
	meta, err := o.getMeta()
	if err != nil {
		return 0, err
	}
	return meta.Size, nil
}

func (o *mooncakeSeekable) ReadAt(_ context.Context, buff []byte, off int64) (int, error) {
	start := time.Now()
	defer func() {
		zap.L().Sugar().Infof("[MooncakeStorage] [Seekable.ReadAt] path=%s offset=%d size=%d total_cost_ms=%.3f",
			o.path, off, len(buff), time.Since(start).Seconds()*1000)
	}()

	meta, err := o.getMeta()
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

	return o.readAtSingleChunk(buff, off, remaining, meta)
}

func (o *mooncakeSeekable) readAtSingleChunk(buff []byte, off int64, remaining int64, meta *mooncakeObjectMeta) (int, error) {
	start := time.Now()
	defer func() {
		zap.L().Sugar().Infof("[MooncakeStorage] [Seekable.readAtSingleChunk] path=%s offset=%d total_cost_ms=%.3f",
			o.path, off, time.Since(start).Seconds()*1000)
	}()

	chunkOffset := (off / meta.ChunkSize) * meta.ChunkSize
	chunkEnd := min(chunkOffset+meta.ChunkSize, meta.Size)
	chunkSize := chunkEnd - chunkOffset

	data, err := o.readChunk(chunkOffset, chunkSize, buff)
	if err != nil {
		return 0, fmt.Errorf("failed to read chunk at %d: %w", chunkOffset, err)
	}

	startInChunk := off - chunkOffset
	if startInChunk >= int64(len(data)) {
		return 0, io.EOF
	}
	endInChunk := min(int64(len(data)), startInChunk+remaining)
	copied := int(endInChunk - startInChunk)

	if copied < len(buff) && off+int64(copied) >= meta.Size {
		return copied, io.EOF
	}
	return copied, nil
}

func (o *mooncakeSeekable) readChunk(chunkOffset, chunkSize int64, buff []byte) ([]byte, error) {
	start := time.Now()
	defer func() {
		zap.L().Sugar().Infof("[MooncakeStorage] [Seekable.readChunk] path=%s chunk_offset=%d size=%d total_cost_ms=%.3f",
			o.path, chunkOffset, chunkSize, time.Since(start).Seconds()*1000)
	}()

	key := o.chunkKey(chunkOffset)

	numaBuf, unlock, err := getOrCreateNumaBuffer(o.store)
	if err != nil {
		buf := make([]byte, chunkSize)
		t := time.Now()
		n, err := o.store.GetInto(key, uintptr(unsafe.Pointer(&buf[0])), uint64(len(buf)))
		zap.L().Sugar().Infof("[MooncakeSeekable] readChunk GetInto(fallback) key: %s, size: %d, cost: %.3f ms", key, len(buf), time.Since(t).Seconds()*1000)
		if err != nil {
			return nil, err
		}
		return buf[:n], nil
	}
	defer unlock()

	t := time.Now()
	n, err := o.store.GetInto(key, uintptr(unsafe.Pointer(&numaBuf.buf[0])), uint64(chunkSize))
	zap.L().Sugar().Infof("[MooncakeSeekable] readChunk GetInto(numa) key: %s, size: %d, cost: %.3f ms", key, chunkSize, time.Since(t).Seconds()*1000)
	if err != nil {
		return nil, err
	}
	// Copy from NUMA buffer to avoid race conditions with other threads
	copy(buff, numaBuf.buf[:n])
	return buff, nil
}

// BatchReadInto implements the BatchReader interface for Mooncake backend.
// It reads multiple chunks in a single BatchGetInto call, writing data directly
// into the caller-provided memory regions.
func (o *mooncakeSeekable) BatchReadInto(ctx context.Context, offsets []int64, sizes []int64, ptrs []uintptr) ([]int, error) {
	if len(offsets) == 0 {
		return []int{}, nil
	}

	// Get metadata to know the object size for boundary checks
	meta, err := o.getMeta()
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

	t := time.Now()
	lengths, err := o.store.BatchGetInto(keys, ptrs, sizeArray)
	zap.L().Sugar().Infof("[MooncakeStorage] [Seekable.BatchReadInto] phase=batch_get_into keys=%d cost_ms=%.3f",
		len(keys), time.Since(t).Seconds()*1000)
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
	o        *mooncakeSeekable
	off      int64 // current read offset within the object
	end      int64 // exclusive end offset
	meta     *mooncakeObjectMeta
	chunkBuf []byte
	chunkOff int64
}

func (r *mooncakeRangeReader) Read(p []byte) (int, error) {
	if r.off >= r.end {
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
			chunkEnd := min(chunkOffset+r.meta.ChunkSize, r.meta.Size)
			chunkSize := chunkEnd - chunkOffset

			// Allocate buffer for readChunk to fill
			buf := make([]byte, chunkSize)
			t := time.Now()
			data, err := r.o.readChunk(chunkOffset, chunkSize, buf)
			zap.L().Sugar().Infof("[MooncakeStorage] [RangeReader.Read] phase=read_chunk path=%s chunk_offset=%d cost_ms=%.3f",
				r.o.path, chunkOffset, time.Since(t).Seconds()*1000)
			if err != nil {
				if total > 0 {
					return total, nil
				}
				return 0, err
			}
			r.chunkBuf = data
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
			r.chunkBuf = nil // consumed current chunk
		}
	}

	if r.off >= r.end {
		return total, io.EOF
	}
	return total, nil
}

func (r *mooncakeRangeReader) Close() error {
	r.chunkBuf = nil
	return nil
}

func (o *mooncakeSeekable) OpenRangeReader(ctx context.Context, off, length int64) (io.ReadCloser, error) {
	start := time.Now()
	defer func() {
		zap.L().Sugar().Infof("[MooncakeStorage] [Seekable.OpenRangeReader] path=%s off=%d length=%d total_cost_ms=%.3f",
			o.path, off, length, time.Since(start).Seconds()*1000)
	}()

	meta, err := o.getMeta()
	if err != nil {
		return nil, err
	}
	if off >= meta.Size {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}

	actualLength := min(length, meta.Size-off)
	return &mooncakeRangeReader{
		o:    o,
		off:  off,
		end:  off + actualLength,
		meta: meta,
	}, nil
}

func (o *mooncakeSeekable) StoreFile(ctx context.Context, localPath string) error {
	start := time.Now()
	defer func() {
		zap.L().Sugar().Infof("[MooncakeStorage] [Seekable.StoreFile] path=%s local_path=%s total_cost_ms=%.3f",
			o.path, localPath, time.Since(start).Seconds()*1000)
	}()

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
	g, _ := errgroup.WithContext(ctx)
	g.SetLimit(mooncakeUploadConcurrency)

	for offset := int64(0); offset < size; offset += MemoryChunkSize {
		off := offset
		g.Go(func() error {
			end := min(off+MemoryChunkSize, size)
			chunk := make([]byte, end-off)

			t := time.Now()
			if _, err := file.ReadAt(chunk, off); err != nil && !errors.Is(err, io.EOF) {
				return fmt.Errorf("failed to read chunk at %d from %s: %w", off, localPath, err)
			}
			readCost := time.Since(t).Seconds() * 1000

			cChunk := C.CBytes(chunk)  // 分配 C 堆内存并拷贝
			t = time.Now()
			err := o.store.Put(o.chunkKey(off), unsafe.Slice((*byte)(cChunk), len(chunk)), nil)
			defer C.free(cChunk)       // 确保释放（如果 Mooncake 同步拷贝）
			zap.L().Sugar().Infof("[MooncakeStorage] [Seekable.StoreFile] phase=put_chunk path=%s chunk_offset=%d size=%d read_cost_ms=%.3f put_cost_ms=%.3f",
				o.path, off, len(chunk), readCost, time.Since(t).Seconds()*1000)
			return err
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

	t := time.Now()
	if err := o.store.Put(o.path, metaBytes, nil); err != nil {
		return fmt.Errorf("failed to write metadata: %w", err)
	}
	zap.L().Sugar().Infof("[MooncakeStorage] [Seekable.StoreFile] phase=put_metadata path=%s size=%d cost_ms=%.3f",
		o.path, len(metaBytes), time.Since(t).Seconds()*1000)

	return nil
}
