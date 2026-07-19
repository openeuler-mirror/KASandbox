package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func mockAndroidRuntimeOps(t *testing.T, validateErr error, startErr error) (androidRuntimeOps, *int, *int) {
	t.Helper()
	start := 0
	stop := 0
	return androidRuntimeOps{
		validateHostPrerequisites: func(e *AndroidEngine) error {
			return validateErr
		},
		startCVD: func(e *AndroidEngine, ctx context.Context, rec *AndroidSandboxRecord) error {
			start++
			if startErr != nil {
				return startErr
			}
			rec.LaunchPID = 1000 + start
			return nil
		},
		stopCVD: func(e *AndroidEngine, ctx context.Context, rec *AndroidSandboxRecord) error {
			stop++
			return nil
		},
	}, &start, &stop
}

func newMockAndroidEngine(t *testing.T, cfg AndroidConfig, validateErr error, startErr error) (*AndroidEngine, *int, *int) {
	t.Helper()
	ops, startCalls, stopCalls := mockAndroidRuntimeOps(t, validateErr, startErr)
	e := NewAndroidEngine(cfg)
	e.ops = ops
	return e, startCalls, stopCalls
}

func newEnabledMockAndroidEngine(t *testing.T, validateErr error, startErr error) (*AndroidEngine, *int, *int) {
	t.Helper()
	return newMockAndroidEngine(t, AndroidConfig{
		Enabled:      true,
		ArtifactsDir: t.TempDir(),
		StateDir:     t.TempDir(),
	}, validateErr, startErr)
}

func newEnabledCNIMockAndroidEngine(t *testing.T, validateErr error, startErr error) (*AndroidEngine, *int, *int) {
	t.Helper()
	return newMockAndroidEngine(t, AndroidConfig{
		Enabled:      true,
		ArtifactsDir: t.TempDir(),
		StateDir:     t.TempDir(),
		CNI:          CNIConfig{Enabled: true},
	}, validateErr, startErr)
}

func androidRunReq(uid string) *runtime.RunPodSandboxRequest {
	return &runtime.RunPodSandboxRequest{Config: &runtime.PodSandboxConfig{
		Metadata: &runtime.PodSandboxMetadata{Name: "android-pod", Namespace: "default", Uid: uid},
		Labels:   map[string]string{"app": "android"},
		Annotations: map[string]string{
			annAndroidBaseInst: "2",
			annAndroidADBPort:  "6620",
		},
	}}
}

func TestAndroidRunStartStopRemoveLifecycleWithMocks(t *testing.T) {
	e, startCalls, stopCalls := newEnabledCNIMockAndroidEngine(t, nil, nil)
	fakeCNI := &fakeCNIManager{addRecord: &CNIRecord{
		SandboxID: "android-uid",
		Network:   "calico",
		IfName:    "eth0",
		NetNSName: "android-uid",
		NetNSPath: "/var/run/netns/android-uid",
		PodIP:     "10.0.0.30",
	}}
	e.cniManager = fakeCNI

	runResp, err := e.RunPodSandbox(context.Background(), androidRunReq("android-uid"))
	if err != nil {
		t.Fatalf("RunPodSandbox: %v", err)
	}
	if runResp.PodSandboxId != "android-uid" || fakeCNI.addCalls != 1 {
		t.Fatalf("run response/cni mismatch: resp=%+v addCalls=%d", runResp, fakeCNI.addCalls)
	}

	createResp, err := e.CreateContainer(context.Background(), &runtime.CreateContainerRequest{
		PodSandboxId: "android-uid",
		Config: &runtime.ContainerConfig{
			Metadata:    &runtime.ContainerMetadata{Name: "app", Attempt: 1},
			Image:       &runtime.ImageSpec{Image: androidDefaultImage},
			Labels:      map[string]string{"role": "android"},
			Annotations: map[string]string{"io.kubernetes.container.hash": "hash-a"},
		},
	})
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if createResp.ContainerId != "android-uid-c" {
		t.Fatalf("container id = %q", createResp.ContainerId)
	}

	if _, err := e.StartContainer(context.Background(), &runtime.StartContainerRequest{ContainerId: "android-uid-c"}); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	if *startCalls != 1 {
		t.Fatalf("start calls = %d, want 1", *startCalls)
	}

	podStatus, err := e.PodSandboxStatus(context.Background(), &runtime.PodSandboxStatusRequest{PodSandboxId: "android-uid"})
	if err != nil {
		t.Fatalf("PodSandboxStatus: %v", err)
	}
	if podStatus.Status.Network.Ip != "10.0.0.30" || podStatus.Status.Annotations["android.dev/pod-ip"] != "10.0.0.30" {
		t.Fatalf("pod status mismatch: %+v", podStatus.Status)
	}

	containerStatus, err := e.ContainerStatus(context.Background(), &runtime.ContainerStatusRequest{ContainerId: "android-uid-c"})
	if err != nil {
		t.Fatalf("ContainerStatus: %v", err)
	}
	if containerStatus.Status.State != runtime.ContainerState_CONTAINER_RUNNING {
		t.Fatalf("container state = %v", containerStatus.Status.State)
	}

	if _, err := e.StopPodSandbox(context.Background(), &runtime.StopPodSandboxRequest{PodSandboxId: "android-uid"}); err != nil {
		t.Fatalf("StopPodSandbox: %v", err)
	}
	if *stopCalls != 1 {
		t.Fatalf("stop calls = %d, want 1", *stopCalls)
	}
	if _, err := e.RemovePodSandbox(context.Background(), &runtime.RemovePodSandboxRequest{PodSandboxId: "android-uid"}); err != nil {
		t.Fatalf("RemovePodSandbox: %v", err)
	}
	if fakeCNI.delCalls != 1 {
		t.Fatalf("CNI del calls = %d, want 1", fakeCNI.delCalls)
	}
	if _, err := e.PodSandboxStatus(context.Background(), &runtime.PodSandboxStatusRequest{PodSandboxId: "android-uid"}); status.Code(err) != codes.NotFound {
		t.Fatalf("removed pod status code = %v, want NotFound", status.Code(err))
	}
}

func TestAndroidRunPodSandboxIdempotentAndRollback(t *testing.T) {
	e, _, _ := newEnabledMockAndroidEngine(t, nil, nil)

	req := androidRunReq("android-uid")
	if _, err := e.RunPodSandbox(context.Background(), req); err != nil {
		t.Fatalf("RunPodSandbox first: %v", err)
	}
	if _, err := e.RunPodSandbox(context.Background(), req); err != nil {
		t.Fatalf("RunPodSandbox idempotent: %v", err)
	}
	if len(e.portOwners) != 1 || len(e.instanceOwners) != 1 {
		t.Fatalf("idempotent run should not allocate twice: ports=%v instances=%v", e.portOwners, e.instanceOwners)
	}

	fakeCNI := &fakeCNIManager{addErr: errors.New("cni failed")}
	cniEngine, _, _ := newEnabledCNIMockAndroidEngine(t, nil, nil)
	cniEngine.cniManager = fakeCNI
	if _, err := cniEngine.RunPodSandbox(context.Background(), androidRunReq("android-cni-fail")); status.Code(err) != codes.Internal {
		t.Fatalf("CNI failure code = %v, want Internal", status.Code(err))
	}
	if len(cniEngine.portOwners) != 0 || len(cniEngine.instanceOwners) != 0 {
		t.Fatalf("CNI failure should rollback allocations: ports=%v instances=%v", cniEngine.portOwners, cniEngine.instanceOwners)
	}
}

func TestAndroidStartFailureMarksSandboxUnknown(t *testing.T) {
	e, _, _ := newEnabledMockAndroidEngine(t, nil, errors.New("launch failed"))
	if _, err := e.RunPodSandbox(context.Background(), androidRunReq("android-uid")); err != nil {
		t.Fatalf("RunPodSandbox: %v", err)
	}
	if _, err := e.CreateContainer(context.Background(), &runtime.CreateContainerRequest{
		PodSandboxId: "android-uid",
		Config:       &runtime.ContainerConfig{Metadata: &runtime.ContainerMetadata{Name: "app"}, Image: &runtime.ImageSpec{Image: androidDefaultImage}},
	}); err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if _, err := e.StartContainer(context.Background(), &runtime.StartContainerRequest{ContainerId: "android-uid-c"}); err == nil {
		t.Fatal("expected StartContainer error")
	}
	rec := e.pods["android-uid"]
	if rec.State != androidSandboxUnknown {
		t.Fatalf("sandbox state = %s, want Unknown", rec.State)
	}
}

func TestAndroidPullImagePrerequisiteFailure(t *testing.T) {
	e, _, _ := newMockAndroidEngine(t, AndroidConfig{Enabled: true}, status.Error(codes.FailedPrecondition, "missing kvm"), nil)
	if _, err := e.PullImage(context.Background(), &runtime.PullImageRequest{Image: &runtime.ImageSpec{Image: androidDefaultImage}}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("PullImage prerequisite code = %v, want FailedPrecondition", status.Code(err))
	}
}

func TestAndroidListFiltersAndRemoveContainer(t *testing.T) {
	e, _, _ := newEnabledMockAndroidEngine(t, nil, nil)
	for _, id := range []string{"pod-a", "pod-b"} {
		req := androidRunReq(id)
		req.Config.Annotations = map[string]string{}
		if _, err := e.RunPodSandbox(context.Background(), req); err != nil {
			t.Fatalf("RunPodSandbox %s: %v", id, err)
		}
		if _, err := e.CreateContainer(context.Background(), &runtime.CreateContainerRequest{
			PodSandboxId: id,
			Config: &runtime.ContainerConfig{
				Metadata: &runtime.ContainerMetadata{Name: "app"},
				Image:    &runtime.ImageSpec{Image: androidDefaultImage},
				Labels:   map[string]string{"pod": id},
			},
		}); err != nil {
			t.Fatalf("CreateContainer %s: %v", id, err)
		}
	}

	listResp, err := e.ListContainers(context.Background(), &runtime.ListContainersRequest{Filter: &runtime.ContainerFilter{LabelSelector: map[string]string{"pod": "pod-b"}}})
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if len(listResp.Containers) != 1 || listResp.Containers[0].PodSandboxId != "pod-b" {
		t.Fatalf("filtered containers mismatch: %+v", listResp.Containers)
	}
	if _, err := e.RemoveContainer(context.Background(), &runtime.RemoveContainerRequest{ContainerId: "pod-b-c"}); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
	if _, err := e.ContainerStatus(context.Background(), &runtime.ContainerStatusRequest{ContainerId: "pod-b-c"}); status.Code(err) != codes.NotFound {
		t.Fatalf("removed container code = %v, want NotFound", status.Code(err))
	}

	// Keep timestamps deterministic enough to ensure list conversion has non-zero values.
	time.Sleep(time.Nanosecond)
}

func TestAndroidListPodSandboxStopContainerAndStubMethods(t *testing.T) {
	e, _, _ := newEnabledMockAndroidEngine(t, nil, nil)
	req := androidRunReq("android-uid")
	req.Config.Annotations = map[string]string{}
	if _, err := e.RunPodSandbox(context.Background(), req); err != nil {
		t.Fatalf("RunPodSandbox: %v", err)
	}
	if _, err := e.CreateContainer(context.Background(), &runtime.CreateContainerRequest{
		PodSandboxId: "android-uid",
		Config:       &runtime.ContainerConfig{Metadata: &runtime.ContainerMetadata{Name: "app"}, Image: &runtime.ImageSpec{Image: androidDefaultImage}},
	}); err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if _, err := e.StartContainer(context.Background(), &runtime.StartContainerRequest{ContainerId: "android-uid-c"}); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	if _, err := e.StopContainer(context.Background(), &runtime.StopContainerRequest{ContainerId: "android-uid-c"}); err != nil {
		t.Fatalf("StopContainer: %v", err)
	}

	pods, err := e.ListPodSandbox(context.Background(), &runtime.ListPodSandboxRequest{Filter: &runtime.PodSandboxFilter{Id: "android-uid"}})
	if err != nil {
		t.Fatalf("ListPodSandbox: %v", err)
	}
	if len(pods.Items) != 1 || pods.Items[0].Id != "android-uid" {
		t.Fatalf("filtered pods mismatch: %+v", pods.Items)
	}

	if e.Type() != EngineTypeAndroid {
		t.Fatalf("Type = %s", e.Type())
	}
	if version, err := e.Version(context.Background(), &runtime.VersionRequest{}); err != nil || version.RuntimeName != "android-cvd" {
		t.Fatalf("Version = %+v err=%v", version, err)
	}
	if images, err := e.ListImages(context.Background(), &runtime.ListImagesRequest{}); err != nil || len(images.Images) != 1 {
		t.Fatalf("ListImages = %+v err=%v", images, err)
	}
	if _, err := e.ListContainerStats(context.Background(), &runtime.ListContainerStatsRequest{}); err != nil {
		t.Fatalf("ListContainerStats: %v", err)
	}
	if _, err := e.UpdateContainerResources(context.Background(), &runtime.UpdateContainerResourcesRequest{}); err != nil {
		t.Fatalf("UpdateContainerResources: %v", err)
	}
	if _, err := e.ReopenContainerLog(context.Background(), &runtime.ReopenContainerLogRequest{}); err != nil {
		t.Fatalf("ReopenContainerLog: %v", err)
	}
	if _, err := e.UpdateRuntimeConfig(context.Background(), &runtime.UpdateRuntimeConfigRequest{}); err != nil {
		t.Fatalf("UpdateRuntimeConfig: %v", err)
	}
	if _, err := e.RemoveImage(context.Background(), &runtime.RemoveImageRequest{}); err != nil {
		t.Fatalf("RemoveImage: %v", err)
	}
	if _, err := e.ImageFsInfo(context.Background(), &runtime.ImageFsInfoRequest{}); err != nil {
		t.Fatalf("ImageFsInfo: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := e.ContainerStats(context.Background(), &runtime.ContainerStatsRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("ContainerStats code = %v, want Unimplemented", status.Code(err))
	}
	if _, err := e.GetContainerEvents(context.Background(), &runtime.GetEventsRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("GetContainerEvents code = %v, want Unimplemented", status.Code(err))
	}
}
