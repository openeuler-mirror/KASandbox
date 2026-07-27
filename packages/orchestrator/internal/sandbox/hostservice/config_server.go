package hostservice

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/cfg"
)

const ConfigServerVsockPort uint32 = 6800

// BuildConfigServerService builds the config_server host-side service for one
// Android sandbox. Android 14 config_server is socket activated: the launcher
// must create the AF_VSOCK listener and pass it as fd 3 with -server_fd=3.
func BuildConfigServerService(
	config cfg.BuilderConfig,
	cid int64,
	sandboxID string,
	cuttlefishConfigPath string,
) (Service, error) {
	binaryPath := filepath.Join(config.CvdHostPackageDir, "bin", "config_server")
	if _, err := os.Stat(binaryPath); err != nil {
		return Service{}, fmt.Errorf("config_server binary not found at %s: %w", binaryPath, err)
	}

	env := []string{
		fmt.Sprintf("HOME=%s", config.CvdHostPackageDir),
	}
	if cuttlefishConfigPath == "" {
		return Service{}, fmt.Errorf("config_server requires cuttlefish_config.json path")
	}
	env = append(env, fmt.Sprintf("CUTTLEFISH_CONFIG_FILE=%s", cuttlefishConfigPath))

	listener, err := VsockListen(VMADDR_CID_ANY, ConfigServerVsockPort)
	if err != nil {
		return Service{}, fmt.Errorf("create config_server listener: %w", err)
	}

	return Service{
		Name:          fmt.Sprintf("config_server:%s", sandboxID),
		Binary:        binaryPath,
		Args:          []string{"-server_fd=3"},
		Env:           env,
		ExtraFiles:    []*os.File{listener},
		RestartPolicy: RestartOnCrash,
		ReadyCheck:    NoReadyCheck{},
	}, nil
}
