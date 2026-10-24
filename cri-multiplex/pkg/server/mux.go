package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/cri-multiplex/pkg/engine"
)

// UnknownHandlerPolicy 决定 RunPodSandbox 收到未注册 runtime handler 时的行为。
type UnknownHandlerPolicy string

const (
	// UnknownHandlerPassthrough 回退到默认后端（保持历史行为：kubelet 用 "runc" 等标准 handler）。
	UnknownHandlerPassthrough UnknownHandlerPolicy = "passthrough"
	// UnknownHandlerReject 直接拒绝，避免把未知后端的负载悄悄落到默认后端。
	UnknownHandlerReject UnknownHandlerPolicy = "reject"
)

// MuxOptions 收敛 Mux 的启动参数，避免构造函数参数列表继续膨胀。
type MuxOptions struct {
	UnknownHandlerPolicy UnknownHandlerPolicy
	RuntimeName          string
	Cleanup              engine.CleanupScheduler
}

const defaultRuntimeName = "e2b-cri-shim"

type MuxServer struct {
	runtime.UnimplementedRuntimeServiceServer
	runtime.UnimplementedImageServiceServer

	reg    *engine.Registry
	routes *engine.RouteTable
	opts   MuxOptions
	// store 只服务 cleanupPending 守卫，Task 7 之后删除。
	store engine.StateStore

	grpcServer *grpc.Server
}

// NewMuxServer 用注册表装配 Mux：之后所有路由都由 Registry 与 RouteTable 决定。
func NewMuxServer(reg *engine.Registry, store engine.StateStore, opts MuxOptions) *MuxServer {
	return &MuxServer{
		reg:    reg,
		routes: engine.NewRouteTable(store),
		opts:   opts,
		store:  store,
	}
}

// NewMuxServerLegacy 保留旧的三个引擎签名，供过渡期与测试使用。
func NewMuxServerLegacy(container, e2b, android engine.RuntimeEngine, stores ...engine.StateStore) *MuxServer {
	var store engine.StateStore
	if len(stores) > 0 {
		store = stores[0]
	}

	reg := engine.NewRegistry(engine.EngineTypeContainer)
	for _, mod := range []engine.BackendModule{
		{
			Descriptor: engine.BackendDescriptor{Type: engine.EngineTypeContainer, Handler: string(engine.EngineTypeContainer)},
			Engine:     container,
		},
		{
			Descriptor: engine.BackendDescriptor{Type: engine.EngineTypeE2B, Handler: string(engine.EngineTypeE2B), ImagePrefixes: []string{"e2b.dev/"}},
			Engine:     e2b,
		},
		{
			Descriptor: engine.BackendDescriptor{Type: engine.EngineTypeAndroid, Handler: string(engine.EngineTypeAndroid), ImagePrefixes: []string{"android.dev/"}},
			Engine:     android,
		},
	} {
		if mod.Engine == nil {
			continue
		}
		if cleaner, ok := mod.Engine.(engine.Cleaner); ok {
			mod.Cleaner = cleaner
		}
		if reconciler, ok := mod.Engine.(engine.Reconciler); ok {
			mod.Reconciler = reconciler
		}
		if restorer, ok := mod.Engine.(engine.StateRestorer); ok {
			mod.Restorer = restorer
		}
		if health, ok := mod.Engine.(engine.HealthChecker); ok {
			mod.Health = health
		}
		if err := reg.Register(mod); err != nil {
			log.Printf("[MuxServer] WARNING: register legacy backend %s: %v", mod.Descriptor.Type, err)
		}
	}
	if err := reg.Freeze(); err != nil {
		log.Printf("[MuxServer] WARNING: freeze legacy registry: %v", err)
	}

	return NewMuxServer(reg, store, MuxOptions{})
}

// Routes 暴露路由表给 main 与 Admin v2 使用。
func (s *MuxServer) Routes() *engine.RouteTable {
	return s.routes
}

// RestoreState 先让各后端恢复自身状态（含路由派生），再加载路由表。
func (s *MuxServer) RestoreState(ctx context.Context) error {
	for _, mod := range s.reg.Modules() {
		if mod.Restorer == nil {
			continue
		}
		if err := mod.Restorer.RestoreState(ctx); err != nil {
			return fmt.Errorf("%s restore state: %w", mod.Descriptor.Type, err)
		}
	}
	if err := s.routes.Restore(); err != nil {
		return fmt.Errorf("load routes: %w", err)
	}

	return nil
}

func (s *MuxServer) cleanupPending(sandboxID string) bool {
	if s == nil || s.store == nil || sandboxID == "" {
		return false
	}
	tasks, err := s.store.LoadCleanupTasks()
	if err != nil {
		log.Printf("[MuxServer] WARNING: load cleanup tasks for sandbox=%s: %v", sandboxID, err)
		return true
	}
	for _, task := range tasks {
		if task.SandboxID == sandboxID {
			return true
		}
	}
	return false
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

// defaultEngine 返回默认后端；没有默认后端时返回 Internal。
func (s *MuxServer) defaultEngine() (engine.RuntimeEngine, error) {
	mod := s.reg.Default()
	if mod == nil {
		return nil, status.Error(codes.Internal, "no default backend registered")
	}

	return mod.Engine, nil
}

// engineByType 按类型取后端；类型未注册时回退默认后端。
func (s *MuxServer) engineByType(typ engine.EngineType) (engine.RuntimeEngine, error) {
	if mod, ok := s.reg.Get(typ); ok {
		return mod.Engine, nil
	}
	log.Printf("[MuxServer] backend %s not registered, fallback to default backend", typ)

	return s.defaultEngine()
}

// resolveEngineByPod 按路由表查找 Pod 所属 engine；
// 路由表未命中时回退到默认后端（containerd）。
// 原因：E2B/Android Pod 只能通过 cri-multiplex 的 RunPodSandbox 创建，
// 创建时会注册到路由表；未注册的 Pod 一定是 containerd 既有的，
// 因此回退到默认后端是安全的，避免 kubelet 切换后对既有 Pod 调用
// PodSandboxStatus/StopPodSandbox 等接口报 "no engine found"。
func (s *MuxServer) resolveEngineByPod(podSandboxID string) (engine.RuntimeEngine, error) {
	typ, ok := s.routes.PodBackend(podSandboxID)
	if !ok {
		log.Printf("[MuxServer] pod %s not in route table, fallback to default backend", podSandboxID)
		return s.defaultEngine()
	}

	return s.engineByType(typ)
}

// resolveEngineByContainer 按路由表查找 Container 所属 engine；未命中同样回退默认后端。
func (s *MuxServer) resolveEngineByContainer(containerID string) (engine.RuntimeEngine, error) {
	typ, ok := s.routes.ContainerBackend(containerID)
	if !ok {
		log.Printf("[MuxServer] container %s not in route table, fallback to default backend", containerID)
		return s.defaultEngine()
	}

	return s.engineByType(typ)
}

func (s *MuxServer) scheduleCleanup(ctx context.Context, typ engine.EngineType, sandboxID, resourceType, reason string) {
	if s.opts.Cleanup == nil {
		return
	}
	result := s.opts.Cleanup.Schedule(ctx, typ, sandboxID, resourceType, reason)
	if result.Err != nil {
		log.Printf("[MuxServer] WARNING: schedule cleanup failed sandbox=%s backend=%s resource=%s reason=%s: %v",
			sandboxID, typ, resourceType, reason, result.Err)
	}
}

// ========== RuntimeService ==========

func (s *MuxServer) RunPodSandbox(ctx context.Context, req *runtime.RunPodSandboxRequest) (*runtime.RunPodSandboxResponse, error) {
	handler := req.GetRuntimeHandler()

	mod, ok := s.reg.ResolveHandler(handler)
	if !ok {
		if s.opts.UnknownHandlerPolicy == UnknownHandlerReject {
			return nil, status.Errorf(codes.InvalidArgument, "unknown runtime handler %q", handler)
		}
		mod = s.reg.Default()
		if mod == nil {
			return nil, status.Error(codes.Internal, "no default backend registered")
		}
	}

	eng := mod.Engine
	log.Printf("[MuxServer] RunPodSandbox routing: handler=%q -> %s", handler, eng.Type())

	resp, err := eng.RunPodSandbox(ctx, req)
	if err != nil {
		return nil, err
	}

	routePersistStart := time.Now()
	if err := s.routes.SetPod(resp.PodSandboxId, eng.Type()); err != nil {
		log.Printf("[MuxServer] ERROR: persist pod route failed sandbox=%s engine=%s: %v", resp.PodSandboxId, eng.Type(), err)
		s.scheduleCleanup(ctx, eng.Type(), resp.PodSandboxId, "route", "persist-route-failed")
		return nil, status.Errorf(codes.Internal, "persist route for pod sandbox %s: %v", resp.PodSandboxId, err)
	}
	log.Printf("[PerfTrace] sandbox=%s route_persist_ms=%d", resp.PodSandboxId, time.Since(routePersistStart).Milliseconds())
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

	if err := s.routes.SetContainer(resp.ContainerId, req.PodSandboxId, eng.Type()); err != nil {
		log.Printf("[MuxServer] ERROR: persist container route failed container=%s engine=%s: %v", resp.ContainerId, eng.Type(), err)
		s.scheduleCleanup(ctx, eng.Type(), req.PodSandboxId, "route", "persist-route-failed")
		return nil, status.Errorf(codes.Internal, "persist route for container %s: %v", resp.ContainerId, err)
	}
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
		if derr := s.routes.DeleteContainer(req.ContainerId); derr != nil {
			log.Printf("[MuxServer] WARNING: delete route for container %s: %v", req.ContainerId, derr)
		}
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
	_, routed := s.routes.PodBackend(req.PodSandboxId)
	eng, err := s.resolveEngineByPod(req.PodSandboxId)
	if err != nil {
		return nil, err
	}

	resp, err := eng.RemovePodSandbox(ctx, req)
	if !routed && status.Code(err) == codes.NotFound {
		log.Printf("[MuxServer] RemovePodSandbox: sandbox %s already absent, treating as success", req.PodSandboxId)
		return &runtime.RemovePodSandboxResponse{}, nil
	}
	if err == nil {
		if s.cleanupPending(req.PodSandboxId) {
			log.Printf("[MuxServer] retaining routes while cleanup is pending sandbox=%s", req.PodSandboxId)
		} else if derr := s.routes.DeleteSandbox(req.PodSandboxId); derr != nil {
			log.Printf("[MuxServer] WARNING: delete routes for sandbox %s: %v", req.PodSandboxId, derr)
		}
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

	for _, mod := range s.reg.Modules() {
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
		}(mod.Engine)
	}

	wg.Wait()
	if len(errs) > 0 && len(allItems) == 0 {
		return nil, fmt.Errorf("all engines failed: %w", errors.Join(errs...))
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

	for _, mod := range s.reg.Modules() {
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
		}(mod.Engine)
	}

	wg.Wait()
	if len(errs) > 0 && len(allContainers) == 0 {
		return nil, fmt.Errorf("all engines failed: %w", errors.Join(errs...))
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

	for _, mod := range s.reg.Modules() {
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
		}(mod.Engine)
	}

	wg.Wait()
	if len(errs) > 0 && len(allStats) == 0 {
		return nil, fmt.Errorf("all engines failed: %w", errors.Join(errs...))
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

func (s *MuxServer) Version(ctx context.Context, req *runtime.VersionRequest) (*runtime.VersionResponse, error) {
	runtimeName := s.opts.RuntimeName
	if runtimeName == "" {
		runtimeName = defaultRuntimeName
	}

	return &runtime.VersionResponse{
		Version:           "0.1.0",
		RuntimeName:       runtimeName,
		RuntimeVersion:    "0.1.0",
		RuntimeApiVersion: "v1",
	}, nil
}

func (s *MuxServer) Status(ctx context.Context, req *runtime.StatusRequest) (*runtime.StatusResponse, error) {
	for _, mod := range s.reg.Modules() {
		if mod.Health == nil {
			continue
		}
		healthCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := mod.Health.Health(healthCtx)
		cancel()
		if err != nil {
			log.Printf("[MuxServer] WARNING: backend %s health check failed: %v", mod.Descriptor.Type, err)
		}
	}

	return &runtime.StatusResponse{
		Status: &runtime.RuntimeStatus{
			Conditions: []*runtime.RuntimeCondition{
				{Type: runtime.RuntimeReady, Status: true},
				{Type: runtime.NetworkReady, Status: true},
			},
		},
	}, nil
}

func (s *MuxServer) UpdateRuntimeConfig(ctx context.Context, req *runtime.UpdateRuntimeConfigRequest) (*runtime.UpdateRuntimeConfigResponse, error) {
	return &runtime.UpdateRuntimeConfigResponse{}, nil
}

func (s *MuxServer) GetContainerEvents(req *runtime.GetEventsRequest, srv runtime.RuntimeService_GetContainerEventsServer) error {
	return fmt.Errorf("GetContainerEvents not supported by multiplexer")
}

// ========== ImageService：按注册的前缀路由 ==========

func (s *MuxServer) ListImages(ctx context.Context, req *runtime.ListImagesRequest) (*runtime.ListImagesResponse, error) {
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		allImages []*runtime.Image
		errs      []error
	)

	for _, mod := range s.reg.Modules() {
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
		}(mod.Engine)
	}

	wg.Wait()
	if len(errs) > 0 && len(allImages) == 0 {
		return nil, fmt.Errorf("all engines failed: %w", errors.Join(errs...))
	}
	return &runtime.ListImagesResponse{Images: allImages}, nil
}

// imageEngine 按镜像前缀解析后端；未命中走默认后端。
func (s *MuxServer) imageEngine(image string) (engine.RuntimeEngine, error) {
	if mod, ok := s.reg.ResolveImage(image); ok {
		return mod.Engine, nil
	}

	return s.defaultEngine()
}

func (s *MuxServer) ImageStatus(ctx context.Context, req *runtime.ImageStatusRequest) (*runtime.ImageStatusResponse, error) {
	eng, err := s.imageEngine(req.GetImage().GetImage())
	if err != nil {
		return nil, err
	}

	return eng.ImageStatus(ctx, req)
}

func (s *MuxServer) PullImage(ctx context.Context, req *runtime.PullImageRequest) (*runtime.PullImageResponse, error) {
	eng, err := s.imageEngine(req.GetImage().GetImage())
	if err != nil {
		return nil, err
	}

	return eng.PullImage(ctx, req)
}

func (s *MuxServer) RemoveImage(ctx context.Context, req *runtime.RemoveImageRequest) (*runtime.RemoveImageResponse, error) {
	eng, err := s.imageEngine(req.GetImage().GetImage())
	if err != nil {
		return nil, err
	}

	return eng.RemoveImage(ctx, req)
}

func (s *MuxServer) ImageFsInfo(ctx context.Context, req *runtime.ImageFsInfoRequest) (*runtime.ImageFsInfoResponse, error) {
	var errs []error
	tried := make(map[engine.EngineType]struct{}, len(s.reg.Modules()))

	if def := s.reg.Default(); def != nil {
		tried[def.Descriptor.Type] = struct{}{}
		resp, err := def.Engine.ImageFsInfo(ctx, req)
		if err == nil {
			return resp, nil
		}
		errs = append(errs, fmt.Errorf("%s.ImageFsInfo: %w", def.Descriptor.Type, err))
	}

	for _, mod := range s.reg.Modules() {
		if _, ok := tried[mod.Descriptor.Type]; ok {
			continue
		}
		resp, err := mod.Engine.ImageFsInfo(ctx, req)
		if err == nil {
			return resp, nil
		}
		errs = append(errs, fmt.Errorf("%s.ImageFsInfo: %w", mod.Descriptor.Type, err))
	}

	return nil, fmt.Errorf("all engines failed: %w", errors.Join(errs...))
}
