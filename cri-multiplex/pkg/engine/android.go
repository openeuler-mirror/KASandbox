package engine

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

const (
	androidRuntimeHandler = "android"
	androidDefaultImage   = "android.dev/cvd:local"

	annAndroidADBPort    = "android.dev/adb-port"
	annAndroidWebRTCPort = "android.dev/webrtc-port"
	annAndroidLaunchArgs = "android.dev/launch-args"
	annAndroidInstanceID = "android.dev/instance-id"
	annAndroidBaseInst   = "android.dev/base-instance-num"
)

type AndroidConfig struct {
	Enabled              bool
	ArtifactsDir         string
	NodeIP               string
	ADBPortStart         int
	BaseInstanceNumStart int
	WebRTCPortStart      int
	LaunchTimeout        time.Duration
	StateDir             string
	CVDGroup             string
}

type androidSandboxState string

const (
	androidSandboxCreated    androidSandboxState = "Created"
	androidSandboxVMStarting androidSandboxState = "VMStarting"
	androidSandboxRunning    androidSandboxState = "Running"
	androidSandboxStopped    androidSandboxState = "Stopped"
	androidSandboxRemoved    androidSandboxState = "Removed"
	androidSandboxUnknown    androidSandboxState = "Unknown"
)

type androidContainerState string

const (
	androidContainerCreated androidContainerState = "Created"
	androidContainerRunning androidContainerState = "Running"
	androidContainerExited  androidContainerState = "Exited"
	androidContainerRemoved androidContainerState = "Removed"
)

type AndroidSandboxRecord struct {
	CRISandboxID string
	PodUID       string
	Name         string
	Namespace    string

	ArtifactsDir    string
	WorkDir         string
	InstanceID      string
	BaseInstanceNum int
	NodeIP          string
	ADBPort         int
	WebRTCPort      int
	LaunchPID       int
	LaunchPGID      int
	LaunchLogPath   string

	State     androidSandboxState
	CreatedAt time.Time
	StartedAt time.Time
	StoppedAt time.Time

	Labels      map[string]string
	Annotations map[string]string
}

type AndroidContainerRecord struct {
	ContainerID  string
	CRISandboxID string
	Name         string
	Attempt      uint32
	Image        string
	ImageRef     string
	State        androidContainerState
	CreatedAt    time.Time
	StartedAt    time.Time
	FinishedAt   time.Time
	ExitCode     int32
	Labels       map[string]string
	Annotations  map[string]string
	LogPath      string
	FullLogPath  string
}

type AndroidEngine struct {
	cfg            AndroidConfig
	mu             sync.Mutex
	pods           map[string]*AndroidSandboxRecord
	containers     map[string]*AndroidContainerRecord
	portOwners     map[int]string
	instanceOwners map[int]string
}

func NewAndroidEngine(cfg AndroidConfig) *AndroidEngine {
	if cfg.ArtifactsDir == "" {
		cfg.ArtifactsDir = "/home/fjq/cf17"
	}
	if cfg.ADBPortStart == 0 {
		cfg.ADBPortStart = 6520
	}
	if cfg.BaseInstanceNumStart == 0 {
		cfg.BaseInstanceNumStart = 1
	}
	if cfg.LaunchTimeout == 0 {
		cfg.LaunchTimeout = 180 * time.Second
	}
	if cfg.StateDir == "" {
		cfg.StateDir = "/var/lib/cri-multiplex/android"
	}
	if cfg.CVDGroup == "" {
		cfg.CVDGroup = "cvdnetwork"
	}
	return &AndroidEngine{
		cfg:            cfg,
		pods:           make(map[string]*AndroidSandboxRecord),
		containers:     make(map[string]*AndroidContainerRecord),
		portOwners:     make(map[int]string),
		instanceOwners: make(map[int]string),
	}
}

func (e *AndroidEngine) Type() EngineType { return EngineTypeAndroid }

func (e *AndroidEngine) ensureEnabled() error {
	if !e.cfg.Enabled {
		return status.Error(codes.Unavailable, "android runtime is disabled; start cri-multiplex with --android-enabled")
	}
	return nil
}

func (e *AndroidEngine) validateHostPrerequisites() error {
	launchPath := filepath.Join(e.cfg.ArtifactsDir, "bin", "launch_cvd")
	if st, err := os.Stat(launchPath); err != nil {
		return status.Errorf(codes.FailedPrecondition, "android launch_cvd not found: %s: %v", launchPath, err)
	} else if st.Mode()&0111 == 0 {
		return status.Errorf(codes.FailedPrecondition, "android launch_cvd is not executable: %s", launchPath)
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return status.Errorf(codes.FailedPrecondition, "/dev/kvm is required for android runtime: %v", err)
	}
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		return status.Errorf(codes.FailedPrecondition, "/dev/net/tun is required for android runtime: %v", err)
	}
	if _, err := lookupGroupID(e.cfg.CVDGroup); err != nil {
		return status.Errorf(codes.FailedPrecondition, "android CVD group %q is required: %v", e.cfg.CVDGroup, err)
	}
	return nil
}

func (e *AndroidEngine) RunPodSandbox(ctx context.Context, req *runtime.RunPodSandboxRequest) (*runtime.RunPodSandboxResponse, error) {
	if err := e.ensureEnabled(); err != nil {
		return nil, err
	}
	if req.Config == nil || req.Config.Metadata == nil {
		return nil, status.Error(codes.InvalidArgument, "missing pod sandbox metadata")
	}
	if err := e.validateHostPrerequisites(); err != nil {
		return nil, err
	}

	sandboxID := req.Config.Metadata.Uid
	if sandboxID == "" {
		return nil, status.Error(codes.InvalidArgument, "missing pod UID")
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if existing, ok := e.pods[sandboxID]; ok && existing.State != androidSandboxRemoved {
		log.Printf("[AndroidEngine] RunPodSandbox idempotent: sandbox=%s state=%s", sandboxID, existing.State)
		return &runtime.RunPodSandboxResponse{PodSandboxId: sandboxID}, nil
	}

	baseInstanceNum, err := e.allocateInstanceNumLocked(req.Config.Annotations[annAndroidBaseInst], sandboxID)
	if err != nil {
		return nil, err
	}
	defaultADBPort := e.cfg.ADBPortStart + baseInstanceNum - e.cfg.BaseInstanceNumStart
	adbPort, err := e.allocatePortLocked(req.Config.Annotations[annAndroidADBPort], defaultADBPort, sandboxID)
	if err != nil {
		delete(e.instanceOwners, baseInstanceNum)
		return nil, err
	}
	webrtcPort := 0
	if e.cfg.WebRTCPortStart > 0 || req.Config.Annotations[annAndroidWebRTCPort] != "" {
		webrtcPort, err = e.allocatePortLocked(req.Config.Annotations[annAndroidWebRTCPort], e.cfg.WebRTCPortStart, sandboxID)
		if err != nil {
			delete(e.portOwners, adbPort)
			delete(e.instanceOwners, baseInstanceNum)
			return nil, err
		}
	}

	now := time.Now()
	rec := &AndroidSandboxRecord{
		CRISandboxID:    sandboxID,
		PodUID:          sandboxID,
		Name:            req.Config.Metadata.Name,
		Namespace:       req.Config.Metadata.Namespace,
		ArtifactsDir:    e.cfg.ArtifactsDir,
		WorkDir:         filepath.Join(e.cfg.StateDir, sandboxID),
		InstanceID:      annotationOrDefault(req.Config.Annotations, annAndroidInstanceID, shortAndroidID(sandboxID)),
		BaseInstanceNum: baseInstanceNum,
		NodeIP:          e.cfg.NodeIP,
		ADBPort:         adbPort,
		WebRTCPort:      webrtcPort,
		State:           androidSandboxCreated,
		CreatedAt:       now,
		Labels:          copyStringMap(req.Config.Labels),
		Annotations:     copyStringMap(req.Config.Annotations),
	}
	rec.LaunchLogPath = filepath.Join(rec.WorkDir, "launch_cvd.log")
	e.pods[sandboxID] = rec
	log.Printf("[AndroidEngine] sandbox created: cri_id=%s pod=%s/%s base_instance_num=%d adb=%d workdir=%s",
		sandboxID, rec.Namespace, rec.Name, rec.BaseInstanceNum, rec.ADBPort, rec.WorkDir)
	return &runtime.RunPodSandboxResponse{PodSandboxId: sandboxID}, nil
}

func (e *AndroidEngine) StopPodSandbox(ctx context.Context, req *runtime.StopPodSandboxRequest) (*runtime.StopPodSandboxResponse, error) {
	if err := e.ensureEnabled(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	rec, ok := e.pods[req.PodSandboxId]
	e.mu.Unlock()
	if !ok {
		return &runtime.StopPodSandboxResponse{}, nil
	}
	if err := e.stopCVD(ctx, rec); err != nil {
		log.Printf("[AndroidEngine] StopPodSandbox warning: sandbox=%s stop failed: %v", req.PodSandboxId, err)
	}
	e.mu.Lock()
	if cur, ok := e.pods[req.PodSandboxId]; ok && cur.State != androidSandboxRemoved {
		cur.State = androidSandboxStopped
		cur.StoppedAt = time.Now()
	}
	for _, c := range e.containers {
		if c.CRISandboxID == req.PodSandboxId && c.State != androidContainerRemoved {
			c.State = androidContainerExited
			c.FinishedAt = time.Now()
			c.ExitCode = 0
		}
	}
	e.mu.Unlock()
	return &runtime.StopPodSandboxResponse{}, nil
}

func (e *AndroidEngine) RemovePodSandbox(ctx context.Context, req *runtime.RemovePodSandboxRequest) (*runtime.RemovePodSandboxResponse, error) {
	if err := e.ensureEnabled(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	rec, ok := e.pods[req.PodSandboxId]
	e.mu.Unlock()
	if !ok {
		return &runtime.RemovePodSandboxResponse{}, nil
	}
	if err := e.stopCVD(ctx, rec); err != nil {
		log.Printf("[AndroidEngine] RemovePodSandbox warning: sandbox=%s stop failed: %v", req.PodSandboxId, err)
	}
	e.mu.Lock()
	delete(e.portOwners, rec.ADBPort)
	if rec.WebRTCPort > 0 {
		delete(e.portOwners, rec.WebRTCPort)
	}
	delete(e.instanceOwners, rec.BaseInstanceNum)
	for id, c := range e.containers {
		if c.CRISandboxID == req.PodSandboxId {
			c.State = androidContainerRemoved
			delete(e.containers, id)
		}
	}
	rec.State = androidSandboxRemoved
	delete(e.pods, req.PodSandboxId)
	e.mu.Unlock()
	log.Printf("[AndroidEngine] sandbox removed: cri_id=%s", req.PodSandboxId)
	return &runtime.RemovePodSandboxResponse{}, nil
}

func (e *AndroidEngine) PodSandboxStatus(ctx context.Context, req *runtime.PodSandboxStatusRequest) (*runtime.PodSandboxStatusResponse, error) {
	if err := e.ensureEnabled(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	rec, ok := e.pods[req.PodSandboxId]
	e.mu.Unlock()
	if !ok {
		return nil, status.Errorf(codes.NotFound, "android sandbox %s not found", req.PodSandboxId)
	}
	anns := copyStringMap(rec.Annotations)
	anns["android.dev/adb-url"] = fmt.Sprintf("%s:%d", rec.NodeIP, rec.ADBPort)
	anns["android.dev/adb-port"] = strconv.Itoa(rec.ADBPort)
	anns["android.dev/cvd-state"] = string(rec.State)
	anns["android.dev/instance-id"] = rec.InstanceID
	anns["android.dev/base-instance-num"] = strconv.Itoa(rec.BaseInstanceNum)
	anns["android.dev/artifacts-dir"] = rec.ArtifactsDir
	anns["android.dev/launch-log"] = rec.LaunchLogPath
	if rec.WebRTCPort > 0 {
		anns["android.dev/webrtc-port"] = strconv.Itoa(rec.WebRTCPort)
		anns["android.dev/webrtc-url"] = fmt.Sprintf("http://%s:%d", rec.NodeIP, rec.WebRTCPort)
	}
	return &runtime.PodSandboxStatusResponse{Status: &runtime.PodSandboxStatus{
		Id: rec.CRISandboxID,
		Metadata: &runtime.PodSandboxMetadata{
			Name:      rec.Name,
			Uid:       rec.PodUID,
			Namespace: rec.Namespace,
		},
		State:       androidPodState(rec.State),
		CreatedAt:   rec.CreatedAt.UnixNano(),
		Network:     &runtime.PodSandboxNetworkStatus{Ip: rec.NodeIP},
		Labels:      rec.Labels,
		Annotations: anns,
	}}, nil
}

func (e *AndroidEngine) ListPodSandbox(ctx context.Context, req *runtime.ListPodSandboxRequest) (*runtime.ListPodSandboxResponse, error) {
	if err := e.ensureEnabled(); err != nil {
		return &runtime.ListPodSandboxResponse{}, nil
	}
	e.mu.Lock()
	items := make([]*runtime.PodSandbox, 0, len(e.pods))
	for _, rec := range e.pods {
		if rec.State == androidSandboxRemoved {
			continue
		}
		items = append(items, &runtime.PodSandbox{
			Id: rec.CRISandboxID,
			Metadata: &runtime.PodSandboxMetadata{
				Name:      rec.Name,
				Uid:       rec.PodUID,
				Namespace: rec.Namespace,
			},
			State:       androidPodState(rec.State),
			CreatedAt:   rec.CreatedAt.UnixNano(),
			Labels:      rec.Labels,
			Annotations: rec.Annotations,
		})
	}
	e.mu.Unlock()
	return &runtime.ListPodSandboxResponse{Items: filterPodSandbox(items, req.Filter)}, nil
}

func (e *AndroidEngine) CreateContainer(ctx context.Context, req *runtime.CreateContainerRequest) (*runtime.CreateContainerResponse, error) {
	if err := e.ensureEnabled(); err != nil {
		return nil, err
	}
	if req.Config == nil || req.Config.Metadata == nil {
		return nil, status.Error(codes.InvalidArgument, "missing container metadata")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.pods[req.PodSandboxId]; !ok {
		return nil, status.Errorf(codes.NotFound, "android sandbox %s not found", req.PodSandboxId)
	}
	containerID := req.PodSandboxId + "-c"
	labels := copyStringMap(req.Config.Labels)
	if hash, ok := req.Config.Annotations["io.kubernetes.container.hash"]; ok {
		labels["io.kubernetes.container.hash"] = hash
	}
	if rc, ok := req.Config.Annotations["io.kubernetes.container.restartCount"]; ok {
		labels["io.kubernetes.container.restartCount"] = rc
	}
	rec := &AndroidContainerRecord{
		ContainerID:  containerID,
		CRISandboxID: req.PodSandboxId,
		Name:         req.Config.Metadata.Name,
		Attempt:      req.Config.Metadata.Attempt,
		Image:        req.Config.Image.Image,
		ImageRef:     req.Config.Image.Image,
		State:        androidContainerCreated,
		CreatedAt:    time.Now(),
		Labels:       labels,
		Annotations:  copyStringMap(req.Config.Annotations),
		LogPath:      req.Config.LogPath,
	}
	e.containers[containerID] = rec
	log.Printf("[AndroidEngine] CreateContainer: pod=%s container=%s image=%s", req.PodSandboxId, containerID, rec.Image)
	return &runtime.CreateContainerResponse{ContainerId: containerID}, nil
}

func (e *AndroidEngine) StartContainer(ctx context.Context, req *runtime.StartContainerRequest) (*runtime.StartContainerResponse, error) {
	if err := e.ensureEnabled(); err != nil {
		return nil, err
	}
	sandboxID := stripContainerSuffix(req.ContainerId)
	e.mu.Lock()
	pod, ok := e.pods[sandboxID]
	container, cok := e.containers[req.ContainerId]
	if !ok || !cok {
		e.mu.Unlock()
		return nil, status.Errorf(codes.NotFound, "android container %s not found", req.ContainerId)
	}
	if pod.State == androidSandboxRunning && container.State == androidContainerRunning {
		e.mu.Unlock()
		return &runtime.StartContainerResponse{}, nil
	}
	pod.State = androidSandboxVMStarting
	e.mu.Unlock()

	if err := e.startCVD(ctx, pod); err != nil {
		e.mu.Lock()
		pod.State = androidSandboxUnknown
		e.mu.Unlock()
		return nil, err
	}

	e.mu.Lock()
	pod.State = androidSandboxRunning
	pod.StartedAt = time.Now()
	container.State = androidContainerRunning
	container.StartedAt = pod.StartedAt
	container.FinishedAt = time.Time{}
	container.ExitCode = 0
	e.mu.Unlock()
	log.Printf("[AndroidEngine] StartContainer: android vm ready sandbox=%s adb=%s:%d", sandboxID, pod.NodeIP, pod.ADBPort)
	return &runtime.StartContainerResponse{}, nil
}

func (e *AndroidEngine) StopContainer(ctx context.Context, req *runtime.StopContainerRequest) (*runtime.StopContainerResponse, error) {
	if err := e.ensureEnabled(); err != nil {
		return nil, err
	}
	sandboxID := stripContainerSuffix(req.ContainerId)
	e.mu.Lock()
	pod := e.pods[sandboxID]
	container := e.containers[req.ContainerId]
	e.mu.Unlock()
	if pod != nil {
		if err := e.stopCVD(ctx, pod); err != nil {
			log.Printf("[AndroidEngine] StopContainer warning: container=%s stop failed: %v", req.ContainerId, err)
		}
	}
	e.mu.Lock()
	if container != nil && container.State != androidContainerRemoved {
		container.State = androidContainerExited
		container.FinishedAt = time.Now()
		container.ExitCode = 0
	}
	if pod != nil && pod.State != androidSandboxRemoved {
		pod.State = androidSandboxStopped
		pod.StoppedAt = time.Now()
	}
	e.mu.Unlock()
	return &runtime.StopContainerResponse{}, nil
}

func (e *AndroidEngine) RemoveContainer(ctx context.Context, req *runtime.RemoveContainerRequest) (*runtime.RemoveContainerResponse, error) {
	if err := e.ensureEnabled(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	if c, ok := e.containers[req.ContainerId]; ok {
		c.State = androidContainerRemoved
		c.FinishedAt = time.Now()
	}
	delete(e.containers, req.ContainerId)
	e.mu.Unlock()
	return &runtime.RemoveContainerResponse{}, nil
}

func (e *AndroidEngine) ListContainers(ctx context.Context, req *runtime.ListContainersRequest) (*runtime.ListContainersResponse, error) {
	if err := e.ensureEnabled(); err != nil {
		return &runtime.ListContainersResponse{}, nil
	}
	e.mu.Lock()
	items := make([]*runtime.Container, 0, len(e.containers))
	for _, c := range e.containers {
		if c.State == androidContainerRemoved {
			continue
		}
		items = append(items, &runtime.Container{
			Id:           c.ContainerID,
			PodSandboxId: c.CRISandboxID,
			Metadata: &runtime.ContainerMetadata{
				Name:    c.Name,
				Attempt: c.Attempt,
			},
			State:       androidCRIContainerState(c.State),
			Image:       &runtime.ImageSpec{Image: c.Image},
			ImageRef:    c.ImageRef,
			CreatedAt:   c.CreatedAt.UnixNano(),
			Labels:      c.Labels,
			Annotations: c.Annotations,
		})
	}
	e.mu.Unlock()
	return &runtime.ListContainersResponse{Containers: filterContainers(items, req.Filter)}, nil
}

func (e *AndroidEngine) ContainerStatus(ctx context.Context, req *runtime.ContainerStatusRequest) (*runtime.ContainerStatusResponse, error) {
	if err := e.ensureEnabled(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	c, ok := e.containers[req.ContainerId]
	e.mu.Unlock()
	if !ok || c.State == androidContainerRemoved {
		return nil, status.Errorf(codes.NotFound, "android container %s not found", req.ContainerId)
	}
	return &runtime.ContainerStatusResponse{Status: &runtime.ContainerStatus{
		Id: req.ContainerId,
		Metadata: &runtime.ContainerMetadata{
			Name:    c.Name,
			Attempt: c.Attempt,
		},
		State:       androidCRIContainerState(c.State),
		CreatedAt:   c.CreatedAt.UnixNano(),
		StartedAt:   timeToUnixNano(c.StartedAt),
		FinishedAt:  timeToUnixNano(c.FinishedAt),
		ExitCode:    c.ExitCode,
		Image:       &runtime.ImageSpec{Image: c.Image},
		ImageRef:    c.ImageRef,
		Labels:      c.Labels,
		Annotations: c.Annotations,
		LogPath:     c.LogPath,
	}}, nil
}

func (e *AndroidEngine) ContainerStats(ctx context.Context, req *runtime.ContainerStatsRequest) (*runtime.ContainerStatsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "android ContainerStats is not implemented")
}

func (e *AndroidEngine) ListContainerStats(ctx context.Context, req *runtime.ListContainerStatsRequest) (*runtime.ListContainerStatsResponse, error) {
	return &runtime.ListContainerStatsResponse{}, nil
}

func (e *AndroidEngine) UpdateContainerResources(ctx context.Context, req *runtime.UpdateContainerResourcesRequest) (*runtime.UpdateContainerResourcesResponse, error) {
	return &runtime.UpdateContainerResourcesResponse{}, nil
}

func (e *AndroidEngine) ReopenContainerLog(ctx context.Context, req *runtime.ReopenContainerLogRequest) (*runtime.ReopenContainerLogResponse, error) {
	return &runtime.ReopenContainerLogResponse{}, nil
}

func (e *AndroidEngine) ExecSync(ctx context.Context, req *runtime.ExecSyncRequest) (*runtime.ExecSyncResponse, error) {
	return nil, status.Error(codes.Unimplemented, "android ExecSync is not implemented in MVP")
}

func (e *AndroidEngine) Exec(ctx context.Context, req *runtime.ExecRequest) (*runtime.ExecResponse, error) {
	return nil, status.Error(codes.Unimplemented, "android Exec is not implemented in MVP")
}

func (e *AndroidEngine) Attach(ctx context.Context, req *runtime.AttachRequest) (*runtime.AttachResponse, error) {
	return nil, status.Error(codes.Unimplemented, "android Attach is not implemented in MVP")
}

func (e *AndroidEngine) PortForward(ctx context.Context, req *runtime.PortForwardRequest) (*runtime.PortForwardResponse, error) {
	return nil, status.Error(codes.Unimplemented, "android PortForward is not implemented in MVP")
}

func (e *AndroidEngine) Status(ctx context.Context, req *runtime.StatusRequest) (*runtime.StatusResponse, error) {
	return &runtime.StatusResponse{Status: &runtime.RuntimeStatus{Conditions: []*runtime.RuntimeCondition{
		{Type: runtime.RuntimeReady, Status: e.cfg.Enabled},
		{Type: runtime.NetworkReady, Status: true},
	}}}, nil
}

func (e *AndroidEngine) Version(ctx context.Context, req *runtime.VersionRequest) (*runtime.VersionResponse, error) {
	return &runtime.VersionResponse{
		Version:           "0.1.0",
		RuntimeName:       "android-cvd",
		RuntimeVersion:    "0.1.0",
		RuntimeApiVersion: "v1",
	}, nil
}

func (e *AndroidEngine) UpdateRuntimeConfig(ctx context.Context, req *runtime.UpdateRuntimeConfigRequest) (*runtime.UpdateRuntimeConfigResponse, error) {
	return &runtime.UpdateRuntimeConfigResponse{}, nil
}

func (e *AndroidEngine) GetContainerEvents(ctx context.Context, req *runtime.GetEventsRequest, opts ...grpc.CallOption) (runtime.RuntimeService_GetContainerEventsClient, error) {
	return nil, status.Error(codes.Unimplemented, "android GetContainerEvents is not implemented")
}

func (e *AndroidEngine) ListImages(ctx context.Context, req *runtime.ListImagesRequest) (*runtime.ListImagesResponse, error) {
	return &runtime.ListImagesResponse{Images: []*runtime.Image{androidImage()}}, nil
}

func (e *AndroidEngine) ImageStatus(ctx context.Context, req *runtime.ImageStatusRequest) (*runtime.ImageStatusResponse, error) {
	if !isAndroidImage(req.Image.Image) {
		return &runtime.ImageStatusResponse{}, nil
	}
	return &runtime.ImageStatusResponse{Image: androidImage()}, nil
}

func (e *AndroidEngine) PullImage(ctx context.Context, req *runtime.PullImageRequest) (*runtime.PullImageResponse, error) {
	if !isAndroidImage(req.Image.Image) {
		return nil, status.Error(codes.InvalidArgument, "not an android image")
	}
	if err := e.ensureEnabled(); err != nil {
		return nil, err
	}
	if err := e.validateHostPrerequisites(); err != nil {
		return nil, err
	}
	return &runtime.PullImageResponse{ImageRef: androidDefaultImage}, nil
}

func (e *AndroidEngine) RemoveImage(ctx context.Context, req *runtime.RemoveImageRequest) (*runtime.RemoveImageResponse, error) {
	return &runtime.RemoveImageResponse{}, nil
}

func (e *AndroidEngine) ImageFsInfo(ctx context.Context, req *runtime.ImageFsInfoRequest) (*runtime.ImageFsInfoResponse, error) {
	return &runtime.ImageFsInfoResponse{}, nil
}

func (e *AndroidEngine) Close() error { return nil }

func (e *AndroidEngine) allocatePortLocked(requested string, start int, owner string) (int, error) {
	if requested != "" {
		port, err := strconv.Atoi(requested)
		if err != nil || port <= 0 || port > 65535 {
			return 0, status.Errorf(codes.InvalidArgument, "invalid android port %q", requested)
		}
		if current, ok := e.portOwners[port]; ok && current != owner {
			return 0, status.Errorf(codes.ResourceExhausted, "android port %d is already allocated", port)
		}
		e.portOwners[port] = owner
		return port, nil
	}
	if start <= 0 {
		return 0, nil
	}
	for port := start; port <= 65535; port++ {
		if _, ok := e.portOwners[port]; !ok {
			e.portOwners[port] = owner
			return port, nil
		}
	}
	return 0, status.Error(codes.ResourceExhausted, "no android host ports available")
}

func (e *AndroidEngine) allocateInstanceNumLocked(requested string, owner string) (int, error) {
	if requested != "" {
		instanceNum, err := strconv.Atoi(requested)
		if err != nil || instanceNum <= 0 {
			return 0, status.Errorf(codes.InvalidArgument, "invalid android base instance num %q", requested)
		}
		if current, ok := e.instanceOwners[instanceNum]; ok && current != owner {
			return 0, status.Errorf(codes.ResourceExhausted, "android base instance num %d is already allocated", instanceNum)
		}
		e.instanceOwners[instanceNum] = owner
		return instanceNum, nil
	}
	for instanceNum := e.cfg.BaseInstanceNumStart; instanceNum < e.cfg.BaseInstanceNumStart+1024; instanceNum++ {
		if _, ok := e.instanceOwners[instanceNum]; !ok {
			e.instanceOwners[instanceNum] = owner
			return instanceNum, nil
		}
	}
	return 0, status.Error(codes.ResourceExhausted, "no android base instance nums available")
}

func (e *AndroidEngine) startCVD(ctx context.Context, rec *AndroidSandboxRecord) error {
	if err := os.MkdirAll(rec.WorkDir, 0755); err != nil {
		return status.Errorf(codes.Internal, "create android work dir: %v", err)
	}
	logFile, err := os.OpenFile(rec.LaunchLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return status.Errorf(codes.Internal, "open launch log: %v", err)
	}
	defer logFile.Close()

	args := []string{
		fmt.Sprintf("--base_instance_num=%d", rec.BaseInstanceNum),
		"--gpu_mode=guest_swiftshader",
		"--extra_kernel_cmdline=arm64.nompam",
		"--start_webrtc=true",
		"--vm_manager=qemu_cli",
	}
	if extra := strings.TrimSpace(rec.Annotations[annAndroidLaunchArgs]); extra != "" {
		args = append(args, strings.Fields(extra)...)
	}
	launchPath := filepath.Join(rec.ArtifactsDir, "bin", "launch_cvd")
	cmd := exec.CommandContext(context.Background(), launchPath, args...)
	cmd.Dir = rec.ArtifactsDir
	cmd.Env = append(os.Environ(), "HOME="+rec.ArtifactsDir)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	sysProcAttr, err := e.cvdSysProcAttr(true)
	if err != nil {
		return err
	}
	cmd.SysProcAttr = sysProcAttr
	if err := cmd.Start(); err != nil {
		return status.Errorf(codes.Internal, "start launch_cvd: %v", err)
	}

	e.mu.Lock()
	rec.LaunchPID = cmd.Process.Pid
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
		rec.LaunchPGID = pgid
	}
	e.mu.Unlock()
	exitCh := make(chan error, 1)
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("[AndroidEngine] launch_cvd exited: sandbox=%s pid=%d err=%v", rec.CRISandboxID, cmd.Process.Pid, err)
			exitCh <- err
		} else {
			log.Printf("[AndroidEngine] launch_cvd exited: sandbox=%s pid=%d", rec.CRISandboxID, cmd.Process.Pid)
			exitCh <- nil
		}
	}()

	waitCtx, cancel := context.WithTimeout(ctx, e.cfg.LaunchTimeout)
	defer cancel()
	if err := waitTCPReady(waitCtx, rec.NodeIP, rec.ADBPort, exitCh); err != nil {
		_ = e.stopCVD(context.Background(), rec)
		return status.Errorf(codes.DeadlineExceeded, "android ADB %s:%d not ready: %v", rec.NodeIP, rec.ADBPort, err)
	}
	select {
	case err := <-exitCh:
		_ = e.stopCVD(context.Background(), rec)
		if err != nil {
			return status.Errorf(codes.Internal, "launch_cvd exited after ADB became ready: %v", err)
		}
		return status.Error(codes.Internal, "launch_cvd exited after ADB became ready")
	case <-time.After(5 * time.Second):
	}
	return nil
}

func (e *AndroidEngine) stopCVD(ctx context.Context, rec *AndroidSandboxRecord) error {
	if rec == nil {
		return nil
	}
	// cvd_internal_stop in the cf17 package is not instance-scoped. For
	// multi-instance safety, clean up only the process group started for this
	// CRI sandbox.
	e.killCVDProcessGroup(rec)
	return nil
}

func (e *AndroidEngine) stopCVDGlobal(ctx context.Context, rec *AndroidSandboxRecord) error {
	if rec == nil {
		return nil
	}
	stopPath := filepath.Join(rec.ArtifactsDir, "bin", "cvd")
	stopArgs := []string{"stop"}
	if _, err := os.Stat(stopPath); err != nil {
		stopPath = filepath.Join(rec.ArtifactsDir, "bin", "cvd_internal_stop")
		stopArgs = nil
	}
	if _, err := os.Stat(stopPath); err != nil {
		e.killCVDProcessGroup(rec)
		return nil
	}
	stopCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(stopCtx, stopPath, stopArgs...)
	cmd.Dir = rec.ArtifactsDir
	cmd.Env = append(os.Environ(), "HOME="+rec.ArtifactsDir)
	if sysProcAttr, err := e.cvdSysProcAttr(false); err == nil {
		cmd.SysProcAttr = sysProcAttr
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("[AndroidEngine] cvd stop failed sandbox=%s: %v output=%s", rec.CRISandboxID, err, string(out))
	}
	e.killCVDProcessGroup(rec)
	return nil
}

func (e *AndroidEngine) killCVDProcessGroup(rec *AndroidSandboxRecord) {
	if rec.LaunchPGID > 0 {
		_ = syscall.Kill(-rec.LaunchPGID, syscall.SIGTERM)
		time.Sleep(2 * time.Second)
		_ = syscall.Kill(-rec.LaunchPGID, syscall.SIGKILL)
	} else if rec.LaunchPID > 0 {
		_ = syscall.Kill(rec.LaunchPID, syscall.SIGTERM)
	}
}

func (e *AndroidEngine) cvdSysProcAttr(setPGID bool) (*syscall.SysProcAttr, error) {
	attr := &syscall.SysProcAttr{Setpgid: setPGID}
	groupID, err := lookupGroupID(e.cfg.CVDGroup)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "lookup android CVD group %q: %v", e.cfg.CVDGroup, err)
	}
	attr.Credential = &syscall.Credential{
		Uid:    uint32(os.Getuid()),
		Gid:    uint32(os.Getgid()),
		Groups: []uint32{uint32(groupID)},
	}
	return attr, nil
}

func waitTCPReady(ctx context.Context, host string, port int, exitCh <-chan error) error {
	if host == "" {
		host = "127.0.0.1"
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	var lastErr error
	for {
		dialer := net.Dialer{Timeout: 1 * time.Second}
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err
		select {
		case err := <-exitCh:
			if err != nil {
				return fmt.Errorf("launch_cvd exited before ADB ready: %w", err)
			}
			return fmt.Errorf("launch_cvd exited before ADB ready")
		case <-ctx.Done():
			return lastErr
		case <-ticker.C:
		}
	}
}

func androidPodState(s androidSandboxState) runtime.PodSandboxState {
	if s == androidSandboxRunning {
		return runtime.PodSandboxState_SANDBOX_READY
	}
	return runtime.PodSandboxState_SANDBOX_NOTREADY
}

func androidCRIContainerState(s androidContainerState) runtime.ContainerState {
	switch s {
	case androidContainerCreated:
		return runtime.ContainerState_CONTAINER_CREATED
	case androidContainerRunning:
		return runtime.ContainerState_CONTAINER_RUNNING
	default:
		return runtime.ContainerState_CONTAINER_EXITED
	}
}

func timeToUnixNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func annotationOrDefault(anns map[string]string, key, fallback string) string {
	if anns != nil && anns[key] != "" {
		return anns[key]
	}
	return fallback
}

func shortAndroidID(id string) string {
	cleaned := e2bSandboxIDFromCRI(id)
	if len(cleaned) > 12 {
		return cleaned[:12]
	}
	return cleaned
}

func copyStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func isAndroidImage(image string) bool {
	return image == androidDefaultImage || strings.HasPrefix(image, "android.dev/")
}

func androidImage() *runtime.Image {
	return &runtime.Image{
		Id:       androidDefaultImage,
		RepoTags: []string{androidDefaultImage},
		Size_:    1,
	}
}

func lookupGroupID(name string) (int, error) {
	data, err := os.ReadFile("/etc/group")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) < 3 || parts[0] != name {
			continue
		}
		gid, err := strconv.Atoi(parts[2])
		if err != nil {
			return 0, fmt.Errorf("invalid gid %q in /etc/group", parts[2])
		}
		return gid, nil
	}
	return 0, fmt.Errorf("group not found in /etc/group")
}
