package orchestrator

import (
	"context"
	"fmt"

	"github.com/e2b-dev/infra/packages/api/internal/orchestrator/nodemanager"
	"github.com/e2b-dev/infra/packages/shared/pkg/consts"
	nomadapi "github.com/hashicorp/nomad/api"
)

type nomadDiscovery struct {
	client *nomadapi.Client
}

func NewNomadDiscovery(client *nomadapi.Client) NodeDiscovery {
	return &nomadDiscovery{client: client}
}

func (n *nomadDiscovery) ListNodes(ctx context.Context) ([]nodemanager.NomadServiceDiscovery, error) {
	opt := &nomadapi.QueryOptions{
		Filter: `Status == "ready"`,
	}
	nodes, _, err := n.client.Nodes().List(opt.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	out := make([]nodemanager.NomadServiceDiscovery, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, nodemanager.NomadServiceDiscovery{
			NomadNodeShortID:    node.ID[:consts.NodeIDLength],
			OrchestratorAddress: fmt.Sprintf("%s:%d", node.Address, consts.OrchestratorAPIPort),
			IPAddress:           node.Address,
		})
	}
	return out, nil
}

