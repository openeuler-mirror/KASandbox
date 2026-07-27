package config

import (
	"fmt"
	"strings"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/vmm"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/core/oci/auth"
	templatemanager "github.com/e2b-dev/infra/packages/shared/pkg/grpc/template-manager"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// androidAllowedStepType is the only build step instruction permitted for
// Android templates. Android guests can't run the Linux/Docker build steps
// (RUN, ENV, WORKDIR, USER, ...); only copying files in is allowed.
// startCmd/readyCmd are carried as separate fields, not as steps.
const androidAllowedStepType = "COPY"

type MultiDiskConfig struct {
	OS         string
	Persistent string
	SDCard     string
}

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

	// FromImageRaw is a registry reference containing a raw disk image layer.
	// A non-empty value registers the disk as the rootfs directly.
	FromImageRaw string

	// FromImageMultiDisk contains registry references for the Android disks.
	FromImageMultiDisk *MultiDiskConfig

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

	// VMM type to use for building the template.
	VMMType string

	// OsType is the guest operating system family to use for the build.
	OsType vmm.OsType

	// Kernel version to use
	KernelVersion string
}

func MemfilePageSize(hugePages bool) int64 {
	if hugePages {
		return header.HugepageSize
	}

	return header.PageSize
}

func (e TemplateConfig) RootfsBlockSize() int64 {
	return header.RootfsBlockSize
}

func (e TemplateConfig) UsesRawImage() bool {
	return e.FromImageRaw != ""
}

func (e TemplateConfig) UsesMultiDisk() bool { return e.FromImageMultiDisk != nil }

func (e TemplateConfig) GuestOS() vmm.OsType {
	return e.OsType.OrDefault()
}

// IsWindows reports whether the template build targets a Windows guest.
func (e TemplateConfig) IsWindows() bool {
	return e.GuestOS() == vmm.OsWindows
}

func (e TemplateConfig) IsAndroid() bool { return e.GuestOS() == vmm.OsAndroid }

// Validate checks Android-specific source and build-step invariants. Windows
// validation and build behavior remain owned by the Windows implementation.
func (e TemplateConfig) Validate() error {
	osType, err := vmm.ParseOsType(string(e.OsType))
	if err != nil {
		return fmt.Errorf("invalid os type: %w", err)
	}

	if osType != vmm.OsAndroid {
		return nil
	}

	for _, step := range e.Steps {
		if !strings.EqualFold(step.GetType(), androidAllowedStepType) {
			return fmt.Errorf("unsupported build step %q for an Android template: only %s is allowed", step.GetType(), androidAllowedStepType)
		}
	}

	if e.FromImageMultiDisk == nil || e.FromImageMultiDisk.OS == "" || e.FromImageMultiDisk.Persistent == "" || e.FromImageMultiDisk.SDCard == "" {
		return fmt.Errorf("Android templates require os, persistent, and sdcard disk registry references")
	}
	if e.FromImageRaw != "" {
		return fmt.Errorf("Android templates cannot use fromImageRaw")
	}

	return nil
}
