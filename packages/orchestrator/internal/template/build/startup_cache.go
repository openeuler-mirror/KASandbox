package build

import (
	"context"
	"fmt"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/stratovirt"
)

// Optimize invalidates the finalized template after updating its prefetch
// metadata. Refill that final cache entry before publishing build success;
// the first user restore must not pay for downloading and expanding it again.
func (b *Builder) prepareAndroidStartupCache(ctx context.Context, buildID string) error {
	ctx, span := tracer.Start(ctx, "prepare Android startup cache")
	defer span.End()

	t, err := b.templateCache.GetTemplate(ctx, buildID, false, false)
	if err != nil {
		return err
	}
	if _, err := t.Metadata(); err != nil {
		return fmt.Errorf("fetch metadata: %w", err)
	}
	if _, err := t.Memfile(ctx); err != nil {
		return fmt.Errorf("fetch memory storage: %w", err)
	}
	if _, err := t.Disks(ctx); err != nil {
		return fmt.Errorf("fetch disk storage: %w", err)
	}
	snapfile, err := t.Snapfile()
	if err != nil {
		return fmt.Errorf("fetch snapshot: %w", err)
	}
	if err := stratovirt.PrepareSnapshot(snapfile, b.config.StorageConfig.SandboxCacheDir); err != nil {
		return fmt.Errorf("expand snapshot: %w", err)
	}
	return nil
}
