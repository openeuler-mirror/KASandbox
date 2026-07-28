//go:build windows

package port

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/rs/zerolog"
	gopsnet "github.com/shirou/gopsutil/v4/net"

	"github.com/e2b-dev/infra/packages/envd/internal/services/cgroups"
)

const (
	forwardDisabledEnv    = "E2B_FORWARD_DISABLED"
	forwardBindIPEnv      = "E2B_FORWARD_BIND_IP"
	forwardIgnorePortsEnv = "E2B_FORWARD_IGNORE_PORTS"

	defaultForwardBindIP      = "169.254.0.21"
	defaultForwardIgnorePorts = ""
)

type Forwarder struct {
	logger            *zerolog.Logger
	scannerSubscriber *ScannerSubscriber
	ports             map[uint32]*ForwardedPort
	bindIP            string
	ignoredPorts      map[uint32]struct{}
	mu                sync.Mutex
	disabled          bool
}

type PortState string

const (
	PortStateForward PortState = "FORWARD"
	PortStateDelete  PortState = "DELETE"
)

type ForwardedPort struct {
	cancel   context.CancelFunc
	listener net.Listener
	state    PortState
	port     uint32
	targetIP string
}

func NewForwarder(
	logger *zerolog.Logger,
	scanner *Scanner,
	_ cgroups.Manager,
) *Forwarder {
	forwarder := &Forwarder{
		logger:       logger,
		bindIP:       forwardBindIP(),
		ports:        make(map[uint32]*ForwardedPort),
		ignoredPorts: forwardIgnoredPorts(),
		disabled:     forwardDisabled(),
	}

	if scanner != nil && !forwarder.disabled {
		forwarder.scannerSubscriber = scanner.AddSubscriber(
			logger,
			"windows-port-forwarder",
			&ScannerFilter{
				IPs:   []string{"127.0.0.1", "localhost", "::1"},
				State: "LISTEN",
			},
		)
	}

	return forwarder
}

func (f *Forwarder) StartForwarding(ctx context.Context) {
	if f.disabled {
		f.logger.Debug().Msg("Port forwarding is disabled on Windows")
		<-ctx.Done()

		return
	}

	if f.scannerSubscriber == nil {
		f.logger.Error().Msg("Cannot start forwarding because scanner subscriber is nil")

		return
	}

	defer f.stopAllForwarding()

	for {
		select {
		case <-ctx.Done():
			return
		case procs, ok := <-f.scannerSubscriber.Messages:
			if !ok {
				return
			}

			f.refreshForwardedPorts(ctx, procs)
		}
	}
}

func (f *Forwarder) refreshForwardedPorts(ctx context.Context, procs []gopsnet.ConnectionStat) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, forwarded := range f.ports {
		forwarded.state = PortStateDelete
	}

	seen := make(map[uint32]struct{})
	for _, proc := range procs {
		port := proc.Laddr.Port
		if _, ignored := f.ignoredPorts[port]; ignored {
			continue
		}
		if _, ok := seen[port]; ok {
			continue
		}
		seen[port] = struct{}{}

		if forwarded, ok := f.ports[port]; ok {
			forwarded.state = PortStateForward
			continue
		}

		forwarded := &ForwardedPort{
			port:     port,
			state:    PortStateForward,
			targetIP: proc.Laddr.IP,
		}
		if f.startPortForwarding(ctx, forwarded) {
			f.ports[port] = forwarded
		}
	}

	for port, forwarded := range f.ports {
		if forwarded.state == PortStateDelete {
			f.stopPortForwarding(forwarded)
			delete(f.ports, port)
		}
	}
}

func (f *Forwarder) startPortForwarding(ctx context.Context, forwarded *ForwardedPort) bool {
	listenAddress := net.JoinHostPort(f.bindIP, strconv.FormatUint(uint64(forwarded.port), 10))
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		if isAddressInUse(err) {
			f.logger.Debug().
				Err(err).
				Str("listen_address", listenAddress).
				Uint32("port", forwarded.port).
				Msg("Skipping port forwarding because the external address is already in use")
		} else {
			f.logger.Error().
				Err(err).
				Str("listen_address", listenAddress).
				Uint32("port", forwarded.port).
				Msg("Failed to start port forwarding listener")
		}

		return false
	}

	forwardCtx, cancel := context.WithCancel(ctx)
	forwarded.listener = listener
	forwarded.cancel = cancel

	f.logger.Debug().
		Str("listen_address", listenAddress).
		Uint32("port", forwarded.port).
		Msg("Started Windows port forwarding")

	go f.acceptConnections(forwardCtx, forwarded)

	return true
}

func (f *Forwarder) acceptConnections(ctx context.Context, forwarded *ForwardedPort) {
	for {
		conn, err := forwarded.listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				f.logger.Debug().
					Err(err).
					Uint32("port", forwarded.port).
					Msg("Port forwarding listener stopped accepting connections")

				return
			}
		}

		go f.proxyConnection(ctx, forwarded, conn)
	}
}

func (f *Forwarder) proxyConnection(ctx context.Context, forwarded *ForwardedPort, client net.Conn) {
	defer client.Close()

	var dialer net.Dialer
	targetAddress := net.JoinHostPort(forwarded.targetIP, strconv.FormatUint(uint64(forwarded.port), 10))
	backend, err := dialer.DialContext(ctx, "tcp", targetAddress)
	if err != nil {
		f.logger.Debug().
			Err(err).
			Str("target_address", targetAddress).
			Uint32("port", forwarded.port).
			Msg("Failed to connect to forwarded localhost port")

		return
	}
	defer backend.Close()

	done := make(chan struct{}, 2)
	go proxyCopy(done, client, backend)
	go proxyCopy(done, backend, client)

	select {
	case <-ctx.Done():
	case <-done:
	}
}

func proxyCopy(done chan<- struct{}, dst net.Conn, src net.Conn) {
	_, _ = io.Copy(dst, src)
	_ = dst.Close()
	_ = src.Close()

	select {
	case done <- struct{}{}:
	default:
	}
}

func (f *Forwarder) stopPortForwarding(forwarded *ForwardedPort) {
	if forwarded.cancel != nil {
		forwarded.cancel()
	}
	if forwarded.listener != nil {
		_ = forwarded.listener.Close()
	}

	f.logger.Debug().
		Uint32("port", forwarded.port).
		Msg("Stopped Windows port forwarding")
}

func (f *Forwarder) stopAllForwarding() {
	f.mu.Lock()
	defer f.mu.Unlock()

	for port, forwarded := range f.ports {
		f.stopPortForwarding(forwarded)
		delete(f.ports, port)
	}
}

func forwardDisabled() bool {
	value := strings.TrimSpace(os.Getenv(forwardDisabledEnv))
	if value == "" {
		return false
	}

	disabled, err := strconv.ParseBool(value)
	if err != nil {
		return false
	}

	return disabled
}

func forwardBindIP() string {
	if bindIP := strings.TrimSpace(os.Getenv(forwardBindIPEnv)); bindIP != "" {
		return bindIP
	}

	return defaultForwardBindIP
}

func forwardIgnoredPorts() map[uint32]struct{} {
	raw := os.Getenv(forwardIgnorePortsEnv)
	if strings.TrimSpace(raw) == "" {
		raw = defaultForwardIgnorePorts
	}

	ignored := make(map[uint32]struct{})
	for _, part := range strings.Split(raw, ",") {
		portRaw := strings.TrimSpace(part)
		if portRaw == "" {
			continue
		}

		port, err := strconv.ParseUint(portRaw, 10, 32)
		if err != nil || port > 65535 {
			continue
		}
		ignored[uint32(port)] = struct{}{}
	}

	return ignored
}

func isAddressInUse(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.Errno(10048)) {
		return true
	}

	lowerErr := strings.ToLower(err.Error())
	return strings.Contains(lowerErr, "address already in use") ||
		strings.Contains(lowerErr, "only one usage of each socket address")
}
