package orchestrator

import (
	"context"
	"fmt"

	"github.com/e2b-dev/infra/packages/api/internal/orchestrator/nodemanager"
	"github.com/e2b-dev/infra/packages/shared/pkg/clusters/discovery"
	"github.com/e2b-dev/infra/packages/shared/pkg/consts"
)

// staticDiscovery 将 shared StaticDiscovery 适配到 NodeDiscovery 接口。
// systemd 承载下 sandbox 节点池与模板构建共用 E2B_STATIC_ALLOCATIONS
// 静态清单（活性由 grpc_health_v1 探测判定），不再依赖 K8s sandbox 标签。
// 清单 NodeID 约定与 K8s Node 名一致，与 k8sDiscovery 行为对齐（不截短 ID）。
type staticDiscovery struct {
	inner *discovery.StaticDiscovery
}

func NewStaticNodeDiscovery(ctx context.Context) NodeDiscovery {
	return &staticDiscovery{inner: discovery.NewStaticDiscoveryFromEnv(ctx)}
}

func (s *staticDiscovery) ListNodes(ctx context.Context) ([]nodemanager.NomadServiceDiscovery, error) {
	allocs, err := s.inner.ListOrchestratorAndTemplateBuilderAllocations(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]nodemanager.NomadServiceDiscovery, 0, len(allocs))
	for _, a := range allocs {
		out = append(out, nodemanager.NomadServiceDiscovery{
			NomadNodeShortID:    a.NodeID,
			OrchestratorAddress: fmt.Sprintf("%s:%d", a.AllocationIP, consts.OrchestratorAPIPort),
			IPAddress:           a.AllocationIP,
		})
	}
	return out, nil
}
