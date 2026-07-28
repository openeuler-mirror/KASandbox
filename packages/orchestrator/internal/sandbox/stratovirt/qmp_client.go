package stratovirt

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

type QMPEvent struct {
	Event string `json:"event"`
	Data  any    `json:"data,omitempty"`
}

type qmpClient struct {
	socketPath string
	conn       net.Conn
	decoder    *json.Decoder
	mu         sync.Mutex
}

func newQMPClient(socketPath string) *qmpClient {
	return &qmpClient{
		socketPath: socketPath,
	}
}

func (c *qmpClient) connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return fmt.Errorf("error connecting to QMP socket %s: %w", c.socketPath, err)
	}

	c.conn = conn
	c.decoder = json.NewDecoder(conn)

	var greeting struct {
		QMP struct {
			Version struct {
				Qemu struct {
					Major int `json:"major"`
					Micro int `json:"micro"`
					Minor int `json:"minor"`
				} `json:"qemu"`
				Package string `json:"package,omitempty"`
			} `json:"version"`
			Capabilities []string `json:"capabilities"`
		} `json:"QMP"`
	}

	if err := c.decoder.Decode(&greeting); err != nil {
		conn.Close()
		c.conn = nil
		c.decoder = nil
		return fmt.Errorf("error reading QMP greeting: %w", err)
	}

	zap.L().Sugar().Infof("QMP greeting: version=%d.%d.%d, package=%s, capabilities=%v",
		greeting.QMP.Version.Qemu.Major,
		greeting.QMP.Version.Qemu.Micro,
		greeting.QMP.Version.Qemu.Minor,
		greeting.QMP.Version.Package,
		greeting.QMP.Capabilities)

	return nil
}

func (c *qmpClient) executeCommandWithReturn(ctx context.Context, command string, args map[string]any, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return fmt.Errorf("QMP connection not established")
	}

	cmd := map[string]any{
		"execute": command,
	}
	if args != nil {
		cmd["arguments"] = args
	}

	payload, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("error marshaling QMP command %s: %w", command, err)
	}

	deadline, ok := ctx.Deadline()
	if ok {
		c.conn.SetDeadline(deadline)
	} else {
		c.conn.SetDeadline(time.Now().Add(30 * time.Second))
	}

	if _, err := c.conn.Write(append(payload, '\n')); err != nil {
		return fmt.Errorf("error writing QMP command %s: %w", command, err)
	}

	var response struct {
		Return json.RawMessage `json:"return,omitempty"`
		Error  *qmpErr         `json:"error,omitempty"`
	}

	if err := c.readResponse(&response); err != nil {
		return fmt.Errorf("error reading QMP response for %s: %w", command, err)
	}

	if response.Error != nil {
		return fmt.Errorf("QMP command %s failed: class=%s, desc=%s",
			command, response.Error.Class, response.Error.Desc)
	}

	if out != nil && len(response.Return) > 0 {
		if err := json.Unmarshal(response.Return, out); err != nil {
			return fmt.Errorf("error unmarshaling QMP %s return: %w", command, err)
		}
	}

	return nil
}

const maxQMPEventSkips = 1024

func (c *qmpClient) readResponse(target any) error {
	if c.decoder == nil {
		return fmt.Errorf("QMP decoder not initialized")
	}

	skipped := 0
	for {
		var raw json.RawMessage
		if err := c.decoder.Decode(&raw); err != nil {
			return fmt.Errorf("error reading QMP message: %w", err)
		}

		var event QMPEvent
		if err := json.Unmarshal(raw, &event); err == nil && event.Event != "" {
			skipped++
			if skipped > maxQMPEventSkips {
				return fmt.Errorf("too many QMP async events (%d) without a command response", skipped)
			}
			zap.L().Sugar().Debugf("Skipping QMP async event: %s", event.Event)
			continue
		}

		if err := json.Unmarshal(raw, target); err != nil {
			return fmt.Errorf("error parsing QMP response: %w", err)
		}
		return nil
	}
}

type qmpErr struct {
	Class string `json:"class"`
	Desc  string `json:"desc"`
}

func (c *qmpClient) close() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
		c.decoder = nil
	}
}

func (c *qmpClient) negotiateCapabilities(ctx context.Context) error {
	return c.executeCommandWithReturn(ctx, "qmp_capabilities", nil, nil)
}

func (c *qmpClient) stopVM(ctx context.Context) error {
	return c.executeCommandWithReturn(ctx, "stop", nil, nil)
}

func (c *qmpClient) migrate(ctx context.Context, uri string) error {
	return c.executeCommandWithReturn(ctx, "migrate", map[string]any{"uri": uri}, nil)
}

func (c *qmpClient) quitVM(ctx context.Context) error {
	err := c.executeCommandWithReturn(ctx, "quit", nil, nil)
	if err != nil {
		logger.L().Warn(ctx, "error sending quit command", zap.Error(err))
	}

	c.close()

	return err
}
