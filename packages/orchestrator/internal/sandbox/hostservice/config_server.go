package hostservice

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/cfg"
)

const ConfigServerVsockPort uint32 = 6800

func BuildConfigServerService(
	config cfg.BuilderConfig,
	androidVersion string,
	cuttlefishConfigPath string,
	netNSName string,
	listener *UnixListener,
) (Service, error) {
	hostPackageDir := config.CvdHostPackageDirForVersion(androidVersion)
	binaryPath := filepath.Join(hostPackageDir, "bin", "config_server")
	if _, err := os.Stat(binaryPath); err != nil {
		return Service{}, fmt.Errorf("config_server binary not found at %s: %w", binaryPath, err)
	}

	if cuttlefishConfigPath == "" {
		return Service{}, fmt.Errorf("config_server requires cuttlefish_config.json path")
	}
	if listener == nil || listener.File == nil {
		return Service{}, fmt.Errorf("config_server requires a Unix listener")
	}

	return Service{
		Name:   "config_server",
		Binary: binaryPath,
		Args:   []string{"-server_fd=3"},
		Env: []string{
			fmt.Sprintf("HOME=%s", hostPackageDir),
			fmt.Sprintf("CUTTLEFISH_CONFIG_FILE=%s", cuttlefishConfigPath),
			"CUTTLEFISH_INSTANCE=1",
		},
		NetNSName:     netNSName,
		ExtraFiles:    []*os.File{listener.File},
		RestartPolicy: RestartOnCrash,
		ReadyCheck:    &ConfigServerReady{Path: listener.Path},
	}, nil
}
