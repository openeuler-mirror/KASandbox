package engine

import (
	"context"

	"google.golang.org/grpc"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

type EngineType string

const (
	EngineTypeContainer EngineType = "container"
	EngineTypeE2B       EngineType = "e2b"
)

type RuntimeEngine interface {
	Type() EngineType

	RunPodSandbox(ctx context.Context, req *runtime.RunPodSandboxRequest) (*runtime.RunPodSandboxResponse, error)
	StopPodSandbox(ctx context.Context, req *runtime.StopPodSandboxRequest) (*runtime.StopPodSandboxResponse, error)
	RemovePodSandbox(ctx context.Context, req *runtime.RemovePodSandboxRequest) (*runtime.RemovePodSandboxResponse, error)
	PodSandboxStatus(ctx context.Context, req *runtime.PodSandboxStatusRequest) (*runtime.PodSandboxStatusResponse, error)
	ListPodSandbox(ctx context.Context, req *runtime.ListPodSandboxRequest) (*runtime.ListPodSandboxResponse, error)

	CreateContainer(ctx context.Context, req *runtime.CreateContainerRequest) (*runtime.CreateContainerResponse, error)
	StartContainer(ctx context.Context, req *runtime.StartContainerRequest) (*runtime.StartContainerResponse, error)
	StopContainer(ctx context.Context, req *runtime.StopContainerRequest) (*runtime.StopContainerResponse, error)
	RemoveContainer(ctx context.Context, req *runtime.RemoveContainerRequest) (*runtime.RemoveContainerResponse, error)
	ListContainers(ctx context.Context, req *runtime.ListContainersRequest) (*runtime.ListContainersResponse, error)
	ContainerStatus(ctx context.Context, req *runtime.ContainerStatusRequest) (*runtime.ContainerStatusResponse, error)
	ContainerStats(ctx context.Context, req *runtime.ContainerStatsRequest) (*runtime.ContainerStatsResponse, error)
	ListContainerStats(ctx context.Context, req *runtime.ListContainerStatsRequest) (*runtime.ListContainerStatsResponse, error)

	UpdateContainerResources(ctx context.Context, req *runtime.UpdateContainerResourcesRequest) (*runtime.UpdateContainerResourcesResponse, error)
	ReopenContainerLog(ctx context.Context, req *runtime.ReopenContainerLogRequest) (*runtime.ReopenContainerLogResponse, error)

	ExecSync(ctx context.Context, req *runtime.ExecSyncRequest) (*runtime.ExecSyncResponse, error)
	Exec(ctx context.Context, req *runtime.ExecRequest) (*runtime.ExecResponse, error)
	Attach(ctx context.Context, req *runtime.AttachRequest) (*runtime.AttachResponse, error)
	PortForward(ctx context.Context, req *runtime.PortForwardRequest) (*runtime.PortForwardResponse, error)

	Status(ctx context.Context, req *runtime.StatusRequest) (*runtime.StatusResponse, error)
	Version(ctx context.Context, req *runtime.VersionRequest) (*runtime.VersionResponse, error)
	UpdateRuntimeConfig(ctx context.Context, req *runtime.UpdateRuntimeConfigRequest) (*runtime.UpdateRuntimeConfigResponse, error)

	GetContainerEvents(ctx context.Context, req *runtime.GetEventsRequest, opts ...grpc.CallOption) (runtime.RuntimeService_GetContainerEventsClient, error)

	ListImages(ctx context.Context, req *runtime.ListImagesRequest) (*runtime.ListImagesResponse, error)
	ImageStatus(ctx context.Context, req *runtime.ImageStatusRequest) (*runtime.ImageStatusResponse, error)
	PullImage(ctx context.Context, req *runtime.PullImageRequest) (*runtime.PullImageResponse, error)
	RemoveImage(ctx context.Context, req *runtime.RemoveImageRequest) (*runtime.RemoveImageResponse, error)
	ImageFsInfo(ctx context.Context, req *runtime.ImageFsInfoRequest) (*runtime.ImageFsInfoResponse, error)

	Close() error
}
