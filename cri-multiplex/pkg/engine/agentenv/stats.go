package agentenv

import (
	"context"
	"strconv"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	runtimeapi "github.com/cri-multiplex/pkg/agentenv/runtime/v1"
)

const (
	statsScopeAnnotation       = "agentenv.dev/stats-scope"
	statsNativeIDAnnotation    = "agentenv.dev/native-sandbox-id"
	statsCPUPercentAnnotation  = "agentenv.dev/cpu-used-percent"
	statsDiskTotalAnnotation   = "agentenv.dev/disk-total-bytes"
	statsScopeSandboxAggregate = "sandbox-aggregate"
)

// ContainerStats 返回 Runtime 可证明的 sandbox 聚合统计。
// 同一 microVM 的多个逻辑容器只有一个 accounting container 携带 aggregate 指标，
// 其他容器只返回 CRI 身份字段，避免复制共享 VM 指标。
func (e *Engine) ContainerStats(ctx context.Context, req *runtime.ContainerStatsRequest) (*runtime.ContainerStatsResponse, error) {
	if req == nil || req.GetContainerId() == "" {
		return nil, status.Error(codes.InvalidArgument, "container id is required")
	}
	record, ok := e.getContainer(req.GetContainerId())
	if !ok || record.State == containerStateRemoved {
		return nil, status.Errorf(codes.NotFound, "container %s not found", req.GetContainerId())
	}

	owner := e.statsOwnerForPod(record.CRISandboxID)
	var metric *runtimeapi.SandboxStats
	if record.State == containerStateRunning && owner == record.ContainerID {
		pod, found := e.getPod(record.CRISandboxID)
		if !found {
			return nil, status.Errorf(codes.NotFound, "pod sandbox %s not found", record.CRISandboxID)
		}
		if pod.NativeSandboxID == "" {
			return nil, status.Errorf(codes.FailedPrecondition, "pod sandbox %s has no native id", record.CRISandboxID)
		}
		metrics, err := e.fetchSandboxStats(ctx, []string{pod.NativeSandboxID})
		if err != nil {
			return nil, err
		}
		metric = metrics[pod.NativeSandboxID]
	}
	return &runtime.ContainerStatsResponse{
		Stats: containerStatsFromRecord(record, metric),
	}, nil
}

func (e *Engine) ListContainerStats(ctx context.Context, req *runtime.ListContainerStatsRequest) (*runtime.ListContainerStatsResponse, error) {
	var filter *runtime.ContainerStatsFilter
	if req != nil {
		filter = req.Filter
	}

	records := e.listContainers()
	owners := statsOwners(records)
	candidates := make([]containerRecord, 0, len(records))
	sandboxIDs := make([]string, 0)
	nativeIDs := map[string]string{}
	seenSandbox := map[string]struct{}{}
	for _, record := range records {
		if record.State == containerStateRemoved || !containerStatsMatches(record, filter) {
			continue
		}
		candidates = append(candidates, record)
		if record.State == containerStateRunning && owners[record.CRISandboxID] == record.ContainerID {
			pod, found := e.getPod(record.CRISandboxID)
			if !found {
				return nil, status.Errorf(codes.NotFound, "pod sandbox %s not found", record.CRISandboxID)
			}
			if pod.NativeSandboxID == "" {
				return nil, status.Errorf(codes.FailedPrecondition, "pod sandbox %s has no native id", record.CRISandboxID)
			}
			nativeIDs[record.CRISandboxID] = pod.NativeSandboxID
			if _, found := seenSandbox[pod.NativeSandboxID]; !found {
				seenSandbox[pod.NativeSandboxID] = struct{}{}
				sandboxIDs = append(sandboxIDs, pod.NativeSandboxID)
			}
		}
	}
	metrics, err := e.fetchSandboxStats(ctx, sandboxIDs)
	if err != nil {
		return nil, err
	}

	items := make([]*runtime.ContainerStats, 0, len(candidates))
	for _, record := range candidates {
		var metric *runtimeapi.SandboxStats
		if record.State == containerStateRunning && owners[record.CRISandboxID] == record.ContainerID {
			metric = metrics[nativeIDs[record.CRISandboxID]]
		}
		items = append(items, containerStatsFromRecord(record, metric))
	}
	return &runtime.ListContainerStatsResponse{Stats: items}, nil
}
func (e *Engine) statsOwnerForPod(podID string) string {
	for _, record := range e.listContainers() {
		if record.CRISandboxID == podID && record.State == containerStateRunning {
			return record.ContainerID
		}
	}
	return ""
}

func statsOwners(records []containerRecord) map[string]string {
	owners := make(map[string]string)
	for _, record := range records {
		if record.State != containerStateRunning {
			continue
		}
		if _, found := owners[record.CRISandboxID]; !found {
			owners[record.CRISandboxID] = record.ContainerID
		}
	}
	return owners
}

func containerStatsMatches(record containerRecord, filter *runtime.ContainerStatsFilter) bool {
	if filter == nil {
		return true
	}
	if filter.GetId() != "" && filter.GetId() != record.ContainerID {
		return false
	}
	if filter.GetPodSandboxId() != "" && filter.GetPodSandboxId() != record.CRISandboxID {
		return false
	}
	return labelsMatch(record.Labels, filter.GetLabelSelector())
}

func (e *Engine) fetchSandboxStats(ctx context.Context, sandboxIDs []string) (map[string]*runtimeapi.SandboxStats, error) {
	if len(sandboxIDs) == 0 {
		return map[string]*runtimeapi.SandboxStats{}, nil
	}
	if err := e.ensureClients(); err != nil {
		return nil, err
	}
	if e.admin == nil {
		return nil, status.Error(codes.Unavailable, "AgentENV stats client is not initialized")
	}
	response, err := e.admin.GetStats(ctx, &runtimeapi.SandboxStatsRequest{NativeSandboxIds: sandboxIDs})
	if err != nil {
		return nil, e.mapRuntimeError(err)
	}
	stats := make(map[string]*runtimeapi.SandboxStats, len(response.GetSandboxes()))
	for _, item := range response.GetSandboxes() {
		if item == nil || item.GetNativeSandboxId() == "" {
			continue
		}
		stats[item.GetNativeSandboxId()] = item
	}
	return stats, nil
}
func containerStatsFromRecord(record containerRecord, metric *runtimeapi.SandboxStats) *runtime.ContainerStats {
	annotations := cloneStringMap(record.Annotations)
	if metric == nil {
		return &runtime.ContainerStats{Attributes: containerStatsAttributes(record, annotations)}
	}
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[statsScopeAnnotation] = statsScopeSandboxAggregate
	annotations[statsNativeIDAnnotation] = metric.GetNativeSandboxId()
	annotations[statsCPUPercentAnnotation] = strconv.FormatFloat(metric.GetCpuUsedPercent(), 'f', 3, 64)
	annotations[statsDiskTotalAnnotation] = strconv.FormatUint(metric.GetDiskTotalBytes(), 10)

	return &runtime.ContainerStats{
		Attributes: containerStatsAttributes(record, annotations),
		Memory:     memoryUsageFromSandboxStats(metric),
		WritableLayer: &runtime.FilesystemUsage{
			Timestamp: metricTimestampNano(metric),
			FsId:      &runtime.FilesystemIdentifier{Mountpoint: "/"},
			UsedBytes: uint64Value(metric.GetDiskUsedBytes()),
		},
	}
}

func containerStatsAttributes(record containerRecord, annotations map[string]string) *runtime.ContainerAttributes {
	return &runtime.ContainerAttributes{
		Id: record.ContainerID,
		Metadata: &runtime.ContainerMetadata{
			Name:    record.Name,
			Attempt: record.Attempt,
		},
		Labels:      cloneStringMap(record.Labels),
		Annotations: annotations,
	}
}

func memoryUsageFromSandboxStats(metric *runtimeapi.SandboxStats) *runtime.MemoryUsage {
	used := metric.GetMemoryUsedBytes()
	total := metric.GetMemoryTotalBytes()
	cache := metric.GetMemoryCacheBytes()
	workingSet := used - min(used, cache)
	available := total - min(total, used)
	return &runtime.MemoryUsage{
		Timestamp:       metricTimestampNano(metric),
		WorkingSetBytes: uint64Value(workingSet),
		AvailableBytes:  uint64Value(available),
		UsageBytes:      uint64Value(used),
	}
}

func metricTimestampNano(metric *runtimeapi.SandboxStats) int64 {
	if metric == nil || metric.GetSampledAt() == nil {
		return 0
	}
	const nanosPerSecond = int64(1_000_000_000)
	seconds := metric.GetSampledAt().GetSeconds()
	if seconds > int64(^uint64(0)>>1)/nanosPerSecond {
		return int64(^uint64(0) >> 1)
	}
	return seconds * nanosPerSecond
}

func uint64Value(value uint64) *runtime.UInt64Value {
	return &runtime.UInt64Value{Value: value}
}
