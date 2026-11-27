//go:build linux

package rootfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

var tracer = otel.Tracer("github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/rootfs")

// ErrDeferredExportNotSupported is returned by PrepareExportDiff on providers
// that can't defer the rootfs export (e.g. DirectProvider). Callers use it to
// fall back to the synchronous ExportDiff instead of failing the pause.
var ErrDeferredExportNotSupported = errors.New("deferred rootfs export not supported by this provider")

type Provider interface {
	Start(ctx context.Context) error
	Close(ctx context.Context) error
	Path() (string, error)
	ExportDiff(ctx context.Context, out *os.File, closeSandbox func(context.Context) error) (*header.DiffMetadata, error)
	// PrepareExportDiff ejects the writable cache and stops the sandbox, returning
	// the frozen ejected cache without reflinking it, so the caller can seal it
	// into a diff in the background. Only the NBD provider supports it.
	PrepareExportDiff(ctx context.Context, closeSandbox func(context.Context) error) (*block.Cache, error)
	// ExportDiffInPlace exports the rootfs diff without destroying the overlay, so
	// a sandbox that resumes in place keeps running on the same cache.
	ExportDiffInPlace(ctx context.Context, out *os.File) (*header.DiffMetadata, error)
	// SwapForBackgroundSeal flushes the device, swaps a fresh writable cache onto
	// the live overlay and returns the previous (now frozen) cache so the caller
	// can seal it in the background while the VM keeps running. NBD provider only.
	SwapForBackgroundSeal(ctx context.Context) (*block.Cache, error)
	// FoldSealed folds the sealing cache into the live writable cache and detaches
	// it for closing, so the writable cache is a complete diff again and a
	// subsequent SwapForBackgroundSeal can proceed. Returns nil if none is sealing.
	FoldSealed(ctx context.Context) (*block.Cache, error)
}

// SealedLayer describes a write layer frozen by SealLayer: a sparse file
// holding the blocks written during its epoch at their raw device offsets,
// plus the sorted list of those offsets. Storage offset equals device offset,
// so a header mapping into the layer is the identity.
type SealedLayer struct {
	Path         string
	DirtyOffsets []int64
	Size         int64
	BlockSize    int64
}

// ViewResetter is implemented by providers that can swap their entire served
// view — read path and write layer — under a live NBD mount, for in-place
// rollback. The caller must hold the VM paused across the call.
type ViewResetter interface {
	ResetView(ctx context.Context, device block.ReadonlyDevice, newCachePath string) error
}

// LayerSealer is implemented by providers that can freeze their write layer
// into a read-only layer file while the VM stays up, installing a fresh
// empty write layer in its place. The caller must hold the VM paused across
// the call so the layer is an image of the disk at one instant.
type LayerSealer interface {
	SealLayer(ctx context.Context, newCachePath string, sealedLayerPath string) (*SealedLayer, error)
}

// flush flushes the data to the operating system's buffer.
func flush(ctx context.Context, path string) error {
	ctx, span := tracer.Start(ctx, "flush", trace.WithAttributes(attribute.String("path", path)))
	defer span.End()

	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open path: %w", err)
	}
	defer func() {
		err := file.Close()
		if err != nil {
			logger.L().Error(ctx, "failed to close path", zap.Error(err))
		}
	}()

	err = syscall.Fsync(int(file.Fd()))
	if err != nil {
		return fmt.Errorf("failed to fsync path: %w", err)
	}

	err = file.Sync()
	if err != nil {
		return fmt.Errorf("failed to sync path: %w", err)
	}

	return nil
}
