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
	instance, _ := newCRITestEngine(t, &fakeLifecycle{})
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
	instance := newWithClients(
		Config{RuntimeAddress: "passthrough:///agentenv-test", NodeName: "node-1", NodeIP: "10.0.0.1"},
		store,
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
	attempts := 0
	instance := newWithClients(
		Config{RuntimeAddress: "passthrough:///agentenv-test", NodeName: "node-1", NodeIP: "10.0.0.1"},
		store,
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
	instance, _ := newReadyContainerTestEngine(t)
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

func TestVirtualImageProjection(t *testing.T) {
	instance, _ := newReadyContainerTestEngine(t)
	ctx := context.Background()
	ref := "agentenv.dev/app:v1"
	for i := 0; i < 2; i++ {
		image, err := instance.ImageStatus(ctx, &runtime.ImageStatusRequest{Image: &runtime.ImageSpec{Image: ref}})
		if err != nil || image.GetImage().GetId() != ref {
			t.Fatalf("ImageStatus: %v %v", image, err)
		}
		if _, err := instance.RemoveImage(ctx, &runtime.RemoveImageRequest{Image: &runtime.ImageSpec{Image: ref}}); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := instance.ListImages(ctx, &runtime.ListImagesRequest{})
	if err != nil || len(listed.GetImages()) != 0 {
		t.Fatalf("ListImages: %v %v", listed, err)
	}
	if _, err := instance.PullImage(ctx, &runtime.PullImageRequest{Image: &runtime.ImageSpec{Image: "docker.io/unrelated:latest"}}); err == nil {
		t.Fatal("accepted unrelated image")
	}
}
