package config

import (
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/core/oci/auth"
	templatemanager "github.com/e2b-dev/infra/packages/shared/pkg/grpc/template-manager"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

const (
	InstanceBuildPrefix = "b"

	// TemplateDefaultUser is the default user to use in the template to run all commands.
	TemplateDefaultUser = "user"
)

type TemplateConfig struct {
	// Builder version
	Version string

	// TeamID is the ID of the team to build the template for.
	TeamID string

	// TemplateID is the ID of the template to build.
	TemplateID string

	// CacheScope is the scope of layers and files caches.
	CacheScope string

	// Command to run when building the template.
	StartCmd string

	// The number of vCPUs to allocate to the VM.
	VCpuCount int64

	// The amount of RAM memory to allocate to the VM, in MiB.
	MemoryMB int64

	// The amount of free disk to allocate to the VM, in MiB.
	DiskSizeMB int64

	// HugePages sets whether the VM use huge pages.
	HugePages bool

	// Command to run to check if the template is ready.
	ReadyCmd string

	// FromImage is the base image to use for building the template.
	FromImage string

	// FromTemplate is the base template to use for building the template.
	FromTemplate *templatemanager.FromTemplateConfig

	// RegistryAuthProvider provides authentication for pulling the FromImage.
	RegistryAuthProvider auth.RegistryAuthProvider

	// Force rebuild of the template even if it is already cached.
	Force *bool

	// Steps to build the template.
	Steps []*templatemanager.TemplateStep

	// Firecracker version to use
	FirecrackerVersion string

	// Kernel version to use
	KernelVersion string
}

func MemfilePageSize(hugePages bool) int64 {
	if hugePages {
		return header.HugepageSize
	}

	// Keep the memfile block size at the granularity Firecracker uses for its
	// dirty/resident/empty bitmaps. With hugepages disabled Firecracker always
	// reports these bitmaps at 4 KiB per bit (HugePageConfig::None => 4096),
	// independent of the host base-page size. The memfile block size must match
	// that granularity: CreateMapping() converts bit indexes to offsets with
	// `index * blockSize`, so a larger block size would scale the 4 KiB indexes
	// by the host page size and produce out-of-range mappings.
	//
	// Hosts with a base page larger than 4 KiB (e.g. 64 KiB ARM64) are handled
	// on the I/O side instead: UFFD copies use the host page size as copy unit
	// and read it from multiple 4 KiB blocks (see uffd/prefetch and block.ReadSlice).
	return header.PageSize
}

func (e TemplateConfig) RootfsBlockSize() int64 {
	return header.RootfsBlockSize
}
