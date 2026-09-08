package rootfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/nbd"
	featureflags "github.com/e2b-dev/infra/packages/shared/pkg/feature-flags"
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

func (o *NBDProvider) ExportDiff(
	ctx context.Context,
	out io.Writer,
	closeSandbox func(ctx context.Context) error,
) (*header.DiffMetadata, error) {
	ctx, span := tracer.Start(ctx, "cow-export")
	defer span.End()

	// Every write layer the sandbox has, not only the current one: a
	// checkpoint seals the write layer and opens a fresh one, so after the
	// first checkpoint the current layer holds only the writes since. The
	// snapshot has to carry all of them.
	layers, err := o.overlay.EjectLayers()
	if err != nil {
		return nil, fmt.Errorf("error ejecting cache: %w", err)
	}
	cache, sealed := layers[len(layers)-1], layers[:len(layers)-1]

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
		return nil, fmt.Errorf("timeout waiting for overlay device to be released")
	}
	telemetry.ReportEvent(ctx, "sandbox stopped")

	var m *header.DiffMetadata
	if len(sealed) == 0 {
		// Never checkpointed: the one write layer, exported as always.
		m, err = cache.ExportToDiff(ctx, out)
	} else {
		m, err = block.ExportLayersToDiff(ctx, layers, out)
	}
	if err != nil {
		return nil, fmt.Errorf("error exporting cache: %w", err)
	}

	telemetry.ReportEvent(ctx, "cache exported")

	err = cache.Close()
	if err != nil {
		return nil, fmt.Errorf("error closing cache: %w", err)
	}

	// Sealed layer files belong to the checkpoint store; only the mappings go.
	for _, layer := range sealed {
		if err := layer.CloseKeepFile(); err != nil {
			return nil, fmt.Errorf("error unmapping sealed layer: %w", err)
		}
	}

	return m, nil
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
		return fmt.Errorf("error releasing previous view: %w", err)
	}

	return nil
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

	nbdPath, err := o.Path()
	if err != nil {
		return fmt.Errorf("failed to get cow path: %w", err)
	}

	file, err := os.Open(nbdPath)
	if err != nil {
		return fmt.Errorf("failed to open path: %w", err)
	}
	defer func() {
		err := file.Close()
		if err != nil {
			logger.L().Error(ctx, "failed to close nbd file", zap.Error(err))
		}
	}()

	if err := unix.IoctlSetInt(int(file.Fd()), unix.BLKFLSBUF, 0); err != nil {
		return fmt.Errorf("ioctl BLKFLSBUF failed: %w", err)
	}

	return flush(ctx, nbdPath)
}
