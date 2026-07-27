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

func AllocateFreePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("allocate free port: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port, nil
}

// AllocateFreeUDPPort binds a UDP socket to a random port on 127.0.0.1, reads
// the assigned port, then immediately closes the socket so the caller (e.g.,
// webrtc_bin's UDP streaming listener) can rebind. Same race-window caveat
// as AllocateFreePort — acceptable in practice.
func AllocateFreeUDPPort() (int, error) {
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return 0, fmt.Errorf("allocate free udp port: %w", err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	_ = conn.Close()
	return port, nil
}

func BuildVsockProxyService(config cfg.BuilderConfig, cid int64, sandboxID string) (Service, int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Service{}, 0, fmt.Errorf("bind adb tcp listener: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	tcpListener, ok := listener.(*net.TCPListener)
	if !ok {
		_ = listener.Close()
		return Service{}, 0, fmt.Errorf("adb listener is not a TCP listener")
	}
	listenerFile, err := tcpListener.File()
	_ = listener.Close()
	if err != nil {
		return Service{}, 0, fmt.Errorf("duplicate adb tcp listener fd: %w", err)
	}

	binaryPath := filepath.Join(config.CvdHostPackageDir, "bin", "socket_vsock_proxy")
	if _, err := os.Stat(binaryPath); err != nil {
		_ = listenerFile.Close()
		return Service{}, 0, fmt.Errorf("socket_vsock_proxy binary not found at %s: %w", binaryPath, err)
	}

	args := []string{
		"--server_type", "tcp",
		"--server_fd", "3",
		"--client_type", "vsock",
		"--client_vsock_id", fmt.Sprintf("%d", cid),
		"--client_vsock_port", fmt.Sprintf("%d", adbVsockPort),
		"--label", fmt.Sprintf("adb:%s", sandboxID),
	}

	return Service{
		Name:          fmt.Sprintf("socket_vsock_proxy:adb:%s", sandboxID),
		Binary:        binaryPath,
		Args:          args,
		Env:           []string{fmt.Sprintf("HOME=%s", config.CvdHostPackageDir)},
		ExtraFiles:    []*os.File{listenerFile},
		RestartPolicy: RestartOnCrash,
		// End-to-end readiness is checked after the VMM has restored. A TCP
		// dial here would make the proxy attempt CID:5555 before the guest exists.
		ReadyCheck: NoReadyCheck{},
	}, port, nil
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
