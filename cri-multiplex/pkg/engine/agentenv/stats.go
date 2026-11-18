package agentenv

import (
	"context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// ContainerStats 只返回逻辑容器身份，不把共享 VM 指标冒充容器指标。
func (e *Engine) ContainerStats(_ context.Context, req *runtime.ContainerStatsRequest) (*runtime.ContainerStatsResponse, error) {
	if req == nil || req.GetContainerId() == "" {
		return nil, status.Error(codes.InvalidArgument, "container id is required")
	}
	record, ok := e.getContainer(req.GetContainerId())
	if !ok || record.State == containerStateRemoved {
		return nil, status.Error(codes.NotFound, "container not found")
	}
	return &runtime.ContainerStatsResponse{Stats: projectionStats(record)}, nil
}

func (e *Engine) ListContainerStats(_ context.Context, req *runtime.ListContainerStatsRequest) (*runtime.ListContainerStatsResponse, error) {
	var filter *runtime.ContainerStatsFilter
	if req != nil {
		filter = req.Filter
	}
	result := &runtime.ListContainerStatsResponse{}
	for _, record := range e.listContainers() {
		if record.State == containerStateRemoved {
			continue
		}
		if filter != nil && ((filter.GetId() != "" && filter.GetId() != record.ContainerID) || (filter.GetPodSandboxId() != "" && filter.GetPodSandboxId() != record.CRISandboxID) || !labelsMatch(record.Labels, filter.GetLabelSelector())) {
			continue
		}
		result.Stats = append(result.Stats, projectionStats(record))
	}
	return result, nil
}

func projectionStats(record containerRecord) *runtime.ContainerStats {
	return &runtime.ContainerStats{Attributes: &runtime.ContainerAttributes{
		Id: record.ContainerID, Metadata: &runtime.ContainerMetadata{Name: record.Name, Attempt: record.Attempt},
		Labels: cloneStringMap(record.Labels), Annotations: cloneStringMap(record.Annotations),
	}}
}
