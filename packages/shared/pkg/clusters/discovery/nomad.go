package discovery

import (
	"context"
	"fmt"

	nomadapi "github.com/hashicorp/nomad/api"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

const (
	templateManagersTaskGroup = "template-manager"
	templateManagerJobPrefix  = "template-manager"

	orchestratorsTaskGroup = "client-orchestrator"
	orchestratorJobPrefix  = "orchestrator"
)

type NomadQueryFilter string

var FilterTemplateBuilders = NomadQueryFilter(
	fmt.Sprintf(
		"ClientStatus == \"running\" and TaskGroup == \"%s\" and JobID contains \"%s\"",
		templateManagersTaskGroup,
		templateManagerJobPrefix,
	),
)

var FilterTemplateBuildersAndOrchestrators = NomadQueryFilter(
	fmt.Sprintf(
		"ClientStatus == \"running\" and ((TaskGroup == \"%s\" and JobID contains \"%s\") or (TaskGroup == \"%s\" and JobID contains \"%s\"))",
		templateManagersTaskGroup,
		templateManagerJobPrefix,
		orchestratorsTaskGroup,
		orchestratorJobPrefix,
	),
)

// NomadDiscovery Nomad服务发现实现
type NomadDiscovery struct {
	client *nomadapi.Client
	filter NomadQueryFilter
}

// NewNomadDiscovery 创建Nomad发现客户端
func NewNomadDiscovery(client *nomadapi.Client, filter NomadQueryFilter) ServiceDiscovery {
	return &NomadDiscovery{
		client: client,
		filter: filter,
	}
}

// ListOrchestratorAndTemplateBuilderAllocations 查询Template Builder和Orchestrator的Allocations
func (n *NomadDiscovery) ListOrchestratorAndTemplateBuilderAllocations(ctx context.Context) ([]Allocation, error) {
	options := &nomadapi.QueryOptions{
		// https://developer.hashicorp.com/nomad/api-docs/allocations#resources
		// Return allocation resources as part of the response
		Params: map[string]string{"resources": "true"},
		Filter: string(n.filter),
	}

	results, _, err := n.client.Allocations().List(options.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("failed to list Nomad allocations in service discovery: %w", err)
	}

	result := make([]Allocation, 0)
	for _, v := range results {
		if v.AllocatedResources == nil {
			logger.L().Warn(ctx, "No allocated resources found", zap.String("job", v.JobID), zap.String("alloc", v.ID))

			continue
		}

		nets := v.AllocatedResources.Shared.Networks
		if len(nets) == 0 {
			logger.L().Warn(ctx, "No allocation networks found", zap.String("job", v.JobID), zap.String("alloc", v.ID))

			continue
		}

		net := nets[0]
		item := Allocation{
			// For some historical reasons and better developer experience we are using cloud instances name
			// so we can easily map Nomad nodes to cloud instances and skip searching by Nomad client UUIDs.
			NodeID:       v.NodeName,
			AllocationID: v.ID,
			AllocationIP: net.IP,
		}

		result = append(result, item)
	}

	return result, nil
}
