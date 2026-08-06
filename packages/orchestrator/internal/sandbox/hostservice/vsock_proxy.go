package hostservice

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/cfg"
)

const adbVsockPort = 5555

func NewADBListener() (*os.File, string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", fmt.Errorf("bind adb tcp listener: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	tcpListener, ok := listener.(*net.TCPListener)
	if !ok {
		_ = listener.Close()
		return nil, "", fmt.Errorf("adb listener is not a TCP listener")
	}
	listenerFile, err := tcpListener.File()
	_ = listener.Close()
	if err != nil {
		return nil, "", fmt.Errorf("duplicate adb tcp listener fd: %w", err)
	}
	return listenerFile, fmt.Sprintf("127.0.0.1:%d", port), nil
}

func BuildVsockProxyService(config cfg.BuilderConfig, androidVersion string, cid int64, sandboxID, cuttlefishConfigPath, netNSName string, listenerFile *os.File) (Service, error) {
	if listenerFile == nil {
		return Service{}, fmt.Errorf("ADB proxy requires a TCP listener")
	}
	if cuttlefishConfigPath == "" {
		return Service{}, fmt.Errorf("ADB proxy requires cuttlefish_config.json path")
	}

	hostPackageDir := config.CvdHostPackageDirForVersion(androidVersion)
	binaryPath := filepath.Join(hostPackageDir, "bin", "socket_vsock_proxy")
	if _, err := os.Stat(binaryPath); err != nil {
		return Service{}, fmt.Errorf("socket_vsock_proxy binary not found at %s: %w", binaryPath, err)
	}

	args := []string{
		"--server_type=tcp",
		"--server_fd=3",
		"--client_type=vsock",
		fmt.Sprintf("--client_vsock_id=%d", cid),
		fmt.Sprintf("--client_vsock_port=%d", adbVsockPort),
		"--label=adb",
	}

	return Service{
		Name:   fmt.Sprintf("socket_vsock_proxy:adb:%s", sandboxID),
		Binary: binaryPath,
		Args:   args,
		NetNSName: netNSName,
		Env: []string{
			fmt.Sprintf("HOME=%s", hostPackageDir),
			fmt.Sprintf("CUTTLEFISH_CONFIG_FILE=%s", cuttlefishConfigPath),
			"CUTTLEFISH_INSTANCE=1",
		},
		ExtraFiles:    []*os.File{listenerFile},
		RestartPolicy: RestartOnCrash,
		// ReadyCheck stays nil because end-to-end readiness is checked after the
		// VMM has restored. A TCP dial here would make the proxy attempt CID:5555
		// before the guest exists.
	}, nil
}

// PollVsockProxyReady polls the socket_vsock_proxy until the end-to-end path
// to the guest's adbd is verified working. It does a TCP connect to the proxy
// port, then waits briefly: if the connection stays open (read timeout) or
// returns data, the vsock dial to the guest succeeded and adbd is reachable.
// If the connection is closed immediately (EOF), the vsock dial failed and we
// retry. Returns nil once the path is confirmed, or an error on timeout.
func PollVsockProxyReady(ctx context.Context, proxyAddr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for guest adbd via vsock proxy at %s after %s", proxyAddr, timeout)
		}

		conn, err := net.DialTimeout("tcp", proxyAddr, 1*time.Second)
		if err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}

		buf := make([]byte, 1)
		_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, readErr := conn.Read(buf)
		_ = conn.Close()

		if n > 0 || readErr == nil || errors.Is(readErr, os.ErrDeadlineExceeded) {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
