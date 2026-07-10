package orchestrator

import (
	"context"

	"github.com/e2b-dev/infra/packages/api/internal/orchestrator/nodemanager"
)

type NodeDiscovery interface {
	ListNodes(ctx context.Context) ([]nodemanager.NomadServiceDiscovery, error)
}

