package cube

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func (e *Engine) CreateContainer(ctx context.Context, req *runtime.CreateContainerRequest) (*runtime.CreateContainerResponse, error) {
	pod, err := e.begin(req.GetPodSandboxId())
	if err != nil {
		return nil, err
	}
	defer e.end(req.GetPodSandboxId())
	if pod == nil {
		return nil, status.Errorf(codes.NotFound, "sandbox %s not found", req.GetPodSandboxId())
	}
	if pod.State != podRunning {
		return nil, status.Errorf(codes.FailedPrecondition, "sandbox %s is %s", pod.CRISandboxID, pod.State)
	}
	labels := copyMap(req.GetConfig().GetLabels())
	if labels == nil {
		labels = map[string]string{}
	}
	if hash, ok := req.GetConfig().GetAnnotations()[labelHash]; ok {
		labels[labelHash] = hash
	}
	if rc, ok := req.GetConfig().GetAnnotations()[labelRestart]; ok {
		labels[labelRestart] = rc
	}
	now := time.Now()
	pod.ContainerID = req.GetPodSandboxId() + "-c"
	pod.ContainerName = req.GetConfig().GetMetadata().GetName()
	pod.ContainerImage = req.GetConfig().GetImage().GetImage()
	pod.ContainerState = ctrCreated
	pod.ContainerLabels = labels
	pod.ContainerAnnotations = copyMap(req.GetConfig().GetAnnotations())
	pod.ContainerCreatedAt = now
	pod.ContainerStartedAt = time.Time{}
	pod.ContainerFinishedAt = time.Time{}
	pod.ContainerExitCode = 0
	pod.ContainerAttempt = req.GetConfig().GetMetadata().GetAttempt()
	pod.ContainerLogPath = req.GetConfig().GetLogPath()
	if err := e.commit(*pod); err != nil {
		return nil, err
	}
	return &runtime.CreateContainerResponse{ContainerId: pod.ContainerID}, nil
}

func (e *Engine) StartContainer(ctx context.Context, req *runtime.StartContainerRequest) (*runtime.StartContainerResponse, error) {
	pod, ok := e.podByContainer(req.GetContainerId())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "container %s not found", req.GetContainerId())
	}
	locked, err := e.begin(pod.CRISandboxID)
	if err != nil {
		return nil, err
	}
	defer e.end(pod.CRISandboxID)
	if locked.State != podRunning {
		return nil, status.Errorf(codes.FailedPrecondition, "sandbox %s is %s", locked.CRISandboxID, locked.State)
	}
	locked.ContainerState = ctrRunning
	locked.ContainerStartedAt = time.Now()
	locked.ContainerFinishedAt = time.Time{}
	locked.ContainerExitCode = 0
	if err := e.commit(*locked); err != nil {
		return nil, err
	}
	return &runtime.StartContainerResponse{}, nil
}

func (e *Engine) StopContainer(ctx context.Context, req *runtime.StopContainerRequest) (*runtime.StopContainerResponse, error) {
	pod, ok := e.podByContainer(req.GetContainerId())
	if !ok {
		return &runtime.StopContainerResponse{}, nil
	}
	locked, err := e.begin(pod.CRISandboxID)
	if err != nil {
		return nil, err
	}
	defer e.end(pod.CRISandboxID)
	if locked.ContainerState == ctrRunning || locked.ContainerState == ctrCreated {
		locked.ContainerState = ctrExited
		locked.ContainerFinishedAt = time.Now()
		locked.ContainerExitCode = 0
		if err := e.commit(*locked); err != nil {
			return nil, err
		}
	}
	return &runtime.StopContainerResponse{}, nil
}

func (e *Engine) RemoveContainer(ctx context.Context, req *runtime.RemoveContainerRequest) (*runtime.RemoveContainerResponse, error) {
	pod, ok := e.podByContainer(req.GetContainerId())
	if !ok {
		return &runtime.RemoveContainerResponse{}, nil
	}
	locked, err := e.begin(pod.CRISandboxID)
	if err != nil {
		return nil, err
	}
	defer e.end(pod.CRISandboxID)
	clearContainer(locked)
	if err := e.commit(*locked); err != nil {
		return nil, err
	}
	return &runtime.RemoveContainerResponse{}, nil
}

func (e *Engine) ListContainers(ctx context.Context, req *runtime.ListContainersRequest) (*runtime.ListContainersResponse, error) {
	e.mu.Lock()
	items := make([]*runtime.Container, 0, len(e.pods))
	for _, pod := range e.pods {
		if pod.ContainerID == "" {
			continue
		}
		items = append(items, containerStatusItem(*pod))
	}
	e.mu.Unlock()
	return &runtime.ListContainersResponse{Containers: filterContainers(items, req.GetFilter())}, nil
}

func (e *Engine) ContainerStatus(ctx context.Context, req *runtime.ContainerStatusRequest) (*runtime.ContainerStatusResponse, error) {
	pod, ok := e.podByContainer(req.GetContainerId())
	if !ok || pod.ContainerID == "" {
		return nil, status.Errorf(codes.NotFound, "container %s not found", req.GetContainerId())
	}
	return &runtime.ContainerStatusResponse{Status: containerStatus(pod)}, nil
}

func (e *Engine) ContainerStats(ctx context.Context, req *runtime.ContainerStatsRequest) (*runtime.ContainerStatsResponse, error) {
	pod, ok := e.podByContainer(req.GetContainerId())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "container %s not found", req.GetContainerId())
	}
	return &runtime.ContainerStatsResponse{Stats: zeroStats(pod)}, nil
}

func (e *Engine) ListContainerStats(ctx context.Context, req *runtime.ListContainerStatsRequest) (*runtime.ListContainerStatsResponse, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	stats := make([]*runtime.ContainerStats, 0, len(e.pods))
	for _, pod := range e.pods {
		if pod.ContainerID == "" {
			continue
		}
		stats = append(stats, zeroStats(*pod))
	}
	return &runtime.ListContainerStatsResponse{Stats: stats}, nil
}

func (e *Engine) UpdateContainerResources(ctx context.Context, req *runtime.UpdateContainerResourcesRequest) (*runtime.UpdateContainerResourcesResponse, error) {
	return &runtime.UpdateContainerResourcesResponse{}, nil
}

func (e *Engine) ReopenContainerLog(ctx context.Context, req *runtime.ReopenContainerLogRequest) (*runtime.ReopenContainerLogResponse, error) {
	return &runtime.ReopenContainerLogResponse{}, nil
}

func (e *Engine) ExecSync(ctx context.Context, req *runtime.ExecSyncRequest) (*runtime.ExecSyncResponse, error) {
	pod, ok := e.podByContainer(req.GetContainerId())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "container %s not found", req.GetContainerId())
	}
	if pod.State != podRunning || pod.ContainerState != ctrRunning {
		return nil, status.Errorf(codes.FailedPrecondition, "container %s is not running", req.GetContainerId())
	}
	// Align with the CubeSandbox SDK / direct path: run the command through
	// guest envd (SandboxIP:49983), which returns stdout/stderr/exit code.
	// CubeboxMgr.Exec only returns Ret and is not used here.
	result, err := e.execViaEnvd(ctx, pod.SandboxIP, pod.EnvdAccessToken, req.GetCmd(), req.GetTimeout())
	if err != nil {
		return nil, err
	}
	return &runtime.ExecSyncResponse{
		Stdout:   result.Stdout,
		Stderr:   result.Stderr,
		ExitCode: result.ExitCode,
	}, nil
}

func (e *Engine) Exec(ctx context.Context, req *runtime.ExecRequest) (*runtime.ExecResponse, error) {
	return nil, status.Error(codes.Unimplemented, "cube Exec streaming is not implemented")
}

func (e *Engine) Attach(ctx context.Context, req *runtime.AttachRequest) (*runtime.AttachResponse, error) {
	return nil, status.Error(codes.Unimplemented, "cube Attach is not implemented")
}

func (e *Engine) PortForward(ctx context.Context, req *runtime.PortForwardRequest) (*runtime.PortForwardResponse, error) {
	return nil, status.Error(codes.Unimplemented, "cube PortForward is not implemented")
}

func (e *Engine) Status(ctx context.Context, req *runtime.StatusRequest) (*runtime.StatusResponse, error) {
	return &runtime.StatusResponse{Status: &runtime.RuntimeStatus{Conditions: []*runtime.RuntimeCondition{
		{Type: runtime.RuntimeReady, Status: true},
		{Type: runtime.NetworkReady, Status: true},
	}}}, nil
}

func (e *Engine) Version(ctx context.Context, req *runtime.VersionRequest) (*runtime.VersionResponse, error) {
	return &runtime.VersionResponse{
		Version:           "0.1.0",
		RuntimeName:       "cube",
		RuntimeVersion:    "0.1.0",
		RuntimeApiVersion: "v1",
	}, nil
}

func (e *Engine) UpdateRuntimeConfig(ctx context.Context, req *runtime.UpdateRuntimeConfigRequest) (*runtime.UpdateRuntimeConfigResponse, error) {
	return &runtime.UpdateRuntimeConfigResponse{}, nil
}

func (e *Engine) GetContainerEvents(ctx context.Context, req *runtime.GetEventsRequest, opts ...grpc.CallOption) (runtime.RuntimeService_GetContainerEventsClient, error) {
	return nil, status.Error(codes.Unimplemented, "cube GetContainerEvents is not implemented")
}

func (e *Engine) ListImages(ctx context.Context, req *runtime.ListImagesRequest) (*runtime.ListImagesResponse, error) {
	return &runtime.ListImagesResponse{}, nil
}

func (e *Engine) ImageStatus(ctx context.Context, req *runtime.ImageStatusRequest) (*runtime.ImageStatusResponse, error) {
	return &runtime.ImageStatusResponse{}, nil
}

func (e *Engine) PullImage(ctx context.Context, req *runtime.PullImageRequest) (*runtime.PullImageResponse, error) {
	return nil, status.Error(codes.Unimplemented, "cube templates are not pulled through CRI")
}

func (e *Engine) RemoveImage(ctx context.Context, req *runtime.RemoveImageRequest) (*runtime.RemoveImageResponse, error) {
	return nil, status.Error(codes.Unimplemented, "cube templates are not removed through CRI")
}

func (e *Engine) ImageFsInfo(ctx context.Context, req *runtime.ImageFsInfoRequest) (*runtime.ImageFsInfoResponse, error) {
	return &runtime.ImageFsInfoResponse{}, nil
}

func clearContainer(pod *CubePodState) {
	pod.ContainerID = ""
	pod.ContainerName = ""
	pod.ContainerImage = ""
	pod.ContainerState = ""
	pod.ContainerLabels = nil
	pod.ContainerAnnotations = nil
	pod.ContainerCreatedAt = time.Time{}
	pod.ContainerStartedAt = time.Time{}
	pod.ContainerFinishedAt = time.Time{}
	pod.ContainerExitCode = 0
	pod.ContainerAttempt = 0
	pod.ContainerLogPath = ""
}

func containerStatusItem(pod CubePodState) *runtime.Container {
	return &runtime.Container{
		Id:           pod.ContainerID,
		PodSandboxId: pod.CRISandboxID,
		Metadata: &runtime.ContainerMetadata{
			Name:    pod.ContainerName,
			Attempt: pod.ContainerAttempt,
		},
		Image:       &runtime.ImageSpec{Image: pod.ContainerImage},
		ImageRef:    pod.ContainerImage,
		State:       containerState(pod.ContainerState),
		CreatedAt:   unixNano(pod.ContainerCreatedAt),
		Labels:      copyMap(pod.ContainerLabels),
		Annotations: copyMap(pod.ContainerAnnotations),
	}
}

func containerStatus(pod CubePodState) *runtime.ContainerStatus {
	return &runtime.ContainerStatus{
		Id:          pod.ContainerID,
		Metadata:    &runtime.ContainerMetadata{Name: pod.ContainerName, Attempt: pod.ContainerAttempt},
		State:       containerState(pod.ContainerState),
		CreatedAt:   unixNano(pod.ContainerCreatedAt),
		StartedAt:   unixNano(pod.ContainerStartedAt),
		FinishedAt:  unixNano(pod.ContainerFinishedAt),
		ExitCode:    pod.ContainerExitCode,
		Image:       &runtime.ImageSpec{Image: pod.ContainerImage},
		ImageRef:    pod.ContainerImage,
		Labels:      copyMap(pod.ContainerLabels),
		Annotations: copyMap(pod.ContainerAnnotations),
		LogPath:     pod.ContainerLogPath,
	}
}

func zeroStats(pod CubePodState) *runtime.ContainerStats {
	return &runtime.ContainerStats{
		Attributes: &runtime.ContainerAttributes{
			Id: pod.ContainerID,
			Metadata: &runtime.ContainerMetadata{
				Name:    pod.ContainerName,
				Attempt: pod.ContainerAttempt,
			},
			Labels:      copyMap(pod.ContainerLabels),
			Annotations: copyMap(pod.ContainerAnnotations),
		},
	}
}

func containerState(state string) runtime.ContainerState {
	switch state {
	case ctrCreated:
		return runtime.ContainerState_CONTAINER_CREATED
	case ctrRunning:
		return runtime.ContainerState_CONTAINER_RUNNING
	default:
		return runtime.ContainerState_CONTAINER_EXITED
	}
}

func filterContainers(items []*runtime.Container, f *runtime.ContainerFilter) []*runtime.Container {
	if f == nil {
		return items
	}
	out := make([]*runtime.Container, 0, len(items))
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
		if !labelsMatch(item.Labels, f.LabelSelector) {
			continue
		}
		out = append(out, item)
	}
	return out
}
