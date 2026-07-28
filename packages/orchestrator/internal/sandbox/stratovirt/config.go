package stratovirt

import (
	"path/filepath"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/cfg"
)

const (
	StratoVirtBinaryName = "stratovirt"
	SandboxKernelFile    = "vmlinux.bin"
	SandboxRootfsFile    = "rootfs.ext4"

	entropyBytesSize    int64 = 1024
	entropyRefillTime   int64 = 100
	entropyOneTimeBurst int64 = 0
)

type Config struct {
	KernelVersion     string
	StratoVirtVersion string
}

func (t Config) SandboxKernelDir() string {
	return t.KernelVersion
}

func (t Config) HostKernelPath(config cfg.BuilderConfig) string {
	return filepath.Join(config.HostKernelsDir, t.KernelVersion, SandboxKernelFile)
}

func (t Config) StratoVirtPath(config cfg.BuilderConfig) string {
	return filepath.Join(config.StratoVirtVersionsDir, t.StratoVirtVersion, StratoVirtBinaryName)
}
