package rootfs

import (
	"context"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

var tracer = otel.Tracer("github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/rootfs")

type Provider interface {
	Start(ctx context.Context) error
	Close(ctx context.Context) error
	Path() (string, error)
	ExportDiff(ctx context.Context, out io.Writer, closeSandbox func(context.Context) error) (*header.DiffMetadata, error)
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
