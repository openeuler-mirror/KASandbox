package hostservice

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/cfg"
)

const ModemSimulatorVsockPort uint32 = 9600

func BuildModemSimulatorService(
	config cfg.BuilderConfig,
	androidVersion string,
	cuttlefishConfigPath string,
	netNSName string,
	listener *UnixListener,
) (Service, error) {
	hostPackageDir := config.CvdHostPackageDirForVersion(androidVersion)
	binaryPath := filepath.Join(hostPackageDir, "bin", "modem_simulator")
	if _, err := os.Stat(binaryPath); err != nil {
		return Service{}, fmt.Errorf("modem_simulator binary not found at %s: %w", binaryPath, err)
	}

	if cuttlefishConfigPath == "" {
		return Service{}, fmt.Errorf("modem_simulator requires cuttlefish_config.json path")
	}
	if listener == nil || listener.File == nil {
		return Service{}, fmt.Errorf("modem_simulator requires a Unix listener")
	}

	env := []string{
		fmt.Sprintf("HOME=%s", hostPackageDir),
		// modem_simulator links Android's host bionic. ANDROID_ROOT provides
		// the fallback /usr/share/zoneinfo/tzdata path shipped in the matching
		// CVD host package; ANDROID_TZDATA_ROOT must also be set because bionic
		// treats a missing variable as fatal before trying that fallback.
		fmt.Sprintf("ANDROID_ROOT=%s", hostPackageDir),
		fmt.Sprintf("ANDROID_TZDATA_ROOT=%s", hostPackageDir),
		fmt.Sprintf("CUTTLEFISH_CONFIG_FILE=%s", cuttlefishConfigPath),
		"CUTTLEFISH_INSTANCE=1",
	}

	return Service{
		Name:   "modem_simulator",
		Binary: binaryPath,
		// gflags accepts both "-flag=value" and "--flag value"; we use the
		// single-dash =value form to match upstream launch_cvd invocation
		// (run_cvd/launch/modem.cpp:99-100).
		Args: []string{
			"-sim_type=1",
			"-server_fds=3",
		},
		NetNSName:     netNSName,
		Env:           env,
		ExtraFiles:    []*os.File{listener.File},
		RestartPolicy: RestartOnCrash,
		ReadyCheck:    &ModemSimulatorReady{Path: listener.Path},
	}, nil
}
