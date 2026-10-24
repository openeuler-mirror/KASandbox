package containerd

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/cri-multiplex/pkg/engine"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

type Engine struct {
	socketPath string
	mu         sync.Once
	conn       *grpc.ClientConn
	client     runtime.RuntimeServiceClient
	imgClient  runtime.ImageServiceClient
	initErr    error
}

func New(socketPath string) *Engine {
	return &Engine{socketPath: socketPath}
}

func (e *Engine) ensureConn() error {
	e.mu.Do(func() {
		e.conn, e.initErr = grpc.Dial(
			"unix://"+e.socketPath,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16*1024*1024)),
		)
		if e.initErr == nil {
			e.client = runtime.NewRuntimeServiceClient(e.conn)
			e.imgClient = runtime.NewImageServiceClient(e.conn)
		}
	})
	return e.initErr
}

func (e *Engine) Type() engine.EngineType { return engine.EngineTypeContainer }

func (e *Engine) RunPodSandbox(ctx context.Context, req *runtime.RunPodSandboxRequest) (*runtime.RunPodSandboxResponse, error) {
	log.Printf("[Engine] RunPodSandbox: runtime_handler=%s", req.RuntimeHandler)
	if err := e.ensureConn(); err != nil {
		return nil, fmt.Errorf("containerd not available: %w", err)
	}
	return e.client.RunPodSandbox(ctx, req)
}

func (e *Engine) CreateContainer(ctx context.Context, req *runtime.CreateContainerRequest) (*runtime.CreateContainerResponse, error) {
	log.Printf("[Engine] CreateContainer: pod_sandbox_id=%s", req.PodSandboxId)
	if err := e.ensureConn(); err != nil {
		return nil, fmt.Errorf("containerd not available: %w", err)
	}
	return e.client.CreateContainer(ctx, req)
}

func (e *Engine) StartContainer(ctx context.Context, req *runtime.StartContainerRequest) (*runtime.StartContainerResponse, error) {
	if err := e.ensureConn(); err != nil {
		return nil, fmt.Errorf("containerd not available: %w", err)
	}
	return e.client.StartContainer(ctx, req)
}

func (e *Engine) StopContainer(ctx context.Context, req *runtime.StopContainerRequest) (*runtime.StopContainerResponse, error) {
	if err := e.ensureConn(); err != nil {
		return nil, fmt.Errorf("containerd not available: %w", err)
	}
	return e.client.StopContainer(ctx, req)
}

func (e *Engine) RemoveContainer(ctx context.Context, req *runtime.RemoveContainerRequest) (*runtime.RemoveContainerResponse, error) {
	if err := e.ensureConn(); err != nil {
		return nil, fmt.Errorf("containerd not available: %w", err)
	}
	return e.client.RemoveContainer(ctx, req)
}

func (e *Engine) ListContainers(ctx context.Context, req *runtime.ListContainersRequest) (*runtime.ListContainersResponse, error) {
	if err := e.ensureConn(); err != nil {
		return nil, fmt.Errorf("containerd not available: %w", err)
	}
	return e.client.ListContainers(ctx, req)
}

func (e *Engine) ContainerStatus(ctx context.Context, req *runtime.ContainerStatusRequest) (*runtime.ContainerStatusResponse, error) {
	if err := e.ensureConn(); err != nil {
		return nil, fmt.Errorf("containerd not available: %w", err)
	}
	return e.client.ContainerStatus(ctx, req)
}

func (e *Engine) StopPodSandbox(ctx context.Context, req *runtime.StopPodSandboxRequest) (*runtime.StopPodSandboxResponse, error) {
	if err := e.ensureConn(); err != nil {
		return nil, fmt.Errorf("containerd not available: %w", err)
	}
	return e.client.StopPodSandbox(ctx, req)
}

func (e *Engine) RemovePodSandbox(ctx context.Context, req *runtime.RemovePodSandboxRequest) (*runtime.RemovePodSandboxResponse, error) {
	if err := e.ensureConn(); err != nil {
		return nil, fmt.Errorf("containerd not available: %w", err)
	}
	return e.client.RemovePodSandbox(ctx, req)
}

func (e *Engine) PodSandboxStatus(ctx context.Context, req *runtime.PodSandboxStatusRequest) (*runtime.PodSandboxStatusResponse, error) {
	if err := e.ensureConn(); err != nil {
		return nil, fmt.Errorf("containerd not available: %w", err)
	}
	return e.client.PodSandboxStatus(ctx, req)
}

func (e *Engine) ListPodSandbox(ctx context.Context, req *runtime.ListPodSandboxRequest) (*runtime.ListPodSandboxResponse, error) {
	if err := e.ensureConn(); err != nil {
		return nil, fmt.Errorf("containerd not available: %w", err)
	}
	return e.client.ListPodSandbox(ctx, req)
}

func (e *Engine) ContainerStats(ctx context.Context, req *runtime.ContainerStatsRequest) (*runtime.ContainerStatsResponse, error) {
	if err := e.ensureConn(); err != nil {
		return nil, fmt.Errorf("containerd not available: %w", err)
	}
	return e.client.ContainerStats(ctx, req)
}

func (e *Engine) ListContainerStats(ctx context.Context, req *runtime.ListContainerStatsRequest) (*runtime.ListContainerStatsResponse, error) {
	if err := e.ensureConn(); err != nil {
		return nil, fmt.Errorf("containerd not available: %w", err)
	}
	return e.client.ListContainerStats(ctx, req)
}

func (e *Engine) UpdateContainerResources(ctx context.Context, req *runtime.UpdateContainerResourcesRequest) (*runtime.UpdateContainerResourcesResponse, error) {
	if err := e.ensureConn(); err != nil {
		return nil, fmt.Errorf("containerd not available: %w", err)
	}
	return e.client.UpdateContainerResources(ctx, req)
}

func (e *Engine) ReopenContainerLog(ctx context.Context, req *runtime.ReopenContainerLogRequest) (*runtime.ReopenContainerLogResponse, error) {
	if err := e.ensureConn(); err != nil {
		return nil, fmt.Errorf("containerd not available: %w", err)
	}
	return e.client.ReopenContainerLog(ctx, req)
}

func (e *Engine) ExecSync(ctx context.Context, req *runtime.ExecSyncRequest) (*runtime.ExecSyncResponse, error) {
	if err := e.ensureConn(); err != nil {
		return nil, fmt.Errorf("containerd not available: %w", err)
	}
	return e.client.ExecSync(ctx, req)
}

func (e *Engine) Exec(ctx context.Context, req *runtime.ExecRequest) (*runtime.ExecResponse, error) {
	if err := e.ensureConn(); err != nil {
		return nil, fmt.Errorf("containerd not available: %w", err)
	}
	return e.client.Exec(ctx, req)
}

func (e *Engine) Attach(ctx context.Context, req *runtime.AttachRequest) (*runtime.AttachResponse, error) {
	if err := e.ensureConn(); err != nil {
		return nil, fmt.Errorf("containerd not available: %w", err)
	}
	return e.client.Attach(ctx, req)
}

func (e *Engine) PortForward(ctx context.Context, req *runtime.PortForwardRequest) (*runtime.PortForwardResponse, error) {
	if err := e.ensureConn(); err != nil {
		return nil, fmt.Errorf("containerd not available: %w", err)
	}
	return e.client.PortForward(ctx, req)
}

func (e *Engine) Status(ctx context.Context, req *runtime.StatusRequest) (*runtime.StatusResponse, error) {
	if err := e.ensureConn(); err != nil {
		return nil, fmt.Errorf("containerd not available: %w", err)
	}
	return e.client.Status(ctx, req)
}

func (e *Engine) Version(ctx context.Context, req *runtime.VersionRequest) (*runtime.VersionResponse, error) {
	if err := e.ensureConn(); err != nil {
		return nil, fmt.Errorf("containerd not available: %w", err)
	}
	return e.client.Version(ctx, req)
}

func (e *Engine) UpdateRuntimeConfig(ctx context.Context, req *runtime.UpdateRuntimeConfigRequest) (*runtime.UpdateRuntimeConfigResponse, error) {
	if err := e.ensureConn(); err != nil {
		return nil, fmt.Errorf("containerd not available: %w", err)
	}
	return e.client.UpdateRuntimeConfig(ctx, req)
}

func (e *Engine) GetContainerEvents(ctx context.Context, req *runtime.GetEventsRequest, opts ...grpc.CallOption) (runtime.RuntimeService_GetContainerEventsClient, error) {
	if err := e.ensureConn(); err != nil {
		return nil, fmt.Errorf("containerd not available: %w", err)
	}
	return e.client.GetContainerEvents(ctx, req, opts...)
}

func (e *Engine) ListImages(ctx context.Context, req *runtime.ListImagesRequest) (*runtime.ListImagesResponse, error) {
	if err := e.ensureConn(); err != nil {
		return nil, fmt.Errorf("containerd not available: %w", err)
	}
	return e.imgClient.ListImages(ctx, req)
}

func (e *Engine) ImageStatus(ctx context.Context, req *runtime.ImageStatusRequest) (*runtime.ImageStatusResponse, error) {
	if err := e.ensureConn(); err != nil {
		return nil, fmt.Errorf("containerd not available: %w", err)
	}
	return e.imgClient.ImageStatus(ctx, req)
}

func (e *Engine) PullImage(ctx context.Context, req *runtime.PullImageRequest) (*runtime.PullImageResponse, error) {
	if err := e.ensureConn(); err != nil {
		return nil, fmt.Errorf("containerd not available: %w", err)
	}
	return e.imgClient.PullImage(ctx, req)
}

func (e *Engine) RemoveImage(ctx context.Context, req *runtime.RemoveImageRequest) (*runtime.RemoveImageResponse, error) {
	if err := e.ensureConn(); err != nil {
		return nil, fmt.Errorf("containerd not available: %w", err)
	}
	return e.imgClient.RemoveImage(ctx, req)
}

func (e *Engine) ImageFsInfo(ctx context.Context, req *runtime.ImageFsInfoRequest) (*runtime.ImageFsInfoResponse, error) {
	if err := e.ensureConn(); err != nil {
		return nil, fmt.Errorf("containerd not available: %w", err)
	}
	return e.imgClient.ImageFsInfo(ctx, req)
}

func (e *Engine) Close() error {
	if e.conn != nil {
		return e.conn.Close()
	}
	return nil
}
