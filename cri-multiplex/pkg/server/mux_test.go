package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/cri-multiplex/pkg/engine"
)

type fakeRuntimeEngine struct {
	typ engine.EngineType

	err error

	podID       string
	containerID string
	calls       map[string]int

	listPods       []*runtime.PodSandbox
	listContainers []*runtime.Container
	listStats      []*runtime.ContainerStats
	listImages     []*runtime.Image
	imageFsInfo    *runtime.ImageFsInfoResponse
}

func newFakeEngine(typ engine.EngineType) *fakeRuntimeEngine {
	return &fakeRuntimeEngine{typ: typ, calls: map[string]int{}}
}

func (f *fakeRuntimeEngine) record(name string) {
	f.calls[name]++
}

func (f *fakeRuntimeEngine) Type() engine.EngineType { return f.typ }

func (f *fakeRuntimeEngine) RunPodSandbox(ctx context.Context, req *runtime.RunPodSandboxRequest) (*runtime.RunPodSandboxResponse, error) {
	f.record("RunPodSandbox")
	if f.err != nil {
		return nil, f.err
	}
	if f.podID == "" {
		f.podID = string(f.typ) + "-pod"
	}
	return &runtime.RunPodSandboxResponse{PodSandboxId: f.podID}, nil
}

func (f *fakeRuntimeEngine) StopPodSandbox(ctx context.Context, req *runtime.StopPodSandboxRequest) (*runtime.StopPodSandboxResponse, error) {
	f.record("StopPodSandbox")
	return &runtime.StopPodSandboxResponse{}, f.err
}

func (f *fakeRuntimeEngine) RemovePodSandbox(ctx context.Context, req *runtime.RemovePodSandboxRequest) (*runtime.RemovePodSandboxResponse, error) {
	f.record("RemovePodSandbox")
	return &runtime.RemovePodSandboxResponse{}, f.err
}

func (f *fakeRuntimeEngine) PodSandboxStatus(ctx context.Context, req *runtime.PodSandboxStatusRequest) (*runtime.PodSandboxStatusResponse, error) {
	f.record("PodSandboxStatus")
	return &runtime.PodSandboxStatusResponse{Status: &runtime.PodSandboxStatus{Id: req.PodSandboxId}}, f.err
}

func (f *fakeRuntimeEngine) ListPodSandbox(ctx context.Context, req *runtime.ListPodSandboxRequest) (*runtime.ListPodSandboxResponse, error) {
	f.record("ListPodSandbox")
	if f.err != nil {
		return nil, f.err
	}
	return &runtime.ListPodSandboxResponse{Items: f.listPods}, nil
}

func (f *fakeRuntimeEngine) CreateContainer(ctx context.Context, req *runtime.CreateContainerRequest) (*runtime.CreateContainerResponse, error) {
	f.record("CreateContainer")
	if f.err != nil {
		return nil, f.err
	}
	if f.containerID == "" {
		f.containerID = string(f.typ) + "-container"
	}
	return &runtime.CreateContainerResponse{ContainerId: f.containerID}, nil
}

func (f *fakeRuntimeEngine) StartContainer(ctx context.Context, req *runtime.StartContainerRequest) (*runtime.StartContainerResponse, error) {
	f.record("StartContainer")
	return &runtime.StartContainerResponse{}, f.err
}

func (f *fakeRuntimeEngine) StopContainer(ctx context.Context, req *runtime.StopContainerRequest) (*runtime.StopContainerResponse, error) {
	f.record("StopContainer")
	return &runtime.StopContainerResponse{}, f.err
}

func (f *fakeRuntimeEngine) RemoveContainer(ctx context.Context, req *runtime.RemoveContainerRequest) (*runtime.RemoveContainerResponse, error) {
	f.record("RemoveContainer")
	return &runtime.RemoveContainerResponse{}, f.err
}

func (f *fakeRuntimeEngine) ListContainers(ctx context.Context, req *runtime.ListContainersRequest) (*runtime.ListContainersResponse, error) {
	f.record("ListContainers")
	if f.err != nil {
		return nil, f.err
	}
	return &runtime.ListContainersResponse{Containers: f.listContainers}, nil
}

func (f *fakeRuntimeEngine) ContainerStatus(ctx context.Context, req *runtime.ContainerStatusRequest) (*runtime.ContainerStatusResponse, error) {
	f.record("ContainerStatus")
	return &runtime.ContainerStatusResponse{Status: &runtime.ContainerStatus{Id: req.ContainerId}}, f.err
}

func (f *fakeRuntimeEngine) ContainerStats(ctx context.Context, req *runtime.ContainerStatsRequest) (*runtime.ContainerStatsResponse, error) {
	f.record("ContainerStats")
	return &runtime.ContainerStatsResponse{Stats: &runtime.ContainerStats{}}, f.err
}

func (f *fakeRuntimeEngine) ListContainerStats(ctx context.Context, req *runtime.ListContainerStatsRequest) (*runtime.ListContainerStatsResponse, error) {
	f.record("ListContainerStats")
	if f.err != nil {
		return nil, f.err
	}
	return &runtime.ListContainerStatsResponse{Stats: f.listStats}, nil
}

func (f *fakeRuntimeEngine) UpdateContainerResources(ctx context.Context, req *runtime.UpdateContainerResourcesRequest) (*runtime.UpdateContainerResourcesResponse, error) {
	f.record("UpdateContainerResources")
	return &runtime.UpdateContainerResourcesResponse{}, f.err
}

func (f *fakeRuntimeEngine) ReopenContainerLog(ctx context.Context, req *runtime.ReopenContainerLogRequest) (*runtime.ReopenContainerLogResponse, error) {
	f.record("ReopenContainerLog")
	return &runtime.ReopenContainerLogResponse{}, f.err
}

func (f *fakeRuntimeEngine) ExecSync(ctx context.Context, req *runtime.ExecSyncRequest) (*runtime.ExecSyncResponse, error) {
	f.record("ExecSync")
	return &runtime.ExecSyncResponse{}, f.err
}

func (f *fakeRuntimeEngine) Exec(ctx context.Context, req *runtime.ExecRequest) (*runtime.ExecResponse, error) {
	f.record("Exec")
	return &runtime.ExecResponse{}, f.err
}

func (f *fakeRuntimeEngine) Attach(ctx context.Context, req *runtime.AttachRequest) (*runtime.AttachResponse, error) {
	f.record("Attach")
	return &runtime.AttachResponse{}, f.err
}

func (f *fakeRuntimeEngine) PortForward(ctx context.Context, req *runtime.PortForwardRequest) (*runtime.PortForwardResponse, error) {
	f.record("PortForward")
	return &runtime.PortForwardResponse{}, f.err
}

func (f *fakeRuntimeEngine) Status(ctx context.Context, req *runtime.StatusRequest) (*runtime.StatusResponse, error) {
	f.record("Status")
	return &runtime.StatusResponse{}, f.err
}

func (f *fakeRuntimeEngine) Version(ctx context.Context, req *runtime.VersionRequest) (*runtime.VersionResponse, error) {
	f.record("Version")
	return &runtime.VersionResponse{}, f.err
}

func (f *fakeRuntimeEngine) UpdateRuntimeConfig(ctx context.Context, req *runtime.UpdateRuntimeConfigRequest) (*runtime.UpdateRuntimeConfigResponse, error) {
	f.record("UpdateRuntimeConfig")
	return &runtime.UpdateRuntimeConfigResponse{}, f.err
}

func (f *fakeRuntimeEngine) GetContainerEvents(ctx context.Context, req *runtime.GetEventsRequest, opts ...grpc.CallOption) (runtime.RuntimeService_GetContainerEventsClient, error) {
	f.record("GetContainerEvents")
	return nil, f.err
}

func (f *fakeRuntimeEngine) ListImages(ctx context.Context, req *runtime.ListImagesRequest) (*runtime.ListImagesResponse, error) {
	f.record("ListImages")
	if f.err != nil {
		return nil, f.err
	}
	return &runtime.ListImagesResponse{Images: f.listImages}, nil
}

func (f *fakeRuntimeEngine) ImageStatus(ctx context.Context, req *runtime.ImageStatusRequest) (*runtime.ImageStatusResponse, error) {
	f.record("ImageStatus")
	return &runtime.ImageStatusResponse{Image: &runtime.Image{Id: string(f.typ)}}, f.err
}

func (f *fakeRuntimeEngine) PullImage(ctx context.Context, req *runtime.PullImageRequest) (*runtime.PullImageResponse, error) {
	f.record("PullImage")
	return &runtime.PullImageResponse{ImageRef: string(f.typ)}, f.err
}

func (f *fakeRuntimeEngine) RemoveImage(ctx context.Context, req *runtime.RemoveImageRequest) (*runtime.RemoveImageResponse, error) {
	f.record("RemoveImage")
	return &runtime.RemoveImageResponse{}, f.err
}

func (f *fakeRuntimeEngine) ImageFsInfo(ctx context.Context, req *runtime.ImageFsInfoRequest) (*runtime.ImageFsInfoResponse, error) {
	f.record("ImageFsInfo")
	if f.err != nil {
		return nil, f.err
	}
	if f.imageFsInfo != nil {
		return f.imageFsInfo, nil
	}
	return &runtime.ImageFsInfoResponse{}, nil
}

func (f *fakeRuntimeEngine) Close() error {
	f.record("Close")
	return nil
}

func TestMuxRunPodSandboxRoutesByRuntimeHandler(t *testing.T) {
	container := newFakeEngine(engine.EngineTypeContainer)
	e2b := newFakeEngine(engine.EngineTypeE2B)
	android := newFakeEngine(engine.EngineTypeAndroid)
	s := NewMuxServer(container, e2b, android)

	cases := []struct {
		handler string
		want    *fakeRuntimeEngine
	}{
		{handler: "e2b", want: e2b},
		{handler: "android", want: android},
		{handler: "", want: container},
		{handler: "runc", want: container},
	}
	for _, tc := range cases {
		resp, err := s.RunPodSandbox(context.Background(), &runtime.RunPodSandboxRequest{
			RuntimeHandler: tc.handler,
			Config:         &runtime.PodSandboxConfig{Metadata: &runtime.PodSandboxMetadata{Name: "pod"}},
		})
		if err != nil {
			t.Fatalf("RunPodSandbox(%q): %v", tc.handler, err)
		}
		if tc.want.calls["RunPodSandbox"] == 0 {
			t.Fatalf("handler %q did not call expected engine %s", tc.handler, tc.want.typ)
		}
		if val, ok := s.podRoutes.Load(resp.PodSandboxId); !ok || val != tc.want.typ {
			t.Fatalf("pod route = (%v, %v), want %s", val, ok, tc.want.typ)
		}
	}
}

func TestMuxContainerRouteLifecycle(t *testing.T) {
	container := newFakeEngine(engine.EngineTypeContainer)
	e2b := newFakeEngine(engine.EngineTypeE2B)
	android := newFakeEngine(engine.EngineTypeAndroid)
	s := NewMuxServer(container, e2b, android)
	s.podRoutes.Store("pod-e2b", engine.EngineTypeE2B)

	createResp, err := s.CreateContainer(context.Background(), &runtime.CreateContainerRequest{PodSandboxId: "pod-e2b"})
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if e2b.calls["CreateContainer"] != 1 {
		t.Fatalf("CreateContainer calls: e2b=%d", e2b.calls["CreateContainer"])
	}
	if val, ok := s.containerRoutes.Load(createResp.ContainerId); !ok || val != engine.EngineTypeE2B {
		t.Fatalf("container route = (%v, %v)", val, ok)
	}

	if _, err := s.StartContainer(context.Background(), &runtime.StartContainerRequest{ContainerId: createResp.ContainerId}); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	if e2b.calls["StartContainer"] != 1 {
		t.Fatalf("StartContainer should route to e2b, calls=%d", e2b.calls["StartContainer"])
	}

	if _, err := s.RemoveContainer(context.Background(), &runtime.RemoveContainerRequest{ContainerId: createResp.ContainerId}); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
	if _, ok := s.containerRoutes.Load(createResp.ContainerId); ok {
		t.Fatal("container route should be removed after successful RemoveContainer")
	}
}

func TestMuxRouteMissFallsBackToContainer(t *testing.T) {
	container := newFakeEngine(engine.EngineTypeContainer)
	s := NewMuxServer(container, newFakeEngine(engine.EngineTypeE2B), newFakeEngine(engine.EngineTypeAndroid))

	if _, err := s.PodSandboxStatus(context.Background(), &runtime.PodSandboxStatusRequest{PodSandboxId: "unknown"}); err != nil {
		t.Fatalf("PodSandboxStatus fallback: %v", err)
	}
	if _, err := s.Exec(context.Background(), &runtime.ExecRequest{ContainerId: "unknown"}); err != nil {
		t.Fatalf("Exec fallback: %v", err)
	}
	if container.calls["PodSandboxStatus"] != 1 || container.calls["Exec"] != 1 {
		t.Fatalf("container fallback calls mismatch: %v", container.calls)
	}
}

func TestMuxDelegatesPodContainerAndStreamingMethods(t *testing.T) {
	container := newFakeEngine(engine.EngineTypeContainer)
	e2b := newFakeEngine(engine.EngineTypeE2B)
	android := newFakeEngine(engine.EngineTypeAndroid)
	s := NewMuxServer(container, e2b, android)
	s.podRoutes.Store("pod-android", engine.EngineTypeAndroid)
	s.containerRoutes.Store("ctr-android", engine.EngineTypeAndroid)

	ctx := context.Background()
	containerMethods := []struct {
		name string
		call func() error
	}{
		{"StopContainer", func() error {
			_, err := s.StopContainer(ctx, &runtime.StopContainerRequest{ContainerId: "ctr-android"})
			return err
		}},
		{"ContainerStatus", func() error {
			_, err := s.ContainerStatus(ctx, &runtime.ContainerStatusRequest{ContainerId: "ctr-android"})
			return err
		}},
		{"ContainerStats", func() error {
			_, err := s.ContainerStats(ctx, &runtime.ContainerStatsRequest{ContainerId: "ctr-android"})
			return err
		}},
		{"UpdateContainerResources", func() error {
			_, err := s.UpdateContainerResources(ctx, &runtime.UpdateContainerResourcesRequest{ContainerId: "ctr-android"})
			return err
		}},
		{"ReopenContainerLog", func() error {
			_, err := s.ReopenContainerLog(ctx, &runtime.ReopenContainerLogRequest{ContainerId: "ctr-android"})
			return err
		}},
		{"ExecSync", func() error {
			_, err := s.ExecSync(ctx, &runtime.ExecSyncRequest{ContainerId: "ctr-android"})
			return err
		}},
		{"Attach", func() error { _, err := s.Attach(ctx, &runtime.AttachRequest{ContainerId: "ctr-android"}); return err }},
	}
	for _, tc := range containerMethods {
		if err := tc.call(); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if android.calls[tc.name] != 1 {
			t.Fatalf("%s calls on android = %d, want 1", tc.name, android.calls[tc.name])
		}
	}

	podMethods := []struct {
		name string
		call func() error
	}{
		{"StopPodSandbox", func() error {
			_, err := s.StopPodSandbox(ctx, &runtime.StopPodSandboxRequest{PodSandboxId: "pod-android"})
			return err
		}},
		{"PortForward", func() error {
			_, err := s.PortForward(ctx, &runtime.PortForwardRequest{PodSandboxId: "pod-android"})
			return err
		}},
	}
	for _, tc := range podMethods {
		if err := tc.call(); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if android.calls[tc.name] != 1 {
			t.Fatalf("%s calls on android = %d, want 1", tc.name, android.calls[tc.name])
		}
	}

	if _, err := s.RemovePodSandbox(ctx, &runtime.RemovePodSandboxRequest{PodSandboxId: "pod-android"}); err != nil {
		t.Fatalf("RemovePodSandbox: %v", err)
	}
	if android.calls["RemovePodSandbox"] != 1 {
		t.Fatalf("RemovePodSandbox calls on android = %d, want 1", android.calls["RemovePodSandbox"])
	}
	if _, ok := s.podRoutes.Load("pod-android"); ok {
		t.Fatal("pod route should be removed after successful RemovePodSandbox")
	}
}

func TestMuxFanOutListsForContainersStatsAndImages(t *testing.T) {
	container := newFakeEngine(engine.EngineTypeContainer)
	container.listContainers = []*runtime.Container{{Id: "container-c"}}
	container.listStats = []*runtime.ContainerStats{{Attributes: &runtime.ContainerAttributes{Id: "container-c"}}}
	container.listImages = []*runtime.Image{{Id: "container-image"}}
	e2b := newFakeEngine(engine.EngineTypeE2B)
	e2b.listContainers = []*runtime.Container{{Id: "e2b-c"}}
	e2b.listStats = []*runtime.ContainerStats{{Attributes: &runtime.ContainerAttributes{Id: "e2b-c"}}}
	e2b.listImages = []*runtime.Image{{Id: "e2b-image"}}
	android := newFakeEngine(engine.EngineTypeAndroid)
	android.listContainers = []*runtime.Container{{Id: "android-c"}}
	android.listStats = []*runtime.ContainerStats{{Attributes: &runtime.ContainerAttributes{Id: "android-c"}}}
	android.listImages = []*runtime.Image{{Id: "android-image"}}
	s := NewMuxServer(container, e2b, android)

	containers, err := s.ListContainers(context.Background(), &runtime.ListContainersRequest{})
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if len(containers.Containers) != 3 {
		t.Fatalf("container count = %d, want 3", len(containers.Containers))
	}
	stats, err := s.ListContainerStats(context.Background(), &runtime.ListContainerStatsRequest{})
	if err != nil {
		t.Fatalf("ListContainerStats: %v", err)
	}
	if len(stats.Stats) != 3 {
		t.Fatalf("stats count = %d, want 3", len(stats.Stats))
	}
	images, err := s.ListImages(context.Background(), &runtime.ListImagesRequest{})
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(images.Images) != 3 {
		t.Fatalf("image count = %d, want 3", len(images.Images))
	}
}

func TestMuxFanOutListPartialAndAllFailure(t *testing.T) {
	container := newFakeEngine(engine.EngineTypeContainer)
	container.listPods = []*runtime.PodSandbox{{Id: "container-pod"}}
	e2b := newFakeEngine(engine.EngineTypeE2B)
	e2b.err = errors.New("e2b failed")
	android := newFakeEngine(engine.EngineTypeAndroid)
	android.listPods = []*runtime.PodSandbox{{Id: "android-pod"}}
	s := NewMuxServer(container, e2b, android)

	resp, err := s.ListPodSandbox(context.Background(), &runtime.ListPodSandboxRequest{})
	if err != nil {
		t.Fatalf("ListPodSandbox partial failure: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("partial failure item count = %d, want 2", len(resp.Items))
	}

	container.err = errors.New("container failed")
	android.err = errors.New("android failed")
	if _, err := s.ListPodSandbox(context.Background(), &runtime.ListPodSandboxRequest{}); err == nil {
		t.Fatal("expected all engines failed error")
	}
}

func TestMuxImageRouting(t *testing.T) {
	container := newFakeEngine(engine.EngineTypeContainer)
	e2b := newFakeEngine(engine.EngineTypeE2B)
	android := newFakeEngine(engine.EngineTypeAndroid)
	s := NewMuxServer(container, e2b, android)

	if _, err := s.ImageStatus(context.Background(), &runtime.ImageStatusRequest{Image: &runtime.ImageSpec{Image: "e2b.dev/t:b"}}); err != nil {
		t.Fatalf("ImageStatus e2b: %v", err)
	}
	if _, err := s.PullImage(context.Background(), &runtime.PullImageRequest{Image: &runtime.ImageSpec{Image: "android.dev/cvd:local"}}); err != nil {
		t.Fatalf("PullImage android: %v", err)
	}
	if _, err := s.RemoveImage(context.Background(), &runtime.RemoveImageRequest{Image: &runtime.ImageSpec{Image: "busybox"}}); err != nil {
		t.Fatalf("RemoveImage container: %v", err)
	}
	if e2b.calls["ImageStatus"] != 1 || android.calls["PullImage"] != 1 || container.calls["RemoveImage"] != 1 {
		t.Fatalf("image route calls mismatch: container=%v e2b=%v android=%v", container.calls, e2b.calls, android.calls)
	}
}

func TestMuxImageFsInfoFallback(t *testing.T) {
	container := newFakeEngine(engine.EngineTypeContainer)
	container.err = errors.New("container failed")
	e2b := newFakeEngine(engine.EngineTypeE2B)
	e2b.imageFsInfo = &runtime.ImageFsInfoResponse{ImageFilesystems: []*runtime.FilesystemUsage{{Timestamp: 1}}}
	android := newFakeEngine(engine.EngineTypeAndroid)
	s := NewMuxServer(container, e2b, android)

	resp, err := s.ImageFsInfo(context.Background(), &runtime.ImageFsInfoRequest{})
	if err != nil {
		t.Fatalf("ImageFsInfo fallback to e2b: %v", err)
	}
	if len(resp.ImageFilesystems) != 1 || resp.ImageFilesystems[0].Timestamp != 1 {
		t.Fatalf("unexpected e2b fallback response: %+v", resp)
	}

	e2b.err = errors.New("e2b failed")
	android.imageFsInfo = &runtime.ImageFsInfoResponse{ImageFilesystems: []*runtime.FilesystemUsage{{Timestamp: 2}}}
	resp, err = s.ImageFsInfo(context.Background(), &runtime.ImageFsInfoRequest{})
	if err != nil {
		t.Fatalf("ImageFsInfo fallback to android: %v", err)
	}
	if len(resp.ImageFilesystems) != 1 || resp.ImageFilesystems[0].Timestamp != 2 {
		t.Fatalf("unexpected android fallback response: %+v", resp)
	}
}

func TestMuxLocalStatusVersionAndEvents(t *testing.T) {
	s := NewMuxServer(newFakeEngine(engine.EngineTypeContainer), newFakeEngine(engine.EngineTypeE2B), newFakeEngine(engine.EngineTypeAndroid))

	statusResp, err := s.Status(context.Background(), &runtime.StatusRequest{})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(statusResp.Status.Conditions) != 2 || !statusResp.Status.Conditions[0].Status || !statusResp.Status.Conditions[1].Status {
		t.Fatalf("status conditions mismatch: %+v", statusResp.Status.Conditions)
	}
	versionResp, err := s.Version(context.Background(), &runtime.VersionRequest{})
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if versionResp.RuntimeName != "e2b-cri-shim" {
		t.Fatalf("runtime name = %q", versionResp.RuntimeName)
	}
	if err := s.GetContainerEvents(&runtime.GetEventsRequest{}, nil); err == nil {
		t.Fatal("GetContainerEvents should return unsupported error")
	}
	if _, err := s.UpdateRuntimeConfig(context.Background(), &runtime.UpdateRuntimeConfigRequest{}); err != nil {
		t.Fatalf("UpdateRuntimeConfig: %v", err)
	}
	s.Stop()
}

func TestMuxImageRoutingAllPrefixes(t *testing.T) {
	container := newFakeEngine(engine.EngineTypeContainer)
	e2b := newFakeEngine(engine.EngineTypeE2B)
	android := newFakeEngine(engine.EngineTypeAndroid)
	s := NewMuxServer(container, e2b, android)

	imageCalls := []struct {
		name  string
		image string
		call  func(context.Context, string) error
		want  *fakeRuntimeEngine
	}{
		{"ImageStatus-e2b", "e2b.dev/t:b", func(ctx context.Context, image string) error {
			_, err := s.ImageStatus(ctx, &runtime.ImageStatusRequest{Image: &runtime.ImageSpec{Image: image}})
			return err
		}, e2b},
		{"ImageStatus-android", "android.dev/cvd:local", func(ctx context.Context, image string) error {
			_, err := s.ImageStatus(ctx, &runtime.ImageStatusRequest{Image: &runtime.ImageSpec{Image: image}})
			return err
		}, android},
		{"ImageStatus-container", "busybox", func(ctx context.Context, image string) error {
			_, err := s.ImageStatus(ctx, &runtime.ImageStatusRequest{Image: &runtime.ImageSpec{Image: image}})
			return err
		}, container},
		{"PullImage-e2b", "e2b.dev/t:b", func(ctx context.Context, image string) error {
			_, err := s.PullImage(ctx, &runtime.PullImageRequest{Image: &runtime.ImageSpec{Image: image}})
			return err
		}, e2b},
		{"PullImage-android", "android.dev/cvd:local", func(ctx context.Context, image string) error {
			_, err := s.PullImage(ctx, &runtime.PullImageRequest{Image: &runtime.ImageSpec{Image: image}})
			return err
		}, android},
		{"PullImage-container", "busybox", func(ctx context.Context, image string) error {
			_, err := s.PullImage(ctx, &runtime.PullImageRequest{Image: &runtime.ImageSpec{Image: image}})
			return err
		}, container},
		{"RemoveImage-e2b", "e2b.dev/t:b", func(ctx context.Context, image string) error {
			_, err := s.RemoveImage(ctx, &runtime.RemoveImageRequest{Image: &runtime.ImageSpec{Image: image}})
			return err
		}, e2b},
		{"RemoveImage-android", "android.dev/cvd:local", func(ctx context.Context, image string) error {
			_, err := s.RemoveImage(ctx, &runtime.RemoveImageRequest{Image: &runtime.ImageSpec{Image: image}})
			return err
		}, android},
		{"RemoveImage-container", "busybox", func(ctx context.Context, image string) error {
			_, err := s.RemoveImage(ctx, &runtime.RemoveImageRequest{Image: &runtime.ImageSpec{Image: image}})
			return err
		}, container},
	}
	for _, tc := range imageCalls {
		before := tc.want.calls
		methodName := strings.Split(tc.name, "-")[0]
		countBefore := before[methodName]
		if err := tc.call(context.Background(), tc.image); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if tc.want.calls[methodName] != countBefore+1 {
			t.Fatalf("%s did not call expected engine %s", tc.name, tc.want.typ)
		}
	}
}
