package fc

import (
	"path/filepath"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/cfg"
)

const (
	SandboxKernelFile = "vmlinux.bin"

	FirecrackerBinaryName = "firecracker"

	SandboxRootfsFile = "rootfs.ext4"

	entropyBytesSize    int64 = 1024 // 1 KB
	entropyRefillTime   int64 = 100
	entropyOneTimeBurst int64 = 0
)

type Config struct {
	KernelVersion      string
	FirecrackerVersion string
}

func (t Config) SandboxKernelDir() string {
	return t.KernelVersion
}

func (t Config) HostKernelPath(config cfg.BuilderConfig) string {
	return filepath.Join(config.HostKernelsDir, t.KernelVersion, SandboxKernelFile)
}

func (t Config) FirecrackerPath(config cfg.BuilderConfig) string {
	return filepath.Join(config.FirecrackerVersionsDir, t.FirecrackerVersion, FirecrackerBinaryName)
}
