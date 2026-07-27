package hostservice

import (
	"context"
	"fmt"
	"net"
	"time"
)

type TCPPortReady struct {
	Address string
}

func (r *TCPPortReady) Check(ctx context.Context) error {
	conn, err := net.DialTimeout("tcp", r.Address, 500*time.Millisecond)
	if err != nil {
		return fmt.Errorf("tcp port %s not connectable: %w", r.Address, err)
	}
	_ = conn.Close()
	return nil
}

func (r *TCPPortReady) String() string {
	return fmt.Sprintf("tcp-port:%s", r.Address)
}

type NoReadyCheck struct{}

func (NoReadyCheck) Check(ctx context.Context) error { return nil }
func (NoReadyCheck) String() string                  { return "none" }
