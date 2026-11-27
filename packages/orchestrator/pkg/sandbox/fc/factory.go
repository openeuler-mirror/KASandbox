package fc

import (
	"context"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/cfg"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/network"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/rootfs"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/vmm"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

// Factory adapts the Firecracker process to the VMM backend contract so the
// sandbox runtime can pick a backend per guest OS.
type Factory struct{}

func NewFactory() *Factory {
	return &Factory{}
}

func NewDefaultFactory() *Factory {
	return NewFactory()
}

func (f *Factory) NewProcess(
	ctx context.Context,
	execCtx context.Context,
	builderConfig cfg.BuilderConfig,
	slot *network.Slot,
	files *storage.SandboxFiles,
	versions vmm.VMMConfig,
	rootfsProviders []rootfs.Provider,
	rootfsPaths vmm.RootfsPaths,
) (vmm.Process, error) {
	fcConfig := Config{
		KernelVersion:      versions.KernelVersion,
		FirecrackerVersion: versions.VMMVersion,
	}

	return NewProcess(
		ctx,
		execCtx,
		builderConfig,
		slot,
		files,
		fcConfig,
		rootfsProviders[0],
		RootfsPaths(rootfsPaths),
	)
}
