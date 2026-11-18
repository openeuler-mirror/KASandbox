package agentenv

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	runtimeapi "github.com/cri-multiplex/pkg/agentenv/runtime/v1"
	"github.com/cri-multiplex/pkg/engine"
)

func newReadyContainerTestEngine(t *testing.T) (*Engine, *memoryStateStore, *engine.RouteTable) {
	t.Helper()
	const nativeID = "019a0000-0000-7000-8000-000000000201"
	lifecycle := &fakeLifecycle{create: func(_ context.Context, req *runtimeapi.SandboxCreateRequest) (*runtimeapi.SandboxCreateResponse, error) {
		return &runtimeapi.SandboxCreateResponse{Sandbox: &runtimeapi.SandboxInfo{
			NativeSandboxId:  nativeID,
			ProductSandboxId: req.GetSpec().GetProductSandboxId(),
			Phase:            runtimeapi.SandboxPhase_RUNNING,
		}}, nil
	}}
	instance, store, routes := newCRITestEngine(t, lifecycle)
	instance.exec = &fakeExec{}
	if _, err := instance.RunPodSandbox(context.Background(), criRunRequest(map[string]string{
		annTemplate:  "template-1",
		annSandboxID: "product-container-test",
	})); err != nil {
		t.Fatalf("RunPodSandbox: %v", err)
	}
	return instance, store, routes
}

func criCreateContainerRequest(podID, name, image string, attempt uint32) *runtime.CreateContainerRequest {
	logPath := ""
	if file, err := os.CreateTemp("", "agentenv-container-*.log"); err == nil {
		logPath = file.Name()
		_ = file.Close()
	}
	return &runtime.CreateContainerRequest{
		PodSandboxId: podID,
		Config: &runtime.ContainerConfig{
			Metadata: &runtime.ContainerMetadata{Name: name, Attempt: attempt},
			Image:    &runtime.ImageSpec{Image: image},
			Command:  []string{"/bin/sh"},
			Args:     []string{"-c", "sleep 3600"},
			LogPath:  logPath,
			Labels:   map[string]string{"io.kubernetes.container.name": name},
			Annotations: map[string]string{
				"io.kubernetes.container.hash":         "hash-" + name,
				"io.kubernetes.container.restartCount": "0",
			},
		},
	}
}

func TestCreateContainerResolvesRelativeLogPath(t *testing.T) {
	instance, _, _ := newReadyContainerTestEngine(t)
	pod, ok := instance.getPod("cri-1")
	if !ok {
		t.Fatal("pod projection not found")
	}
	pod.LogDirectory = "/var/log/pods/agentenv-e2e"
	if err := instance.putPod(pod); err != nil {
		t.Fatalf("putPod: %v", err)
	}

	req := criCreateContainerRequest("cri-1", "workload", "agentenv.dev/app:v1", 0)
	req.Config.LogPath = "workload/0.log"
	response, err := instance.CreateContainer(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	record, ok := instance.getContainer(response.GetContainerId())
	if !ok {
		t.Fatal("container projection not found")
	}
	want := filepath.Join(pod.LogDirectory, req.Config.LogPath)
	if record.LogPath != want {
		t.Fatalf("log path = %q, want %q", record.LogPath, want)
	}
	statusResponse, err := instance.ContainerStatus(context.Background(), &runtime.ContainerStatusRequest{ContainerId: response.GetContainerId()})
	if err != nil || statusResponse.GetStatus().GetLogPath() != want {
		t.Fatalf("status log path = %q err=%v, want %q", statusResponse.GetStatus().GetLogPath(), err, want)
	}
}

func TestCreateContainerRejectsRelativeLogPathWithoutDirectory(t *testing.T) {
	instance, _, _ := newReadyContainerTestEngine(t)
	req := criCreateContainerRequest("cri-1", "workload", "agentenv.dev/app:v1", 0)
	req.Config.LogPath = "workload/0.log"
	_, err := instance.CreateContainer(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestCreateContainerRejectsNonVirtualImages(t *testing.T) {
	instance, _, _ := newReadyContainerTestEngine(t)
	for _, image := range []string{
		"docker.io/library/alpine:latest",
		"agentenv.dev/",
		"agentenv.dev//app:v1",
		"agentenv.dev/../app:v1",
		" agentenv.dev/app:v1",
	} {
		t.Run(image, func(t *testing.T) {
			_, err := instance.CreateContainer(context.Background(),
				criCreateContainerRequest("cri-1", "app", image, 0))
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("image %q code = %v, want InvalidArgument", image, status.Code(err))
			}
		})
	}
}
func TestContainerLifecycleProjectionAndPodCleanup(t *testing.T) {
	instance, store, _ := newReadyContainerTestEngine(t)
	first, err := instance.CreateContainer(context.Background(), criCreateContainerRequest("cri-1", "app", "agentenv.dev/app:v1", 0))
	if err != nil {
		t.Fatalf("CreateContainer first: %v", err)
	}
	second, err := instance.CreateContainer(context.Background(), criCreateContainerRequest("cri-1", "sidecar", "agentenv.dev/sidecar:v1", 0))
	if err != nil {
		t.Fatalf("CreateContainer second: %v", err)
	}
	if first.GetContainerId() == "" || first.GetContainerId() == second.GetContainerId() {
		t.Fatalf("container ids are not unique: first=%q second=%q", first.GetContainerId(), second.GetContainerId())
	}
	duplicate, err := instance.CreateContainer(context.Background(), criCreateContainerRequest("cri-1", "app", "agentenv.dev/app:v1", 0))
	if err != nil || duplicate.GetContainerId() != first.GetContainerId() {
		t.Fatalf("CreateContainer should be idempotent: id=%q err=%v", duplicate.GetContainerId(), err)
	}

	list, err := instance.ListContainers(context.Background(), &runtime.ListContainersRequest{Filter: &runtime.ContainerFilter{
		PodSandboxId: "cri-1",
	}})
	if err != nil || len(list.GetContainers()) != 2 {
		t.Fatalf("ListContainers created: len=%d err=%v", len(list.GetContainers()), err)
	}
	for _, item := range list.GetContainers() {
		if item.GetLabels()["io.kubernetes.container.hash"] == "" {
			t.Fatalf("container hash annotation was not projected to labels: %+v", item.GetLabels())
		}
	}

	if _, err := instance.StartContainer(context.Background(), &runtime.StartContainerRequest{ContainerId: first.GetContainerId()}); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	containerStatus, err := instance.ContainerStatus(context.Background(), &runtime.ContainerStatusRequest{ContainerId: first.GetContainerId()})
	if err != nil {
		t.Fatalf("ContainerStatus running: %v", err)
	}
	if containerStatus.GetStatus().GetState() != runtime.ContainerState_CONTAINER_RUNNING || containerStatus.GetStatus().GetStartedAt() <= 0 {
		t.Fatalf("running status mismatch: %+v", containerStatus.GetStatus())
	}
	running, err := instance.ListContainers(context.Background(), &runtime.ListContainersRequest{Filter: &runtime.ContainerFilter{
		State: &runtime.ContainerStateValue{State: runtime.ContainerState_CONTAINER_RUNNING},
	}})
	if err != nil || len(running.GetContainers()) != 1 || running.GetContainers()[0].GetId() != first.GetContainerId() {
		t.Fatalf("running filter mismatch: %+v err=%v", running.GetContainers(), err)
	}

	if _, err := instance.StopContainer(context.Background(), &runtime.StopContainerRequest{ContainerId: first.GetContainerId()}); err != nil {
		t.Fatalf("StopContainer: %v", err)
	}
	containerStatus, err = instance.ContainerStatus(context.Background(), &runtime.ContainerStatusRequest{ContainerId: first.GetContainerId()})
	if err != nil || containerStatus.GetStatus().GetState() != runtime.ContainerState_CONTAINER_EXITED || containerStatus.GetStatus().GetFinishedAt() <= 0 {
		t.Fatalf("exited status mismatch: %+v err=%v", containerStatus.GetStatus(), err)
	}
	if _, err := instance.RemoveContainer(context.Background(), &runtime.RemoveContainerRequest{ContainerId: first.GetContainerId()}); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
	if _, err := instance.ContainerStatus(context.Background(), &runtime.ContainerStatusRequest{ContainerId: first.GetContainerId()}); status.Code(err) != codes.NotFound {
		t.Fatalf("removed container code = %v, want NotFound", status.Code(err))
	}

	if _, err := instance.StopPodSandbox(context.Background(), &runtime.StopPodSandboxRequest{PodSandboxId: "cri-1"}); err != nil {
		t.Fatalf("StopPodSandbox: %v", err)
	}
	containerStatus, err = instance.ContainerStatus(context.Background(), &runtime.ContainerStatusRequest{ContainerId: second.GetContainerId()})
	if err != nil || containerStatus.GetStatus().GetState() != runtime.ContainerState_CONTAINER_EXITED {
		t.Fatalf("pod stop did not stop container: %+v err=%v", containerStatus.GetStatus(), err)
	}
	if _, err := instance.RemovePodSandbox(context.Background(), &runtime.RemovePodSandboxRequest{PodSandboxId: "cri-1"}); err != nil {
		t.Fatalf("RemovePodSandbox: %v", err)
	}
	if _, err := instance.ContainerStatus(context.Background(), &runtime.ContainerStatusRequest{ContainerId: second.GetContainerId()}); status.Code(err) != codes.NotFound {
		t.Fatalf("container survived pod removal: %+v", err)
	}
	var persisted persistedAgentENVState
	if found, err := store.LoadBackendState(podStatePath, &persisted); err != nil || !found || len(persisted.Containers) != 0 {
		t.Fatalf("container state not cleaned: found=%v err=%v state=%+v", found, err, persisted)
	}
}

func TestContainerProjectionRestoreAndValidation(t *testing.T) {
	instance, store, _ := newReadyContainerTestEngine(t)
	created, err := instance.CreateContainer(context.Background(), criCreateContainerRequest("cri-1", "app", "agentenv.dev/app:v1", 0))
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if _, err := instance.StartContainer(context.Background(), &runtime.StartContainerRequest{ContainerId: created.GetContainerId()}); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}

	restored := newWithClients(
		Config{RuntimeAddress: "passthrough:///agentenv-test", NodeName: "node-1", NodeIP: "10.0.0.1"},
		store,
		engine.NewRouteTable(store),
		&fakeLifecycle{},
		&fakeAdmin{},
	)
	if err := restored.RestoreState(context.Background()); err != nil {
		t.Fatalf("RestoreState: %v", err)
	}
	containerStatus, err := restored.ContainerStatus(context.Background(), &runtime.ContainerStatusRequest{ContainerId: created.GetContainerId()})
	if err != nil || containerStatus.GetStatus().GetState() != runtime.ContainerState_CONTAINER_RUNNING {
		t.Fatalf("restored container mismatch: %+v err=%v", containerStatus.GetStatus(), err)
	}
	if _, err := restored.CreateContainer(context.Background(), nil); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("nil create config code = %v, want InvalidArgument", status.Code(err))
	}
	if _, err := restored.CreateContainer(context.Background(), criCreateContainerRequest("missing", "app", "agentenv.dev/app:v1", 0)); status.Code(err) != codes.NotFound {
		t.Fatalf("missing pod code = %v, want NotFound", status.Code(err))
	}
	if _, err := restored.StartContainer(context.Background(), &runtime.StartContainerRequest{ContainerId: "missing"}); status.Code(err) != codes.NotFound {
		t.Fatalf("missing container code = %v, want NotFound", status.Code(err))
	}
}

func TestCreateContainerRequiresReadyPod(t *testing.T) {
	instance, _, _ := newReadyContainerTestEngine(t)
	if _, err := instance.StopPodSandbox(context.Background(), &runtime.StopPodSandboxRequest{PodSandboxId: "cri-1"}); err != nil {
		t.Fatalf("StopPodSandbox: %v", err)
	}
	if _, err := instance.CreateContainer(context.Background(), criCreateContainerRequest("cri-1", "app", "agentenv.dev/app:v1", 0)); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("not-ready pod code = %v, want FailedPrecondition", status.Code(err))
	}
}
