package storage

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/e2b-dev/infra/packages/shared/pkg/id"
)

type SandboxFiles struct {
	TemplateCacheFiles

	SandboxID string
	tmpDir    string
	// We use random id to avoid collision between the paused and restored sandbox caches
	randomID string
}

type Config struct {
	SandboxCacheDir  string `env:"SANDBOX_CACHE_DIR,expand"  envDefault:"${ORCHESTRATOR_BASE_PATH}/sandbox"`
	SnapshotCacheDir string `env:"SNAPSHOT_CACHE_DIR,expand" envDefault:"/mnt/snapshot-cache"`
	TemplateCacheDir string `env:"TEMPLATE_CACHE_DIR,expand" envDefault:"${ORCHESTRATOR_BASE_PATH}/template"`
}

func (c TemplateCacheFiles) NewSandboxFiles(sandboxID string) *SandboxFiles {
	randomID := id.Generate()

	return &SandboxFiles{
		TemplateCacheFiles: c,
		SandboxID:          sandboxID,
		randomID:           randomID,
		tmpDir:             os.TempDir(),
	}
}

func (c TemplateCacheFiles) NewSandboxFilesWithStaticID(sandboxID string, staticID string) *SandboxFiles {
	return &SandboxFiles{
		TemplateCacheFiles: c,
		SandboxID:          sandboxID,
		randomID:           staticID,
		tmpDir:             os.TempDir(),
	}
}

func (s *SandboxFiles) SandboxCacheRootfsPath(config Config) string {
	return filepath.Join(config.SandboxCacheDir, fmt.Sprintf("rootfs-%s-%s.cow", s.SandboxID, s.randomID))
}

func (s *SandboxFiles) SandboxCacheDiskPath(config Config, diskName string) string {
	if diskName == RootfsName {
		return s.SandboxCacheRootfsPath(config)
	}

	return filepath.Join(config.SandboxCacheDir, fmt.Sprintf("%s-%s-%s.cow", diskName, s.SandboxID, s.randomID))
}

func (s *SandboxFiles) SandboxRestoreDir(config Config) string {
	return filepath.Join(config.SandboxCacheDir, fmt.Sprintf("restore-%s-%s", s.SandboxID, s.randomID))
}

func (s *SandboxFiles) SandboxFirecrackerSocketPath() string {
	return filepath.Join(s.tmpDir, fmt.Sprintf("fc-%s-%s.sock", s.SandboxID, s.randomID))
}

func (s *SandboxFiles) SandboxUffdSocketPath() string {
	return filepath.Join(s.tmpDir, fmt.Sprintf("uffd-%s-%s.sock", s.SandboxID, s.randomID))
}

func (s *SandboxFiles) SandboxCacheRootfsLinkPath(config Config) string {
	return filepath.Join(config.SandboxCacheDir, fmt.Sprintf("rootfs-%s-%s.link", s.SandboxID, s.randomID))
}

func (s *SandboxFiles) SandboxCacheDiskLinkPath(config Config, diskName string) string {
	if diskName == RootfsName {
		return s.SandboxCacheRootfsLinkPath(config)
	}

	return filepath.Join(config.SandboxCacheDir, fmt.Sprintf("%s-%s-%s.link", diskName, s.SandboxID, s.randomID))
}

func (s *SandboxFiles) SandboxSerialLogPath() string {
	return filepath.Join(s.tmpDir, fmt.Sprintf("serial-%s-%s.log", s.SandboxID, s.randomID))
}

func (s *SandboxFiles) SandboxAndroidLogcatPath() string {
	return filepath.Join(s.tmpDir, fmt.Sprintf("logcat-%s-%s.log", s.SandboxID, s.randomID))
}
