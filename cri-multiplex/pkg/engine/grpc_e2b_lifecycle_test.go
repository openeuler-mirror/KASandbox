package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cri-multiplex/pkg/envd/process"
	"github.com/cri-multiplex/pkg/orchestrator"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func newTestGRPCE2BEngine(client *fakeSandboxServiceClient) *grpcE2BEngine {
	return &grpcE2BEngine{
		orchestratorAddr:      "fake",
		orchestratorProxyAddr: "fake-proxy",
		nodeIP:                "192.0.2.10",
		hostPortOps:           defaultHostPortMappingOps(),
		client:                client,
		tracker:               newPodTracker(),
		imageCache:            make(map[string]*e2bImageMeta),
		streamingReqs:         make(map[string]*execStreamRequest),
		attachReqs:            make(map[string]*attachStreamRequest),
		hostPortManager:       NewHostPortManager(20000, 20010),
	}
}

func e2bRunReq(uid string) *runtime.RunPodSandboxRequest {
	return &runtime.RunPodSandboxRequest{Config: &runtime.PodSandboxConfig{
		Metadata: &runtime.PodSandboxMetadata{Name: "pod-a", Namespace: "ns-a", Uid: uid},
		Labels:   map[string]string{"app": "e2b"},
		Annotations: map[string]string{
			annTemplateID:      "tmpl-a",
			annBuildID:         "build-a",
			annTeamID:          "team-a",
			annEnvdAccessToken: "token-a",
		},
	}}
}

func TestGRPCE2BRunPodSandboxWithInjectedClientAndCNI(t *testing.T) {
	client := &fakeSandboxServiceClient{}
	e := newTestGRPCE2BEngine(client)
	e.cniConfig.Enabled = true
	e.cniManager = &fakeCNIManager{addRecord: &CNIRecord{
		SandboxID: "uid-a",
		Network:   "calico",
		IfName:    "eth0",
		NetNSPath: "/var/run/netns/e2b-uid-a",
		PodIP:     "10.0.0.20",
		Gateway:   "10.0.0.1",
		DNS:       []string{"10.96.0.10"},
	}}

	resp, err := e.RunPodSandbox(context.Background(), e2bRunReq("uid-a"))
	if err != nil {
		t.Fatalf("RunPodSandbox: %v", err)
	}
	if resp.PodSandboxId != "uid-a" {
		t.Fatalf("pod id = %q", resp.PodSandboxId)
	}
	if client.createCalls != 1 {
		t.Fatalf("create calls = %d, want 1", client.createCalls)
	}
	if client.lastCreate.Sandbox.RuntimeNetwork == nil || client.lastCreate.Sandbox.RuntimeNetwork.PodIp != "10.0.0.20" {
		t.Fatalf("runtime network mismatch: %+v", client.lastCreate.Sandbox.RuntimeNetwork)
	}
	pod, ok := e.tracker.Get("uid-a")
	if !ok || pod.podIP != "10.0.0.20" || !pod.cniEnabled || pod.envdAccessToken != "token-a" {
		t.Fatalf("tracker pod mismatch: %+v ok=%v", pod, ok)
	}
}

func TestGRPCE2BRunPodSandboxCNIRollbackOnCreateFailure(t *testing.T) {
	client := &fakeSandboxServiceClient{createErr: status.Error(codes.Internal, "create failed")}
	fakeCNI := &fakeCNIManager{}
	e := newTestGRPCE2BEngine(client)
	e.cniConfig.Enabled = true
	e.cniManager = fakeCNI

	if _, err := e.RunPodSandbox(context.Background(), e2bRunReq("uid-a")); status.Code(err) != codes.Internal {
		t.Fatalf("RunPodSandbox error code = %v, want Internal", status.Code(err))
	}
	if fakeCNI.addCalls != 1 || fakeCNI.delCalls != 1 {
		t.Fatalf("CNI calls = add:%d del:%d, want 1/1", fakeCNI.addCalls, fakeCNI.delCalls)
	}
	if _, ok := e.tracker.Get("uid-a"); ok {
		t.Fatal("tracker should not contain failed sandbox")
	}
}

func TestGRPCE2BPodAndContainerLifecycle(t *testing.T) {
	client := &fakeSandboxServiceClient{}
	e := newTestGRPCE2BEngine(client)
	e.tracker.Add("uid-a", &podInfo{
		sandboxID:       "uid-a",
		e2bSandboxID:    "e2buid",
		podUID:          "uid-a",
		name:            "pod-a",
		namespace:       "ns-a",
		labels:          map[string]string{"app": "e2b"},
		annotations:     map[string]string{annTemplateID: "tmpl-a"},
		createdAt:       time.Now(),
		state:           stateRunning,
		envdAccessToken: "token-a",
		podIP:           "10.0.0.20",
		cniEnabled:      true,
		cniRecord:       &CNIRecord{SandboxID: "uid-a", Network: "calico", NetNSPath: "/var/run/netns/e2b-uid-a", PodIP: "10.0.0.20"},
	})

	createResp, err := e.CreateContainer(context.Background(), &runtime.CreateContainerRequest{
		PodSandboxId: "uid-a",
		Config: &runtime.ContainerConfig{
			Metadata:    &runtime.ContainerMetadata{Name: "app"},
			Image:       &runtime.ImageSpec{Image: "e2b.dev/tmpl-a:build-a"},
			Command:     []string{"echo"},
			Args:        []string{"hello"},
			Labels:      map[string]string{"role": "test"},
			Annotations: map[string]string{"io.kubernetes.container.hash": "hash-a"},
			Stdin:       true,
			Tty:         true,
		},
	})
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if createResp.ContainerId != "uid-a-c" {
		t.Fatalf("container id = %q", createResp.ContainerId)
	}

	if _, err := e.StartContainer(context.Background(), &runtime.StartContainerRequest{ContainerId: "uid-a-c"}); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	statusResp, err := e.ContainerStatus(context.Background(), &runtime.ContainerStatusRequest{ContainerId: "uid-a-c"})
	if err != nil {
		t.Fatalf("ContainerStatus: %v", err)
	}
	if statusResp.Status.State != runtime.ContainerState_CONTAINER_RUNNING || statusResp.Status.Labels["io.kubernetes.container.hash"] != "hash-a" {
		t.Fatalf("container status mismatch: %+v", statusResp.Status)
	}

	podStatus, err := e.PodSandboxStatus(context.Background(), &runtime.PodSandboxStatusRequest{PodSandboxId: "uid-a"})
	if err != nil {
		t.Fatalf("PodSandboxStatus: %v", err)
	}
	if podStatus.Status.Network.Ip != "10.0.0.20" || podStatus.Status.Annotations["e2b.dev/cni-enabled"] != "true" {
		t.Fatalf("pod status mismatch: %+v", podStatus.Status)
	}

	if _, err := e.StopContainer(context.Background(), &runtime.StopContainerRequest{ContainerId: "uid-a-c"}); err != nil {
		t.Fatalf("StopContainer: %v", err)
	}
	if _, err := e.RemoveContainer(context.Background(), &runtime.RemoveContainerRequest{ContainerId: "uid-a-c"}); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
	if _, err := e.ContainerStatus(context.Background(), &runtime.ContainerStatusRequest{ContainerId: "uid-a-c"}); status.Code(err) != codes.NotFound {
		t.Fatalf("removed container status code = %v, want NotFound", status.Code(err))
	}
}

func TestGRPCE2BHostPortMappingSetupAndCleanup(t *testing.T) {
	var setupCalls []PortMapping
	var cleanupCalls []PortMapping
	client := &fakeSandboxServiceClient{createResp: &orchestrator.SandboxCreateResponse{
		ClientId: "client-a",
		HostIp:   "172.16.0.2",
	}}
	e := newTestGRPCE2BEngine(client)
	e.hostPortOps = hostPortMappingOps{
		setup: func(nodeIP string, hostPort int, sandboxIP string, sandboxPort int) error {
			if nodeIP != "192.0.2.10" || sandboxIP != "172.16.0.2" {
				t.Fatalf("unexpected setup endpoints nodeIP=%s sandboxIP=%s", nodeIP, sandboxIP)
			}
			setupCalls = append(setupCalls, PortMapping{HostPort: hostPort, SandboxPort: sandboxPort})
			return nil
		},
		cleanup: func(nodeIP string, hostPort int, sandboxIP string, sandboxPort int) error {
			if nodeIP != "192.0.2.10" || sandboxIP != "172.16.0.2" {
				t.Fatalf("unexpected cleanup endpoints nodeIP=%s sandboxIP=%s", nodeIP, sandboxIP)
			}
			cleanupCalls = append(cleanupCalls, PortMapping{HostPort: hostPort, SandboxPort: sandboxPort})
			return nil
		},
	}
	req := e2bRunReq("uid-a")
	req.Config.Annotations[annExposePorts] = "49983,8080"

	if _, err := e.RunPodSandbox(context.Background(), req); err != nil {
		t.Fatalf("RunPodSandbox: %v", err)
	}
	if len(setupCalls) != 2 {
		t.Fatalf("setup call count = %d, want 2: %+v", len(setupCalls), setupCalls)
	}
	pod, _ := e.tracker.Get("uid-a")
	if pod.hostPort == 0 || len(pod.portMappings) != 2 {
		t.Fatalf("pod hostport fields mismatch: hostPort=%d mappings=%+v", pod.hostPort, pod.portMappings)
	}

	statusResp, err := e.PodSandboxStatus(context.Background(), &runtime.PodSandboxStatusRequest{PodSandboxId: "uid-a"})
	if err != nil {
		t.Fatalf("PodSandboxStatus: %v", err)
	}
	if statusResp.Status.Annotations["e2b.dev/host-port-49983"] == "" || statusResp.Status.Annotations["e2b.dev/access-url-8080"] == "" {
		t.Fatalf("hostport annotations missing: %+v", statusResp.Status.Annotations)
	}

	if _, err := e.StopPodSandbox(context.Background(), &runtime.StopPodSandboxRequest{PodSandboxId: "uid-a"}); err != nil {
		t.Fatalf("StopPodSandbox: %v", err)
	}
	if len(cleanupCalls) != 2 {
		t.Fatalf("cleanup call count = %d, want 2: %+v", len(cleanupCalls), cleanupCalls)
	}
	if pod.hostPort != 0 || len(pod.portMappings) != 0 {
		t.Fatalf("hostport fields should be cleared after stop: hostPort=%d mappings=%+v", pod.hostPort, pod.portMappings)
	}
}

func TestGRPCE2BListPodSandboxMarksMissingActiveSandboxNotReady(t *testing.T) {
	client := &fakeSandboxServiceClient{listResp: &orchestrator.SandboxListResponse{
		Sandboxes: []*orchestrator.RunningSandbox{{Config: &orchestrator.SandboxConfig{SandboxId: "active-e2b"}}},
	}}
	e := newTestGRPCE2BEngine(client)
	e.tracker.Add("active", &podInfo{sandboxID: "active", e2bSandboxID: "active-e2b", name: "active", createdAt: time.Now(), state: stateRunning})
	e.tracker.Add("missing", &podInfo{sandboxID: "missing", e2bSandboxID: "missing-e2b", name: "missing", createdAt: time.Now(), state: stateRunning})

	resp, err := e.ListPodSandbox(context.Background(), &runtime.ListPodSandboxRequest{})
	if err != nil {
		t.Fatalf("ListPodSandbox: %v", err)
	}
	states := map[string]runtime.PodSandboxState{}
	for _, item := range resp.Items {
		states[item.Id] = item.State
	}
	if states["active"] != runtime.PodSandboxState_SANDBOX_READY {
		t.Fatalf("active state = %v", states["active"])
	}
	if states["missing"] != runtime.PodSandboxState_SANDBOX_NOTREADY {
		t.Fatalf("missing state = %v", states["missing"])
	}
}

func TestGRPCE2BRemovePodSandboxDeletesOrchestratorAndCNI(t *testing.T) {
	client := &fakeSandboxServiceClient{}
	fakeCNI := &fakeCNIManager{}
	e := newTestGRPCE2BEngine(client)
	e.cniConfig.Enabled = true
	e.cniManager = fakeCNI
	e.tracker.Add("uid-a", &podInfo{
		sandboxID:    "uid-a",
		e2bSandboxID: "e2buid",
		state:        stateRunning,
		cniEnabled:   true,
		cniRecord:    &CNIRecord{SandboxID: "uid-a", NetNSPath: "/var/run/netns/e2b-uid-a"},
	})

	if _, err := e.RemovePodSandbox(context.Background(), &runtime.RemovePodSandboxRequest{PodSandboxId: "uid-a"}); err != nil {
		t.Fatalf("RemovePodSandbox: %v", err)
	}
	if client.deleteCalls != 1 || client.lastDelete.SandboxId != "e2buid" {
		t.Fatalf("delete mismatch: calls=%d req=%+v", client.deleteCalls, client.lastDelete)
	}
	if fakeCNI.delCalls != 1 {
		t.Fatalf("CNI del calls = %d, want 1", fakeCNI.delCalls)
	}
	if _, ok := e.tracker.Get("uid-a"); ok {
		t.Fatal("tracker should delete pod")
	}
}

func TestGRPCE2BImageCacheWithFakeOrchestrator(t *testing.T) {
	client := &fakeSandboxServiceClient{buildsResp: &orchestrator.SandboxListCachedBuildsResponse{
		Builds: []*orchestrator.CachedBuildInfo{{BuildId: "build-a", ExpirationTime: timestamppb.Now()}},
	}}
	e := newTestGRPCE2BEngine(client)
	image := &runtime.ImageSpec{Image: "e2b.dev/tmpl-a:build-a"}

	pullResp, err := e.PullImage(context.Background(), &runtime.PullImageRequest{Image: image})
	if err != nil {
		t.Fatalf("PullImage: %v", err)
	}
	if pullResp.ImageRef != image.Image {
		t.Fatalf("image ref = %q", pullResp.ImageRef)
	}
	statusResp, err := e.ImageStatus(context.Background(), &runtime.ImageStatusRequest{Image: image})
	if err != nil {
		t.Fatalf("ImageStatus: %v", err)
	}
	if statusResp.Image == nil || statusResp.Image.Id != image.Image {
		t.Fatalf("image status mismatch: %+v", statusResp.Image)
	}
	listResp, err := e.ListImages(context.Background(), &runtime.ListImagesRequest{})
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(listResp.Images) < 2 {
		t.Fatalf("expected cached builds plus image cache entries, got %+v", listResp.Images)
	}
	if _, err := e.RemoveImage(context.Background(), &runtime.RemoveImageRequest{Image: image}); err != nil {
		t.Fatalf("RemoveImage: %v", err)
	}
	statusResp, err = e.ImageStatus(context.Background(), &runtime.ImageStatusRequest{Image: image})
	if err != nil {
		t.Fatalf("ImageStatus after remove: %v", err)
	}
	if statusResp.Image != nil {
		t.Fatalf("removed image should not be present: %+v", statusResp.Image)
	}
}

func TestGRPCE2BExecAttachCreateStreamingTokens(t *testing.T) {
	e := newTestGRPCE2BEngine(&fakeSandboxServiceClient{})
	e.nodeIP = "127.0.0.1"
	e.tracker.Add("uid-a", &podInfo{sandboxID: "uid-a", state: stateRunning, envdAccessToken: "token-a"})
	t.Cleanup(func() {
		if e.streamingListener != nil {
			_ = e.streamingListener.Close()
		}
	})

	execResp, err := e.Exec(context.Background(), &runtime.ExecRequest{ContainerId: "uid-a-c", Cmd: []string{"echo", "hi"}, Stdout: true})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(execResp.Url, "/exec/") {
		t.Fatalf("exec URL = %q", execResp.Url)
	}
	e.streamingMu.RLock()
	if len(e.streamingReqs) != 1 {
		t.Fatalf("streaming req count = %d, want 1", len(e.streamingReqs))
	}
	e.streamingMu.RUnlock()

	attachResp, err := e.Attach(context.Background(), &runtime.AttachRequest{ContainerId: "uid-a-c", Stdin: true, Stdout: true})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if !strings.Contains(attachResp.Url, "/attach/") {
		t.Fatalf("attach URL = %q", attachResp.Url)
	}
	e.streamingMu.RLock()
	if len(e.attachReqs) != 1 {
		t.Fatalf("attach req count = %d, want 1", len(e.attachReqs))
	}
	e.streamingMu.RUnlock()
}

func TestGRPCE2BExecSyncWithMockEnvd(t *testing.T) {
	var responseBody bytes.Buffer
	for _, event := range []*process.ProcessEvent{
		{Event: &process.ProcessEvent_Start{Start: &process.ProcessEvent_StartEvent{Pid: 123}}},
		{Event: &process.ProcessEvent_Data{Data: &process.ProcessEvent_DataEvent{
			Output: &process.ProcessEvent_DataEvent_Stdout{Stdout: []byte("out")},
		}}},
		{Event: &process.ProcessEvent_Data{Data: &process.ProcessEvent_DataEvent{
			Output: &process.ProcessEvent_DataEvent_Stderr{Stderr: []byte("err")},
		}}},
		{Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{ExitCode: 7}}},
	} {
		payload, err := proto.Marshal(&process.StartResponse{Event: event})
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		if err := writeGRPCFrame(&responseBody, payload); err != nil {
			t.Fatalf("write frame: %v", err)
		}
	}

	e := newTestGRPCE2BEngine(&fakeSandboxServiceClient{})
	e.tracker.Add("uid-a", &podInfo{sandboxID: "uid-a", state: stateRunning, envdAccessToken: "token-a"})
	e.envdHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/process.Process/Start" || req.Header.Get("X-Access-Token") != "token-a" {
			t.Fatalf("unexpected envd request path=%s token=%s", req.URL.Path, req.Header.Get("X-Access-Token"))
		}
		return httpResponse(http.StatusOK, responseBody.Bytes()), nil
	})}

	resp, err := e.ExecSync(context.Background(), &runtime.ExecSyncRequest{ContainerId: "uid-a-c", Cmd: []string{"echo", "hi"}, Timeout: 1})
	if err != nil {
		t.Fatalf("ExecSync: %v", err)
	}
	if string(resp.Stdout) != "out" || string(resp.Stderr) != "err" || resp.ExitCode != 7 {
		t.Fatalf("ExecSync response mismatch: stdout=%q stderr=%q exit=%d", resp.Stdout, resp.Stderr, resp.ExitCode)
	}
}

func TestGRPCE2BExecSyncErrors(t *testing.T) {
	e := newTestGRPCE2BEngine(&fakeSandboxServiceClient{})
	if _, err := e.ExecSync(context.Background(), &runtime.ExecSyncRequest{ContainerId: "missing-c"}); status.Code(err) != codes.NotFound {
		t.Fatalf("missing sandbox code = %v", status.Code(err))
	}

	e.tracker.Add("stopped", &podInfo{sandboxID: "stopped", state: stateStopped})
	if _, err := e.ExecSync(context.Background(), &runtime.ExecSyncRequest{ContainerId: "stopped-c"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("stopped sandbox code = %v", status.Code(err))
	}

	e.tracker.Add("running", &podInfo{sandboxID: "running", state: stateRunning})
	e.envdHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := e.ExecSync(ctx, &runtime.ExecSyncRequest{ContainerId: "running-c", Timeout: 1}); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("envd timeout code = %v", status.Code(err))
	}
}

func TestGRPCE2BDoListRequestRawAndFramedResponses(t *testing.T) {
	rawPayload, err := proto.Marshal(&process.ListResponse{Processes: []*process.ProcessInfo{{Pid: 111}}})
	if err != nil {
		t.Fatalf("marshal raw ListResponse: %v", err)
	}
	var framed bytes.Buffer
	framedPayload, err := proto.Marshal(&process.ListResponse{Processes: []*process.ProcessInfo{{Pid: 222}}})
	if err != nil {
		t.Fatalf("marshal framed ListResponse: %v", err)
	}
	if err := writeGRPCFrame(&framed, framedPayload); err != nil {
		t.Fatalf("write framed ListResponse: %v", err)
	}

	for _, tc := range []struct {
		name string
		body []byte
		want uint32
	}{
		{name: "raw", body: rawPayload, want: 111},
		{name: "framed", body: framed.Bytes(), want: 222},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestGRPCE2BEngine(&fakeSandboxServiceClient{})
			e.envdHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path != "/process.Process/List" || req.Header.Get("X-Access-Token") != "token-a" {
					t.Fatalf("unexpected List request path=%s token=%s", req.URL.Path, req.Header.Get("X-Access-Token"))
				}
				return httpResponse(http.StatusOK, tc.body), nil
			})}
			resp, err := e.doListRequest(context.Background(), "sandbox-a", "token-a")
			if err != nil {
				t.Fatalf("doListRequest: %v", err)
			}
			if len(resp.Processes) != 1 || resp.Processes[0].Pid != tc.want {
				t.Fatalf("processes = %+v, want pid %d", resp.Processes, tc.want)
			}
		})
	}
}

func TestGRPCE2BDoListRequestErrors(t *testing.T) {
	e := newTestGRPCE2BEngine(&fakeSandboxServiceClient{})
	e.envdHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return httpResponse(http.StatusInternalServerError, []byte("boom")), nil
	})}
	if _, err := e.doListRequest(context.Background(), "sandbox-a", "token-a"); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected HTTP status error, got %v", err)
	}
}

func TestGRPCE2BSendInputToEnvd(t *testing.T) {
	var requests []*process.SendInputRequest
	e := newTestGRPCE2BEngine(&fakeSandboxServiceClient{})
	e.envdHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/process.Process/SendInput" || req.Header.Get("X-Access-Token") != "token-a" {
			t.Fatalf("unexpected SendInput request path=%s token=%s", req.URL.Path, req.Header.Get("X-Access-Token"))
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		var input process.SendInputRequest
		if err := proto.Unmarshal(body, &input); err != nil {
			t.Fatalf("unmarshal SendInputRequest: %v", err)
		}
		requests = append(requests, &input)
		return httpResponse(http.StatusOK, nil), nil
	})}

	e.sendInputToEnvd(context.Background(), "sandbox-a", 123, "token-a", []byte("stdin"), false)
	e.sendInputToEnvd(context.Background(), "sandbox-a", 123, "token-a", []byte("pty"), true)

	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	if got := requests[0].Input.GetStdin(); string(got) != "stdin" {
		t.Fatalf("stdin payload = %q", got)
	}
	if got := requests[1].Input.GetPty(); string(got) != "pty" {
		t.Fatalf("pty payload = %q", got)
	}
	for i, req := range requests {
		if req.Process.GetPid() != 123 {
			t.Fatalf("request %d pid = %d, want 123", i, req.Process.GetPid())
		}
	}
}
