package stratovirt

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
	rootfsProviders []rootfs.Provider,
	rootfsPaths vmm.RootfsPaths,
) (vmm.Process, error) {
	svConfig := Config{
		KernelVersion:     versions.KernelVersion,
		StratoVirtVersion: versions.VMMVersion,
		OsType:            versions.OsType,
		AndroidVersion:    versions.AndroidVersion,
	}
	return NewProcess(
		ctx,
		execCtx,
		builderConfig,
		slot,
		files,
		svConfig,
		rootfsProviders,
		rootfsPaths,
	)
}
