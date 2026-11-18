package agentenv

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	runtimeapi "github.com/cri-multiplex/pkg/agentenv/runtime/v1"
)

func TestContainerStatsUsesSingleSandboxAccountingContainer(t *testing.T) {
	instance, _, _ := newReadyContainerTestEngine(t)
	first, err := instance.CreateContainer(context.Background(), criCreateContainerRequest("cri-1", "app", "agentenv.dev/app:v1", 0))
	if err != nil {
		t.Fatalf("CreateContainer first: %v", err)
	}
	second, err := instance.CreateContainer(context.Background(), criCreateContainerRequest("cri-1", "sidecar", "agentenv.dev/sidecar:v1", 0))
	if err != nil {
		t.Fatalf("CreateContainer second: %v", err)
	}
	for _, id := range []string{first.GetContainerId(), second.GetContainerId()} {
		if _, err := instance.StartContainer(context.Background(), &runtime.StartContainerRequest{ContainerId: id}); err != nil {
			t.Fatalf("StartContainer %s: %v", id, err)
		}
	}

	adminCalls := 0
	instance.admin = &fakeAdmin{getStats: func(_ context.Context, req *runtimeapi.SandboxStatsRequest) (*runtimeapi.SandboxStatsResponse, error) {
		adminCalls++
		if len(req.GetNativeSandboxIds()) != 1 || req.GetNativeSandboxIds()[0] != "019a0000-0000-7000-8000-000000000201" {
			t.Fatalf("stats sandbox ids = %v", req.GetNativeSandboxIds())
		}
		return &runtimeapi.SandboxStatsResponse{Sandboxes: []*runtimeapi.SandboxStats{{
			NativeSandboxId:  "019a0000-0000-7000-8000-000000000201",
			SampledAt:        timestamppb.New(time.Unix(1_700_000_000, 0).UTC()),
			CpuCount:         2,
			CpuUsedPercent:   37.5,
			MemoryUsedBytes:  512,
			MemoryTotalBytes: 1024,
			MemoryCacheBytes: 128,
			DiskUsedBytes:    2048,
			DiskTotalBytes:   4096,
		}}}, nil
	}}

	owner := instance.statsOwnerForPod("cri-1")
	if owner == "" {
		t.Fatal("stats owner was not selected")
	}
	ownerResponse, err := instance.ContainerStats(context.Background(), &runtime.ContainerStatsRequest{ContainerId: owner})
	if err != nil {
		t.Fatalf("ContainerStats owner: %v", err)
	}
	ownerStats := ownerResponse.GetStats()
	if ownerStats.GetMemory().GetUsageBytes().GetValue() != 512 ||
		ownerStats.GetMemory().GetWorkingSetBytes().GetValue() != 384 ||
		ownerStats.GetMemory().GetAvailableBytes().GetValue() != 512 {
		t.Fatalf("memory stats mismatch: %+v", ownerStats.GetMemory())
	}
	if ownerStats.GetWritableLayer().GetUsedBytes().GetValue() != 2048 {
		t.Fatalf("filesystem stats mismatch: %+v", ownerStats.GetWritableLayer())
	}
	if ownerStats.GetAttributes().GetAnnotations()[statsScopeAnnotation] != statsScopeSandboxAggregate {
		t.Fatalf("stats scope annotation missing: %+v", ownerStats.GetAttributes().GetAnnotations())
	}
	if adminCalls != 1 {
		t.Fatalf("admin calls after owner stats = %d, want 1", adminCalls)
	}
	record, _ := instance.getContainer(owner)
	if _, mutated := record.Annotations[statsScopeAnnotation]; mutated {
		t.Fatal("stats response mutated persisted container annotations")
	}

	nonOwner := first.GetContainerId()
	if nonOwner == owner {
		nonOwner = second.GetContainerId()
	}
	nonOwnerResponse, err := instance.ContainerStats(context.Background(), &runtime.ContainerStatsRequest{ContainerId: nonOwner})
	if err != nil {
		t.Fatalf("ContainerStats non-owner: %v", err)
	}
	if nonOwnerResponse.GetStats().GetMemory() != nil || nonOwnerResponse.GetStats().GetWritableLayer() != nil {
		t.Fatalf("non-owner received aggregate stats: %+v", nonOwnerResponse.GetStats())
	}
	if adminCalls != 1 {
		t.Fatalf("non-owner triggered another stats call: %d", adminCalls)
	}

	listResponse, err := instance.ListContainerStats(context.Background(), &runtime.ListContainerStatsRequest{})
	if err != nil {
		t.Fatalf("ListContainerStats: %v", err)
	}
	withMetrics := 0
	for _, item := range listResponse.GetStats() {
		if item.GetMemory() != nil || item.GetWritableLayer() != nil {
			withMetrics++
			if item.GetAttributes().GetId() != owner {
				t.Fatalf("metrics attributed to %s, owner is %s", item.GetAttributes().GetId(), owner)
			}
		}
	}
	if withMetrics != 1 {
		t.Fatalf("containers with aggregate metrics = %d, want 1", withMetrics)
	}
	if adminCalls != 2 {
		t.Fatalf("admin calls after list stats = %d, want 2", adminCalls)
	}
}
func TestContainerStatsValidationAndNonOwnerFilter(t *testing.T) {
	instance, _, _ := newReadyContainerTestEngine(t)
	first, err := instance.CreateContainer(context.Background(), criCreateContainerRequest("cri-1", "app", "agentenv.dev/app:v1", 0))
	if err != nil {
		t.Fatalf("CreateContainer first: %v", err)
	}
	second, err := instance.CreateContainer(context.Background(), criCreateContainerRequest("cri-1", "sidecar", "agentenv.dev/sidecar:v1", 0))
	if err != nil {
		t.Fatalf("CreateContainer second: %v", err)
	}
	for _, id := range []string{first.GetContainerId(), second.GetContainerId()} {
		if _, err := instance.StartContainer(context.Background(), &runtime.StartContainerRequest{ContainerId: id}); err != nil {
			t.Fatalf("StartContainer %s: %v", id, err)
		}
	}
	adminCalls := 0
	instance.admin = &fakeAdmin{getStats: func(context.Context, *runtimeapi.SandboxStatsRequest) (*runtimeapi.SandboxStatsResponse, error) {
		adminCalls++
		return &runtimeapi.SandboxStatsResponse{}, nil
	}}

	owner := instance.statsOwnerForPod("cri-1")
	nonOwner := first.GetContainerId()
	if nonOwner == owner {
		nonOwner = second.GetContainerId()
	}
	response, err := instance.ListContainerStats(context.Background(), &runtime.ListContainerStatsRequest{
		Filter: &runtime.ContainerStatsFilter{Id: nonOwner},
	})
	if err != nil {
		t.Fatalf("ListContainerStats non-owner filter: %v", err)
	}
	if len(response.GetStats()) != 1 || response.GetStats()[0].GetMemory() != nil {
		t.Fatalf("non-owner filter returned aggregate stats: %+v", response.GetStats())
	}
	if adminCalls != 0 {
		t.Fatalf("non-owner filter called Runtime: %d", adminCalls)
	}
	if _, err := instance.ContainerStats(context.Background(), &runtime.ContainerStatsRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty container id code = %v", status.Code(err))
	}
	if _, err := instance.ContainerStats(context.Background(), &runtime.ContainerStatsRequest{ContainerId: "missing"}); status.Code(err) != codes.NotFound {
		t.Fatalf("missing container code = %v", status.Code(err))
	}
}
