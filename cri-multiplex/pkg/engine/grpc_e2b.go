package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cri-multiplex/pkg/envd/process"
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
	annExecutionID        = "e2b.dev/execution-id"
	annEnvdAccessToken    = "e2b.dev/envd-access-token"
	annEnvVars            = "e2b.dev/env-vars"
	annNetwork            = "e2b.dev/network"
	annVolumeMounts       = "e2b.dev/volume-mounts"
	annAutoResume         = "e2b.dev/auto-resume"
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

type e2bImageMeta struct {
	templateID string
	buildID    string
	pulledAt   time.Time
}

type execStreamRequest struct {
	sandboxID   string
	cmd         []string
	tty         bool
	stdin       bool
	stdout      bool
	stderr      bool
	accessToken string
}

type attachStreamRequest struct {
	sandboxID   string
	tty         bool
	stdin       bool
	stdout      bool
	stderr      bool
	accessToken string
}

type grpcE2BEngine struct {
	orchestratorAddr      string
	orchestratorProxyAddr string
	nodeIP                string
	mu                    sync.Mutex
	conn                  *grpc.ClientConn
	client                orchestrator.SandboxServiceClient
	tracker               *podTracker
	imageCache            map[string]*e2bImageMeta
	imageMu               sync.RWMutex
	envdHTTPClient        *http.Client

	streamingListener net.Listener
	streamingReqs     map[string]*execStreamRequest
	attachReqs        map[string]*attachStreamRequest
	streamingMu       sync.RWMutex
	streamingOnce     sync.Once

	hostPortManager *HostPortManager // 新增：宿主机端口管理
}

func newGRPCE2BEngine(orchestratorAddr, orchestratorProxyAddr, nodeIP string) *grpcE2BEngine {
	log.Printf("[GrpcE2BEngine] orchestrator address: %s, proxy: %s, nodeIP: %s", orchestratorAddr, orchestratorProxyAddr, nodeIP)
	return &grpcE2BEngine{
		orchestratorAddr:      orchestratorAddr,
		orchestratorProxyAddr: orchestratorProxyAddr,
		nodeIP:                nodeIP,
		tracker:               newPodTracker(),
		imageCache:            make(map[string]*e2bImageMeta),
		streamingReqs:         make(map[string]*execStreamRequest),
		attachReqs:            make(map[string]*attachStreamRequest),
		hostPortManager:       NewHostPortManager(20000, 29999), // 避开 NodePort 范围
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

func (e *grpcE2BEngine) RunPodSandbox(ctx context.Context, req *runtime.RunPodSandboxRequest) (*runtime.RunPodSandboxResponse, error) {
	log.Printf("[GrpcE2BEngine] RunPodSandbox: name=%s, handler=%s", req.Config.Metadata.Name, req.RuntimeHandler)
	if err := e.ensureConn(); err != nil {
		return nil, mapE2BError(err)
	}
	if existing, ok := e.tracker.Get(req.Config.Metadata.Uid); ok && existing.state != stateRemoved {
		if existing.state == stateStopped || existing.state == statePaused {
			existing.state = stateRunning
			existing.endedAt = nil
			log.Printf("[GrpcE2BEngine] RunPodSandbox: sandbox %s exists in stopped state, marking running without orchestrator checkpoint", req.Config.Metadata.Uid)
			return &runtime.RunPodSandboxResponse{PodSandboxId: req.Config.Metadata.Uid}, nil
		}
		log.Printf("[GrpcE2BEngine] RunPodSandbox: sandbox %s already exists, returning idempotently", req.Config.Metadata.Uid)
		return &runtime.RunPodSandboxResponse{PodSandboxId: req.Config.Metadata.Uid}, nil
	}
	templateID, buildID, teamID, err := e.validateAnnotations(req.Config.Annotations)
	if err != nil {
		return nil, err
	}
	sandboxID := req.Config.Metadata.Uid
	e2bSandboxID := e2bSandboxIDFromCRI(sandboxID)
	alias := req.Config.Metadata.Name
	now := time.Now()
	cfg := e.annotationsToSandboxConfig(req.Config.Annotations, e2bSandboxID, alias, req.Config.Labels)
	cfg.TemplateId = templateID
	cfg.BuildId = buildID
	cfg.TeamId = teamID
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
		log.Printf("[GrpcE2BEngine] RunPodSandbox: orchestrator.Create FAILED: %v", err)
		return nil, mapE2BError(err)
	}

	hostIP := resp.GetHostIp()
	if hostIP == "" {
		log.Printf("[GrpcE2BEngine] WARNING: orchestrator did not return host_ip for %s", sandboxID)
	}

	// ===== 多端口分配 =====
	var allMappings []PortMapping
	if hostIP != "" && e.hostPortManager != nil {
		// 1. 收集需要暴露的端口（仅来自 annotation）
		var ports []int
		if portsStr, ok := req.Config.Annotations[annExposePorts]; ok && portsStr != "" {
			for _, p := range strings.Split(portsStr, ",") {
				if port, err := strconv.Atoi(strings.TrimSpace(p)); err == nil && port > 0 && port < 65536 {
					ports = append(ports, port)
				}
			}
		}

		// 2. 分配所有端口
		if len(ports) > 0 {
			mappings, err := e.hostPortManager.AllocatePorts(sandboxID, ports)
			if err != nil {
				log.Printf("[GrpcE2BEngine] WARNING: failed to allocate host ports for %s: %v", sandboxID, err)
			} else {
				// 3. 为每个端口创建 iptables 规则
				for _, m := range mappings {
					if err := SetupHostPortMapping(e.nodeIP, m.HostPort, hostIP, m.SandboxPort); err != nil {
						log.Printf("[GrpcE2BEngine] WARNING: failed to setup mapping %d->%d for %s: %v", m.HostPort, m.SandboxPort, sandboxID, err)
					} else {
						log.Printf("[GrpcE2BEngine] HostPort mapping: %s:%d -> %s:%d", e.nodeIP, m.HostPort, hostIP, m.SandboxPort)
					}
				}
				allMappings = mappings
			}
		}
	}
	// ===== 多端口分配结束 =====

	// 提取默认端口映射（如果 49983 在声明中，会自然包含在 allMappings 里）
	defaultHostPort := 0
	for _, m := range allMappings {
		if m.SandboxPort == 49983 {
			defaultHostPort = m.HostPort
			break
		}
	}
	// ===== 多端口分配结束 =====

	envdToken := ""
	if t, ok := req.Config.Annotations[annEnvdAccessToken]; ok && t != "" {
		envdToken = t
	}

	e.tracker.Add(sandboxID, &podInfo{
		sandboxID:       sandboxID,
		e2bSandboxID:    e2bSandboxID,
		podUID:          req.Config.Metadata.Uid,
		name:            req.Config.Metadata.Name,
		namespace:       req.Config.Metadata.Namespace,
		labels:          req.Config.Labels,
		annotations:     req.Config.Annotations,
		createdAt:       now,
		state:           stateRunning,
		templateID:      templateID,
		buildID:         buildID,
		envdAccessToken: envdToken,

		hostIP:       hostIP,
		hostPort:     defaultHostPort,
		portMappings: allMappings,
	})

	log.Printf("[GrpcE2BEngine] sandbox created: cri_id=%s, e2b_id=%s (client_id=%s, host_ip=%s, default_port=%d, mappings=%v, envd_token_set=%v)",
		sandboxID, e2bSandboxID, resp.ClientId, hostIP, defaultHostPort, allMappings, envdToken != "")

	return &runtime.RunPodSandboxResponse{PodSandboxId: sandboxID}, nil
}

func (e *grpcE2BEngine) StopPodSandbox(ctx context.Context, req *runtime.StopPodSandboxRequest) (*runtime.StopPodSandboxResponse, error) {
	log.Printf("[GrpcE2BEngine] StopPodSandbox: id=%s", req.PodSandboxId)

	pod, ok := e.tracker.Get(req.PodSandboxId)
	if !ok {
		log.Printf("[GrpcE2BEngine] StopPodSandbox: sandbox %s not in tracker, treating as success", req.PodSandboxId)
		return &runtime.StopPodSandboxResponse{}, nil
	}

	// ===== 清理所有 HostPort 映射 =====
	if pod.hostIP != "" {
		for _, m := range pod.portMappings {
			if err := CleanupHostPortMapping(e.nodeIP, m.HostPort, pod.hostIP, m.SandboxPort); err != nil {
				log.Printf("[GrpcE2BEngine] WARNING: cleanup mapping %d->%d failed: %v", m.HostPort, m.SandboxPort, err)
			}
		}
		// 释放所有端口
		ports := make([]int, 0, len(pod.portMappings))
		for _, m := range pod.portMappings {
			ports = append(ports, m.SandboxPort)
		}
		e.hostPortManager.ReleasePorts(req.PodSandboxId, ports)
		pod.portMappings = nil
		pod.hostPort = 0
	}
	// ===== 清理结束 =====

	pod.state = stateStopped
	if pod.containerState == containerStateRunning || pod.containerState == containerStateCreated {
		pod.containerState = containerStateExited
		pod.containerExitCode = 0
		pod.containerFinishedAt = time.Now()
	}
	now := time.Now()
	pod.endedAt = &now
	return &runtime.StopPodSandboxResponse{}, nil
}

func (e *grpcE2BEngine) RemovePodSandbox(ctx context.Context, req *runtime.RemovePodSandboxRequest) (*runtime.RemovePodSandboxResponse, error) {
	log.Printf("[GrpcE2BEngine] RemovePodSandbox: id=%s", req.PodSandboxId)
	if err := e.ensureConn(); err != nil {
		return nil, mapE2BError(err)
	}

	// ===== 确保清理所有 HostPort 映射 =====
	if pod, ok := e.tracker.Get(req.PodSandboxId); ok {
		if pod.hostIP != "" {
			for _, m := range pod.portMappings {
				if err := CleanupHostPortMapping(e.nodeIP, m.HostPort, pod.hostIP, m.SandboxPort); err != nil {
					log.Printf("[GrpcE2BEngine] WARNING: cleanup mapping %d->%d failed: %v", m.HostPort, m.SandboxPort, err)
				}
			}
			ports := make([]int, 0, len(pod.portMappings))
			for _, m := range pod.portMappings {
				ports = append(ports, m.SandboxPort)
			}
			e.hostPortManager.ReleasePorts(req.PodSandboxId, ports)
			pod.portMappings = nil
			pod.hostPort = 0
		}
	}
	// ===== 清理结束 =====

	deleteID := req.PodSandboxId
	if pod, ok := e.tracker.Get(req.PodSandboxId); ok {
		deleteID = pod.envdSandboxID()
	}
	_, err := e.client.Delete(ctx, &orchestrator.SandboxDeleteRequest{SandboxId: deleteID})
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

	// 组装 annotations
	anns := make(map[string]string)
	for k, v := range pod.annotations {
		anns[k] = v
	}
	if pod.hostIP != "" {
		anns["e2b.dev/host-ip"] = pod.hostIP
		anns["e2b.dev/node-ip"] = e.nodeIP
	}
	if pod.hostPort > 0 {
		anns["e2b.dev/host-port"] = strconv.Itoa(pod.hostPort)
		anns["e2b.dev/access-url"] = fmt.Sprintf("http://%s:%d", e.nodeIP, pod.hostPort)
	}
	// 新增：返回所有端口映射
	for _, m := range pod.portMappings {
		key := fmt.Sprintf("e2b.dev/host-port-%d", m.SandboxPort)
		anns[key] = strconv.Itoa(m.HostPort)
		key2 := fmt.Sprintf("e2b.dev/access-url-%d", m.SandboxPort)
		anns[key2] = fmt.Sprintf("http://%s:%d", e.nodeIP, m.HostPort)
	}

	status := &runtime.PodSandboxStatus{
		Id:          pod.sandboxID,
		State:       state,
		CreatedAt:   pod.createdAt.UnixNano(),
		Labels:      pod.labels,
		Annotations: anns,
	}
	if pod.name != "" {
		status.Metadata = &runtime.PodSandboxMetadata{
			Name:      pod.name,
			Uid:       pod.podUID,
			Namespace: pod.namespace,
		}
	}

	if pod.hostIP != "" {
		status.Network = &runtime.PodSandboxNetworkStatus{
			Ip: pod.hostIP,
		}
	} else if e.nodeIP != "" {
		// hostIP 未获取到时回退到节点 IP，避免 kubelet 因缺少 network 信息
		// 报 "Pod Sandbox status doesn't have network information" 而卡在 ContainerCreating
		status.Network = &runtime.PodSandboxNetworkStatus{
			Ip: e.nodeIP,
		}
	}

	return &runtime.PodSandboxStatusResponse{Status: status}, nil
}

func (e *grpcE2BEngine) ListPodSandbox(ctx context.Context, req *runtime.ListPodSandboxRequest) (*runtime.ListPodSandboxResponse, error) {
	log.Println("[GrpcE2BEngine] ListPodSandbox")
	if err := e.ensureConn(); err != nil {
		return nil, mapE2BError(err)
	}
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
		if !active[pod.envdSandboxID()] && pod.state == stateRunning {
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

func (e *grpcE2BEngine) CreateContainer(ctx context.Context, req *runtime.CreateContainerRequest) (*runtime.CreateContainerResponse, error) {
	log.Printf("[GrpcE2BEngine] CreateContainer: pod=%s, name=%s, labels=%v, annotations=%v", req.PodSandboxId, req.Config.Metadata.Name, req.Config.Labels, req.Config.Annotations)
	containerID := req.PodSandboxId + "-c"
	if pod, ok := e.tracker.Get(req.PodSandboxId); ok {
		pod.imageRef = req.Config.Image.Image
		// kubelet 将 container.hash 和 restartCount 放在 annotations 中，
		// 但 PLEG 通过 ListContainers 的 labels 提取 hash 判断是否需要重建。
		// 需将 annotations 中的 hash/restartCount 复制到 labels，否则 kubelet 认为容器 hash 不匹配反复 kill。
		labels := make(map[string]string)
		for k, v := range req.Config.Labels {
			labels[k] = v
		}
		if hash, ok := req.Config.Annotations["io.kubernetes.container.hash"]; ok {
			labels["io.kubernetes.container.hash"] = hash
		}
		if rc, ok := req.Config.Annotations["io.kubernetes.container.restartCount"]; ok {
			labels["io.kubernetes.container.restartCount"] = rc
		}
		pod.containerLabels = labels
		pod.containerName = req.Config.Metadata.Name
		pod.containerAnnotations = req.Config.Annotations
		pod.containerCommand = append([]string(nil), req.Config.Command...)
		pod.containerArgs = append([]string(nil), req.Config.Args...)
		pod.containerStdin = req.Config.Stdin
		pod.containerTTY = req.Config.Tty
		pod.containerState = containerStateCreated
		pod.containerCreatedAt = time.Now()
		pod.containerStartedAt = time.Time{}
		pod.containerFinishedAt = time.Time{}
		pod.containerExitCode = 0
	}
	return &runtime.CreateContainerResponse{ContainerId: containerID}, nil
}

func (e *grpcE2BEngine) StartContainer(ctx context.Context, req *runtime.StartContainerRequest) (*runtime.StartContainerResponse, error) {
	log.Printf("[GrpcE2BEngine] StartContainer: id=%s", req.ContainerId)
	sandboxID := stripContainerSuffix(req.ContainerId)
	pod, ok := e.tracker.Get(sandboxID)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "sandbox %s not found", sandboxID)
	}
	if pod.state != stateRunning {
		return nil, status.Errorf(codes.FailedPrecondition, "sandbox %s is not running", sandboxID)
	}
	if pod.containerState == containerStateRunning {
		log.Printf("[GrpcE2BEngine] StartContainer: logical container already running for sandbox=%s", sandboxID)
		return &runtime.StartContainerResponse{}, nil
	}
	pod.containerState = containerStateRunning
	pod.containerStartedAt = time.Now()
	pod.containerFinishedAt = time.Time{}
	pod.containerExitCode = 0
	log.Printf("[GrpcE2BEngine] StartContainer: logical no-op, sandbox envd already ready: sandbox=%s", sandboxID)
	return &runtime.StartContainerResponse{}, nil
}

func (e *grpcE2BEngine) StopContainer(ctx context.Context, req *runtime.StopContainerRequest) (*runtime.StopContainerResponse, error) {
	log.Printf("[GrpcE2BEngine] StopContainer: id=%s", req.ContainerId)
	sandboxID := stripContainerSuffix(req.ContainerId)
	pod, ok := e.tracker.Get(sandboxID)
	if !ok {
		return &runtime.StopContainerResponse{}, nil
	}
	if pod.containerState != containerStateExited && pod.containerState != containerStateRemoved {
		pod.containerState = containerStateExited
		pod.containerExitCode = 0
		pod.containerFinishedAt = time.Now()
	}
	return &runtime.StopContainerResponse{}, nil
}

func (e *grpcE2BEngine) RemoveContainer(ctx context.Context, req *runtime.RemoveContainerRequest) (*runtime.RemoveContainerResponse, error) {
	log.Printf("[GrpcE2BEngine] RemoveContainer: id=%s", req.ContainerId)
	sandboxID := stripContainerSuffix(req.ContainerId)
	if pod, ok := e.tracker.Get(sandboxID); ok {
		pod.containerState = containerStateRemoved
		pod.containerFinishedAt = time.Now()
	}
	return &runtime.RemoveContainerResponse{}, nil
}

func (e *grpcE2BEngine) ListContainers(ctx context.Context, req *runtime.ListContainersRequest) (*runtime.ListContainersResponse, error) {
	log.Println("[GrpcE2BEngine] ListContainers")
	var items []*runtime.Container
	for _, pod := range e.tracker.List() {
		if pod.containerState == containerStateRemoved || pod.containerName == "" {
			continue
		}
		items = append(items, &runtime.Container{
			Id:           pod.sandboxID + "-c",
			PodSandboxId: pod.sandboxID,
			Metadata: &runtime.ContainerMetadata{
				Name:    pod.containerName,
				Attempt: 0,
			},
			State: inferContainerState(pod.containerState),
			Image: &runtime.ImageSpec{
				Image: pod.imageRef,
			},
			ImageRef:    pod.imageRef,
			CreatedAt:   containerCreatedAt(pod),
			Labels:      pod.containerLabels,
			Annotations: pod.containerAnnotations,
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
	if pod.containerState == containerStateRemoved || pod.containerName == "" {
		return nil, status.Errorf(codes.NotFound, "container %s not found", req.ContainerId)
	}
	return &runtime.ContainerStatusResponse{
		Status: &runtime.ContainerStatus{
			Id: req.ContainerId,
			Metadata: &runtime.ContainerMetadata{
				Name:    pod.containerName,
				Attempt: 0,
			},
			State:      inferContainerState(pod.containerState),
			CreatedAt:  containerCreatedAt(pod),
			StartedAt:  containerStartedAt(pod),
			FinishedAt: containerFinishedAt(pod),
			ExitCode:   pod.containerExitCode,
			Image: &runtime.ImageSpec{
				Image: pod.imageRef,
			},
			ImageRef:    pod.imageRef,
			Labels:      pod.containerLabels,
			Annotations: pod.containerAnnotations,
			LogPath:     fmt.Sprintf("/var/log/e2b/%s.log", pod.sandboxID),
		},
	}, nil
}

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
			Size_:    1,
		})
	}
	e.imageMu.RLock()
	for ref, meta := range e.imageCache {
		_ = meta
		images = append(images, &runtime.Image{
			Id:       ref,
			RepoTags: []string{ref},
			Size_:    1,
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
			Size_:    1,
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

// ========== Frame / Envelope 工具 ==========

func writeConnectBinaryEnvelope(w io.Writer, flags uint8, payload []byte) error {
	value := uint64(len(payload))<<2 | uint64(flags&0x03)
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], value)
	if _, err := w.Write(buf[:n]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

func readConnectBinaryEnvelope(r io.Reader) (flags uint8, payload []byte, err error) {
	var value uint64
	var shift uint
	for {
		b := make([]byte, 1)
		if _, err := io.ReadFull(r, b); err != nil {
			return 0, nil, err
		}
		value |= uint64(b[0]&0x7f) << shift
		if b[0]&0x80 == 0 {
			break
		}
		shift += 7
		if shift >= 64 {
			return 0, nil, fmt.Errorf("varint overflow")
		}
	}
	flags = uint8(value & 0x03)
	length := int(value >> 2)
	if length > 0 {
		const maxMessageSize = 16 * 1024 * 1024
		if length > maxMessageSize {
			return 0, nil, fmt.Errorf("message too large: %d bytes", length)
		}
		payload = make([]byte, length)
		if _, err := io.ReadFull(r, payload); err != nil {
			return 0, nil, err
		}
	}
	return flags, payload, nil
}

func writeGRPCFrame(w io.Writer, payload []byte) error {
	if len(payload) > 0xffffffff {
		return fmt.Errorf("payload too large: %d bytes", len(payload))
	}
	prefix := make([]byte, 5)
	prefix[0] = 0
	binary.BigEndian.PutUint32(prefix[1:5], uint32(len(payload)))
	if _, err := w.Write(prefix); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func readGRPCFrame(r io.Reader) (isCompressed bool, payload []byte, err error) {
	prefix := make([]byte, 5)
	if _, err := io.ReadFull(r, prefix); err != nil {
		if err == io.EOF {
			return false, nil, io.EOF
		}
		return false, nil, fmt.Errorf("read gRPC prefix: %w", err)
	}
	isCompressed = prefix[0] != 0
	length := int(binary.BigEndian.Uint32(prefix[1:5]))
	if length == 0 {
		return isCompressed, nil, nil
	}
	const maxMessageSize = 16 * 1024 * 1024
	if length > maxMessageSize {
		return false, nil, fmt.Errorf("gRPC message too large: %d bytes", length)
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return false, nil, fmt.Errorf("read gRPC payload: %w", err)
	}
	return isCompressed, payload, nil
}

const (
	envdExecRetryDelay     = 5 * time.Millisecond
	envdExecRequestTimeout = 5 * time.Second
	envdExecMaxMessageSize = 16 * 1024 * 1024
	envdSandboxPort        = 49983
	envdProxyDomain        = "e2b.app"
	envdHeaderSandboxID    = "E2b-Sandbox-Id"
	envdHeaderSandboxPort  = "E2b-Sandbox-Port"
)

func (e *grpcE2BEngine) getOrchestratorProxyAddr() string {
	if e.orchestratorProxyAddr != "" {
		return e.orchestratorProxyAddr
	}
	host, _, err := net.SplitHostPort(e.orchestratorAddr)
	if err != nil {
		return e.orchestratorAddr + ":5007"
	}
	return net.JoinHostPort(host, "5007")
}

func (e *grpcE2BEngine) getEnvdHTTPClient() *http.Client {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.envdHTTPClient != nil {
		return e.envdHTTPClient
	}
	proxyAddr := e.getOrchestratorProxyAddr()
	e.envdHTTPClient = &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, proxyAddr)
			},
		},
	}
	return e.envdHTTPClient
}

func envdHost(sandboxID string) string {
	return fmt.Sprintf("%d-%s-00000000.%s", envdSandboxPort, sandboxID, envdProxyDomain)
}

func newEnvdRequest(ctx context.Context, method, sandboxID, path string, body io.Reader) (*http.Request, error) {
	host := envdHost(sandboxID)
	req, err := http.NewRequestWithContext(ctx, method, "http://"+host+path, body)
	if err != nil {
		return nil, err
	}
	req.Host = host
	req.Header.Set(envdHeaderSandboxID, sandboxID)
	req.Header.Set(envdHeaderSandboxPort, strconv.Itoa(envdSandboxPort))
	return req, nil
}

func getAccessTokenFromMetadata(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	vals := md.Get("x-access-token")
	if len(vals) > 0 && vals[0] != "" {
		return vals[0]
	}
	vals = md.Get("X-Access-Token")
	if len(vals) > 0 && vals[0] != "" {
		return vals[0]
	}
	return ""
}

func (e *grpcE2BEngine) doExecSyncRequest(
	ctx context.Context,
	sandboxID string,
	accessToken string,
	body []byte,
) (*http.Response, int64, error) {
	requestCount := int64(0)
	for {
		requestCount++
		request, err := newEnvdRequest(ctx, http.MethodPost, sandboxID, "/process.Process/Start", bytes.NewReader(body))
		if err != nil {
			return nil, requestCount, fmt.Errorf("create request: %w", err)
		}
		request.Header.Set("Content-Type", "application/connect+proto")
		request.Header.Set("Accept", "application/connect+proto")
		request.Header.Set("Connect-Protocol-Version", "1")
		if accessToken != "" {
			request.Header.Set("X-Access-Token", accessToken)
		}

		log.Printf("[GrpcE2BEngine] >>> HTTP Request: POST %s", request.URL.String())
		log.Printf("[GrpcE2BEngine] >>> Host: %s", request.Host)
		for k, v := range request.Header {
			log.Printf("[GrpcE2BEngine] >>> Header %s: %v", k, v)
		}
		log.Printf("[GrpcE2BEngine] >>> Body hex (%d bytes): %x", len(body), body)

		response, err := e.getEnvdHTTPClient().Do(request)
		if err == nil {
			log.Printf("[GrpcE2BEngine] <<< HTTP Response: %d, Content-Type: %s", response.StatusCode, response.Header.Get("Content-Type"))
			respPeek := make([]byte, 256)
			n, _ := io.ReadFull(response.Body, respPeek)
			log.Printf("[GrpcE2BEngine] <<< Response body first %d bytes hex: %x", n, respPeek[:n])
			response.Body = io.NopCloser(io.MultiReader(bytes.NewReader(respPeek[:n]), response.Body))
			return response, requestCount, nil
		}
		log.Printf("[GrpcE2BEngine] exec sync request failed, retrying: %v", err)
		select {
		case <-ctx.Done():
			return nil, requestCount, fmt.Errorf("%w with cause: %w", ctx.Err(), context.Cause(ctx))
		case <-time.After(envdExecRetryDelay):
		}
	}
}

func (e *grpcE2BEngine) ExecSync(ctx context.Context, req *runtime.ExecSyncRequest) (*runtime.ExecSyncResponse, error) {
	log.Printf("[GrpcE2BEngine] ExecSync: id=%s, cmd=%v, timeout=%d", req.ContainerId, req.Cmd, req.Timeout)
	sandboxID := stripContainerSuffix(req.ContainerId)
	pod, ok := e.tracker.Get(sandboxID)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "sandbox %s not found", sandboxID)
	}
	if pod.state != stateRunning {
		return nil, status.Errorf(codes.FailedPrecondition, "sandbox %s is not running (state=%d)", sandboxID, pod.state)
	}

	accessToken := getAccessTokenFromMetadata(ctx)
	if accessToken == "" {
		accessToken = pod.envdAccessToken
	}
	if accessToken == "" {
		log.Printf("[GrpcE2BEngine] ExecSync: warning: no access token available for sandbox %s, envd may return 401", sandboxID)
	}

	timeout := time.Duration(req.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	stdin := false
	startReq := &process.StartRequest{
		Process: processConfigFromCommand(req.Cmd),
		Stdin:   &stdin,
	}

	payload, err := proto.Marshal(startReq)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "marshal start request: %v", err)
	}

	var bodyBuf bytes.Buffer
	if err := writeGRPCFrame(&bodyBuf, payload); err != nil {
		return nil, status.Errorf(codes.Internal, "write gRPC frame: %v", err)
	}

	resp, requestCount, err := e.doExecSyncRequest(ctx, pod.envdSandboxID(), accessToken, bodyBuf.Bytes())
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, status.Errorf(codes.DeadlineExceeded, "exec sync timeout after %d retries", requestCount)
		}
		return nil, status.Errorf(codes.Unavailable, "envd request failed after %d retries: %v", requestCount, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, status.Errorf(codes.Internal, "envd returned %d: %s", resp.StatusCode, string(respBody))
	}

	log.Printf("[GrpcE2BEngine] envd returned HTTP 200, content-type=%s", resp.Header.Get("Content-Type"))

	var stdout, stderr bytes.Buffer
	var exitCode int32 = -1
	eventCount := 0

	for {
		compressed, framePayload, err := readGRPCFrame(resp.Body)
		if err == io.EOF {
			log.Printf("[GrpcE2BEngine] gRPC stream reached EOF after %d events", eventCount)
			break
		}
		if err != nil {
			log.Printf("[GrpcE2BEngine] gRPC frame read failed: %v", err)
			break
		}

		log.Printf("[GrpcE2BEngine] read gRPC frame: compressed=%v, payload_len=%d, payload_hex=%x",
			compressed, len(framePayload), framePayload[:min(50, len(framePayload))])

		if len(framePayload) == 0 {
			continue
		}

		var msg process.StartResponse
		if err := proto.Unmarshal(framePayload, &msg); err != nil {
			log.Printf("[GrpcE2BEngine] failed to unmarshal StartResponse from gRPC frame: %v", err)

			var jsonErr struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(framePayload, &jsonErr); err == nil && jsonErr.Error.Code != "" {
				return nil, status.Errorf(codes.Internal, "envd error: [%s] %s", jsonErr.Error.Code, jsonErr.Error.Message)
			}

			var evt process.ProcessEvent
			if err2 := proto.Unmarshal(framePayload, &evt); err2 == nil {
				log.Printf("[GrpcE2BEngine] gRPC frame parsed as raw ProcessEvent")
				msg.Event = &evt
			} else {
				log.Printf("[GrpcE2BEngine] gRPC frame also not a ProcessEvent: %v", err2)
				continue
			}
		}

		if msg.Event == nil {
			log.Printf("[GrpcE2BEngine] StartResponse has no Event")
			continue
		}

		if start := msg.Event.GetStart(); start != nil {
			eventCount++
			log.Printf("[GrpcE2BEngine] event #%d: Start, pid=%d", eventCount, start.GetPid())
		} else if data := msg.Event.GetData(); data != nil {
			eventCount++
			stdoutData := data.GetStdout()
			stderrData := data.GetStderr()
			log.Printf("[GrpcE2BEngine] event #%d: Data, stdout_len=%d, stderr_len=%d", eventCount, len(stdoutData), len(stderrData))
			if len(stdoutData) > 0 {
				stdout.Write(stdoutData)
			}
			if len(stderrData) > 0 {
				stderr.Write(stderrData)
			}
		} else if end := msg.Event.GetEnd(); end != nil {
			eventCount++
			exitCode = end.GetExitCode()
			log.Printf("[GrpcE2BEngine] event #%d: End, exit_code=%d, error=%s", eventCount, exitCode, end.GetError())
			break
		} else if msg.Event.GetKeepalive() != nil {
			log.Printf("[GrpcE2BEngine] event: Keepalive")
		} else {
			log.Printf("[GrpcE2BEngine] unknown event type in ProcessEvent")
		}
	}

	if exitCode == -1 {
		if stdout.Len() > 0 || stderr.Len() > 0 || eventCount > 0 {
			log.Printf("[GrpcE2BEngine] no End event received, but got output (%d bytes stdout, %d bytes stderr, %d events), assuming success",
				stdout.Len(), stderr.Len(), eventCount)
			exitCode = 0
		} else {
			return nil, status.Errorf(codes.Internal, "envd stream closed without end event (events=%d, stdout=%d, stderr=%d)",
				eventCount, stdout.Len(), stderr.Len())
		}
	}

	return &runtime.ExecSyncResponse{
		Stdout:   stdout.Bytes(),
		Stderr:   stderr.Bytes(),
		ExitCode: exitCode,
	}, nil
}

// ========== Streaming Server ==========

func (e *grpcE2BEngine) ensureStreamingServer() {
	e.streamingOnce.Do(func() {
		e.streamingReqs = make(map[string]*execStreamRequest)
		e.attachReqs = make(map[string]*attachStreamRequest)
		lis, err := net.Listen("tcp", e.nodeIP+":0")
		if err != nil {
			lis, err = net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				log.Printf("[GrpcE2BEngine] failed to start streaming server: %v", err)
				return
			}
		}
		e.streamingListener = lis
		mux := http.NewServeMux()
		mux.HandleFunc("/exec/", e.handleExecStream)
		mux.HandleFunc("/attach/", e.handleAttachStream)
		srv := &http.Server{Handler: mux}
		go func() {
			log.Printf("[GrpcE2BEngine] streaming server listening on %s", lis.Addr().String())
			if err := srv.Serve(lis); err != nil && err != http.ErrServerClosed {
				log.Printf("[GrpcE2BEngine] streaming server exited: %v", err)
			}
		}()
	})
}

func (e *grpcE2BEngine) streamingAddr() string {
	if e.streamingListener == nil {
		return ""
	}
	return e.streamingListener.Addr().String()
}

// Exec 实现 CRI Exec 接口
func (e *grpcE2BEngine) Exec(ctx context.Context, req *runtime.ExecRequest) (*runtime.ExecResponse, error) {
	log.Printf("[GrpcE2BEngine] Exec: id=%s, cmd=%v, tty=%v, stdin=%v, stdout=%v, stderr=%v",
		req.ContainerId, req.Cmd, req.Tty, req.Stdin, req.Stdout, req.Stderr)

	sandboxID := stripContainerSuffix(req.ContainerId)
	pod, ok := e.tracker.Get(sandboxID)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "sandbox %s not found", sandboxID)
	}
	if pod.state != stateRunning {
		return nil, status.Errorf(codes.FailedPrecondition, "sandbox %s is not running", sandboxID)
	}

	e.ensureStreamingServer()
	if e.streamingListener == nil {
		return nil, status.Errorf(codes.Internal, "streaming server not available")
	}

	token := generateRandomToken()
	e.streamingMu.Lock()
	e.streamingReqs[token] = &execStreamRequest{
		sandboxID:   sandboxID,
		cmd:         req.Cmd,
		tty:         req.Tty,
		stdin:       req.Stdin,
		stdout:      req.Stdout,
		stderr:      req.Stderr,
		accessToken: pod.envdAccessToken,
	}
	e.streamingMu.Unlock()

	go func() {
		time.Sleep(5 * time.Minute)
		e.streamingMu.Lock()
		delete(e.streamingReqs, token)
		e.streamingMu.Unlock()
	}()

	url := fmt.Sprintf("http://%s/exec/%s", e.streamingAddr(), token)
	log.Printf("[GrpcE2BEngine] Exec streaming URL: %s", url)
	return &runtime.ExecResponse{Url: url}, nil
}

// Attach 实现 CRI Attach 接口
func (e *grpcE2BEngine) Attach(ctx context.Context, req *runtime.AttachRequest) (*runtime.AttachResponse, error) {
	log.Printf("[GrpcE2BEngine] Attach: id=%s, tty=%v, stdin=%v, stdout=%v, stderr=%v",
		req.ContainerId, req.Tty, req.Stdin, req.Stdout, req.Stderr)

	sandboxID := stripContainerSuffix(req.ContainerId)
	pod, ok := e.tracker.Get(sandboxID)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "sandbox %s not found", sandboxID)
	}
	if pod.state != stateRunning {
		return nil, status.Errorf(codes.FailedPrecondition, "sandbox %s is not running", sandboxID)
	}

	e.ensureStreamingServer()
	if e.streamingListener == nil {
		return nil, status.Errorf(codes.Internal, "streaming server not available")
	}

	token := generateRandomToken()
	e.streamingMu.Lock()
	e.attachReqs[token] = &attachStreamRequest{
		sandboxID:   sandboxID,
		tty:         req.Tty,
		stdin:       req.Stdin,
		stdout:      req.Stdout,
		stderr:      req.Stderr,
		accessToken: pod.envdAccessToken,
	}
	e.streamingMu.Unlock()

	go func() {
		time.Sleep(5 * time.Minute)
		e.streamingMu.Lock()
		delete(e.attachReqs, token)
		e.streamingMu.Unlock()
	}()

	url := fmt.Sprintf("http://%s/attach/%s", e.streamingAddr(), token)
	log.Printf("[GrpcE2BEngine] Attach streaming URL: %s", url)
	return &runtime.AttachResponse{Url: url}, nil
}

// doListRequest 调用 envd process.Process/List unary RPC 获取进程列表
func (e *grpcE2BEngine) doListRequest(ctx context.Context, sandboxID, accessToken string) (*process.ListResponse, error) {
	reqPayload := &process.ListRequest{}
	payload, err := proto.Marshal(reqPayload)
	if err != nil {
		return nil, fmt.Errorf("marshal ListRequest: %w", err)
	}

	req, err := newEnvdRequest(ctx, http.MethodPost, sandboxID, "/process.Process/List", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("create List request: %w", err)
	}
	req.Header.Set("Content-Type", "application/proto")
	req.Header.Set("Accept", "application/proto")
	req.Header.Set("Connect-Protocol-Version", "1")
	if accessToken != "" {
		req.Header.Set("X-Access-Token", accessToken)
	}

	resp, err := e.getEnvdHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("do List request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("envd List returned %d: %s", resp.StatusCode, string(body))
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read List body: %w", err)
	}

	var listResp process.ListResponse
	if err := proto.Unmarshal(respBody, &listResp); err != nil {
		// fallback: envd 可能对 unary 也返回 gRPC 帧
		_, framePayload, err2 := readGRPCFrame(bytes.NewReader(respBody))
		if err2 == nil && len(framePayload) > 0 {
			if err2 := proto.Unmarshal(framePayload, &listResp); err2 == nil {
				return &listResp, nil
			}
		}
		return nil, fmt.Errorf("unmarshal ListResponse: %w", err)
	}
	return &listResp, nil
}

func (e *grpcE2BEngine) sendInputToEnvd(ctx context.Context, sandboxID string, pid uint32, accessToken string, data []byte, tty bool) {
	var input *process.ProcessInput
	if tty {
		input = &process.ProcessInput{
			Input: &process.ProcessInput_Pty{Pty: data},
		}
	} else {
		input = &process.ProcessInput{
			Input: &process.ProcessInput_Stdin{Stdin: data},
		}
	}

	inputReq := &process.SendInputRequest{
		Process: &process.ProcessSelector{
			Selector: &process.ProcessSelector_Pid{Pid: pid},
		},
		Input: input,
	}
	payload, err := proto.Marshal(inputReq)
	if err != nil {
		log.Printf("[GrpcE2BEngine] sendInput marshal error: %v", err)
		return
	}

	req, err := newEnvdRequest(ctx, http.MethodPost, sandboxID, "/process.Process/SendInput", bytes.NewReader(payload))
	if err != nil {
		log.Printf("[GrpcE2BEngine] sendInput create request error: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/proto")
	req.Header.Set("Accept", "application/proto")
	req.Header.Set("Connect-Protocol-Version", "1")
	if accessToken != "" {
		req.Header.Set("X-Access-Token", accessToken)
	}

	resp, err := e.getEnvdHTTPClient().Do(req)
	if err != nil {
		log.Printf("[GrpcE2BEngine] sendInput request error: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("[GrpcE2BEngine] sendInput envd error %d: %s", resp.StatusCode, string(body))
		return
	}
}

func generateRandomToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
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
	if v, ok := annotations[annEnvVars]; ok && v != "" && v != "{}" {
		var envVars map[string]string
		if err := json.Unmarshal([]byte(v), &envVars); err == nil {
			cfg.EnvVars = envVars
		}
	}
	if v, ok := annotations[annNetwork]; ok && v != "" && v != "<nil>" {
		var network orchestrator.SandboxNetworkConfig
		if err := json.Unmarshal([]byte(v), &network); err == nil {
			cfg.Network = &network
		}
	}
	if v, ok := annotations[annVolumeMounts]; ok && v != "" && v != "[]" && v != "<nil>" {
		var mounts []*orchestrator.SandboxVolumeMount
		if err := json.Unmarshal([]byte(v), &mounts); err == nil {
			cfg.VolumeMounts = mounts
		}
	}
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

func e2bSandboxIDFromCRI(criID string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(criID) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	id := b.String()
	// The proxy host is "<port>-<sandboxID>-00000000.<domain>"; keep the
	// left-most DNS label within 63 bytes.
	if len(id) >= 8 && len(id) <= 48 {
		return id
	}
	sum := sha256.Sum256([]byte(criID))
	return hex.EncodeToString(sum[:])[:32]
}

func inferPodSandboxState(s e2bState) runtime.PodSandboxState {
	switch s {
	case stateRunning:
		return runtime.PodSandboxState_SANDBOX_READY
	default:
		return runtime.PodSandboxState_SANDBOX_NOTREADY
	}
}

func inferContainerState(s e2bContainerState) runtime.ContainerState {
	switch s {
	case containerStateCreated:
		return runtime.ContainerState_CONTAINER_CREATED
	case containerStateRunning:
		return runtime.ContainerState_CONTAINER_RUNNING
	default:
		return runtime.ContainerState_CONTAINER_EXITED
	}
}

func containerCreatedAt(pod *podInfo) int64 {
	if pod != nil && !pod.containerCreatedAt.IsZero() {
		return pod.containerCreatedAt.UnixNano()
	}
	if pod != nil {
		return pod.createdAt.UnixNano()
	}
	return 0
}

func containerStartedAt(pod *podInfo) int64 {
	if pod != nil && !pod.containerStartedAt.IsZero() {
		return pod.containerStartedAt.UnixNano()
	}
	return 0
}

func containerFinishedAt(pod *podInfo) int64 {
	if pod != nil && !pod.containerFinishedAt.IsZero() {
		return pod.containerFinishedAt.UnixNano()
	}
	return 0
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

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (e *grpcE2BEngine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.conn != nil {
		return e.conn.Close()
	}
	return nil
}
