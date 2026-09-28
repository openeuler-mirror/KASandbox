package hostservice

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/cfg"
	"github.com/e2b-dev/infra/packages/shared/pkg/consts"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd/process"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd/process/processconnect"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

const ModemSimulatorVsockPort uint32 = 9600

const androidRildRestartCommand = "kill $(pidof com.android.phone); /system/bin/setprop ctl.restart vendor.ril-daemon"

const guestCommandOutputLimit = 1 << 20

// Each operation bounds the process stream with its own context deadline.
var androidEnvdClient = &http.Client{}

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
		ReadyCheck:    &ProcessAlive{},
	}, nil
}

// RestartGuestRild synchronously executes the guest rild restart through envd.
func RestartGuestRild(ctx context.Context, envdHostIP string, accessToken *string, sandboxID string, timeout time.Duration) error {
	restartCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := time.Now()
	if _, err := runGuestShellCommand(restartCtx, envdHostIP, accessToken, androidRildRestartCommand); err != nil {
		return fmt.Errorf("android sandbox guest rild restart failed (sandbox_id=%s, duration=%s, timeout=%s, parent_context_error=%v, restart_context_error=%v): %w",
			sandboxID, time.Since(started), timeout, ctx.Err(), restartCtx.Err(), err)
	}

	logger.L().Info(ctx, "android sandbox guest rild restarted",
		logger.WithSandboxID(sandboxID),
		zap.Duration("duration", time.Since(started)))
	return nil
}

// WaitForGuestMobileIP waits for an IPv4 address outside 255.0.0.0/8 on buried_eth0.
func WaitForGuestMobileIP(ctx context.Context, envdHostIP string, accessToken *string, timeout time.Duration) error {
	readyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var lastErr error
	for {
		probeCtx, cancelProbe := context.WithTimeout(readyCtx, 5*time.Second)
		output, err := runGuestShellCommand(probeCtx, envdHostIP, accessToken, "/system/bin/ip -4 addr show dev buried_eth0")
		cancelProbe()
		if err == nil && hasGuestMobileIP(output) {
			return nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("buried_eth0 has no IPv4 address outside 255.0.0.0/8: %s", strings.TrimSpace(output))
		}
		select {
		case <-readyCtx.Done():
			return fmt.Errorf("waiting for buried_eth0 IPv4 address: %w (last check: %v)", readyCtx.Err(), lastErr)
		case <-ticker.C:
		}
	}
}

func hasGuestMobileIP(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "inet" {
			continue
		}
		prefix, err := netip.ParsePrefix(fields[1])
		if err == nil && prefix.Addr().Is4() {
			ip := prefix.Addr().As4()
			if ip[0] != 255 {
				return true
			}
		}
	}
	return false
}

func runGuestShellCommand(ctx context.Context, envdHostIP string, accessToken *string, command string) (string, error) {
	address := fmt.Sprintf("http://%s:%d", envdHostIP, consts.DefaultEnvdServerPort)

	client := processconnect.NewProcessClient(androidEnvdClient, address, connect.WithReadMaxBytes(guestCommandOutputLimit))

	req := connect.NewRequest(&process.StartRequest{
		Process: &process.ProcessConfig{
			Cmd:  "/system/bin/sh",
			Args: []string{"-c", command},
			Cwd:  utils.ToPtr("/"),
		},
	})
	grpc.SetUserHeader(req.Header(), "root")
	if accessToken != nil {
		req.Header().Set("X-Access-Token", *accessToken)
	}

	stream, err := client.Start(ctx, req)
	if err != nil {
		return "", fmt.Errorf("error starting process in envd: %w", err)
	}
	defer stream.Close()

	var stdout strings.Builder
	var outputBytes int
	for stream.Receive() {
		if data := stream.Msg().GetEvent().GetData(); data != nil {
			size := len(data.GetStdout()) + len(data.GetStderr())
			if size > guestCommandOutputLimit-outputBytes {
				return stdout.String(), fmt.Errorf("guest command output exceeds %d bytes", guestCommandOutputLimit)
			}
			outputBytes += size
			stdout.Write(data.GetStdout())
		}
		if end := stream.Msg().GetEvent().GetEnd(); end != nil {
			if code := end.GetExitCode(); code != 0 {
				return stdout.String(), fmt.Errorf("command exited with code %d: %s", code, end.GetStatus())
			}
			return stdout.String(), nil
		}
	}
	if err := stream.Err(); err != nil {
		return stdout.String(), fmt.Errorf("envd process stream failed: %w", err)
	}
	return stdout.String(), fmt.Errorf("envd process stream ended without an End event")
}
