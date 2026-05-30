package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
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
	// 新增：补齐日志中缺失的 SandboxConfig 属性
	annExecutionID     = "e2b.dev/execution-id"
	annEnvdAccessToken = "e2b.dev/envd-access-token"
	annEnvVars         = "e2b.dev/env-vars"
	annNetwork         = "e2b.dev/network"
	annVolumeMounts    = "e2b.dev/volume-mounts"
	annAutoResume      = "e2b.dev/auto-resume"
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
	MaxSandboxLength: 24, // hours
	AllowInternet:    false,
}

type e2bImageMeta struct {
	templateID string
	buildID    string
	pulledAt   time.Time
}

type grpcE2BEngine struct {
	orchestratorAddr string
	nodeIP           string
	mu               sync.Mutex
	conn             *grpc.ClientConn
	client           orchestrator.SandboxServiceClient
	tracker          *podTracker
	imageCache       map[string]*e2bImageMeta
	imageMu          sync.RWMutex
}

func newGRPCE2BEngine(orchestratorAddr, nodeIP string) *grpcE2BEngine {
	log.Printf("[GrpcE2BEngine] orchestrator address: %s, nodeIP: %s", orchestratorAddr, nodeIP)
	return &grpcE2BEngine{
		orchestratorAddr: orchestratorAddr,
		nodeIP:           nodeIP,
		tracker:          newPodTracker(),
		imageCache:       make(map[string]*e2bImageMeta),
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

// ========== 错误映射 ==========

func mapE2BError(err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if ok {
		switch st.Code() {
		case codes.NotFound:
			return status.Error(codes.NotFound, st.Message())
		case codes.PermissionDenied:
			return status.Error(codes.PermissionDenied, st.Message())
		case codes.ResourceExhausted:
			return status.Error(codes.ResourceExhausted, st.Message())
		case codes.DeadlineExceeded:
			return status.Error(codes.DeadlineExceeded, st.Message())
		case codes.Unavailable:
			return status.Error(codes.Unavailable, st.Message())
		default:
			return status.Error(codes.Internal, fmt.Sprintf("e2b error: %v", err))
		}
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "not found") || strings.Contains(msg, "does not exist") {
		return status.Error(codes.NotFound, err.Error())
	}
	if strings.Contains(msg, "permission") || strings.Contains(msg, "denied") {
		return status.Error(codes.PermissionDenied, err.Error())
	}
	if strings.Contains(msg, "resource") || strings.Contains(msg, "exhausted") || strings.Contains(msg, "out of") {
		return status.Error(codes.ResourceExhausted, err.Error())
	}
	if strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline") {
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	if strings.Contains(msg, "connection") || strings.Contains(msg, "unavailable") || strings.Contains(msg, "refused") {
		return status.Error(codes.Unavailable, err.Error())
	}
	return status.Error(codes.Internal, fmt.Sprintf("e2b error: %v", err))
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	st, ok := status.FromError(err)
	if ok && st.Code() == codes.NotFound {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "does not exist")
}

// ========== 必填校验 ==========

func (e *grpcE2BEngine) validateAnnotations(anns map[string]string) (templateID, buildID, teamID string, err error) {
	templateID = anns[annTemplateID]
	buildID = anns[annBuildID]
	teamID = anns[annTeamID]
	if templateID == "" || buildID == "" || teamID == "" {
		return "", "", "", status.Error(codes.InvalidArgument,
			"missing required e2b annotations: e2b.dev/template-id, e2b.dev/build-id, e2b.dev/team-id")
	}
	return templateID, buildID, teamID, nil
}

// ========== PodSandbox 生命周期 ==========

func (e *grpcE2BEngine) RunPodSandbox(ctx context.Context, req *runtime.RunPodSandboxRequest) (*runtime.RunPodSandboxResponse, error) {
	log.Printf("[GrpcE2BEngine] RunPodSandbox: name=%s, handler=%s", req.Config.Metadata.Name, req.RuntimeHandler)
	if err := e.ensureConn(); err != nil {
		return nil, mapE2BError(err)
	}

	// 幂等：已存在且未删除则直接返回
	if existing, ok := e.tracker.Get(req.Config.Metadata.Uid); ok && existing.state != stateRemoved {
		log.Printf("[GrpcE2BEngine] RunPodSandbox: sandbox %s already exists, returning idempotently", req.Config.Metadata.Uid)
		return &runtime.RunPodSandboxResponse{PodSandboxId: req.Config.Metadata.Uid}, nil
	}

	// 必填校验
	templateID, buildID, teamID, err := e.validateAnnotations(req.Config.Annotations)
	if err != nil {
		return nil, err
	}

	sandboxID := req.Config.Metadata.Uid
	alias := req.Config.Metadata.Name
	now := time.Now()

	cfg := e.annotationsToSandboxConfig(req.Config.Annotations, sandboxID, alias, req.Config.Labels)
	cfg.TemplateId = templateID
	cfg.BuildId = buildID
	cfg.TeamId = teamID

	// 计算 end_time（默认 MaxSandboxLength 小时）
	maxLen := cfg.MaxSandboxLength
	if maxLen <= 0 {
		maxLen = 1
	}
	endTime := now.Add(time.Duration(maxLen) * time.Hour)

	e2bReq := &orchestrator.SandboxCreateRequest{
		Sandbox:   cfg,
		StartTime: timestamppb.New(now),
		EndTime:   timestamppb.New(endTime),
	}

	resp, err := e.client.Create(ctx, e2bReq)
	if err != nil {
		return nil, mapE2BError(err)
	}

	e.tracker.Add(sandboxID, &podInfo{
		sandboxID:   sandboxID,
		podUID:      req.Config.Metadata.Uid,
		name:        req.Config.Metadata.Name,
		namespace:   req.Config.Metadata.Namespace,
		labels:      req.Config.Labels,
		annotations: req.Config.Annotations,
		createdAt:   now,
		state:       stateRunning,
		templateID:  templateID,
		buildID:     buildID,
	})

	log.Printf("[GrpcE2BEngine] sandbox created: %s (client_id=%s)", sandboxID, resp.ClientId)
	return &runtime.RunPodSandboxResponse{PodSandboxId: sandboxID}, nil
}

func (e *grpcE2BEngine) StopPodSandbox(ctx context.Context, req *runtime.StopPodSandboxRequest) (*runtime.StopPodSandboxResponse, error) {
	log.Printf("[GrpcE2BEngine] StopPodSandbox: id=%s", req.PodSandboxId)
	if err := e.ensureConn(); err != nil {
		return nil, mapE2BError(err)
	}

	pod, ok := e.tracker.Get(req.PodSandboxId)
	if !ok {
		log.Printf("[GrpcE2BEngine] StopPodSandbox: sandbox %s not in tracker, treating as success", req.PodSandboxId)
		return &runtime.StopPodSandboxResponse{}, nil
	}

	_, err := e.client.Pause(ctx, &orchestrator.SandboxPauseRequest{
		SandboxId:  pod.sandboxID,
		TemplateId: pod.templateID,
		BuildId:    pod.buildID,
	})
	if err != nil && !isNotFound(err) {
		return nil, mapE2BError(err)
	}

	pod.state = statePaused
	now := time.Now()
	pod.endedAt = &now

	return &runtime.StopPodSandboxResponse{}, nil
}

func (e *grpcE2BEngine) RemovePodSandbox(ctx context.Context, req *runtime.RemovePodSandboxRequest) (*runtime.RemovePodSandboxResponse, error) {
	log.Printf("[GrpcE2BEngine] RemovePodSandbox: id=%s", req.PodSandboxId)
	if err := e.ensureConn(); err != nil {
		return nil, mapE2BError(err)
	}

	_, err := e.client.Delete(ctx, &orchestrator.SandboxDeleteRequest{
		SandboxId: req.PodSandboxId,
	})
	if err != nil && !isNotFound(err) {
		return nil, mapE2BError(err)
	}

	e.tracker.Delete(req.PodSandboxId)
	return &runtime.RemovePodSandboxResponse{}, nil
}

func (e *grpcE2BEngine) PodSandboxStatus(ctx context.Context, req *runtime.PodSandboxStatusRequest) (*runtime.PodSandboxStatusResponse, error) {
	log.Printf("[GrpcE2BEngine] PodSandboxStatus: id=%s", req.PodSandboxId)

	pod, ok := e.tracker.Get(req.PodSandboxId)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "sandbox %s not found", req.PodSandboxId)
	}

	state := inferPodSandboxState(pod.state)

	status := &runtime.PodSandboxStatus{
		Id:        pod.sandboxID,
		State:     state,
		CreatedAt: pod.createdAt.UnixNano(),
	}
	if pod.name != "" {
		status.Metadata = &runtime.PodSandboxMetadata{
			Name:      pod.name,
			Uid:       pod.podUID,
			Namespace: pod.namespace,
		}
	}
	status.Labels = pod.labels
	status.Annotations = pod.annotations

	return &runtime.PodSandboxStatusResponse{Status: status}, nil
}

func (e *grpcE2BEngine) ListPodSandbox(ctx context.Context, req *runtime.ListPodSandboxRequest) (*runtime.ListPodSandboxResponse, error) {
	log.Println("[GrpcE2BEngine] ListPodSandbox")
	if err := e.ensureConn(); err != nil {
		return nil, mapE2BError(err)
	}

	// 与 E2B 同步，确认哪些 sandbox 仍存活
	list, err := e.client.List(ctx, &emptypb.Empty{})
	if err != nil {
		return nil, mapE2BError(err)
	}

	active := make(map[string]bool)
	for _, sbx := range list.Sandboxes {
		active[sbx.Config.SandboxId] = true
	}

	var items []*runtime.PodSandbox
	for _, pod := range e.tracker.List() {
		state := inferPodSandboxState(pod.state)
		// 若 E2B 侧已不存在但本地仍为 Running，降级为 NOTREADY
		if !active[pod.sandboxID] && pod.state == stateRunning {
			state = runtime.PodSandboxState_SANDBOX_NOTREADY
		}

		items = append(items, &runtime.PodSandbox{
			Id: pod.sandboxID,
			Metadata: &runtime.PodSandboxMetadata{
				Name:      pod.name,
				Uid:       pod.podUID,
				Namespace: pod.namespace,
			},
			State:       state,
			CreatedAt:   pod.createdAt.UnixNano(),
			Labels:      pod.labels,
			Annotations: pod.annotations,
		})
	}

	items = filterPodSandbox(items, req.Filter)
	return &runtime.ListPodSandboxResponse{Items: items}, nil
}

// ========== Container 生命周期（1 Sandbox = 1 Container） ==========

func (e *grpcE2BEngine) CreateContainer(ctx context.Context, req *runtime.CreateContainerRequest) (*runtime.CreateContainerResponse, error) {
	log.Printf("[GrpcE2BEngine] CreateContainer: pod=%s, name=%s", req.PodSandboxId, req.Config.Metadata.Name)
	containerID := req.PodSandboxId + "-c"
	return &runtime.CreateContainerResponse{ContainerId: containerID}, nil
}

func (e *grpcE2BEngine) StartContainer(ctx context.Context, req *runtime.StartContainerRequest) (*runtime.StartContainerResponse, error) {
	log.Printf("[GrpcE2BEngine] StartContainer: id=%s (sandbox already running)", req.ContainerId)
	return &runtime.StartContainerResponse{}, nil
}

func (e *grpcE2BEngine) StopContainer(ctx context.Context, req *runtime.StopContainerRequest) (*runtime.StopContainerResponse, error) {
	log.Printf("[GrpcE2BEngine] StopContainer: id=%s", req.ContainerId)
	if err := e.ensureConn(); err != nil {
		return nil, mapE2BError(err)
	}

	sandboxID := stripContainerSuffix(req.ContainerId)
	pod, ok := e.tracker.Get(sandboxID)
	if !ok {
		return &runtime.StopContainerResponse{}, nil
	}

	_, err := e.client.Pause(ctx, &orchestrator.SandboxPauseRequest{
		SandboxId:  pod.sandboxID,
		TemplateId: pod.templateID,
		BuildId:    pod.buildID,
	})
	if err != nil && !isNotFound(err) {
		return nil, mapE2BError(err)
	}

	pod.state = statePaused
	return &runtime.StopContainerResponse{}, nil
}

func (e *grpcE2BEngine) RemoveContainer(ctx context.Context, req *runtime.RemoveContainerRequest) (*runtime.RemoveContainerResponse, error) {
	log.Printf("[GrpcE2BEngine] RemoveContainer: id=%s", req.ContainerId)
	if err := e.ensureConn(); err != nil {
		return nil, mapE2BError(err)
	}

	sandboxID := stripContainerSuffix(req.ContainerId)
	_, err := e.client.Delete(ctx, &orchestrator.SandboxDeleteRequest{
		SandboxId: sandboxID,
	})
	if err != nil && !isNotFound(err) {
		return nil, mapE2BError(err)
	}

	e.tracker.Delete(sandboxID)
	return &runtime.RemoveContainerResponse{}, nil
}

func (e *grpcE2BEngine) ListContainers(ctx context.Context, req *runtime.ListContainersRequest) (*runtime.ListContainersResponse, error) {
	log.Println("[GrpcE2BEngine] ListContainers")

	var items []*runtime.Container
	for _, pod := range e.tracker.List() {
		items = append(items, &runtime.Container{
			Id:           pod.sandboxID + "-c",
			PodSandboxId: pod.sandboxID,
			Metadata: &runtime.ContainerMetadata{ // ← 新增
				Name: pod.name,
			},
			State: inferContainerState(pod.state),
			Image: &runtime.ImageSpec{
				Image: fmt.Sprintf("e2b.dev/%s:%s", pod.templateID, pod.buildID),
			},
			CreatedAt:   pod.createdAt.UnixNano(),
			Labels:      pod.labels,
			Annotations: pod.annotations,
		})
	}

	items = filterContainers(items, req.Filter)
	return &runtime.ListContainersResponse{Containers: items}, nil
}

func (e *grpcE2BEngine) ContainerStatus(ctx context.Context, req *runtime.ContainerStatusRequest) (*runtime.ContainerStatusResponse, error) {
	log.Printf("[GrpcE2BEngine] ContainerStatus: id=%s", req.ContainerId)

	sandboxID := stripContainerSuffix(req.ContainerId)
	pod, ok := e.tracker.Get(sandboxID)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "container %s not found", req.ContainerId)
	}

	return &runtime.ContainerStatusResponse{
		Status: &runtime.ContainerStatus{
			Id: req.ContainerId,
			Metadata: &runtime.ContainerMetadata{ // ← 新增
				Name: pod.name,
			},
			State:     inferContainerState(pod.state),
			CreatedAt: pod.createdAt.UnixNano(),
			StartedAt: pod.createdAt.UnixNano(),
			Image: &runtime.ImageSpec{
				Image: fmt.Sprintf("e2b.dev/%s:%s", pod.templateID, pod.buildID),
			},
			Labels:      pod.labels,
			Annotations: pod.annotations,
			LogPath:     fmt.Sprintf("/var/log/e2b/%s.log", pod.sandboxID),
		},
	}, nil
}

// ========== ImageService ==========

func (e *grpcE2BEngine) PullImage(ctx context.Context, req *runtime.PullImageRequest) (*runtime.PullImageResponse, error) {
	log.Printf("[GrpcE2BEngine] PullImage: %s", req.Image.Image)
	if err := e.ensureConn(); err != nil {
		return nil, mapE2BError(err)
	}

	imageRef := req.Image.Image
	if !strings.HasPrefix(imageRef, "e2b.dev/") {
		return nil, status.Error(codes.InvalidArgument, "not an e2b image")
	}

	templateID, buildID, err := parseE2BImageRef(imageRef)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	// 验证 build_id 存在于缓存
	resp, err := e.client.ListCachedBuilds(ctx, &emptypb.Empty{})
	if err != nil {
		return nil, mapE2BError(err)
	}

	found := false
	for _, build := range resp.Builds {
		if build.BuildId == buildID {
			found = true
			break
		}
	}
	if !found {
		return nil, status.Errorf(codes.NotFound, "build %s not found in cached builds", buildID)
	}

	e.imageMu.Lock()
	e.imageCache[imageRef] = &e2bImageMeta{
		templateID: templateID,
		buildID:    buildID,
		pulledAt:   time.Now(),
	}
	e.imageMu.Unlock()

	return &runtime.PullImageResponse{ImageRef: imageRef}, nil
}

func (e *grpcE2BEngine) ListImages(ctx context.Context, req *runtime.ListImagesRequest) (*runtime.ListImagesResponse, error) {
	log.Println("[GrpcE2BEngine] ListImages")
	if err := e.ensureConn(); err != nil {
		return nil, mapE2BError(err)
	}

	resp, err := e.client.ListCachedBuilds(ctx, &emptypb.Empty{})
	if err != nil {
		return nil, mapE2BError(err)
	}

	var images []*runtime.Image
	for _, build := range resp.Builds {
		ref := fmt.Sprintf("e2b.dev/template:%s", build.BuildId)
		images = append(images, &runtime.Image{
			Id:       ref,
			RepoTags: []string{ref},
			Size_:    0,
		})
	}

	// 合并本地已 pull 的镜像（去重）
	e.imageMu.RLock()
	for ref, meta := range e.imageCache {
		_ = meta
		images = append(images, &runtime.Image{
			Id:       ref,
			RepoTags: []string{ref},
			Size_:    0,
		})
	}
	e.imageMu.RUnlock()

	return &runtime.ListImagesResponse{Images: images}, nil
}

func (e *grpcE2BEngine) ImageStatus(ctx context.Context, req *runtime.ImageStatusRequest) (*runtime.ImageStatusResponse, error) {
	log.Printf("[GrpcE2BEngine] ImageStatus: %s", req.Image.Image)

	imageRef := req.Image.Image
	if !strings.HasPrefix(imageRef, "e2b.dev/") {
		return &runtime.ImageStatusResponse{Image: nil}, nil
	}

	e.imageMu.RLock()
	_, ok := e.imageCache[imageRef]
	e.imageMu.RUnlock()
	if !ok {
		return &runtime.ImageStatusResponse{Image: nil}, nil
	}

	return &runtime.ImageStatusResponse{
		Image: &runtime.Image{
			Id:       imageRef,
			RepoTags: []string{imageRef},
			Size_:    0,
		},
	}, nil
}

func (e *grpcE2BEngine) RemoveImage(ctx context.Context, req *runtime.RemoveImageRequest) (*runtime.RemoveImageResponse, error) {
	log.Printf("[GrpcE2BEngine] RemoveImage: %s", req.Image.Image)

	imageRef := req.Image.Image
	if !strings.HasPrefix(imageRef, "e2b.dev/") {
		return &runtime.RemoveImageResponse{}, nil
	}

	e.imageMu.Lock()
	delete(e.imageCache, imageRef)
	e.imageMu.Unlock()

	return &runtime.RemoveImageResponse{}, nil
}

func (e *grpcE2BEngine) ImageFsInfo(ctx context.Context, req *runtime.ImageFsInfoRequest) (*runtime.ImageFsInfoResponse, error) {
	log.Println("[GrpcE2BEngine] ImageFsInfo")
	return &runtime.ImageFsInfoResponse{}, nil
}

// ========== 降级 / 未实现接口 ==========

func (e *grpcE2BEngine) ExecSync(ctx context.Context, req *runtime.ExecSyncRequest) (*runtime.ExecSyncResponse, error) {
	log.Printf("[GrpcE2BEngine] ExecSync: id=%s (unimplemented)", req.ContainerId)
	return nil, status.Error(codes.Unimplemented, "ExecSync not supported in MVP, use httpGet probe")
}

func (e *grpcE2BEngine) Exec(ctx context.Context, req *runtime.ExecRequest) (*runtime.ExecResponse, error) {
	log.Printf("[GrpcE2BEngine] Exec: id=%s (unimplemented)", req.ContainerId)
	return nil, status.Error(codes.Unimplemented, "Exec not supported in MVP")
}

func (e *grpcE2BEngine) Attach(ctx context.Context, req *runtime.AttachRequest) (*runtime.AttachResponse, error) {
	log.Printf("[GrpcE2BEngine] Attach: id=%s (unimplemented)", req.ContainerId)
	return nil, status.Error(codes.Unimplemented, "Attach not supported in MVP")
}

func (e *grpcE2BEngine) PortForward(ctx context.Context, req *runtime.PortForwardRequest) (*runtime.PortForwardResponse, error) {
	log.Printf("[GrpcE2BEngine] PortForward: id=%s (unimplemented)", req.PodSandboxId)
	return nil, status.Error(codes.Unimplemented, "PortForward not supported in MVP")
}

func (e *grpcE2BEngine) UpdateContainerResources(ctx context.Context, req *runtime.UpdateContainerResourcesRequest) (*runtime.UpdateContainerResourcesResponse, error) {
	log.Printf("[GrpcE2BEngine] UpdateContainerResources: id=%s (unimplemented)", req.ContainerId)
	return nil, status.Error(codes.Unimplemented, "UpdateContainerResources not supported for e2b")
}

func (e *grpcE2BEngine) ContainerStats(ctx context.Context, req *runtime.ContainerStatsRequest) (*runtime.ContainerStatsResponse, error) {
	log.Printf("[GrpcE2BEngine] ContainerStats: id=%s (stub)", req.ContainerId)
	return &runtime.ContainerStatsResponse{Stats: &runtime.ContainerStats{}}, nil
}

func (e *grpcE2BEngine) ListContainerStats(ctx context.Context, req *runtime.ListContainerStatsRequest) (*runtime.ListContainerStatsResponse, error) {
	log.Println("[GrpcE2BEngine] ListContainerStats (stub)")
	return &runtime.ListContainerStatsResponse{Stats: []*runtime.ContainerStats{}}, nil
}

func (e *grpcE2BEngine) CheckpointContainer(ctx context.Context, req *runtime.CheckpointContainerRequest) (*runtime.CheckpointContainerResponse, error) {
	log.Printf("[GrpcE2BEngine] CheckpointContainer: id=%s (unimplemented)", req.ContainerId)
	return nil, status.Error(codes.Unimplemented, "CheckpointContainer not supported for e2b")
}

func (e *grpcE2BEngine) ReopenContainerLog(ctx context.Context, req *runtime.ReopenContainerLogRequest) (*runtime.ReopenContainerLogResponse, error) {
	log.Printf("[GrpcE2BEngine] ReopenContainerLog: id=%s (stub)", req.ContainerId)
	return &runtime.ReopenContainerLogResponse{}, nil
}

func (e *grpcE2BEngine) GetContainerEvents(ctx context.Context, req *runtime.GetEventsRequest, opts ...grpc.CallOption) (runtime.RuntimeService_GetContainerEventsClient, error) {
	log.Println("[GrpcE2BEngine] GetContainerEvents (not implemented)")
	return nil, status.Error(codes.Unimplemented, "GetContainerEvents not supported by e2b engine")
}

// ========== 基础接口 ==========

func (e *grpcE2BEngine) Status(ctx context.Context, req *runtime.StatusRequest) (*runtime.StatusResponse, error) {
	log.Println("[GrpcE2BEngine] Status")
	return &runtime.StatusResponse{
		Status: &runtime.RuntimeStatus{
			Conditions: []*runtime.RuntimeCondition{
				{Type: runtime.RuntimeReady, Status: true},
				{Type: runtime.NetworkReady, Status: true},
			},
		},
	}, nil
}

func (e *grpcE2BEngine) Version(ctx context.Context, req *runtime.VersionRequest) (*runtime.VersionResponse, error) {
	log.Println("[GrpcE2BEngine] Version")
	return &runtime.VersionResponse{
		Version:           "0.1.0",
		RuntimeName:       "e2b-cri-shim",
		RuntimeVersion:    "0.1.0",
		RuntimeApiVersion: "v1",
	}, nil
}

func (e *grpcE2BEngine) UpdateRuntimeConfig(ctx context.Context, req *runtime.UpdateRuntimeConfigRequest) (*runtime.UpdateRuntimeConfigResponse, error) {
	log.Println("[GrpcE2BEngine] UpdateRuntimeConfig")
	return &runtime.UpdateRuntimeConfigResponse{}, nil
}

// ========== 辅助函数 ==========

func (e *grpcE2BEngine) annotationsToSandboxConfig(annotations map[string]string, sandboxID, alias string, labels map[string]string) *orchestrator.SandboxConfig {
	cfg := &orchestrator.SandboxConfig{
		SandboxId:          sandboxID,
		Alias:              &alias,
		Metadata:           labels,
		TemplateId:         defaultSandboxConfig.TemplateID,
		BuildId:            defaultSandboxConfig.BuildID,
		TeamId:             defaultSandboxConfig.TeamID,
		Vcpu:               defaultSandboxConfig.VCPU,
		RamMb:              defaultSandboxConfig.RAMMB,
		EnvdVersion:        defaultSandboxConfig.EnvdVersion,
		MaxSandboxLength:   defaultSandboxConfig.MaxSandboxLength,
		KernelVersion:      "vmlinux-6.1.158",
		FirecrackerVersion: "v1.13.1",
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
	setOptionalStr := func(key string, target **string) {
		if v, ok := annotations[key]; ok && v != "" {
			*target = &v
		}
	}

	// 基础字段
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

	setStr(annExecutionID, &cfg.ExecutionId)
	setOptionalStr(annEnvdAccessToken, &cfg.EnvdAccessToken)

	// EnvVars: JSON map 解析
	if v, ok := annotations[annEnvVars]; ok && v != "" && v != "{}" {
		var envVars map[string]string
		if err := json.Unmarshal([]byte(v), &envVars); err == nil {
			cfg.EnvVars = envVars
		}
	}

	// Network: JSON 解析为 SandboxNetworkConfig
	if v, ok := annotations[annNetwork]; ok && v != "" && v != "<nil>" {
		var network orchestrator.SandboxNetworkConfig
		if err := json.Unmarshal([]byte(v), &network); err == nil {
			cfg.Network = &network
		}
	}

	// VolumeMounts: JSON 数组解析
	if v, ok := annotations[annVolumeMounts]; ok && v != "" && v != "[]" && v != "<nil>" {
		var mounts []*orchestrator.SandboxVolumeMount
		if err := json.Unmarshal([]byte(v), &mounts); err == nil {
			cfg.VolumeMounts = mounts
		}
	}

	// AutoResume: JSON 解析
	if v, ok := annotations[annAutoResume]; ok && v != "" && v != "<nil>" {
		var autoResume orchestrator.SandboxAutoResumeConfig
		if err := json.Unmarshal([]byte(v), &autoResume); err == nil {
			cfg.AutoResume = &autoResume
		}
	}

	return cfg
}

func strDeref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func stripContainerSuffix(containerID string) string {
	if len(containerID) > 2 && containerID[len(containerID)-2:] == "-c" {
		return containerID[:len(containerID)-2]
	}
	return containerID
}

func inferPodSandboxState(s e2bState) runtime.PodSandboxState {
	switch s {
	case stateRunning:
		return runtime.PodSandboxState_SANDBOX_READY
	default:
		return runtime.PodSandboxState_SANDBOX_NOTREADY
	}
}

func inferContainerState(s e2bState) runtime.ContainerState {
	switch s {
	case stateRunning:
		return runtime.ContainerState_CONTAINER_RUNNING
	default:
		return runtime.ContainerState_CONTAINER_EXITED
	}
}

func parseE2BImageRef(imageRef string) (templateID, buildID string, err error) {
	if !strings.HasPrefix(imageRef, "e2b.dev/") {
		return "", "", fmt.Errorf("not an e2b image")
	}
	parts := strings.SplitN(strings.TrimPrefix(imageRef, "e2b.dev/"), ":", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid e2b image format, expected e2b.dev/{template_id}:{build_id}")
	}
	return parts[0], parts[1], nil
}

func filterPodSandbox(items []*runtime.PodSandbox, f *runtime.PodSandboxFilter) []*runtime.PodSandbox {
	if f == nil {
		return items
	}
	var out []*runtime.PodSandbox
	for _, item := range items {
		if f.Id != "" && item.Id != f.Id {
			continue
		}
		if f.State != nil && item.State != f.State.State {
			continue
		}
		if f.LabelSelector != nil {
			match := true
			for k, v := range f.LabelSelector {
				if item.Labels[k] != v {
					match = false
					break
				}
			}
			if !match {
				continue
			}
		}
		out = append(out, item)
	}
	return out
}

func filterContainers(items []*runtime.Container, f *runtime.ContainerFilter) []*runtime.Container {
	if f == nil {
		return items
	}
	var out []*runtime.Container
	for _, item := range items {
		if f.Id != "" && item.Id != f.Id {
			continue
		}
		if f.PodSandboxId != "" && item.PodSandboxId != f.PodSandboxId {
			continue
		}
		if f.State != nil && item.State != f.State.State {
			continue
		}
		if f.LabelSelector != nil {
			match := true
			for k, v := range f.LabelSelector {
				if item.Labels[k] != v {
					match = false
					break
				}
			}
			if !match {
				continue
			}
		}
		out = append(out, item)
	}
	return out
}

func (e *grpcE2BEngine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.conn != nil {
		return e.conn.Close()
	}
	return nil
}

