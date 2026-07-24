package engine

import (
	"context"
	"net"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

type fakeCRIServer struct {
	runtime.UnimplementedRuntimeServiceServer
	runtime.UnimplementedImageServiceServer

	calls map[string]int
}

func newFakeCRIServer() *fakeCRIServer {
	return &fakeCRIServer{calls: map[string]int{}}
}

func (s *fakeCRIServer) record(name string) {
	s.calls[name]++
}

func (s *fakeCRIServer) Version(ctx context.Context, req *runtime.VersionRequest) (*runtime.VersionResponse, error) {
	s.record("Version")
	return &runtime.VersionResponse{RuntimeName: "containerd-fake"}, nil
}

func (s *fakeCRIServer) Status(ctx context.Context, req *runtime.StatusRequest) (*runtime.StatusResponse, error) {
	s.record("Status")
	return &runtime.StatusResponse{Status: &runtime.RuntimeStatus{Conditions: []*runtime.RuntimeCondition{{Type: runtime.RuntimeReady, Status: true}}}}, nil
}

func (s *fakeCRIServer) RunPodSandbox(ctx context.Context, req *runtime.RunPodSandboxRequest) (*runtime.RunPodSandboxResponse, error) {
	s.record("RunPodSandbox")
	return &runtime.RunPodSandboxResponse{PodSandboxId: "pod-containerd"}, nil
}

func (s *fakeCRIServer) StopPodSandbox(ctx context.Context, req *runtime.StopPodSandboxRequest) (*runtime.StopPodSandboxResponse, error) {
	s.record("StopPodSandbox")
	return &runtime.StopPodSandboxResponse{}, nil
}

func (s *fakeCRIServer) RemovePodSandbox(ctx context.Context, req *runtime.RemovePodSandboxRequest) (*runtime.RemovePodSandboxResponse, error) {
	s.record("RemovePodSandbox")
	return &runtime.RemovePodSandboxResponse{}, nil
}

func (s *fakeCRIServer) PodSandboxStatus(ctx context.Context, req *runtime.PodSandboxStatusRequest) (*runtime.PodSandboxStatusResponse, error) {
	s.record("PodSandboxStatus")
	return &runtime.PodSandboxStatusResponse{Status: &runtime.PodSandboxStatus{Id: req.PodSandboxId}}, nil
}

func (s *fakeCRIServer) ListPodSandbox(ctx context.Context, req *runtime.ListPodSandboxRequest) (*runtime.ListPodSandboxResponse, error) {
	s.record("ListPodSandbox")
	return &runtime.ListPodSandboxResponse{Items: []*runtime.PodSandbox{{Id: "pod-containerd"}}}, nil
}

func (s *fakeCRIServer) CreateContainer(ctx context.Context, req *runtime.CreateContainerRequest) (*runtime.CreateContainerResponse, error) {
	s.record("CreateContainer")
	return &runtime.CreateContainerResponse{ContainerId: "ctr-containerd"}, nil
}

func (s *fakeCRIServer) StartContainer(ctx context.Context, req *runtime.StartContainerRequest) (*runtime.StartContainerResponse, error) {
	s.record("StartContainer")
	return &runtime.StartContainerResponse{}, nil
}

func (s *fakeCRIServer) StopContainer(ctx context.Context, req *runtime.StopContainerRequest) (*runtime.StopContainerResponse, error) {
	s.record("StopContainer")
	return &runtime.StopContainerResponse{}, nil
}

func (s *fakeCRIServer) RemoveContainer(ctx context.Context, req *runtime.RemoveContainerRequest) (*runtime.RemoveContainerResponse, error) {
	s.record("RemoveContainer")
	return &runtime.RemoveContainerResponse{}, nil
}

func (s *fakeCRIServer) ListContainers(ctx context.Context, req *runtime.ListContainersRequest) (*runtime.ListContainersResponse, error) {
	s.record("ListContainers")
	return &runtime.ListContainersResponse{Containers: []*runtime.Container{{Id: "ctr-containerd"}}}, nil
}

func (s *fakeCRIServer) ContainerStatus(ctx context.Context, req *runtime.ContainerStatusRequest) (*runtime.ContainerStatusResponse, error) {
	s.record("ContainerStatus")
	return &runtime.ContainerStatusResponse{Status: &runtime.ContainerStatus{Id: req.ContainerId}}, nil
}

func (s *fakeCRIServer) ContainerStats(ctx context.Context, req *runtime.ContainerStatsRequest) (*runtime.ContainerStatsResponse, error) {
	s.record("ContainerStats")
	return &runtime.ContainerStatsResponse{Stats: &runtime.ContainerStats{Attributes: &runtime.ContainerAttributes{Id: req.ContainerId}}}, nil
}

func (s *fakeCRIServer) ListContainerStats(ctx context.Context, req *runtime.ListContainerStatsRequest) (*runtime.ListContainerStatsResponse, error) {
	s.record("ListContainerStats")
	return &runtime.ListContainerStatsResponse{Stats: []*runtime.ContainerStats{{Attributes: &runtime.ContainerAttributes{Id: "ctr-containerd"}}}}, nil
}

func (s *fakeCRIServer) UpdateContainerResources(ctx context.Context, req *runtime.UpdateContainerResourcesRequest) (*runtime.UpdateContainerResourcesResponse, error) {
	s.record("UpdateContainerResources")
	return &runtime.UpdateContainerResourcesResponse{}, nil
}

func (s *fakeCRIServer) ReopenContainerLog(ctx context.Context, req *runtime.ReopenContainerLogRequest) (*runtime.ReopenContainerLogResponse, error) {
	s.record("ReopenContainerLog")
	return &runtime.ReopenContainerLogResponse{}, nil
}

func (s *fakeCRIServer) ExecSync(ctx context.Context, req *runtime.ExecSyncRequest) (*runtime.ExecSyncResponse, error) {
	s.record("ExecSync")
	return &runtime.ExecSyncResponse{Stdout: []byte("ok")}, nil
}

func (s *fakeCRIServer) Exec(ctx context.Context, req *runtime.ExecRequest) (*runtime.ExecResponse, error) {
	s.record("Exec")
	return &runtime.ExecResponse{Url: "http://exec"}, nil
}

func (s *fakeCRIServer) Attach(ctx context.Context, req *runtime.AttachRequest) (*runtime.AttachResponse, error) {
	s.record("Attach")
	return &runtime.AttachResponse{Url: "http://attach"}, nil
}

func (s *fakeCRIServer) PortForward(ctx context.Context, req *runtime.PortForwardRequest) (*runtime.PortForwardResponse, error) {
	s.record("PortForward")
	return &runtime.PortForwardResponse{Url: "http://portforward"}, nil
}

func (s *fakeCRIServer) UpdateRuntimeConfig(ctx context.Context, req *runtime.UpdateRuntimeConfigRequest) (*runtime.UpdateRuntimeConfigResponse, error) {
	s.record("UpdateRuntimeConfig")
	return &runtime.UpdateRuntimeConfigResponse{}, nil
}

func (s *fakeCRIServer) ListImages(ctx context.Context, req *runtime.ListImagesRequest) (*runtime.ListImagesResponse, error) {
	s.record("ListImages")
	return &runtime.ListImagesResponse{Images: []*runtime.Image{{Id: "image-containerd"}}}, nil
}

func (s *fakeCRIServer) ImageStatus(ctx context.Context, req *runtime.ImageStatusRequest) (*runtime.ImageStatusResponse, error) {
	s.record("ImageStatus")
	return &runtime.ImageStatusResponse{Image: &runtime.Image{Id: req.Image.Image}}, nil
}

func (s *fakeCRIServer) PullImage(ctx context.Context, req *runtime.PullImageRequest) (*runtime.PullImageResponse, error) {
	s.record("PullImage")
	return &runtime.PullImageResponse{ImageRef: req.Image.Image + "@sha256:fake"}, nil
}

func (s *fakeCRIServer) RemoveImage(ctx context.Context, req *runtime.RemoveImageRequest) (*runtime.RemoveImageResponse, error) {
	s.record("RemoveImage")
	return &runtime.RemoveImageResponse{}, nil
}

func (s *fakeCRIServer) ImageFsInfo(ctx context.Context, req *runtime.ImageFsInfoRequest) (*runtime.ImageFsInfoResponse, error) {
	s.record("ImageFsInfo")
	return &runtime.ImageFsInfoResponse{ImageFilesystems: []*runtime.FilesystemUsage{{Timestamp: 123}}}, nil
}

func startFakeContainerd(t *testing.T) (socketPath string, fake *fakeCRIServer) {
	t.Helper()
	socketPath = filepath.Join(t.TempDir(), "containerd.sock")
	lis, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	grpcServer := grpc.NewServer()
	fake = newFakeCRIServer()
	runtime.RegisterRuntimeServiceServer(grpcServer, fake)
	runtime.RegisterImageServiceServer(grpcServer, fake)
	go func() {
		_ = grpcServer.Serve(lis)
	}()
	t.Cleanup(func() {
		grpcServer.Stop()
		_ = lis.Close()
	})
	return socketPath, fake
}

func TestContainerEngineDelegatesRuntimeAndImageServices(t *testing.T) {
	socketPath, fake := startFakeContainerd(t)
	e := NewContainerEngine(socketPath)
	t.Cleanup(func() { _ = e.Close() })
	ctx := context.Background()

	runtimeCalls := []struct {
		name string
		call func() error
	}{
		{"Version", func() error { _, err := e.Version(ctx, &runtime.VersionRequest{}); return err }},
		{"Status", func() error { _, err := e.Status(ctx, &runtime.StatusRequest{}); return err }},
		{"RunPodSandbox", func() error { _, err := e.RunPodSandbox(ctx, &runtime.RunPodSandboxRequest{}); return err }},
		{"CreateContainer", func() error { _, err := e.CreateContainer(ctx, &runtime.CreateContainerRequest{}); return err }},
		{"StartContainer", func() error { _, err := e.StartContainer(ctx, &runtime.StartContainerRequest{}); return err }},
		{"StopContainer", func() error { _, err := e.StopContainer(ctx, &runtime.StopContainerRequest{}); return err }},
		{"RemoveContainer", func() error { _, err := e.RemoveContainer(ctx, &runtime.RemoveContainerRequest{}); return err }},
		{"ListContainers", func() error { _, err := e.ListContainers(ctx, &runtime.ListContainersRequest{}); return err }},
		{"ContainerStatus", func() error {
			_, err := e.ContainerStatus(ctx, &runtime.ContainerStatusRequest{ContainerId: "ctr"})
			return err
		}},
		{"ContainerStats", func() error {
			_, err := e.ContainerStats(ctx, &runtime.ContainerStatsRequest{ContainerId: "ctr"})
			return err
		}},
		{"ListContainerStats", func() error { _, err := e.ListContainerStats(ctx, &runtime.ListContainerStatsRequest{}); return err }},
		{"StopPodSandbox", func() error { _, err := e.StopPodSandbox(ctx, &runtime.StopPodSandboxRequest{}); return err }},
		{"RemovePodSandbox", func() error { _, err := e.RemovePodSandbox(ctx, &runtime.RemovePodSandboxRequest{}); return err }},
		{"PodSandboxStatus", func() error {
			_, err := e.PodSandboxStatus(ctx, &runtime.PodSandboxStatusRequest{PodSandboxId: "pod"})
			return err
		}},
		{"ListPodSandbox", func() error { _, err := e.ListPodSandbox(ctx, &runtime.ListPodSandboxRequest{}); return err }},
		{"UpdateContainerResources", func() error {
			_, err := e.UpdateContainerResources(ctx, &runtime.UpdateContainerResourcesRequest{})
			return err
		}},
		{"ReopenContainerLog", func() error { _, err := e.ReopenContainerLog(ctx, &runtime.ReopenContainerLogRequest{}); return err }},
		{"ExecSync", func() error { _, err := e.ExecSync(ctx, &runtime.ExecSyncRequest{}); return err }},
		{"Exec", func() error { _, err := e.Exec(ctx, &runtime.ExecRequest{}); return err }},
		{"Attach", func() error { _, err := e.Attach(ctx, &runtime.AttachRequest{}); return err }},
		{"PortForward", func() error { _, err := e.PortForward(ctx, &runtime.PortForwardRequest{}); return err }},
		{"UpdateRuntimeConfig", func() error { _, err := e.UpdateRuntimeConfig(ctx, &runtime.UpdateRuntimeConfigRequest{}); return err }},
	}
	for _, tc := range runtimeCalls {
		if err := tc.call(); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if fake.calls[tc.name] != 1 {
			t.Fatalf("%s fake call count = %d, want 1", tc.name, fake.calls[tc.name])
		}
	}

	imageCalls := []struct {
		name string
		call func() error
	}{
		{"ListImages", func() error { _, err := e.ListImages(ctx, &runtime.ListImagesRequest{}); return err }},
		{"ImageStatus", func() error {
			_, err := e.ImageStatus(ctx, &runtime.ImageStatusRequest{Image: &runtime.ImageSpec{Image: "busybox"}})
			return err
		}},
		{"PullImage", func() error {
			_, err := e.PullImage(ctx, &runtime.PullImageRequest{Image: &runtime.ImageSpec{Image: "busybox"}})
			return err
		}},
		{"RemoveImage", func() error {
			_, err := e.RemoveImage(ctx, &runtime.RemoveImageRequest{Image: &runtime.ImageSpec{Image: "busybox"}})
			return err
		}},
		{"ImageFsInfo", func() error { _, err := e.ImageFsInfo(ctx, &runtime.ImageFsInfoRequest{}); return err }},
	}
	for _, tc := range imageCalls {
		if err := tc.call(); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if fake.calls[tc.name] != 1 {
			t.Fatalf("%s fake call count = %d, want 1", tc.name, fake.calls[tc.name])
		}
	}
}

func TestContainerEngineCloseWithoutConnection(t *testing.T) {
	e := NewContainerEngine(filepath.Join(t.TempDir(), "missing.sock"))
	if e.Type() != EngineTypeContainer {
		t.Fatalf("Type = %s, want container", e.Type())
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close without connection: %v", err)
	}
}
