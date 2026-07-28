package vmm

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/cfg"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/network"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/rootfs"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/template"
	sbxlogger "github.com/e2b-dev/infra/packages/shared/pkg/logger/sandbox"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

type Process interface {
	Create(
		ctx context.Context,
		metadata sbxlogger.LoggerMetadata,
		vcpuCount int64,
		memoryMB int64,
		hugePages bool,
		options ProcessOptions,
	) error
	Resume(
		ctx context.Context,
		metadata sbxlogger.SandboxMetadata,
		uffdSocketPath string,
		snapfile template.File,
		memfile template.File,
		uffdReady chan struct{},
		accessToken *string,
		memoryMB int64,
		vcpuCount int64,
		hugePages bool,
		cgroupFD int,
		traceID string,
	) error
	Stop(ctx context.Context) error
	Pause(ctx context.Context) error
	CreateSnapshot(ctx context.Context, snapfilePath, memfilePath string) error
	Pid() (int, error)
	Exit() *utils.ErrorOnce
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
		rootfsProvider rootfs.Provider,
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
