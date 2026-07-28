package fc

import (
	"context"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/cfg"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/network"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/rootfs"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/vmm"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

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
	rootfsProvider rootfs.Provider,
	rootfsPaths vmm.RootfsPaths,
) (vmm.Process, error) {
	fcConfig := Config{
		KernelVersion:      versions.KernelVersion,
		FirecrackerVersion: versions.VMMVersion,
	}
	p, err := NewProcess(
		ctx,
		execCtx,
		builderConfig,
		slot,
		files,
		fcConfig,
		rootfsProvider,
		rootfsPaths,
	)
	if err != nil {
		return nil, err
	}
	return p, nil
}
