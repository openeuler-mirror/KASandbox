package engine

import (
	"context"
	"fmt"
	"log"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

type ContainerEngine struct {
	conn   *grpc.ClientConn
	client runtime.RuntimeServiceClient
}

func NewContainerEngine(socketPath string) (*ContainerEngine, error) {
	conn, err := grpc.Dial(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16*1024*1024)),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to containerd %s: %w", socketPath, err)
	}
	return &ContainerEngine{
		conn:   conn,
		client: runtime.NewRuntimeServiceClient(conn),
	}, nil
}

func (e *ContainerEngine) Type() EngineType { return EngineTypeContainer }

func (e *ContainerEngine) RunPodSandbox(ctx context.Context, req *runtime.RunPodSandboxRequest) (*runtime.RunPodSandboxResponse, error) {
	log.Printf("[ContainerEngine] RunPodSandbox: runtime_handler=%s", req.RuntimeHandler)
	return e.client.RunPodSandbox(ctx, req)
}

func (e *ContainerEngine) CreateContainer(ctx context.Context, req *runtime.CreateContainerRequest) (*runtime.CreateContainerResponse, error) {
	log.Printf("[ContainerEngine] CreateContainer: pod_sandbox_id=%s", req.PodSandboxId)
	return e.client.CreateContainer(ctx, req)
}

func (e *ContainerEngine) StartContainer(ctx context.Context, req *runtime.StartContainerRequest) (*runtime.StartContainerResponse, error) {
	return e.client.StartContainer(ctx, req)
}

func (e *ContainerEngine) StopContainer(ctx context.Context, req *runtime.StopContainerRequest) (*runtime.StopContainerResponse, error) {
	return e.client.StopContainer(ctx, req)
}

func (e *ContainerEngine) RemoveContainer(ctx context.Context, req *runtime.RemoveContainerRequest) (*runtime.RemoveContainerResponse, error) {
	return e.client.RemoveContainer(ctx, req)
}

func (e *ContainerEngine) ListContainers(ctx context.Context, req *runtime.ListContainersRequest) (*runtime.ListContainersResponse, error) {
	return e.client.ListContainers(ctx, req)
}

func (e *ContainerEngine) ContainerStatus(ctx context.Context, req *runtime.ContainerStatusRequest) (*runtime.ContainerStatusResponse, error) {
	return e.client.ContainerStatus(ctx, req)
}

func (e *ContainerEngine) StopPodSandbox(ctx context.Context, req *runtime.StopPodSandboxRequest) (*runtime.StopPodSandboxResponse, error) {
	return e.client.StopPodSandbox(ctx, req)
}

func (e *ContainerEngine) RemovePodSandbox(ctx context.Context, req *runtime.RemovePodSandboxRequest) (*runtime.RemovePodSandboxResponse, error) {
	return e.client.RemovePodSandbox(ctx, req)
}

func (e *ContainerEngine) PodSandboxStatus(ctx context.Context, req *runtime.PodSandboxStatusRequest) (*runtime.PodSandboxStatusResponse, error) {
	return e.client.PodSandboxStatus(ctx, req)
}

func (e *ContainerEngine) ListPodSandbox(ctx context.Context, req *runtime.ListPodSandboxRequest) (*runtime.ListPodSandboxResponse, error) {
	return e.client.ListPodSandbox(ctx, req)
}

func (e *ContainerEngine) ContainerStats(ctx context.Context, req *runtime.ContainerStatsRequest) (*runtime.ContainerStatsResponse, error) {
	return e.client.ContainerStats(ctx, req)
}

func (e *ContainerEngine) ListContainerStats(ctx context.Context, req *runtime.ListContainerStatsRequest) (*runtime.ListContainerStatsResponse, error) {
	return e.client.ListContainerStats(ctx, req)
}

func (e *ContainerEngine) UpdateContainerResources(ctx context.Context, req *runtime.UpdateContainerResourcesRequest) (*runtime.UpdateContainerResourcesResponse, error) {
	return e.client.UpdateContainerResources(ctx, req)
}

func (e *ContainerEngine) ReopenContainerLog(ctx context.Context, req *runtime.ReopenContainerLogRequest) (*runtime.ReopenContainerLogResponse, error) {
	return e.client.ReopenContainerLog(ctx, req)
}

func (e *ContainerEngine) ExecSync(ctx context.Context, req *runtime.ExecSyncRequest) (*runtime.ExecSyncResponse, error) {
	return e.client.ExecSync(ctx, req)
}

func (e *ContainerEngine) Exec(ctx context.Context, req *runtime.ExecRequest) (*runtime.ExecResponse, error) {
	return e.client.Exec(ctx, req)
}

func (e *ContainerEngine) Attach(ctx context.Context, req *runtime.AttachRequest) (*runtime.AttachResponse, error) {
	return e.client.Attach(ctx, req)
}

func (e *ContainerEngine) PortForward(ctx context.Context, req *runtime.PortForwardRequest) (*runtime.PortForwardResponse, error) {
	return e.client.PortForward(ctx, req)
}

func (e *ContainerEngine) Status(ctx context.Context, req *runtime.StatusRequest) (*runtime.StatusResponse, error) {
	return e.client.Status(ctx, req)
}

func (e *ContainerEngine) Version(ctx context.Context, req *runtime.VersionRequest) (*runtime.VersionResponse, error) {
	return e.client.Version(ctx, req)
}

func (e *ContainerEngine) UpdateRuntimeConfig(ctx context.Context, req *runtime.UpdateRuntimeConfigRequest) (*runtime.UpdateRuntimeConfigResponse, error) {
	return e.client.UpdateRuntimeConfig(ctx, req)
}

func (e *ContainerEngine) GetContainerEvents(ctx context.Context, req *runtime.GetEventsRequest, opts ...grpc.CallOption) (runtime.RuntimeService_GetContainerEventsClient, error) {
	return e.client.GetContainerEvents(ctx, req, opts...)
}

func (e *ContainerEngine) Close() error {
	if e.conn != nil {
		return e.conn.Close()
	}
	return nil
}
