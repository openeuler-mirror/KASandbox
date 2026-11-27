package vmm

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/RoaringBitmap/roaring/v2"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/cfg"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/network"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/rootfs"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/template"
	sbxlogger "github.com/e2b-dev/infra/packages/shared/pkg/logger/sandbox"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// Process is the VMM backend contract the sandbox runtime uses. The method
// set mirrors the 2026.30 Firecracker process exactly, so *fc.Process
// satisfies it as-is; the StratoVirt backend implements the same contract
// for Android/Windows guests.
type Process interface {
	Create(
		ctx context.Context,
		metadata sbxlogger.LoggerMetadata,
		vcpuCount int64,
		memoryMB int64,
		hugePages bool,
		freePageReporting bool,
		freePageHinting bool,
		options ProcessOptions,
		txRateLimit RateLimiterConfig,
		driveRateLimit RateLimiterConfig,
		cgroupFD int,
	) error
	Resume(
		ctx context.Context,
		metadata sbxlogger.SandboxMetadata,
		uffdSocketPath string,
		snapfile template.File,
		uffdReady chan struct{},
		accessToken *string,
		cgroupFD int,
		useMemfd bool,
		useSyncWP bool,
		txRateLimit RateLimiterConfig,
		driveRateLimit RateLimiterConfig,
	) error
	Stop(ctx context.Context) error
	Pause(ctx context.Context) error
	CreateSnapshot(ctx context.Context, snapfilePath string) error
	MemoryInfo(ctx context.Context, blockSize int64) (*header.DiffMetadata, error)
	DirtyMemory(ctx context.Context, blockSize int64) (*header.DiffMetadata, error)
	ExportMemory(
		ctx context.Context,
		include *roaring.Bitmap,
		cachePath string,
		blockSize int64,
		memfd *block.Memfd,
		bgCopy bool,
		originalMemfile block.ReadonlyDevice,
		dedupBestEffort bool,
		dedupDirectIO bool,
		dedupBudget block.DedupBudget,
		inputEmpty *roaring.Bitmap,
		metaOut *utils.SetOnce[*header.DiffMetadata],
		dedupInflightServe bool,
		keepMemfdOpen bool,
	) (block.DiffSource, error)
	ResumeVM(ctx context.Context) error
	OrdinarySnapshotEpoch() uint64
	Pid() (int, error)
	Exit() *utils.ErrorOnce
}

// ProcessOptions are the per-boot options handed to Create. Kept identical
// to the Firecracker process options so both backends share one type.
type ProcessOptions struct {
	// IoEngine is the io engine to use for the rootfs drive.
	IoEngine *string

	// InitScriptPath is the path to the init script that will be executed inside the VM on kernel start.
	InitScriptPath string

	// KernelLogs is a flag to enable kernel logs output to the process stdout.
	KernelLogs bool

	// SystemdToKernelLogs is a flag to enable systemd logs output to the console.
	// It enables the kernel logs by default too.
	SystemdToKernelLogs bool

	// KvmClock is a flag to enable kvm-clock as the clocksource for the kernel.
	KvmClock bool

	// CmdlineArgs are extra guest kernel command line arguments overlaid on the
	// defaults.
	CmdlineArgs map[string]string

	// AccessToken, when non-nil, makes Create write the guest MMDS metadata
	// before the VM boots.
	AccessToken *string

	// Stdout is the writer to which the process stdout will be written.
	Stdout io.Writer

	// Stderr is the writer to which the process stderr will be written.
	Stderr io.Writer
}

// TokenBucketConfig holds parameters for a single token bucket.
// BucketSize < 0 disables the bucket.
type TokenBucketConfig struct {
	BucketSize   int64
	OneTimeBurst int64
	RefillTimeMs int64
}

// RateLimiterConfig holds rate limit parameters for a VMM device (network or
// block). Mirrors the Firecracker RateLimiter structure: two independent
// token buckets.
type RateLimiterConfig struct {
	Ops       TokenBucketConfig // packets; effective rate = BucketSize * 1000 / RefillTimeMs ops/s
	Bandwidth TokenBucketConfig // bytes;   effective rate = BucketSize * 1000 / RefillTimeMs bytes/s
}

type RootfsPaths struct {
	TemplateVersion uint64
	TemplateID      string
	BuildID         string
}

const (
	deprecatedEnvsDisk     = "/mnt/disks/fc-envs/v1"
	deprecatedBuildDirName = "builds"
)

// DeprecatedSandboxRootfsDir returns the legacy per-template rootfs directory.
func (p RootfsPaths) DeprecatedSandboxRootfsDir() string {
	return filepath.Join(deprecatedEnvsDisk, p.TemplateID, deprecatedBuildDirName, p.BuildID)
}

var ConstantRootfsPaths = RootfsPaths{
	// The version is always 2 for the constant rootfs paths format change.
	TemplateVersion: 2,
}

type Factory interface {
	NewProcess(
		ctx context.Context,
		execCtx context.Context,
		builderConfig cfg.BuilderConfig,
		slot *network.Slot,
		files *storage.SandboxFiles,
		versions VMMConfig,
		rootfsProviders []rootfs.Provider,
		rootfsPaths RootfsPaths,
	) (Process, error)
}

type BackendType string

const (
	BackendFirecracker BackendType = "firecracker"
	BackendStratoVirt  BackendType = "stratovirt"
)

// ProcessState returns the process state reported by ps.
func ProcessState(ctx context.Context, pid int) (string, error) {
	output, err := exec.CommandContext(ctx, "ps", "-o", "stat=", "-p", fmt.Sprint(pid)).Output()
	if err != nil {
		return "", fmt.Errorf("error getting state of pid=%d: %w", pid, err)
	}

	return strings.TrimSpace(string(output)), nil
}

func (b BackendType) OrDefault() BackendType {
	if b == "" {
		return BackendFirecracker
	}

	return b
}
