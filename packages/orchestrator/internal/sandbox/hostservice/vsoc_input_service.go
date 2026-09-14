package hostservice

import (
	"context"
	"errors"
	"io"
	"net"
	"os"

	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

var inputVsockPorts = [...]uint32{7000, 7100}

func (e *routeEntry) serveInputConnection(key RouteKey, conn net.Conn) {
	e.mu.Lock()
	if e.closing {
		e.mu.Unlock()
		_ = conn.Close()
		return
	}
	previous := make([]net.Conn, 0, len(e.activeConnections))
	for active := range e.activeConnections {
		previous = append(previous, active)
	}
	e.activeConnections[conn] = struct{}{}
	e.activeWG.Add(1)
	e.connectedOnce.Do(func() { close(e.connected) })
	e.mu.Unlock()
	for _, active := range previous {
		_ = active.Close()
	}

	go func() {
		defer e.activeWG.Done()
		_, err := io.Copy(io.Discard, conn)
		_ = conn.Close()
		e.mu.Lock()
		delete(e.activeConnections, conn)
		e.mu.Unlock()
		// EOF and local closes during replacement or cleanup are normal.
		if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) ||
			errors.Is(err, os.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
			err = nil
		}
		if err != nil {
			logger.L().Warn(context.Background(), "vsock input read failed",
				zap.String("sandbox_id", e.SandboxID),
				zap.Uint32("guest_cid", key.CID),
				zap.String("service", "vsoc_input_service"),
				zap.Uint32("frontend_port", key.DestinationPort),
				zap.Error(err),
			)
		}
	}()
}
