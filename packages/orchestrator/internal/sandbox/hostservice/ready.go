package hostservice

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

type ConfigServerReady struct {
	Path string
}

func (r *ConfigServerReady) Check(ctx context.Context) error {
	dialer := net.Dialer{Timeout: 500 * time.Millisecond}
	conn, err := dialer.DialContext(ctx, "unix", r.Path)
	if err != nil {
		return fmt.Errorf("connect config_server Unix socket %s: %w", r.Path, err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	payload, err := io.ReadAll(conn)
	if err != nil {
		return fmt.Errorf("read config_server response: %w", err)
	}
	if len(payload) == 0 {
		return fmt.Errorf("config_server returned an empty protobuf")
	}
	return nil
}

func (r *ConfigServerReady) String() string { return "config-server:" + r.Path }

type ModemSimulatorReady struct {
	Path string
}

func (r *ModemSimulatorReady) Check(ctx context.Context) error {
	dialer := net.Dialer{Timeout: 500 * time.Millisecond}
	conn, err := dialer.DialContext(ctx, "unix", r.Path)
	if err != nil {
		return fmt.Errorf("connect modem_simulator Unix socket %s: %w", r.Path, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := io.WriteString(conn, "AT\r"); err != nil {
		return fmt.Errorf("write modem_simulator readiness command: %w", err)
	}
	response := make([]byte, 0, 64)
	buf := make([]byte, 64)
	for len(response) < 4096 {
		n, err := conn.Read(buf)
		response = append(response, buf[:n]...)
		responseText := string(response)
		// This modem_simulator build rejects the otherwise valid bare AT command
		// with operation-not-supported. Receiving that exact terminal response still
		// proves the process accepted the connection and handled an AT command.
		if strings.Contains(responseText, "OK\r") || strings.Contains(responseText, "+CME ERROR: 4\r") {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read modem_simulator readiness response: %w", err)
		}
	}
	return fmt.Errorf("modem_simulator returned unexpected response %q", string(response))
}

func (r *ModemSimulatorReady) String() string { return "modem-simulator:" + r.Path }
