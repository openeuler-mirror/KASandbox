//go:build linux

package rootfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/nbd"
	"github.com/e2b-dev/infra/packages/shared/pkg/featureflags"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

type NBDProvider struct {
	overlay      *block.Overlay
	mnt          *nbd.DirectPathMount
	featureFlags *featureflags.Client

	ready *utils.SetOnce[string]

	blockSize int64
	// cachePath is the path of the initial writable cache; fresh caches created
	// by SwapForBackgroundSeal derive a unique path from it.
	cachePath string
	sealGen   atomic.Int64

	finishedOperations chan struct{}
	devicePool         *nbd.DevicePool
}

func NewNBDProvider(ctx context.Context, rootfs block.ReadonlyDevice, cachePath string, devicePool *nbd.DevicePool, featureFlags *featureflags.Client) (Provider, error) {
	size, err := rootfs.Size(ctx)
	if err != nil {
		return nil, fmt.Errorf("error getting device size: %w", err)
	}

	blockSize := rootfs.BlockSize()

	cache, err := block.NewCache(size, blockSize, cachePath, false)
	if err != nil {
		return nil, fmt.Errorf("error creating cache: %w", err)
	}

	overlay := block.NewOverlay(rootfs, cache)

	mnt := nbd.NewDirectPathMount(overlay, devicePool, featureFlags)

	return &NBDProvider{
		mnt:                mnt,
		overlay:            overlay,
		featureFlags:       featureFlags,
		ready:              utils.NewSetOnce[string](),
		finishedOperations: make(chan struct{}, 1),
		blockSize:          blockSize,
		cachePath:          cachePath,
		devicePool:         devicePool,
	}, nil
}

func (o *NBDProvider) Start(ctx context.Context) error {
	deviceIndex, err := o.mnt.Open(ctx)
	if err != nil {
		return o.ready.SetError(fmt.Errorf("error opening overlay file: %w", err))
	}

	return o.ready.SetValue(nbd.GetDevicePath(deviceIndex))
}

// ejectAndStopSandbox detaches the writable cache from the overlay, stops the
// sandbox and waits for the overlay device to be released, returning the ejected
// (now standalone, frozen) cache. The caller owns the returned cache and must
// Close it. Shared by the synchronous ExportDiff and the deferred
// PrepareExportDiff.
func (o *NBDProvider) ejectAndStopSandbox(
	ctx context.Context,
	closeSandbox func(ctx context.Context) error,
) (*block.Cache, error) {
	cache, err := o.overlay.EjectCache()
	if err != nil {
		return nil, fmt.Errorf("error ejecting cache: %w", err)
	}

	// the error is already logged in go routine in SandboxCreate handler
	go func() {
		err := closeSandbox(ctx)
		if err != nil {
			logger.L().Error(ctx, "error stopping sandbox on cow export", zap.Error(err))
		}
	}()

	select {
	case <-o.finishedOperations:
	case <-ctx.Done():
		// Close the cache to avoid leaking the mmaped memory. Log an error
		// if that failed
		closeErr := cache.Close()
		if closeErr != nil {
			logger.L().Warn(ctx, "error closing cache", zap.Error(closeErr))
		}

		return nil, errors.New("timeout waiting for overlay device to be released")
	}
	telemetry.ReportEvent(ctx, "sandbox stopped")

	return cache, nil
}

func (o *NBDProvider) ExportDiff(
	ctx context.Context,
	out *os.File,
	closeSandbox func(ctx context.Context) error,
) (*header.DiffMetadata, error) {
	ctx, span := tracer.Start(ctx, "cow-export")
	defer span.End()

	cache, err := o.ejectAndStopSandbox(ctx, closeSandbox)
	if err != nil {
		return nil, err
	}

	m, err := cache.ExportToDiff(ctx, out)
	if err != nil {
		// Close the cache to avoid leaking the mmaped memory. Log an error
		// if that failed
		closeErr := cache.Close()
		if closeErr != nil {
			logger.L().Warn(ctx, "error closing cache", zap.Error(closeErr))
		}

		return nil, fmt.Errorf("error exporting cache: %w", err)
	}

	telemetry.ReportEvent(ctx, "cache exported")

	err = cache.Close()
	if err != nil {
		return nil, fmt.Errorf("error closing cache: %w", err)
	}

	return m, nil
}

// PrepareExportDiff ejects the writable cache, stops the sandbox and waits for
// the overlay device to be released, then returns the frozen ejected cache
// WITHOUT reflinking it. The caller reflinks it into a diff in the background and
// Closes it, so a pause returns without paying the reflink stall.
func (o *NBDProvider) PrepareExportDiff(
	ctx context.Context,
	closeSandbox func(ctx context.Context) error,
) (*block.Cache, error) {
	ctx, span := tracer.Start(ctx, "cow-export-prepare")
	defer span.End()

	return o.ejectAndStopSandbox(ctx, closeSandbox)
}

// ExportDiffInPlace exports the NBD cache into `out` without closing/destroying
// the underlying cache, so a sandbox that resumes in place keeps running on it.
func (o *NBDProvider) ExportDiffInPlace(
	ctx context.Context,
	out *os.File,
) (*header.DiffMetadata, error) {
	ctx, span := tracer.Start(
		ctx,
		"cow-export",
		trace.WithAttributes(attribute.Bool("in-place", true)),
	)
	defer span.End()

	if err := o.sync(ctx); err != nil {
		return nil, fmt.Errorf("flushing COW device failed: %w", err)
	}

	return o.overlay.ExportDiffInPlace(ctx, out)
}

// SwapForBackgroundSeal flushes the NBD device (so all in-flight writes land in
// the current cache), then swaps a fresh empty cache onto the overlay and returns
// the previous cache. The returned cache is frozen — new guest writes go to the
// fresh cache — so the caller can reflink/export it in the background while the
// VM resumes. Only the device flush stays on the critical path; the reflink is
// deferred.
func (o *NBDProvider) SwapForBackgroundSeal(ctx context.Context) (*block.Cache, error) {
	ctx, span := tracer.Start(ctx, "cow-swap-for-seal")
	defer span.End()

	if err := o.sync(ctx); err != nil {
		return nil, fmt.Errorf("flushing COW device failed: %w", err)
	}

	size, err := o.overlay.Size(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting overlay size: %w", err)
	}

	gen := o.sealGen.Add(1)
	// The fresh live cache must keep the ".cow" SUFFIX: storage's
	// SandboxFileGlobs — the single source of truth for startup reclaim —
	// matches "rootfs-*-*.cow", and after the first fold this file is the
	// ONLY rootfs COW on disk (the fold closes and unlinks the original).
	// A "….cow.sealN" name would survive every unclean orchestrator exit
	// as an invisible, sandbox-cache-sized leak.
	freshPath := fmt.Sprintf("%s-seal%d.cow", strings.TrimSuffix(o.cachePath, ".cow"), gen)
	fresh, err := block.NewCache(size, o.blockSize, freshPath, false)
	if err != nil {
		return nil, fmt.Errorf("creating fresh cache: %w", err)
	}

	old, err := o.overlay.SwapCache(fresh)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("swapping cache: %w", err), fresh.Close())
	}

	return old, nil
}

// FoldSealed folds the sealing cache into the live writable cache and detaches it
// for closing. See block.Overlay.FoldSealing.
func (o *NBDProvider) FoldSealed(ctx context.Context) (*block.Cache, error) {
	ctx, span := tracer.Start(ctx, "cow-fold-sealed")
	defer span.End()
	_ = ctx

	return o.overlay.FoldSealing()
}

func (o *NBDProvider) Close(ctx context.Context) error {
	ctx, span := tracer.Start(ctx, "cow-close")
	defer span.End()

	var errs []error

	err := o.sync(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("error flushing cow device: %w", err))
	}

	err = o.mnt.Close(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("error closing overlay mount: %w", err))
	}

	o.finishedOperations <- struct{}{}

	err = o.overlay.Close()
	if err != nil {
		errs = append(errs, fmt.Errorf("error closing overlay cache: %w", err))
	}

	logger.L().Info(ctx, "overlay device released")

	return errors.Join(errs...)
}

func (o *NBDProvider) Path() (string, error) {
	return o.ready.Wait()
}

func (o *NBDProvider) sync(ctx context.Context) error {
	ctx, span := tracer.Start(ctx, "sync")
	defer span.End()

	if _, err := o.Path(); err != nil {
		return fmt.Errorf("failed to get cow path: %w", err)
	}

	return o.mnt.Flush(ctx)
}

// SealLayer freezes the current write layer as a read-only layer file at
// sealedLayerPath and installs a fresh empty write layer at newCachePath.
// The VM must be paused: the flush barrier below only guarantees that every
// write the kernel has accepted reaches the layer, not that the guest has
// stopped producing new ones.
func (o *NBDProvider) SealLayer(ctx context.Context, newCachePath string, sealedLayerPath string) (*SealedLayer, error) {
	ctx, span := tracer.Start(ctx, "seal-layer")
	defer span.End()

	// Flush barrier: push every write the kernel NBD device holds through the
	// dispatchers into the cache before freezing it.
	if err := o.sync(ctx); err != nil {
		return nil, fmt.Errorf("error flushing nbd device before seal: %w", err)
	}

	size, err := o.overlay.Size(ctx)
	if err != nil {
		return nil, fmt.Errorf("error getting overlay size: %w", err)
	}

	newCache, err := block.NewCache(size, o.blockSize, newCachePath, false)
	if err != nil {
		return nil, fmt.Errorf("error creating new write layer: %w", err)
	}

	sealed, err := o.overlay.Seal(newCache)
	if err != nil {
		closeErr := newCache.Close()

		return nil, errors.Join(fmt.Errorf("error sealing write layer: %w", err), closeErr)
	}

	// From here on the swap has happened: failures must not unwind it, only
	// report it, or the layer stack and the bookkeeping would disagree.
	//
	// Deliberately no synchronous flush of the sealed layer. Its dirty pages
	// sit in the host page cache, and every later reader -- the running
	// sandbox through its SealedView, a restore reopening the file -- reads
	// through that same cache, so consistency owes nothing to a flush. What
	// an msync would buy is durability across a host crash, and these
	// checkpoints do not reach that far: the store's index lives in this
	// process's memory and dies with it. The wait cost 0.49 ms per dirty MB
	// (measured) and was the whole reason sealing scaled with the delta;
	// the kernel writes the pages back on its own schedule anyway.

	if err := sealed.MoveFile(sealedLayerPath); err != nil {
		return nil, fmt.Errorf("error moving sealed layer into store: %w", err)
	}

	return &SealedLayer{
		Path:         sealedLayerPath,
		DirtyOffsets: sealed.DirtyOffsets(),
		Size:         size,
		BlockSize:    o.blockSize,
	}, nil
}

// ResetView swaps the served disk view to `device` with a fresh write layer,
// under the live NBD mount, for in-place rollback. Same quiesce contract as
// SealLayer: VM paused, and the flush barrier below settles everything the
// kernel had accepted.
func (o *NBDProvider) ResetView(ctx context.Context, device block.ReadonlyDevice, newCachePath string) error {
	ctx, span := tracer.Start(ctx, "reset-view")
	defer span.End()

	if err := o.sync(ctx); err != nil {
		return fmt.Errorf("error flushing nbd device before view reset: %w", err)
	}

	size, err := o.overlay.Size(ctx)
	if err != nil {
		return fmt.Errorf("error getting overlay size: %w", err)
	}

	newCache, err := block.NewCache(size, o.blockSize, newCachePath, false)
	if err != nil {
		return fmt.Errorf("error creating new write layer: %w", err)
	}

	if err := o.overlay.ResetView(device, newCache); err != nil {
		// The swap itself happened (it is two pointer writes); only cleanup
		// of the old view failed. Report it — leaked mappings — but the view
		// is correct, which is what rollback needs.
		return fmt.Errorf("error resetting view: %w", err)
	}

	return nil
}
