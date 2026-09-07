package hostservice

import (
	"context"
	"fmt"
	"io"
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
		Name:      fmt.Sprintf("socket_vsock_proxy:adb:%s", sandboxID),
		Binary:    binaryPath,
		Args:      args,
		NetNSName: netNSName,
		Env: []string{
			fmt.Sprintf("HOME=%s", hostPackageDir),
			fmt.Sprintf("CUTTLEFISH_CONFIG_FILE=%s", cuttlefishConfigPath),
			"CUTTLEFISH_INSTANCE=1",
		},
		ExtraFiles:    []*os.File{listenerFile},
		RestartPolicy: RestartOnCrash,
		// End-to-end ADB readiness is checked after envd initialization.
		ReadyCheck: &ProcessAlive{},
	}, nil
}

// PollVsockProxyReady waits until an A_CNXN exchange succeeds through
// socket_vsock_proxy, proving that the complete proxy-to-adbd path is ready.
func PollVsockProxyReady(ctx context.Context, proxyAddr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("timeout waiting for guest adbd via vsock proxy at %s after %s: %w", proxyAddr, timeout, lastErr)
		}
		if err := probeADBPath(ctx, proxyAddr); err == nil {
			return nil
		} else {
			lastErr = err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func probeADBPath(ctx context.Context, proxyAddr string) error {
	dialer := net.Dialer{Timeout: time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return fmt.Errorf("connect vsock proxy: %w", err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return fmt.Errorf("set ADB handshake deadline: %w", err)
	}
	if _, err := conn.Write(adbClientCNXN()); err != nil {
		return fmt.Errorf("send A_CNXN: %w", err)
	}

	header := make([]byte, adbHeaderSize)
	if _, err := io.ReadFull(conn, header); err != nil {
		return fmt.Errorf("read ADB reply: %w", err)
	}
	command, payloadLength, err := parseADBReplyHeader(header)
	if err != nil {
		return err
	}
	if payloadLength > 0 {
		if _, err := io.CopyN(io.Discard, conn, int64(payloadLength)); err != nil {
			return fmt.Errorf("read ADB reply payload: %w", err)
		}
	}
	if !adbReplyProvesPath(command) {
		return fmt.Errorf("unexpected ADB reply command %#x", command)
	}

	return nil
}
