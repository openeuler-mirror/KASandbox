//go:build windows

package port

import (
	"context"

	"github.com/rs/zerolog"

	"github.com/e2b-dev/infra/packages/envd/internal/services/cgroups"
)

type Forwarder struct {
	logger *zerolog.Logger
}

func NewForwarder(
	logger *zerolog.Logger,
	_ *Scanner,
	_ cgroups.Manager,
) *Forwarder {
	return &Forwarder{logger: logger}
}

func (f *Forwarder) StartForwarding(ctx context.Context) {
	// TODO: Implement Windows port forwarding.
	f.logger.Debug().Msg("Port forwarding is disabled on Windows")
	<-ctx.Done()
}
