package cfg

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/network"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

type BuilderConfig struct {
	AllowSandboxInternet       bool          `env:"ALLOW_SANDBOX_INTERNET"   envDefault:"true"`
	DomainName                 string        `env:"DOMAIN_NAME"              envDefault:""`
	EnvdTimeout                time.Duration `env:"ENVD_TIMEOUT"             envDefault:"10s"`
	FirecrackerNetnsExecHelper string        `env:"E2B_FC_NETNS_EXEC_HELPER" envDefault:"/opt/e2b-infra/bin/fc-netns-exec"`
	FirecrackerVersionsDir     string        `env:"FIRECRACKER_VERSIONS_DIR" envDefault:"/fc-versions"`
	StratoVirtVersionsDir      string        `env:"STRATOVIRT_VERSIONS_DIR"  envDefault:"/stratovirt-versions"`
	HostEnvdPath               string        `env:"HOST_ENVD_PATH"           envDefault:"/fc-envd/envd"`
	HostKernelsDir             string        `env:"HOST_KERNELS_DIR"         envDefault:"/fc-kernels"`
	OrchestratorBaseDir        string        `env:"ORCHESTRATOR_BASE_PATH"   envDefault:"/orchestrator"`
	SandboxDir                 string        `env:"SANDBOX_DIR"              envDefault:"/fc-vm"`
	SharedChunkCacheDir        string        `env:"SHARED_CHUNK_CACHE_PATH"`
	TemplatesDir               string        `env:"TEMPLATES_DIR,expand"     envDefault:"${ORCHESTRATOR_BASE_PATH}/build-templates"`
	FirmwarePackagesRoot       string        `env:"FIRMWARE_PACKAGES_ROOT" envDefault:"/firmware"`
	CvdHostPackagesRoot        string        `env:"CVD_HOST_PACKAGES_ROOT" envDefault:"/cvd-host-packages"`
	DefaultCacheDir            string        `env:"DEFAULT_CACHE_DIR,expand" envDefault:"${ORCHESTRATOR_BASE_PATH}/build"`
	ReadyCheckTimeout          time.Duration `env:"READY_CHECK_TIMEOUT"      envDefault:"30s"`

	StorageConfig storage.Config
	NetworkConfig network.Config
}

// CvdHostPackageDirForVersion resolves the Cuttlefish host package directory
// for the given Android version: <CvdHostPackagesRoot>/android-<version>.
// Only multi-version mode is supported; every version lives in its own
// android-<version> subdirectory under the root.
func (c BuilderConfig) CvdHostPackageDirForVersion(version string) string {
	return filepath.Join(c.CvdHostPackagesRoot, "android-"+version)
}

// FirmwareDirForVersion resolves the firmware directory for the given
// Android version: <FirmwarePackagesRoot>/android-<version>. Only
// multi-version mode is supported.
func (c BuilderConfig) FirmwareDirForVersion(version string) string {
	return filepath.Join(c.FirmwarePackagesRoot, "android-"+version)
}

func makePathsAbsolute(c *BuilderConfig) error {
	for _, item := range []*string{
		&c.DefaultCacheDir,
		&c.FirecrackerNetnsExecHelper,
		&c.FirecrackerVersionsDir,
		&c.StratoVirtVersionsDir,
		&c.HostEnvdPath,
		&c.HostKernelsDir,
		&c.OrchestratorBaseDir,
		&c.StorageConfig.SandboxCacheDir,
		&c.SandboxDir,
		&c.SharedChunkCacheDir,
		&c.StorageConfig.SnapshotCacheDir,
		&c.StorageConfig.TemplateCacheDir,
		&c.TemplatesDir,
		&c.FirmwarePackagesRoot,
		&c.CvdHostPackagesRoot,
	} {
		dir := *item

		if dir == "" {
			continue
		}

		if filepath.IsAbs(dir) {
			continue
		}

		dir, err := filepath.Abs(dir)
		if err != nil {
			return fmt.Errorf("failed to resolve %q to absolute path: %w", *item, err)
		}

		*item = dir
	}

	return nil
}

type Config struct {
	BuilderConfig

	ClickhouseConnectionString string            `env:"CLICKHOUSE_CONNECTION_STRING"`
	ForceStop                  bool              `env:"FORCE_STOP"`
	GRPCPort                   uint16            `env:"GRPC_PORT"                    envDefault:"5008"`
	LaunchDarklyAPIKey         string            `env:"LAUNCH_DARKLY_API_KEY"`
	OrchestratorLockPath       string            `env:"ORCHESTRATOR_LOCK_PATH"       envDefault:"/orchestrator.lock"`
	ProxyPort                  uint16            `env:"PROXY_PORT"                   envDefault:"5007"`
	RedisClusterURL            string            `env:"REDIS_CLUSTER_URL"`
	RedisTLSCABase64           string            `env:"REDIS_TLS_CA_BASE64"`
	RedisURL                   string            `env:"REDIS_URL"`
	Services                   []string          `env:"ORCHESTRATOR_SERVICES"        envDefault:"orchestrator"`
	PersistentVolumeMounts     map[string]string `env:"PERSISTENT_VOLUME_MOUNTS"`
}

func Parse() (Config, error) {
	config, err := env.ParseAs[Config]()
	if err != nil {
		return config, err
	}

	bc := config.BuilderConfig
	if err = makePathsAbsolute(&bc); err != nil {
		return config, err
	}

	config.BuilderConfig = bc

	if config.PersistentVolumeMounts != nil {
		for name, path := range config.PersistentVolumeMounts {
			path = filepath.Clean(path)
			path, err = filepath.Abs(path)
			if err != nil {
				return config, fmt.Errorf("failed to make persistent volume mount %q an absolute path: %w", name, err)
			}

			if _, err := os.Stat(path); err != nil {
				return config, fmt.Errorf("failed to access persistent volume mount %q (%q): %w", name, path, err)
			}

			config.PersistentVolumeMounts[name] = path // store the cleaned path
		}
	}

	return config, nil
}

func ParseBuilder() (BuilderConfig, error) {
	model, err := env.ParseAs[BuilderConfig]()
	if err != nil {
		return BuilderConfig{}, err
	}

	if err = makePathsAbsolute(&model); err != nil {
		return BuilderConfig{}, err
	}

	return model, nil
}
