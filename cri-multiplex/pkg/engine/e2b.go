package engine

import (
	"context"
	"fmt"
	"log"

	"google.golang.org/grpc"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

type E2BEngine struct {
	// TODO: REST API client, auth config
	// apiClient *e2b.Client
}

func NewE2BEngine() *E2BEngine {
	log.Println("[E2BEngine] initialized (mock mode)")
	return &E2BEngine{}
}

func (e *E2BEngine) Type() EngineType { return EngineTypeE2B }

func (e *E2BEngine) RunPodSandbox(ctx context.Context, req *runtime.RunPodSandboxRequest) (*runtime.RunPodSandboxResponse, error) {
	log.Printf("[E2BEngine] RunPodSandbox: name=%s, runtime_handler=%s",
		req.Config.Metadata.Name, req.RuntimeHandler)

	// TODO: call E2B REST API to create VM sandbox
	sandboxID := fmt.Sprintf("e2b-%s", req.Config.Metadata.Uid)
	return &runtime.RunPodSandboxResponse{PodSandboxId: sandboxID}, nil
}

func (e *E2BEngine) CreateContainer(ctx context.Context, req *runtime.CreateContainerRequest) (*runtime.CreateContainerResponse, error) {
	log.Printf("[E2BEngine] CreateContainer: pod=%s, name=%s",
		req.PodSandboxId, req.Config.Metadata.Name)

	// TODO: translate CRI ContainerConfig to E2B VM process/command
	containerID := fmt.Sprintf("e2b-ctr-%s-%s", req.PodSandboxId, req.Config.Metadata.Name)
	return &runtime.CreateContainerResponse{ContainerId: containerID}, nil
}

func (e *E2BEngine) StartContainer(ctx context.Context, req *runtime.StartContainerRequest) (*runtime.StartContainerResponse, error) {
	log.Printf("[E2BEngine] StartContainer: id=%s", req.ContainerId)
	// TODO: start process inside VM
	return &runtime.StartContainerResponse{}, nil
}

func (e *E2BEngine) StopContainer(ctx context.Context, req *runtime.StopContainerRequest) (*runtime.StopContainerResponse, error) {
	log.Printf("[E2BEngine] StopContainer: id=%s, timeout=%d", req.ContainerId, req.Timeout)
	return &runtime.StopContainerResponse{}, nil
}

func (e *E2BEngine) RemoveContainer(ctx context.Context, req *runtime.RemoveContainerRequest) (*runtime.RemoveContainerResponse, error) {
	log.Printf("[E2BEngine] RemoveContainer: id=%s", req.ContainerId)
	return &runtime.RemoveContainerResponse{}, nil
}

func (e *E2BEngine) ListContainers(ctx context.Context, req *runtime.ListContainersRequest) (*runtime.ListContainersResponse, error) {
	log.Println("[E2BEngine] ListContainers")
	return &runtime.ListContainersResponse{Containers: []*runtime.Container{}}, nil
}

func (e *E2BEngine) ContainerStatus(ctx context.Context, req *runtime.ContainerStatusRequest) (*runtime.ContainerStatusResponse, error) {
	log.Printf("[E2BEngine] ContainerStatus: id=%s", req.ContainerId)
	return &runtime.ContainerStatusResponse{
		Status: &runtime.ContainerStatus{
			Id:    req.ContainerId,
			State: runtime.ContainerState_CONTAINER_RUNNING,
		},
	}, nil
}

func (e *E2BEngine) StopPodSandbox(ctx context.Context, req *runtime.StopPodSandboxRequest) (*runtime.StopPodSandboxResponse, error) {
	log.Printf("[E2BEngine] StopPodSandbox: id=%s", req.PodSandboxId)
	// TODO: stop E2B Sandbox
	return &runtime.StopPodSandboxResponse{}, nil
}

func (e *E2BEngine) RemovePodSandbox(ctx context.Context, req *runtime.RemovePodSandboxRequest) (*runtime.RemovePodSandboxResponse, error) {
	log.Printf("[E2BEngine] RemovePodSandbox: id=%s", req.PodSandboxId)
	// TODO: destroy E2B Sandbox
	return &runtime.RemovePodSandboxResponse{}, nil
}

func (e *E2BEngine) PodSandboxStatus(ctx context.Context, req *runtime.PodSandboxStatusRequest) (*runtime.PodSandboxStatusResponse, error) {
	log.Printf("[E2BEngine] PodSandboxStatus: id=%s", req.PodSandboxId)
	return &runtime.PodSandboxStatusResponse{
		Status: &runtime.PodSandboxStatus{
			Id:    req.PodSandboxId,
			State: runtime.PodSandboxState_SANDBOX_READY,
		},
	}, nil
}

func (e *E2BEngine) ListPodSandbox(ctx context.Context, req *runtime.ListPodSandboxRequest) (*runtime.ListPodSandboxResponse, error) {
	log.Println("[E2BEngine] ListPodSandbox")
	return &runtime.ListPodSandboxResponse{Items: []*runtime.PodSandbox{}}, nil
}

func (e *E2BEngine) ContainerStats(ctx context.Context, req *runtime.ContainerStatsRequest) (*runtime.ContainerStatsResponse, error) {
	log.Printf("[E2BEngine] ContainerStats: id=%s", req.ContainerId)
	return &runtime.ContainerStatsResponse{Stats: &runtime.ContainerStats{}}, nil
}

func (e *E2BEngine) ListContainerStats(ctx context.Context, req *runtime.ListContainerStatsRequest) (*runtime.ListContainerStatsResponse, error) {
	log.Println("[E2BEngine] ListContainerStats")
	return &runtime.ListContainerStatsResponse{Stats: []*runtime.ContainerStats{}}, nil
}

func (e *E2BEngine) UpdateContainerResources(ctx context.Context, req *runtime.UpdateContainerResourcesRequest) (*runtime.UpdateContainerResourcesResponse, error) {
	log.Printf("[E2BEngine] UpdateContainerResources: id=%s", req.ContainerId)
	return &runtime.UpdateContainerResourcesResponse{}, nil
}

func (e *E2BEngine) ReopenContainerLog(ctx context.Context, req *runtime.ReopenContainerLogRequest) (*runtime.ReopenContainerLogResponse, error) {
	log.Printf("[E2BEngine] ReopenContainerLog: id=%s", req.ContainerId)
	return &runtime.ReopenContainerLogResponse{}, nil
}

func (e *E2BEngine) ExecSync(ctx context.Context, req *runtime.ExecSyncRequest) (*runtime.ExecSyncResponse, error) {
	log.Printf("[E2BEngine] ExecSync: id=%s, cmd=%v", req.ContainerId, req.Cmd)
	return &runtime.ExecSyncResponse{Stdout: []byte("e2b-mock-output"), ExitCode: 0}, nil
}

func (e *E2BEngine) Exec(ctx context.Context, req *runtime.ExecRequest) (*runtime.ExecResponse, error) {
	log.Printf("[E2BEngine] Exec: id=%s", req.ContainerId)
	return &runtime.ExecResponse{Url: "http://e2b-mock/exec"}, nil
}

func (e *E2BEngine) Attach(ctx context.Context, req *runtime.AttachRequest) (*runtime.AttachResponse, error) {
	log.Printf("[E2BEngine] Attach: id=%s", req.ContainerId)
	return &runtime.AttachResponse{Url: "http://e2b-mock/attach"}, nil
}

func (e *E2BEngine) PortForward(ctx context.Context, req *runtime.PortForwardRequest) (*runtime.PortForwardResponse, error) {
	log.Printf("[E2BEngine] PortForward: id=%s, ports=%v", req.PodSandboxId, req.Port)
	return &runtime.PortForwardResponse{Url: "http://e2b-mock/portforward"}, nil
}

func (e *E2BEngine) Status(ctx context.Context, req *runtime.StatusRequest) (*runtime.StatusResponse, error) {
	log.Println("[E2BEngine] Status")
	return &runtime.StatusResponse{
		Status: &runtime.RuntimeStatus{
			Conditions: []*runtime.RuntimeCondition{{Status: true}},
		},
	}, nil
}

func (e *E2BEngine) Version(ctx context.Context, req *runtime.VersionRequest) (*runtime.VersionResponse, error) {
	log.Println("[E2BEngine] Version")
	return &runtime.VersionResponse{
		Version:           "0.1.0",
		RuntimeName:       "e2b",
		RuntimeVersion:    "0.1.0",
		RuntimeApiVersion: "v1",
	}, nil
}

func (e *E2BEngine) UpdateRuntimeConfig(ctx context.Context, req *runtime.UpdateRuntimeConfigRequest) (*runtime.UpdateRuntimeConfigResponse, error) {
	log.Println("[E2BEngine] UpdateRuntimeConfig")
	return &runtime.UpdateRuntimeConfigResponse{}, nil
}

func (e *E2BEngine) GetContainerEvents(ctx context.Context, req *runtime.GetEventsRequest, opts ...grpc.CallOption) (runtime.RuntimeService_GetContainerEventsClient, error) {
	log.Println("[E2BEngine] GetContainerEvents (not implemented)")
	return nil, fmt.Errorf("GetContainerEvents not implemented for E2BEngine")
}

func (e *E2BEngine) ListImages(ctx context.Context, req *runtime.ListImagesRequest) (*runtime.ListImagesResponse, error) {
	log.Println("[E2BEngine] ListImages (not supported)")
	return &runtime.ListImagesResponse{Images: []*runtime.Image{}}, nil
}

func (e *E2BEngine) ImageStatus(ctx context.Context, req *runtime.ImageStatusRequest) (*runtime.ImageStatusResponse, error) {
	log.Println("[E2BEngine] ImageStatus (not supported)")
	return &runtime.ImageStatusResponse{}, nil
}

func (e *E2BEngine) PullImage(ctx context.Context, req *runtime.PullImageRequest) (*runtime.PullImageResponse, error) {
	log.Println("[E2BEngine] PullImage (not supported)")
	return nil, fmt.Errorf("PullImage not supported for E2BEngine")
}

func (e *E2BEngine) RemoveImage(ctx context.Context, req *runtime.RemoveImageRequest) (*runtime.RemoveImageResponse, error) {
	log.Println("[E2BEngine] RemoveImage (not supported)")
	return nil, fmt.Errorf("RemoveImage not supported for E2BEngine")
}

func (e *E2BEngine) ImageFsInfo(ctx context.Context, req *runtime.ImageFsInfoRequest) (*runtime.ImageFsInfoResponse, error) {
	log.Println("[E2BEngine] ImageFsInfo (not supported)")
	return nil, fmt.Errorf("ImageFsInfo not supported for E2BEngine")
}

func (e *E2BEngine) Close() error { return nil }