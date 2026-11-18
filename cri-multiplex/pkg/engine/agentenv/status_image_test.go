package agentenv

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	runtimeapi "github.com/cri-multiplex/pkg/agentenv/runtime/v1"
)

func TestCRIStatusVersionAndRuntimeConfig(t *testing.T) {
	instance, _, _ := newCRITestEngine(t, &fakeLifecycle{})
	version, err := instance.Version(context.Background(), &runtime.VersionRequest{})
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if version.GetRuntimeName() != agentenvRuntimeName || version.GetRuntimeApiVersion() != "v1" || version.GetRuntimeVersion() == "" {
		t.Fatalf("version mismatch: %+v", version)
	}
	if _, err := instance.UpdateRuntimeConfig(context.Background(), &runtime.UpdateRuntimeConfigRequest{}); err != nil {
		t.Fatalf("UpdateRuntimeConfig: %v", err)
	}
	response, err := instance.Status(context.Background(), &runtime.StatusRequest{Verbose: true})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(response.GetStatus().GetConditions()) != 2 {
		t.Fatalf("conditions = %+v", response.GetStatus().GetConditions())
	}
	for _, condition := range response.GetStatus().GetConditions() {
		if !condition.GetStatus() {
			t.Fatalf("healthy condition is false: %+v", condition)
		}
	}
	if response.GetInfo()["agentenv"] == "" {
		t.Fatalf("verbose status info missing: %+v", response.GetInfo())
	}
}

func TestCRIStatusReportsRuntimeFailure(t *testing.T) {
	store := newMemoryStateStore()
	routes := newRouteTable(store)
	instance := newWithClients(
		Config{RuntimeAddress: "passthrough:///agentenv-test", NodeName: "node-1", NodeIP: "10.0.0.1"},
		store,
		routes,
		&fakeLifecycle{},
		&fakeAdmin{capabilities: func(context.Context, *emptypb.Empty) (*runtimeapi.SandboxCapabilities, error) {
			return nil, status.Error(codes.Unavailable, "runtime offline")
		}},
	)
	response, err := instance.Status(context.Background(), &runtime.StatusRequest{})
	if err != nil {
		t.Fatalf("Status should not fail: %v", err)
	}
	conditions := response.GetStatus().GetConditions()
	if len(conditions) != 2 || conditions[0].GetStatus() || conditions[1].GetStatus() {
		t.Fatalf("unhealthy conditions = %+v", conditions)
	}
	if conditions[0].GetReason() == "" || conditions[0].GetMessage() == "" {
		t.Fatalf("runtime failure detail missing: %+v", conditions[0])
	}
}

func TestHealthRetriesOnceAfterRuntimeRestart(t *testing.T) {
	store := newMemoryStateStore()
	routes := newRouteTable(store)
	attempts := 0
	instance := newWithClients(
		Config{RuntimeAddress: "passthrough:///agentenv-test", NodeName: "node-1", NodeIP: "10.0.0.1"},
		store,
		routes,
		&fakeLifecycle{},
		&fakeAdmin{capabilities: func(context.Context, *emptypb.Empty) (*runtimeapi.SandboxCapabilities, error) {
			attempts++
			if attempts == 1 {
				return nil, status.Error(codes.Unavailable, "runtime restarting")
			}
			return &runtimeapi.SandboxCapabilities{}, nil
		}},
	)
	if err := instance.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("capability attempts = %d, want 2", attempts)
	}
}
func TestCreateContainerAcceptsVirtualImageID(t *testing.T) {
	instance, _, _ := newReadyContainerTestEngine(t)
	pulled, err := instance.PullImage(context.Background(), &runtime.PullImageRequest{Image: &runtime.ImageSpec{Image: "agentenv.dev/app:v1"}})
	if err != nil {
		t.Fatalf("PullImage: %v", err)
	}

	response, err := instance.CreateContainer(context.Background(),
		criCreateContainerRequest("cri-1", "app", pulled.GetImageRef(), 0))
	if err != nil {
		t.Fatalf("CreateContainer with image ID: %v", err)
	}
	record, ok := instance.getContainer(response.GetContainerId())
	if !ok {
		t.Fatal("container projection not found")
	}
	if record.ImageName != "agentenv.dev/app:v1" {
		t.Fatalf("ImageName = %q, want original virtual reference", record.ImageName)
	}
	if record.ImageRef != pulled.GetImageRef() {
		t.Fatalf("ImageRef = %q, want %q", record.ImageRef, pulled.GetImageRef())
	}
}

func TestCreateContainerRejectsUnknownVirtualImageID(t *testing.T) {
	instance, _, _ := newReadyContainerTestEngine(t)
	_, err := instance.CreateContainer(context.Background(),
		criCreateContainerRequest("cri-1", "app", "agentenv-image:0123456789abcdef0123456789abcdef", 0))
	if status.Code(err) != codes.NotFound {
		t.Fatalf("code = %v, want NotFound", status.Code(err))
	}
}

func TestVirtualImageProjection(t *testing.T) {
	instance, store, _ := newReadyContainerTestEngine(t)
	pulled, err := instance.PullImage(context.Background(), &runtime.PullImageRequest{Image: &runtime.ImageSpec{Image: "agentenv.dev/app:v1"}})
	if err != nil {
		t.Fatalf("PullImage: %v", err)
	}
	if pulled.GetImageRef() == "" {
		t.Fatal("PullImage returned empty image ref")
	}
	imageStatus, err := instance.ImageStatus(context.Background(), &runtime.ImageStatusRequest{Image: &runtime.ImageSpec{Image: "agentenv.dev/app:v1"}})
	if err != nil || imageStatus.GetImage().GetId() != pulled.GetImageRef() {
		t.Fatalf("ImageStatus mismatch: image=%+v err=%v", imageStatus.GetImage(), err)
	}
	listed, err := instance.ListImages(context.Background(), &runtime.ListImagesRequest{Filter: &runtime.ImageFilter{
		Image: &runtime.ImageSpec{Image: pulled.GetImageRef()},
	}})
	if err != nil || len(listed.GetImages()) != 1 || listed.GetImages()[0].GetId() != pulled.GetImageRef() {
		t.Fatalf("ListImages filter mismatch: %+v err=%v", listed.GetImages(), err)
	}

	if _, err := instance.CreateContainer(context.Background(), criCreateContainerRequest("cri-1", "app", "agentenv.dev/container-only:v2", 0)); err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	all, err := instance.ListImages(context.Background(), &runtime.ListImagesRequest{})
	if err != nil || len(all.GetImages()) != 2 {
		t.Fatalf("image projection from container missing: %+v err=%v", all.GetImages(), err)
	}
	if _, err := instance.RemoveImage(context.Background(), &runtime.RemoveImageRequest{Image: &runtime.ImageSpec{Image: "agentenv.dev/app:v1"}}); err != nil {
		t.Fatalf("RemoveImage: %v", err)
	}
	imageStatus, err = instance.ImageStatus(context.Background(), &runtime.ImageStatusRequest{Image: &runtime.ImageSpec{Image: "agentenv.dev/app:v1"}})
	if err != nil || imageStatus.GetImage() != nil {
		t.Fatalf("removed image still visible: %+v err=%v", imageStatus.GetImage(), err)
	}
	var persisted map[string]any
	if found, err := store.LoadBackendState(podStatePath, &persisted); err != nil || !found || persisted["images"] != nil {
		t.Fatalf("image state not removed: found=%v err=%v state=%+v", found, err, persisted)
	}
}
