package engine

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cri-multiplex/pkg/orchestrator"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

const (
	annBuildID            = "e2b.dev/build-id"
	annTeamID             = "e2b.dev/team-id"
	annVCPU               = "e2b.dev/vcpu"
	annRAMMB              = "e2b.dev/ram-mb"
	annEnvdVersion        = "e2b.dev/envd-version"
	annMaxSandboxLength   = "e2b.dev/max-sandbox-length"
	annAllowInternet      = "e2b.dev/allow-internet"
	annKernelVersion      = "e2b.dev/kernel-version"
	annFirecrackerVersion = "e2b.dev/firecracker-version"
	annTotalDiskSizeMB    = "e2b.dev/total-disk-size-mb"
	annHugePages          = "e2b.dev/huge-pages"
	annAutoPause          = "e2b.dev/auto-pause"
	annSnapshot           = "e2b.dev/snapshot"
	annBaseTemplateID     = "e2b.dev/base-template-id"
)

var defaultSandboxConfig = struct {
	TemplateID       string
	BuildID          string
	TeamID           string
	VCPU             int64
	RAMMB            int64
	EnvdVersion      string
	MaxSandboxLength int64
	AllowInternet    bool
}{
	TemplateID:       "default",
	BuildID:          "latest",
	TeamID:           "cri-multiplex",
	VCPU:             1,
	RAMMB:            2048,
	EnvdVersion:      "latest",
	MaxSandboxLength: 24,
	AllowInternet:    false,
}

type grpcE2BEngine struct {
	orchestratorAddr string
	mu               sync.Mutex
	conn             *grpc.ClientConn
	client           orchestrator.SandboxServiceClient
	tracker          *podTracker
}

func newGRPCE2BEngine(orchestratorAddr string) *grpcE2BEngine {
	log.Printf("[GrpcE2BEngine] orchestrator address: %s", orchestratorAddr)
	return &grpcE2BEngine{
		orchestratorAddr: orchestratorAddr,
		tracker:          newPodTracker(),
	}
}

func (e *grpcE2BEngine) ensureConn() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.conn != nil {
		return nil
	}
	conn, err := grpc.Dial(
		e.orchestratorAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16*1024*1024)),
	)
	if err != nil {
		return fmt.Errorf("orchestrator dial failed: %w", err)
	}
	e.conn = conn
	e.client = orchestrator.NewSandboxServiceClient(conn)
	log.Printf("[GrpcE2BEngine] connected to orchestrator at %s", e.orchestratorAddr)
	return nil
}

func (e *grpcE2BEngine) Type() EngineType { return EngineTypeE2B }

func (e *grpcE2BEngine) RunPodSandbox(ctx context.Context, req *runtime.RunPodSandboxRequest) (*runtime.RunPodSandboxResponse, error) {
	log.Printf("[GrpcE2BEngine] RunPodSandbox: name=%s, handler=%s", req.Config.Metadata.Name, req.RuntimeHandler)
	if err := e.ensureConn(); err != nil {
		return nil, err
	}

	sandboxID := fmt.Sprintf("e2b-%s", req.Config.Metadata.Uid)
	alias := req.Config.Metadata.Name

	cfg := e.annotationsToSandboxConfig(req.Config.Annotations, sandboxID, alias, req.Config.Labels)

	_, err := e.client.Create(ctx, &orchestrator.SandboxCreateRequest{
		Sandbox:   cfg,
		StartTime: timestamppb.Now(),
	})
	if err != nil {
		return nil, fmt.Errorf("orchestrator Create: %w", err)
	}

	pod := &podInfo{
		sandboxID:   sandboxID,
		podUID:      req.Config.Metadata.Uid,
		name:        req.Config.Metadata.Name,
		namespace:   req.Config.Metadata.Namespace,
		labels:      req.Config.Labels,
		annotations: req.Config.Annotations,
		createdAt:   time.Now(),
	}
	e.tracker.Add(sandboxID, pod)

	log.Printf("[GrpcE2BEngine] sandbox created: %s (alias=%s)", sandboxID, alias)
	return &runtime.RunPodSandboxResponse{PodSandboxId: sandboxID}, nil
}

func (e *grpcE2BEngine) StopPodSandbox(ctx context.Context, req *runtime.StopPodSandboxRequest) (*runtime.StopPodSandboxResponse, error) {
	log.Printf("[GrpcE2BEngine] StopPodSandbox: id=%s", req.PodSandboxId)
	if err := e.ensureConn(); err != nil {
		return nil, err
	}

	now := timestamppb.Now()
	_, err := e.client.Update(ctx, &orchestrator.SandboxUpdateRequest{
		SandboxId: req.PodSandboxId,
		EndTime:   now,
	})
	if err != nil {
		return nil, fmt.Errorf("orchestrator Update: %w", err)
	}

	if p, ok := e.tracker.Get(req.PodSandboxId); ok {
		t := now.AsTime()
		p.endedAt = &t
	}

	return &runtime.StopPodSandboxResponse{}, nil
}

func (e *grpcE2BEngine) RemovePodSandbox(ctx context.Context, req *runtime.RemovePodSandboxRequest) (*runtime.RemovePodSandboxResponse, error) {
	log.Printf("[GrpcE2BEngine] RemovePodSandbox: id=%s", req.PodSandboxId)
	if err := e.ensureConn(); err != nil {
		return nil, err
	}

	_, err := e.client.Delete(ctx, &orchestrator.SandboxDeleteRequest{
		SandboxId: req.PodSandboxId,
	})
	if err != nil {
		return nil, fmt.Errorf("orchestrator Delete: %w", err)
	}

	e.tracker.Delete(req.PodSandboxId)

	return &runtime.RemovePodSandboxResponse{}, nil
}

func (e *grpcE2BEngine) PodSandboxStatus(ctx context.Context, req *runtime.PodSandboxStatusRequest) (*runtime.PodSandboxStatusResponse, error) {
	log.Printf("[GrpcE2BEngine] PodSandboxStatus: id=%s", req.PodSandboxId)
	if err := e.ensureConn(); err != nil {
		return nil, err
	}

	state := runtime.PodSandboxState_SANDBOX_NOTREADY
	list, err := e.client.List(ctx, &emptypb.Empty{})
	if err != nil {
		return nil, fmt.Errorf("orchestrator List: %w", err)
	}
	for _, sbx := range list.Sandboxes {
		if sbx.Config.SandboxId == req.PodSandboxId {
			state = runtime.PodSandboxState_SANDBOX_READY
			if sbx.EndTime != nil && sbx.EndTime.IsValid() && sbx.EndTime.AsTime().Before(time.Now()) {
				state = runtime.PodSandboxState_SANDBOX_NOTREADY
			}
			break
		}
	}

	status := &runtime.PodSandboxStatus{
		Id:    req.PodSandboxId,
		State: state,
	}
	if p, ok := e.tracker.Get(req.PodSandboxId); ok {
		status.Metadata = &runtime.PodSandboxMetadata{
			Name:      p.name,
			Uid:       p.podUID,
			Namespace: p.namespace,
		}
		status.Labels = p.labels
		status.Annotations = p.annotations
		status.CreatedAt = p.createdAt.UnixNano()
	}
	return &runtime.PodSandboxStatusResponse{Status: status}, nil
}

func (e *grpcE2BEngine) ListPodSandbox(ctx context.Context, req *runtime.ListPodSandboxRequest) (*runtime.ListPodSandboxResponse, error) {
	log.Println("[GrpcE2BEngine] ListPodSandbox")
	if err := e.ensureConn(); err != nil {
		return nil, err
	}

	list, err := e.client.List(ctx, &emptypb.Empty{})
	if err != nil {
		return nil, fmt.Errorf("orchestrator List: %w", err)
	}

	items := make([]*runtime.PodSandbox, 0, len(list.Sandboxes))
	for _, sbx := range list.Sandboxes {
		items = append(items, &runtime.PodSandbox{
			Id: sbx.Config.SandboxId,
			Metadata: &runtime.PodSandboxMetadata{
				Name: strDeref(sbx.Config.Alias),
			},
			State: runtime.PodSandboxState_SANDBOX_READY,
		})
	}
	return &runtime.ListPodSandboxResponse{Items: items}, nil
}

func (e *grpcE2BEngine) CreateContainer(ctx context.Context, req *runtime.CreateContainerRequest) (*runtime.CreateContainerResponse, error) {
	log.Printf("[GrpcE2BEngine] CreateContainer: pod=%s, name=%s (stub)", req.PodSandboxId, req.Config.Metadata.Name)
	containerID := fmt.Sprintf("e2b-ctr-%s-%s", req.PodSandboxId, req.Config.Metadata.Name)
	return &runtime.CreateContainerResponse{ContainerId: containerID}, nil
}

func (e *grpcE2BEngine) StartContainer(ctx context.Context, req *runtime.StartContainerRequest) (*runtime.StartContainerResponse, error) {
	log.Printf("[GrpcE2BEngine] StartContainer: id=%s (stub — TODO envd Process.Start)", req.ContainerId)
	return &runtime.StartContainerResponse{}, nil
}

func (e *grpcE2BEngine) StopContainer(ctx context.Context, req *runtime.StopContainerRequest) (*runtime.StopContainerResponse, error) {
	log.Printf("[GrpcE2BEngine] StopContainer: id=%s (stub — TODO envd SendSignal)", req.ContainerId)
	return &runtime.StopContainerResponse{}, nil
}

func (e *grpcE2BEngine) RemoveContainer(ctx context.Context, req *runtime.RemoveContainerRequest) (*runtime.RemoveContainerResponse, error) {
	log.Printf("[GrpcE2BEngine] RemoveContainer: id=%s (stub)", req.ContainerId)
	return &runtime.RemoveContainerResponse{}, nil
}

func (e *grpcE2BEngine) ListContainers(ctx context.Context, req *runtime.ListContainersRequest) (*runtime.ListContainersResponse, error) {
	log.Println("[GrpcE2BEngine] ListContainers (stub)")
	return &runtime.ListContainersResponse{Containers: []*runtime.Container{}}, nil
}

func (e *grpcE2BEngine) ContainerStatus(ctx context.Context, req *runtime.ContainerStatusRequest) (*runtime.ContainerStatusResponse, error) {
	log.Printf("[GrpcE2BEngine] ContainerStatus: id=%s (stub)", req.ContainerId)
	return &runtime.ContainerStatusResponse{
		Status: &runtime.ContainerStatus{
			Id:    req.ContainerId,
			State: runtime.ContainerState_CONTAINER_RUNNING,
		},
	}, nil
}

func (e *grpcE2BEngine) ContainerStats(ctx context.Context, req *runtime.ContainerStatsRequest) (*runtime.ContainerStatsResponse, error) {
	log.Printf("[GrpcE2BEngine] ContainerStats: id=%s (stub)", req.ContainerId)
	return &runtime.ContainerStatsResponse{Stats: &runtime.ContainerStats{}}, nil
}

func (e *grpcE2BEngine) ListContainerStats(ctx context.Context, req *runtime.ListContainerStatsRequest) (*runtime.ListContainerStatsResponse, error) {
	log.Println("[GrpcE2BEngine] ListContainerStats (stub)")
	return &runtime.ListContainerStatsResponse{Stats: []*runtime.ContainerStats{}}, nil
}

func (e *grpcE2BEngine) UpdateContainerResources(ctx context.Context, req *runtime.UpdateContainerResourcesRequest) (*runtime.UpdateContainerResourcesResponse, error) {
	log.Printf("[GrpcE2BEngine] UpdateContainerResources: id=%s (stub)", req.ContainerId)
	return &runtime.UpdateContainerResourcesResponse{}, nil
}

func (e *grpcE2BEngine) ReopenContainerLog(ctx context.Context, req *runtime.ReopenContainerLogRequest) (*runtime.ReopenContainerLogResponse, error) {
	log.Printf("[GrpcE2BEngine] ReopenContainerLog: id=%s (stub)", req.ContainerId)
	return &runtime.ReopenContainerLogResponse{}, nil
}

func (e *grpcE2BEngine) ExecSync(ctx context.Context, req *runtime.ExecSyncRequest) (*runtime.ExecSyncResponse, error) {
	log.Printf("[GrpcE2BEngine] ExecSync: id=%s, cmd=%v (stub — TODO envd)", req.ContainerId, req.Cmd)
	return &runtime.ExecSyncResponse{Stdout: []byte(""), ExitCode: 0}, nil
}

func (e *grpcE2BEngine) Exec(ctx context.Context, req *runtime.ExecRequest) (*runtime.ExecResponse, error) {
	log.Printf("[GrpcE2BEngine] Exec: id=%s (stub — TODO envd)", req.ContainerId)
	return &runtime.ExecResponse{Url: ""}, nil
}

func (e *grpcE2BEngine) Attach(ctx context.Context, req *runtime.AttachRequest) (*runtime.AttachResponse, error) {
	log.Printf("[GrpcE2BEngine] Attach: id=%s (stub — TODO envd)", req.ContainerId)
	return &runtime.AttachResponse{Url: ""}, nil
}

func (e *grpcE2BEngine) PortForward(ctx context.Context, req *runtime.PortForwardRequest) (*runtime.PortForwardResponse, error) {
	log.Printf("[GrpcE2BEngine] PortForward: id=%s, ports=%v (stub)", req.PodSandboxId, req.Port)
	return &runtime.PortForwardResponse{Url: ""}, nil
}

func (e *grpcE2BEngine) Status(ctx context.Context, req *runtime.StatusRequest) (*runtime.StatusResponse, error) {
	log.Println("[GrpcE2BEngine] Status")
	return &runtime.StatusResponse{
		Status: &runtime.RuntimeStatus{
			Conditions: []*runtime.RuntimeCondition{{Status: true}},
		},
	}, nil
}

func (e *grpcE2BEngine) Version(ctx context.Context, req *runtime.VersionRequest) (*runtime.VersionResponse, error) {
	log.Println("[GrpcE2BEngine] Version")
	return &runtime.VersionResponse{
		Version:           "0.1.0",
		RuntimeName:       "e2b",
		RuntimeVersion:    "0.1.0",
		RuntimeApiVersion: "v1",
	}, nil
}

func (e *grpcE2BEngine) UpdateRuntimeConfig(ctx context.Context, req *runtime.UpdateRuntimeConfigRequest) (*runtime.UpdateRuntimeConfigResponse, error) {
	log.Println("[GrpcE2BEngine] UpdateRuntimeConfig")
	return &runtime.UpdateRuntimeConfigResponse{}, nil
}

func (e *grpcE2BEngine) GetContainerEvents(ctx context.Context, req *runtime.GetEventsRequest, opts ...grpc.CallOption) (runtime.RuntimeService_GetContainerEventsClient, error) {
	log.Println("[GrpcE2BEngine] GetContainerEvents (not implemented)")
	return nil, fmt.Errorf("GetContainerEvents not implemented")
}

func (e *grpcE2BEngine) ListImages(ctx context.Context, req *runtime.ListImagesRequest) (*runtime.ListImagesResponse, error) {
	log.Println("[GrpcE2BEngine] ListImages")
	return &runtime.ListImagesResponse{Images: []*runtime.Image{}}, nil
}

func (e *grpcE2BEngine) ImageStatus(ctx context.Context, req *runtime.ImageStatusRequest) (*runtime.ImageStatusResponse, error) {
	log.Println("[GrpcE2BEngine] ImageStatus")
	return &runtime.ImageStatusResponse{}, nil
}

func (e *grpcE2BEngine) PullImage(ctx context.Context, req *runtime.PullImageRequest) (*runtime.PullImageResponse, error) {
	log.Println("[GrpcE2BEngine] PullImage")
	return &runtime.PullImageResponse{}, nil
}

func (e *grpcE2BEngine) RemoveImage(ctx context.Context, req *runtime.RemoveImageRequest) (*runtime.RemoveImageResponse, error) {
	log.Println("[GrpcE2BEngine] RemoveImage")
	return &runtime.RemoveImageResponse{}, nil
}

func (e *grpcE2BEngine) ImageFsInfo(ctx context.Context, req *runtime.ImageFsInfoRequest) (*runtime.ImageFsInfoResponse, error) {
	log.Println("[GrpcE2BEngine] ImageFsInfo")
	return &runtime.ImageFsInfoResponse{}, nil
}

func (e *grpcE2BEngine) annotationsToSandboxConfig(annotations map[string]string, sandboxID, alias string, labels map[string]string) *orchestrator.SandboxConfig {
	cfg := &orchestrator.SandboxConfig{
		SandboxId:        sandboxID,
		Alias:            &alias,
		Metadata:         labels,
		TemplateId:       defaultSandboxConfig.TemplateID,
		BuildId:          defaultSandboxConfig.BuildID,
		TeamId:           defaultSandboxConfig.TeamID,
		Vcpu:             defaultSandboxConfig.VCPU,
		RamMb:            defaultSandboxConfig.RAMMB,
		EnvdVersion:      defaultSandboxConfig.EnvdVersion,
		MaxSandboxLength: defaultSandboxConfig.MaxSandboxLength,
	}
	if defaultSandboxConfig.AllowInternet {
		v := true
		cfg.AllowInternetAccess = &v
	}
	if annotations == nil {
		return cfg
	}

	setStr := func(key string, target *string) {
		if v, ok := annotations[key]; ok {
			*target = v
		}
	}
	setInt := func(key string, target *int64) {
		if v, ok := annotations[key]; ok {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				*target = n
			}
		}
	}
	setBool := func(key string, target *bool) {
		if v, ok := annotations[key]; ok {
			*target = v == "true" || v == "1"
		}
	}
	setOptionalBool := func(key string, target **bool) {
		if v, ok := annotations[key]; ok {
			b := v == "true" || v == "1"
			*target = &b
		}
	}

	setStr(annTemplateID, &cfg.TemplateId)
	setStr(annBuildID, &cfg.BuildId)
	setStr(annTeamID, &cfg.TeamId)
	setInt(annVCPU, &cfg.Vcpu)
	setInt(annRAMMB, &cfg.RamMb)
	setStr(annEnvdVersion, &cfg.EnvdVersion)
	setInt(annMaxSandboxLength, &cfg.MaxSandboxLength)
	setOptionalBool(annAllowInternet, &cfg.AllowInternetAccess)
	setStr(annKernelVersion, &cfg.KernelVersion)
	setStr(annFirecrackerVersion, &cfg.FirecrackerVersion)
	setInt(annTotalDiskSizeMB, &cfg.TotalDiskSizeMb)
	setBool(annHugePages, &cfg.HugePages)
	setBool(annAutoPause, &cfg.AutoPause)
	setBool(annSnapshot, &cfg.Snapshot)
	setStr(annBaseTemplateID, &cfg.BaseTemplateId)

	return cfg
}

func strDeref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (e *grpcE2BEngine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.conn != nil {
		return e.conn.Close()
	}
	return nil
}
