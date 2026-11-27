package socket

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/e2b-dev/infra/packages/shared/pkg/env"
)

const (
	waitInterval              = 10 * time.Millisecond
	defaultWaitTimeoutSeconds = 300
)

// Wait waits for the given file to exist.
func Wait(ctx context.Context, socketPath string) error {
	timeoutSeconds, err := env.GetEnvAsInt("SOCKET_WAIT_TIMEOUT_SECONDS", defaultWaitTimeoutSeconds)
	if err != nil {
		return err
	}

	// KASandbox: bound wait via SOCKET_WAIT_TIMEOUT_SECONDS (default 300s),
	// independent of short caller deadlines.
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	ticker := time.NewTicker(waitInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("cancelled wait for socket '%s': %w, cause: %w", socketPath, ctx.Err(), context.Cause(ctx))
		case <-ticker.C:
			if _, err := os.Stat(socketPath); err != nil {
				continue
			}

			return nil
		}
	}
}
