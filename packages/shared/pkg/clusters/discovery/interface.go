package discovery

import (
	"context"
)

// Allocation 表示服务发现返回的通用工作负载实例（对应Nomad Allocation或K8s Pod）
type Allocation struct {
	NodeID       string
	AllocationID string
	AllocationIP string
}

// ServiceDiscovery 服务发现通用接口
type ServiceDiscovery interface {
	ListOrchestratorAndTemplateBuilderAllocations(ctx context.Context) ([]Allocation, error)
}

