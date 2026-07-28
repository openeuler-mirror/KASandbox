package hostservice

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/cfg"
)

// modemSimulatorBasePort is the CVD-upstream base vsock port for the first
// modem_simulator instance. Per assemble_cvd/flags.cc:1317-1325:
//
//	port = 9600 + (modem_simulator_count * (num - 1)) + index
//	calc_vsock_port(port) = port + (vsock_guest_cid - 3)
//
// With modem_simulator_count=1, num=1, index=0, vsock port = 9600 + (cid - 3).
const modemSimulatorBasePort uint32 = 9600

// ModemSimulatorVsockPort returns the vsock port the modem_simulator host
// service listens on for the given CID. Exposed so android_host.go can bake
// the value into the synthesized cuttlefish_config.json.
func ModemSimulatorVsockPort(cid int64) int {
	return int(modemSimulatorBasePort) + int(cid-VsockCIDBase)
}

// BuildModemSimulatorService builds the modem_simulator host-side service for
// one Android sandbox using the upstream CVD FD-passing (socket activation)
// pattern: the orchestrator creates a vsock SOCK_STREAM server socket and
// passes it to the binary as inherited fd 3 (-server_fds=3). modem_simulator
// reads CUTTLEFISH_CONFIG_FILE to look up its modem_simulator_host_id.
//
// modem_simulator does NOT listen on any host TCP port — it's purely a
// vsock-bridged service (Ports.ModemSimulatorPort is left 0).
func BuildModemSimulatorService(
	config cfg.BuilderConfig,
	cid int64,
	sandboxID string,
	cuttlefishConfigPath string,
) (Service, error) {
	binaryPath := filepath.Join(config.CvdHostPackageDir, "bin", "modem_simulator")
	if _, err := os.Stat(binaryPath); err != nil {
		return Service{}, fmt.Errorf("modem_simulator binary not found at %s: %w", binaryPath, err)
	}

	vsockPort, err := resolveModemSimulatorPort(cuttlefishConfigPath, cid)
	if err != nil {
		return Service{}, fmt.Errorf("resolve modem_simulator vsock port: %w", err)
	}

	listener, err := VsockListen(VMADDR_CID_ANY, vsockPort)
	if err != nil {
		return Service{}, fmt.Errorf("create modem_simulator vsock listener on port %d: %w", vsockPort, err)
	}

	env := []string{
		fmt.Sprintf("HOME=%s", config.CvdHostPackageDir),
		// modem_simulator links Android's host bionic. ANDROID_ROOT provides
		// the fallback /usr/share/zoneinfo/tzdata path shipped in the matching
		// CVD host package; ANDROID_TZDATA_ROOT must also be set because bionic
		// treats a missing variable as fatal before trying that fallback.
		fmt.Sprintf("ANDROID_ROOT=%s", config.CvdHostPackageDir),
		fmt.Sprintf("ANDROID_TZDATA_ROOT=%s", config.CvdHostPackageDir),
	}
	if cuttlefishConfigPath != "" {
		env = append(env, fmt.Sprintf("CUTTLEFISH_CONFIG_FILE=%s", cuttlefishConfigPath))
	}

	return Service{
		Name:   fmt.Sprintf("modem_simulator:%s", sandboxID),
		Binary: binaryPath,
		// gflags accepts both "-flag=value" and "--flag value"; we use the
		// single-dash =value form to match upstream launch_cvd invocation
		// (run_cvd/launch/modem.cpp:99-100).
		Args: []string{
			"-sim_type=1",
			"-server_fds=3",
		},
		Env:           env,
		ExtraFiles:    []*os.File{listener},
		RestartPolicy: RestartOnCrash,
		ReadyCheck:    NoReadyCheck{},
	}, nil
}

// resolveModemSimulatorPort parses the first port from cuttlefish_config.json's
// modem_simulator_ports field. Falls back to modemSimulatorBasePort + (cid - VsockCIDBase)
// when the config is missing or the field is empty.
func resolveModemSimulatorPort(cuttlefishConfigPath string, cid int64) (uint32, error) {
	fallback := modemSimulatorBasePort + uint32(cid-VsockCIDBase)
	if cuttlefishConfigPath == "" {
		return fallback, nil
	}
	cfg, err := LoadCuttlefishConfig(cuttlefishConfigPath)
	if err != nil {
		// Config load failed — fall back to formula. Don't hard-fail the
		// sandbox because the orchestrator can still compute a port that
		// matches CVD's default formula.
		return fallback, nil
	}
	instance, _, err := cfg.FirstInstance()
	if err != nil {
		return fallback, nil
	}
	if instance.ModemSimulatorPorts == "" {
		return fallback, nil
	}
	// instance.ModemSimulatorPorts is "9600,9601,..." — take the first.
	first := strings.SplitN(instance.ModemSimulatorPorts, ",", 2)[0]
	port, err := strconv.ParseUint(strings.TrimSpace(first), 10, 32)
	if err != nil {
		return fallback, nil
	}
	return uint32(port), nil
}
