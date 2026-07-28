package hostservice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

// VMADDR_CID_ANY is the host-side CID for listeners that accept connections
// from any local guest. Re-exported under a shorter name; also available
// as unix.VMADDR_CID_ANY.
const VMADDR_CID_ANY uint32 = 0xFFFFFFFF

const (
	vsockListenBacklog = 5
	unixSocketPathMax  = 107
)

// VsockListen creates an AF_VSOCK SOCK_STREAM listener for the process-local
// global Android mux.
func VsockListen(cid, port uint32) (*os.File, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket(cid=%d, port=%d): %w", cid, port, err)
	}

	// golang.org/x/sys/unix v0.40.0 exports vsock sockaddr as "SockaddrVM".
	sa := &unix.SockaddrVM{
		CID:  cid,
		Port: port,
	}
	if err := unix.Bind(fd, sa); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("vsock bind(cid=%d, port=%d): %w", cid, port, err)
	}
	if err := unix.Listen(fd, vsockListenBacklog); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("vsock listen(cid=%d, port=%d): %w", cid, port, err)
	}

	// os.NewFile takes ownership of the FD; Close() will close it.
	return os.NewFile(uintptr(fd), fmt.Sprintf("vsock-listen:%d:%d", cid, port)), nil
}

type vsockListener struct {
	port      uint32
	file      *os.File
	closeOnce sync.Once
}

func (l *vsockListener) close() {
	if l == nil || l.file == nil {
		return
	}
	l.closeOnce.Do(func() {
		_ = unix.Shutdown(int(l.file.Fd()), unix.SHUT_RDWR)
		_ = l.file.Close()
	})
}

type vsockAddr struct {
	cid  uint32
	port uint32
}

func (a vsockAddr) Network() string { return "vsock" }
func (a vsockAddr) String() string  { return fmt.Sprintf("%d:%d", a.cid, a.port) }

type vsockConn struct {
	file   *os.File
	fd     int
	local  vsockAddr
	remote vsockAddr
}

func (c *vsockConn) Read(p []byte) (int, error)         { return c.file.Read(p) }
func (c *vsockConn) Write(p []byte) (int, error)        { return c.file.Write(p) }
func (c *vsockConn) Close() error                       { return c.file.Close() }
func (c *vsockConn) LocalAddr() net.Addr                { return c.local }
func (c *vsockConn) RemoteAddr() net.Addr               { return c.remote }
func (c *vsockConn) SetDeadline(t time.Time) error      { return c.file.SetDeadline(t) }
func (c *vsockConn) SetReadDeadline(t time.Time) error  { return c.file.SetReadDeadline(t) }
func (c *vsockConn) SetWriteDeadline(t time.Time) error { return c.file.SetWriteDeadline(t) }
func (c *vsockConn) CloseWrite() error                  { return unix.Shutdown(c.fd, unix.SHUT_WR) }

type UnixListener struct {
	Path string
	File *os.File
}

func NewUnixListener(path string) (*UnixListener, error) {
	if path == "" {
		return nil, fmt.Errorf("Unix listener path is empty")
	}
	if len([]byte(path)) > unixSocketPathMax {
		return nil, fmt.Errorf("Unix listener path exceeds sockaddr_un.sun_path limit: %s", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create Unix listener directory: %w", err)
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("refusing to remove non-socket Unix listener path %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale Unix listener %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspect Unix listener path %s: %w", path, err)
	}

	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen on Unix socket %s: %w", path, err)
	}
	listener.SetUnlinkOnClose(false)
	file, err := listener.File()
	_ = listener.Close()
	if err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("duplicate Unix listener fd %s: %w", path, err)
	}
	return &UnixListener{Path: path, File: file}, nil
}

func (l *UnixListener) Close() error {
	if l == nil {
		return nil
	}
	if l.File != nil {
		_ = l.File.Close()
	}
	if err := os.Remove(l.Path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove Unix listener path %s: %w", l.Path, err)
	}
	return nil
}

type RouteKey struct {
	CID             uint32
	DestinationPort uint32
}

type SandboxRoute struct {
	CID               uint32
	SandboxID         string
	ConfigBackendPath string
	ModemBackendPath  string
}

type routeEntry struct {
	SandboxID   string
	BackendPath string

	mu                sync.Mutex
	activeConnections map[net.Conn]struct{}
	activeWG          sync.WaitGroup
	closing           bool
	connected         chan struct{}
	connectedOnce     sync.Once
}

func newRouteEntry(sandboxID, backendPath string) *routeEntry {
	return &routeEntry{
		SandboxID:         sandboxID,
		BackendPath:       backendPath,
		activeConnections: make(map[net.Conn]struct{}),
		connected:         make(chan struct{}),
	}
}

func (e *routeEntry) addConnections(frontend, backend net.Conn) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closing {
		return fmt.Errorf("route for sandbox %s is closing", e.SandboxID)
	}
	e.activeConnections[frontend] = struct{}{}
	e.activeConnections[backend] = struct{}{}
	e.activeWG.Add(1)
	e.connectedOnce.Do(func() { close(e.connected) })
	return nil
}

func (e *routeEntry) removeConnections(frontend, backend net.Conn) {
	e.mu.Lock()
	delete(e.activeConnections, frontend)
	delete(e.activeConnections, backend)
	e.mu.Unlock()
	e.activeWG.Done()
}

func (e *routeEntry) activeCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.activeConnections) / 2
}

func (e *routeEntry) closeAndWait(ctx context.Context) error {
	e.mu.Lock()
	e.closing = true
	connections := make([]net.Conn, 0, len(e.activeConnections))
	for conn := range e.activeConnections {
		connections = append(connections, conn)
	}
	e.mu.Unlock()

	for _, conn := range connections {
		_ = conn.Close()
	}

	done := make(chan struct{})
	go func() {
		e.activeWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type VsockMux struct {
	mu        sync.RWMutex
	listeners map[uint32]*vsockListener
	routes    map[RouteKey]*routeEntry

	startMu   sync.Mutex
	started   bool
	startErr  error
	ctx       context.Context
	cancel    context.CancelFunc
	acceptWG  sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

func NewVsockMux() *VsockMux {
	return &VsockMux{
		listeners: make(map[uint32]*vsockListener),
		routes:    make(map[RouteKey]*routeEntry),
	}
}

func (m *VsockMux) Start(ctx context.Context) error {
	m.startMu.Lock()
	defer m.startMu.Unlock()
	if m.started {
		return m.startErr
	}
	m.started = true
	m.ctx, m.cancel = context.WithCancel(context.WithoutCancel(ctx))

	for _, port := range []uint32{ConfigServerVsockPort, ModemSimulatorVsockPort} {
		file, err := VsockListen(VMADDR_CID_ANY, port)
		if err != nil {
			m.startErr = fmt.Errorf("bind global vsock listener on port %d: %w", port, err)
			m.closeListeners()
			m.cancel()
			return m.startErr
		}
		listener := &vsockListener{port: port, file: file}
		m.listeners[port] = listener
		m.acceptWG.Add(1)
		go m.acceptLoop(listener)
	}
	return nil
}

func (m *VsockMux) Register(route SandboxRoute) error {
	if route.CID < uint32(VsockCIDBase) {
		return fmt.Errorf("invalid guest CID %d", route.CID)
	}
	if route.SandboxID == "" {
		return fmt.Errorf("sandbox ID is empty")
	}
	if route.ConfigBackendPath == "" || route.ModemBackendPath == "" {
		return fmt.Errorf("both config and modem backend paths are required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.routes {
		if key.CID == route.CID {
			logger.L().Warn(context.Background(), "duplicate vsock CID registration",
				zap.String("sandbox_id", route.SandboxID),
				zap.Uint32("guest_cid", route.CID),
			)
			return fmt.Errorf("guest CID %d is already registered", route.CID)
		}
	}
	m.routes[RouteKey{CID: route.CID, DestinationPort: ConfigServerVsockPort}] = newRouteEntry(route.SandboxID, route.ConfigBackendPath)
	m.routes[RouteKey{CID: route.CID, DestinationPort: ModemSimulatorVsockPort}] = newRouteEntry(route.SandboxID, route.ModemBackendPath)
	return nil
}

func (m *VsockMux) Unregister(ctx context.Context, cid uint32) error {
	m.mu.Lock()
	entries := make([]*routeEntry, 0, 2)
	for key, entry := range m.routes {
		if key.CID == cid {
			delete(m.routes, key)
			entries = append(entries, entry)
		}
	}
	m.mu.Unlock()

	var errs []error
	for _, entry := range entries {
		if err := entry.closeAndWait(ctx); err != nil {
			logger.L().Error(ctx, "vsock route unregister timed out",
				zap.Uint32("guest_cid", cid),
				zap.String("sandbox_id", entry.SandboxID),
				zap.Int("active_connections", entry.activeCount()),
				zap.Error(err),
			)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *VsockMux) WaitForConnection(ctx context.Context, cid, destinationPort uint32) error {
	m.mu.RLock()
	entry := m.routes[RouteKey{CID: cid, DestinationPort: destinationPort}]
	m.mu.RUnlock()
	if entry == nil {
		return fmt.Errorf("route for CID %d port %d is not registered", cid, destinationPort)
	}
	select {
	case <-entry.connected:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *VsockMux) Close(ctx context.Context) error {
	m.closeOnce.Do(func() {
		if m.cancel != nil {
			m.cancel()
		}
		m.closeListeners()

		m.mu.RLock()
		cidSet := make(map[uint32]struct{})
		for key := range m.routes {
			cidSet[key.CID] = struct{}{}
		}
		m.mu.RUnlock()
		var errs []error
		for cid := range cidSet {
			if err := m.Unregister(ctx, cid); err != nil {
				errs = append(errs, err)
			}
		}

		done := make(chan struct{})
		go func() {
			m.acceptWG.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-ctx.Done():
			errs = append(errs, ctx.Err())
		}
		m.closeErr = errors.Join(errs...)
	})
	return m.closeErr
}

func (m *VsockMux) closeListeners() {
	for _, listener := range m.listeners {
		listener.close()
	}
}

func (m *VsockMux) acceptLoop(listener *vsockListener) {
	defer m.acceptWG.Done()
	fd := int(listener.file.Fd())
	for {
		acceptedFD, peer, err := unix.Accept4(fd, unix.SOCK_CLOEXEC)
		if err != nil {
			if m.ctx != nil && m.ctx.Err() != nil {
				return
			}
			if errors.Is(err, unix.EBADF) || errors.Is(err, unix.EINVAL) {
				return
			}
			logger.L().Error(context.Background(), "vsock mux accept failed",
				zap.Uint32("frontend_port", listener.port),
				zap.Error(err),
			)
			continue
		}

		peerVM, ok := peer.(*unix.SockaddrVM)
		if !ok {
			_ = unix.Close(acceptedFD)
			continue
		}
		file := os.NewFile(uintptr(acceptedFD), fmt.Sprintf("vsock:%d:%d", peerVM.CID, listener.port))
		frontend := &vsockConn{
			file:   file,
			fd:     acceptedFD,
			local:  vsockAddr{cid: VMADDR_CID_ANY, port: listener.port},
			remote: vsockAddr{cid: peerVM.CID, port: peerVM.Port},
		}
		// Dispatch performs the backend dial synchronously. This preserves the
		// accept order for modem connections before the next accept is handled.
		m.dispatch(listener.port, peerVM.CID, frontend)
	}
}

func (m *VsockMux) dispatch(destinationPort, sourceCID uint32, frontend net.Conn) {
	key := RouteKey{CID: sourceCID, DestinationPort: destinationPort}
	m.mu.RLock()
	entry := m.routes[key]
	if entry == nil {
		m.mu.RUnlock()
		logger.L().Warn(context.Background(), "vsock connection for unknown CID",
			zap.Uint32("guest_cid", sourceCID),
			zap.Uint32("frontend_port", destinationPort),
			zap.Bool("unknown_cid", true),
		)
		_ = frontend.Close()
		return
	}

	backend, err := net.Dial("unix", entry.BackendPath)
	if err != nil {
		m.mu.RUnlock()
		logger.L().Error(context.Background(), "vsock backend dial failed",
			zap.String("sandbox_id", entry.SandboxID),
			zap.Uint32("guest_cid", sourceCID),
			zap.Uint32("frontend_port", destinationPort),
			zap.String("backend_path", entry.BackendPath),
			zap.Bool("backend_dial_error", true),
			zap.Error(err),
		)
		_ = frontend.Close()
		return
	}
	if err := entry.addConnections(frontend, backend); err != nil {
		m.mu.RUnlock()
		_ = frontend.Close()
		_ = backend.Close()
		return
	}
	m.mu.RUnlock()

	go m.forward(key, entry, frontend, backend)
}

type closeWriter interface {
	CloseWrite() error
}

func copyAndCloseWrite(dst, src net.Conn) (int64, error) {
	n, err := io.Copy(dst, src)
	if writer, ok := dst.(closeWriter); ok {
		_ = writer.CloseWrite()
	}
	return n, err
}

func (m *VsockMux) forward(key RouteKey, entry *routeEntry, frontend, backend net.Conn) {
	defer entry.removeConnections(frontend, backend)
	defer frontend.Close()
	defer backend.Close()

	type result struct {
		bytes int64
		err   error
	}
	guestToBackend := make(chan result, 1)
	backendToGuest := make(chan result, 1)
	go func() {
		n, err := copyAndCloseWrite(backend, frontend)
		guestToBackend <- result{bytes: n, err: err}
	}()
	go func() {
		n, err := copyAndCloseWrite(frontend, backend)
		backendToGuest <- result{bytes: n, err: err}
	}()
	rx := <-guestToBackend
	tx := <-backendToGuest

	service := "config"
	if key.DestinationPort == ModemSimulatorVsockPort {
		service = "modem"
	}
	logger.L().Info(context.Background(), "vsock connection closed",
		zap.String("sandbox_id", entry.SandboxID),
		zap.Uint32("guest_cid", key.CID),
		zap.String("service", service),
		zap.Uint32("frontend_port", key.DestinationPort),
		zap.String("backend_path", entry.BackendPath),
		zap.Int("active_connections", entry.activeCount()-1),
		zap.Int64("bytes_rx", rx.bytes),
		zap.Int64("bytes_tx", tx.bytes),
		zap.Error(errors.Join(rx.err, tx.err)),
	)
}
