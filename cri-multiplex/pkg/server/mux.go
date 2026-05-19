package server

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"sync"

	"google.golang.org/grpc"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/cri-multiplex/pkg/engine"
)

type MuxServer struct {
	runtime.UnimplementedRuntimeServiceServer
	runtime.UnimplementedImageServiceServer

	containerEngine engine.RuntimeEngine
	e2bEngine       engine.RuntimeEngine

	podRoutes       sync.Map // podSandboxID -> engine.EngineType
	containerRoutes sync.Map // containerID -> engine.EngineType

	grpcServer *grpc.Server
}

func NewMuxServer(containerEngine, e2bEngine engine.RuntimeEngine) *MuxServer {
	return &MuxServer{
		containerEngine: containerEngine,
		e2bEngine:       e2bEngine,
	}
}

func (s *MuxServer) Start(socketPath string) error {
	if err := os.RemoveAll(socketPath); err != nil {
		return fmt.Errorf("remove existing socket %s: %w", socketPath, err)
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", socketPath, err)
	}

	s.grpcServer = grpc.NewServer()
	runtime.RegisterRuntimeServiceServer(s.grpcServer, s)
	runtime.RegisterImageServiceServer(s.grpcServer, s)

	log.Printf("cri-multiplex listening on %s", socketPath)
	return s.grpcServer.Serve(listener)
}

func (s *MuxServer) Stop() {
	if s.grpcServer != nil {
		s.grpcServer.GracefulStop()
	}
}

func (s *MuxServer) resolveEngineByPod(podSandboxID string) (engine.RuntimeEngine, error) {
	val, ok := s.podRoutes.Load(podSandboxID)
	if !ok {
		return nil, fmt.Errorf("no engine found for pod sandbox %s", podSandboxID)
	}
	engineType := val.(engine.EngineType)
	switch engineType {
	case engine.EngineTypeE2B:
		return s.e2bEngine, nil
	default:
		return s.containerEngine, nil
	}
}

func (s *MuxServer) resolveEngineByContainer(containerID string) (engine.RuntimeEngine, error) {
	val, ok := s.containerRoutes.Load(containerID)
	if !ok {
		return nil, fmt.Errorf("no engine found for container %s", containerID)
	}
	engineType := val.(engine.EngineType)
	switch engineType {
	case engine.EngineTypeE2B:
		return s.e2bEngine, nil
	default:
		return s.containerEngine, nil
	}
}

// ========== RuntimeService ==========

func (s *MuxServer) RunPodSandbox(ctx context.Context, req *runtime.RunPodSandboxRequest) (*runtime.RunPodSandboxResponse, error) {
	handler := req.RuntimeHandler
	var eng engine.RuntimeEngine

	if handler == "e2b" {
		eng = s.e2bEngine
	} else {
		eng = s.containerEngine
	}

	log.Printf("[MuxServer] RunPodSandbox routing: handler=%q -> %s", handler, eng.Type())

	resp, err := eng.RunPodSandbox(ctx, req)
	if err != nil {
		return nil, err
	}

	s.podRoutes.Store(resp.PodSandboxId, eng.Type())
	log.Printf("[MuxServer] registered pod %s -> %s", resp.PodSandboxId, eng.Type())
	return resp, nil
}

func (s *MuxServer) CreateContainer(ctx context.Context, req *runtime.CreateContainerRequest) (*runtime.CreateContainerResponse, error) {
	eng, err := s.resolveEngineByPod(req.PodSandboxId)
	if err != nil {
		return nil, err
	}

	resp, err := eng.CreateContainer(ctx, req)
	if err != nil {
		return nil, err
	}

	s.containerRoutes.Store(resp.ContainerId, eng.Type())
	log.Printf("[MuxServer] registered container %s -> %s", resp.ContainerId, eng.Type())
	return resp, nil
}

func (s *MuxServer) StartContainer(ctx context.Context, req *runtime.StartContainerRequest) (*runtime.StartContainerResponse, error) {
	eng, err := s.resolveEngineByContainer(req.ContainerId)
	if err != nil {
		return nil, err
	}
	return eng.StartContainer(ctx, req)
}

func (s *MuxServer) StopContainer(ctx context.Context, req *runtime.StopContainerRequest) (*runtime.StopContainerResponse, error) {
	eng, err := s.resolveEngineByContainer(req.ContainerId)
	if err != nil {
		return nil, err
	}
	return eng.StopContainer(ctx, req)
}

func (s *MuxServer) RemoveContainer(ctx context.Context, req *runtime.RemoveContainerRequest) (*runtime.RemoveContainerResponse, error) {
	eng, err := s.resolveEngineByContainer(req.ContainerId)
	if err != nil {
		return nil, err
	}

	resp, err := eng.RemoveContainer(ctx, req)
	if err == nil {
		s.containerRoutes.Delete(req.ContainerId)
	}
	return resp, err
}

func (s *MuxServer) ContainerStatus(ctx context.Context, req *runtime.ContainerStatusRequest) (*runtime.ContainerStatusResponse, error) {
	eng, err := s.resolveEngineByContainer(req.ContainerId)
	if err != nil {
		return nil, err
	}
	return eng.ContainerStatus(ctx, req)
}

func (s *MuxServer) StopPodSandbox(ctx context.Context, req *runtime.StopPodSandboxRequest) (*runtime.StopPodSandboxResponse, error) {
	eng, err := s.resolveEngineByPod(req.PodSandboxId)
	if err != nil {
		return nil, err
	}
	return eng.StopPodSandbox(ctx, req)
}

func (s *MuxServer) RemovePodSandbox(ctx context.Context, req *runtime.RemovePodSandboxRequest) (*runtime.RemovePodSandboxResponse, error) {
	eng, err := s.resolveEngineByPod(req.PodSandboxId)
	if err != nil {
		return nil, err
	}

	resp, err := eng.RemovePodSandbox(ctx, req)
	if err == nil {
		s.podRoutes.Delete(req.PodSandboxId)
	}
	return resp, err
}

func (s *MuxServer) PodSandboxStatus(ctx context.Context, req *runtime.PodSandboxStatusRequest) (*runtime.PodSandboxStatusResponse, error) {
	eng, err := s.resolveEngineByPod(req.PodSandboxId)
	if err != nil {
		return nil, err
	}
	return eng.PodSandboxStatus(ctx, req)
}

func (s *MuxServer) ListPodSandbox(ctx context.Context, req *runtime.ListPodSandboxRequest) (*runtime.ListPodSandboxResponse, error) {
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		allItems []*runtime.PodSandbox
		errs     []error
	)

	engines := []engine.RuntimeEngine{s.containerEngine, s.e2bEngine}
	for _, eng := range engines {
		wg.Add(1)
		go func(e engine.RuntimeEngine) {
			defer wg.Done()
			resp, err := e.ListPodSandbox(ctx, req)
			mu.Lock()
			if err != nil {
				errs = append(errs, fmt.Errorf("%s.ListPodSandbox: %w", e.Type(), err))
			} else {
				allItems = append(allItems, resp.Items...)
			}
			mu.Unlock()
		}(eng)
	}

	wg.Wait()
	if len(errs) > 0 && len(allItems) == 0 {
		return nil, fmt.Errorf("all engines failed: %v", errs)
	}
	return &runtime.ListPodSandboxResponse{Items: allItems}, nil
}

func (s *MuxServer) ListContainers(ctx context.Context, req *runtime.ListContainersRequest) (*runtime.ListContainersResponse, error) {
	var (
		wg            sync.WaitGroup
		mu            sync.Mutex
		allContainers []*runtime.Container
		errs          []error
	)

	engines := []engine.RuntimeEngine{s.containerEngine, s.e2bEngine}
	for _, eng := range engines {
		wg.Add(1)
		go func(e engine.RuntimeEngine) {
			defer wg.Done()
			resp, err := e.ListContainers(ctx, req)
			mu.Lock()
			if err != nil {
				errs = append(errs, fmt.Errorf("%s.ListContainers: %w", e.Type(), err))
			} else {
				allContainers = append(allContainers, resp.Containers...)
			}
			mu.Unlock()
		}(eng)
	}

	wg.Wait()
	if len(errs) > 0 && len(allContainers) == 0 {
		return nil, fmt.Errorf("all engines failed: %v", errs)
	}
	return &runtime.ListContainersResponse{Containers: allContainers}, nil
}

func (s *MuxServer) ContainerStats(ctx context.Context, req *runtime.ContainerStatsRequest) (*runtime.ContainerStatsResponse, error) {
	eng, err := s.resolveEngineByContainer(req.ContainerId)
	if err != nil {
		return nil, err
	}
	return eng.ContainerStats(ctx, req)
}

func (s *MuxServer) ListContainerStats(ctx context.Context, req *runtime.ListContainerStatsRequest) (*runtime.ListContainerStatsResponse, error) {
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		allStats []*runtime.ContainerStats
		errs     []error
	)

	engines := []engine.RuntimeEngine{s.containerEngine, s.e2bEngine}
	for _, eng := range engines {
		wg.Add(1)
		go func(e engine.RuntimeEngine) {
			defer wg.Done()
			resp, err := e.ListContainerStats(ctx, req)
			mu.Lock()
			if err != nil {
				errs = append(errs, fmt.Errorf("%s.ListContainerStats: %w", e.Type(), err))
			} else {
				allStats = append(allStats, resp.Stats...)
			}
			mu.Unlock()
		}(eng)
	}

	wg.Wait()
	if len(errs) > 0 && len(allStats) == 0 {
		return nil, fmt.Errorf("all engines failed: %v", errs)
	}
	return &runtime.ListContainerStatsResponse{Stats: allStats}, nil
}

func (s *MuxServer) UpdateContainerResources(ctx context.Context, req *runtime.UpdateContainerResourcesRequest) (*runtime.UpdateContainerResourcesResponse, error) {
	eng, err := s.resolveEngineByContainer(req.ContainerId)
	if err != nil {
		return nil, err
	}
	return eng.UpdateContainerResources(ctx, req)
}

func (s *MuxServer) ReopenContainerLog(ctx context.Context, req *runtime.ReopenContainerLogRequest) (*runtime.ReopenContainerLogResponse, error) {
	eng, err := s.resolveEngineByContainer(req.ContainerId)
	if err != nil {
		return nil, err
	}
	return eng.ReopenContainerLog(ctx, req)
}

func (s *MuxServer) ExecSync(ctx context.Context, req *runtime.ExecSyncRequest) (*runtime.ExecSyncResponse, error) {
	eng, err := s.resolveEngineByContainer(req.ContainerId)
	if err != nil {
		return nil, err
	}
	return eng.ExecSync(ctx, req)
}

func (s *MuxServer) Exec(ctx context.Context, req *runtime.ExecRequest) (*runtime.ExecResponse, error) {
	eng, err := s.resolveEngineByContainer(req.ContainerId)
	if err != nil {
		return nil, err
	}
	return eng.Exec(ctx, req)
}

func (s *MuxServer) Attach(ctx context.Context, req *runtime.AttachRequest) (*runtime.AttachResponse, error) {
	eng, err := s.resolveEngineByContainer(req.ContainerId)
	if err != nil {
		return nil, err
	}
	return eng.Attach(ctx, req)
}

func (s *MuxServer) PortForward(ctx context.Context, req *runtime.PortForwardRequest) (*runtime.PortForwardResponse, error) {
	eng, err := s.resolveEngineByPod(req.PodSandboxId)
	if err != nil {
		return nil, err
	}
	return eng.PortForward(ctx, req)
}

func (s *MuxServer) Status(ctx context.Context, req *runtime.StatusRequest) (*runtime.StatusResponse, error) {
	return s.containerEngine.Status(ctx, req)
}

func (s *MuxServer) Version(ctx context.Context, req *runtime.VersionRequest) (*runtime.VersionResponse, error) {
	return s.containerEngine.Version(ctx, req)
}

func (s *MuxServer) UpdateRuntimeConfig(ctx context.Context, req *runtime.UpdateRuntimeConfigRequest) (*runtime.UpdateRuntimeConfigResponse, error) {
	return s.containerEngine.UpdateRuntimeConfig(ctx, req)
}

func (s *MuxServer) GetContainerEvents(req *runtime.GetEventsRequest, srv runtime.RuntimeService_GetContainerEventsServer) error {
	return fmt.Errorf("GetContainerEvents not supported by multiplexer")
}

// ========== ImageService ==========

func (s *MuxServer) ListImages(ctx context.Context, req *runtime.ListImagesRequest) (*runtime.ListImagesResponse, error) {
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		allImages []*runtime.Image
		errs     []error
	)

	engines := []engine.RuntimeEngine{s.containerEngine, s.e2bEngine}
	for _, eng := range engines {
		wg.Add(1)
		go func(e engine.RuntimeEngine) {
			defer wg.Done()
			resp, err := e.ListImages(ctx, req)
			mu.Lock()
			if err != nil {
				errs = append(errs, fmt.Errorf("%s.ListImages: %w", e.Type(), err))
			} else {
				allImages = append(allImages, resp.Images...)
			}
			mu.Unlock()
		}(eng)
	}

	wg.Wait()
	if len(errs) > 0 && len(allImages) == 0 {
		return nil, fmt.Errorf("all engines failed: %v", errs)
	}
	return &runtime.ListImagesResponse{Images: allImages}, nil
}

func (s *MuxServer) ImageStatus(ctx context.Context, req *runtime.ImageStatusRequest) (*runtime.ImageStatusResponse, error) {
	resp, err := s.containerEngine.ImageStatus(ctx, req)
	if err != nil {
		return s.e2bEngine.ImageStatus(ctx, req)
	}
	return resp, nil
}

func (s *MuxServer) PullImage(ctx context.Context, req *runtime.PullImageRequest) (*runtime.PullImageResponse, error) {
	resp, err := s.containerEngine.PullImage(ctx, req)
	if err != nil {
		return s.e2bEngine.PullImage(ctx, req)
	}
	return resp, nil
}

func (s *MuxServer) RemoveImage(ctx context.Context, req *runtime.RemoveImageRequest) (*runtime.RemoveImageResponse, error) {
	resp, err := s.containerEngine.RemoveImage(ctx, req)
	if err != nil {
		return s.e2bEngine.RemoveImage(ctx, req)
	}
	return resp, nil
}

func (s *MuxServer) ImageFsInfo(ctx context.Context, req *runtime.ImageFsInfoRequest) (*runtime.ImageFsInfoResponse, error) {
	resp, err := s.containerEngine.ImageFsInfo(ctx, req)
	if err != nil {
		return s.e2bEngine.ImageFsInfo(ctx, req)
	}
	return resp, nil
}
