package block

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

type Overlay struct {
	// mu guards device and cache, which Seal replaces underneath the NBD
	// dispatchers while the VM is paused. Reads and writes hold it shared;
	// their real synchronization is the caches' own locking.
	mu           sync.RWMutex
	device       ReadonlyDevice
	cache        *Cache
	cacheEjected atomic.Bool
	// layersEjected is set by EjectLayers: the sealed layers under this
	// overlay have been handed to the caller, so Close leaves them mapped.
	layersEjected atomic.Bool
	blockSize     int64
}

var _ Device = (*Overlay)(nil)

func NewOverlay(device ReadonlyDevice, cache *Cache) *Overlay {
	blockSize := device.BlockSize()

	return &Overlay{
		device:    device,
		cache:     cache,
		blockSize: blockSize,
	}
}

func (o *Overlay) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	o.mu.RLock()
	device, cache := o.device, o.cache
	o.mu.RUnlock()

	blocks := header.BlocksOffsets(int64(len(p)), o.blockSize)

	for _, blockOff := range blocks {
		n, err := cache.ReadAt(p[blockOff:blockOff+o.blockSize], off+blockOff)
		if err == nil {
			continue
		}

		if !errors.As(err, &BytesNotAvailableError{}) {
			return n, fmt.Errorf("error reading from cache: %w", err)
		}

		n, err = device.ReadAt(ctx, p[blockOff:blockOff+o.blockSize], off+blockOff)
		if err != nil {
			return n, fmt.Errorf("error reading from device: %w", err)
		}
	}

	return len(p), nil
}

func (o *Overlay) EjectCache() (*Cache, error) {
	if !o.cacheEjected.CompareAndSwap(false, true) {
		return nil, fmt.Errorf("cache already ejected")
	}

	o.mu.RLock()
	defer o.mu.RUnlock()

	return o.cache, nil
}

// EjectLayers is EjectCache for an overlay that may have been sealed. It
// returns every write layer the sandbox has produced, bottom-most first: the
// sealed layers in the order they were sealed, then the current write layer
// last. That is the whole set of blocks the sandbox has written on top of its
// template — what a snapshot of the sandbox has to carry, and what
// EjectCache alone stops describing after the first Seal, when the earlier
// layers have moved under the write layer as SealedViews.
//
// An overlay that was never sealed returns exactly the one cache EjectCache
// would. Like EjectCache, this takes the layers off Close: the top layer is
// the caller's to Close, the sealed ones (whose files belong to the
// checkpoint store) the caller's to CloseKeepFile.
func (o *Overlay) EjectLayers() ([]*Cache, error) {
	if !o.cacheEjected.CompareAndSwap(false, true) {
		return nil, fmt.Errorf("cache already ejected")
	}

	o.mu.RLock()
	defer o.mu.RUnlock()

	// The chain is walked from the newest sealed layer down to the oldest.
	var layers []*Cache
	for view, ok := o.device.(*SealedView); ok; view, ok = view.device.(*SealedView) {
		layers = append(layers, view.cache)
	}
	slices.Reverse(layers)
	o.layersEjected.Store(true)

	return append(layers, o.cache), nil
}

// Seal freezes the current write layer and installs newCache as a fresh empty
// one. The sealed layer stays on the read path: it is folded into the device
// below as a SealedView, exactly like OverlayFS demoting an upper to a
// read-only lower and opening a new upper.
//
// The caller must have quiesced writes first — VM paused and the NBD device
// flushed — so the sealed layer is a crash-consistent image of the disk at
// the pause instant. Returns the sealed layer's cache; its file now holds the
// layer and must not be deleted with Close (use CloseKeepFile).
func (o *Overlay) Seal(newCache *Cache) (*Cache, error) {
	if o.cacheEjected.Load() {
		return nil, fmt.Errorf("cache already ejected")
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	sealed := o.cache
	o.device = NewSealedView(o.device, sealed)
	o.cache = newCache

	return sealed, nil
}

// ResetView replaces the overlay's entire view in one swap: the read path
// below and the write layer above. It is the disk half of an in-place
// rollback — the NBD dispatchers keep their Device pointer (this overlay),
// while everything behind it becomes the restored checkpoint's stack plus a
// fresh write layer.
//
// The old write layer belongs to the timeline being discarded and is closed
// (its file deleted); old sealed views are unmapped, their files staying with
// the checkpoint store. The caller must have quiesced I/O — VM paused, NBD
// flushed — exactly as for Seal.
func (o *Overlay) ResetView(device ReadonlyDevice, cache *Cache) error {
	if o.cacheEjected.Load() {
		return fmt.Errorf("cache already ejected")
	}

	o.mu.Lock()
	oldDevice, oldCache := o.device, o.cache
	o.device = device
	o.cache = cache
	o.mu.Unlock()

	var errs []error

	// The discarded timeline's write layer: file and all.
	errs = append(errs, oldCache.Close())

	// Sealed layers of the old chain: unmap only, the files belong to the
	// checkpoint store and other checkpoints' views may reference them.
	if view, ok := oldDevice.(*SealedView); ok {
		errs = append(errs, view.Close())
	}

	return errors.Join(errs...)
}

// This method will not be very optimal if the length is not the same as the block size, because we cannot be just exposing the cache slice,
// but creating and copying the bytes from the cache and device to the new slice.
//
// When we are implementing this we might want to just enforce the length to be the same as the block size.
func (o *Overlay) Slice(_ context.Context, _, _ int64) ([]byte, error) {
	return nil, fmt.Errorf("not implemented")
}

func (o *Overlay) WriteAt(p []byte, off int64) (int, error) {
	o.mu.RLock()
	cache := o.cache
	o.mu.RUnlock()

	return cache.WriteAt(p, off)
}

func (o *Overlay) Size(_ context.Context) (int64, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()

	return o.cache.Size()
}

func (o *Overlay) BlockSize() int64 {
	return o.blockSize
}

func (o *Overlay) Close() error {
	o.mu.RLock()
	device, cache := o.device, o.cache
	o.mu.RUnlock()

	var errs []error

	if !o.cacheEjected.Load() {
		errs = append(errs, cache.Close())
	}

	// Sealed layers accumulated under this overlay are unmapped here; their
	// files belong to the checkpoint store and stay. Unless EjectLayers
	// handed them out, in which case they are still being read.
	if view, ok := device.(*SealedView); ok && !o.layersEjected.Load() {
		errs = append(errs, view.Close())
	}

	return errors.Join(errs...)
}

func (o *Overlay) Header() *header.Header {
	o.mu.RLock()
	defer o.mu.RUnlock()

	return o.device.Header()
}
