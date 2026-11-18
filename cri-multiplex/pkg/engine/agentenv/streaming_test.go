package agentenv

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	runtimeapi "github.com/cri-multiplex/pkg/agentenv/runtime/v1"
)

func startStreamingTestContainer(t *testing.T) (*Engine, string) {
	t.Helper()
	instance, _, _ := newReadyContainerTestEngine(t)
	instance.exec = &fakeExec{startProcess: func(context.Context, *runtimeapi.SandboxProcessStartRequest) (*runtimeapi.SandboxProcessStartResponse, error) {
		return &runtimeapi.SandboxProcessStartResponse{Pid: 4242}, nil
	}}
	created, err := instance.CreateContainer(context.Background(), criCreateContainerRequest("cri-1", "workload", "agentenv.dev/app:v1", 0))
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if _, err := instance.StartContainer(context.Background(), &runtime.StartContainerRequest{ContainerId: created.GetContainerId()}); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	return instance, created.GetContainerId()
}

func TestExecAndAttachRegisterStreamingRequests(t *testing.T) {
	instance, containerID := startStreamingTestContainer(t)
	defer instance.Close()

	execResponse, err := instance.Exec(context.Background(), &runtime.ExecRequest{
		ContainerId: containerID,
		Cmd:         []string{"/bin/sh", "-c", "printf ok"},
		Tty:         true,
		Stdin:       true,
		Stdout:      true,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	parsed, err := url.Parse(execResponse.GetUrl())
	if err != nil {
		t.Fatalf("parse exec URL: %v", err)
	}
	execToken := strings.TrimPrefix(parsed.Path, "/exec/")
	request, ok := instance.getStreamRequest(execToken)
	if !ok {
		t.Fatal("exec stream request not registered")
	}
	if request.attach || !request.tty || !request.stdin || !request.stdout || len(request.command) != 3 {
		t.Fatalf("unexpected exec request: %+v", request)
	}

	attachResponse, err := instance.Attach(context.Background(), &runtime.AttachRequest{
		ContainerId: containerID,
		Stdin:       true,
		Stdout:      true,
	})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	parsed, err = url.Parse(attachResponse.GetUrl())
	if err != nil {
		t.Fatalf("parse attach URL: %v", err)
	}
	attachToken := strings.TrimPrefix(parsed.Path, "/attach/")
	request, ok = instance.getStreamRequest(attachToken)
	if !ok || !request.attach {
		t.Fatalf("attach stream request missing or not attach: %+v ok=%v", request, ok)
	}
}

func TestSPDYOptionalGraceOnlyForTTY(t *testing.T) {
	if got := (&streamSession{}).spdyOptionalGrace(); got != 0 {
		t.Fatalf("non-TTY grace = %v, want 0", got)
	}
	if got := (&streamSession{tty: true}).spdyOptionalGrace(); got != spdyOptionalStreamGrace {
		t.Fatalf("TTY grace = %v, want %v", got, spdyOptionalStreamGrace)
	}
}

type testSPDYStream struct {
	headers http.Header
}

func (s *testSPDYStream) Read([]byte) (int, error)    { return 0, io.EOF }
func (s *testSPDYStream) Write(p []byte) (int, error) { return len(p), nil }
func (s *testSPDYStream) Close() error                { return nil }
func (s *testSPDYStream) Reset() error                { return nil }
func (s *testSPDYStream) Headers() http.Header        { return s.headers }
func (s *testSPDYStream) Identifier() uint32          { return 1 }

func TestCollectSPDYStreamsNonTTYReturnsWithoutGrace(t *testing.T) {
	reply := make(chan struct{})
	close(reply)
	streams := make(chan spdyStreamAndReply, 2)
	streams <- spdyStreamAndReply{
		stream:    &testSPDYStream{headers: http.Header{http.CanonicalHeaderKey(corev1.StreamType): []string{corev1.StreamTypeError}}},
		replySent: reply,
	}
	streams <- spdyStreamAndReply{
		stream:    &testSPDYStream{headers: http.Header{http.CanonicalHeaderKey(corev1.StreamType): []string{corev1.StreamTypeStdout}}},
		replySent: reply,
	}

	session := &streamSession{stdout: true}
	started := time.Now()
	if err := session.collectSPDYStreams(context.Background(), &spdyStreamTransport{}, streams); err != nil {
		t.Fatalf("collectSPDYStreams: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 200*time.Millisecond {
		t.Fatalf("non-TTY stream collection took %v, expected immediate return", elapsed)
	}
}

func TestPortForwardRegistersAuthorizedPorts(t *testing.T) {
	instance, _, _ := newReadyContainerTestEngine(t)
	defer instance.Close()
	pod, ok := instance.getPod("cri-1")
	if !ok {
		t.Fatal("pod projection not found")
	}
	pod.State = criStateReady
	if err := instance.putPod(pod); err != nil {
		t.Fatalf("putPod: %v", err)
	}

	response, err := instance.PortForward(context.Background(), &runtime.PortForwardRequest{
		PodSandboxId: "cri-1",
		Port:         []int32{8080, 8443},
	})
	if err != nil {
		t.Fatalf("PortForward: %v", err)
	}
	parsed, err := url.Parse(response.GetUrl())
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	token := strings.TrimPrefix(parsed.Path, "/portforward/")
	request, ok := instance.getPortForwardRequest(token)
	if !ok {
		t.Fatal("port-forward request not registered")
	}
	if request.podIP == "" || len(request.ports) != 2 {
		t.Fatalf("unexpected port-forward request: %+v", request)
	}
	if _, ok := request.ports[8080]; !ok {
		t.Fatalf("authorized ports = %#v", request.ports)
	}
}

func TestPortForwardRejectsInvalidPorts(t *testing.T) {
	instance, _, _ := newReadyContainerTestEngine(t)
	defer instance.Close()
	for _, ports := range [][]int32{{0}, {65536}} {
		_, err := instance.PortForward(context.Background(), &runtime.PortForwardRequest{
			PodSandboxId: "cri-1",
			Port:         ports,
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("ports=%v code=%v, want InvalidArgument", ports, status.Code(err))
		}
	}
}

func TestExecRejectsInvalidRequests(t *testing.T) {
	instance, _, _ := newCRITestEngine(t, &fakeLifecycle{})
	tests := []*runtime.ExecRequest{
		nil,
		{},
		{ContainerId: "missing", Cmd: []string{"/bin/sh"}},
		{ContainerId: "missing"},
	}
	for _, request := range tests {
		_, err := instance.Exec(context.Background(), request)
		if status.Code(err) != codes.InvalidArgument && status.Code(err) != codes.NotFound {
			t.Fatalf("request %+v code = %v, want InvalidArgument or NotFound", request, status.Code(err))
		}
	}
}

func TestExecWebSocketForwardsOutputAndExit(t *testing.T) {
	instance, containerID := startStreamingTestContainer(t)
	defer instance.Close()
	instance.exec.(*fakeExec).connectProcess = func(_ context.Context, req *runtimeapi.SandboxProcessConnectRequest) (runtimeapi.ProcessEventStream, error) {
		if req.GetPid() != 4242 {
			t.Fatalf("pid = %d, want 4242", req.GetPid())
		}
		return &fakeProcessEventStream{events: []*runtimeapi.SandboxProcessConnectResponse{
			{Event: &runtimeapi.SandboxProcessConnectResponse_Output{Output: &runtimeapi.SandboxProcessOutputChunk{
				Sequence: 1,
				Stream:   runtimeapi.SandboxProcessOutputStream_SANDBOX_PROCESS_OUTPUT_STREAM_STDOUT,
				Data:     []byte("ok"),
			}}},
			{Event: &runtimeapi.SandboxProcessConnectResponse_Exit{Exit: &runtimeapi.SandboxProcessExitInfo{Exited: true, ExitCode: 0}}},
		}}, nil
	}

	response, err := instance.Exec(context.Background(), &runtime.ExecRequest{
		ContainerId: containerID,
		Cmd:         []string{"/bin/sh", "-c", "printf ok"},
		Stdout:      true,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	parsed, err := url.Parse(response.GetUrl())
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	parsed.Scheme = "ws"
	dialer := websocket.Dialer{Subprotocols: []string{streamingV5Subprotocol}}
	conn, _, err := dialer.Dial(parsed.String(), nil)
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	sawOutput := false
	sawStatus := false
	for !sawStatus {
		_, message, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if len(message) == 0 {
			continue
		}
		switch message[0] {
		case channelStdOut:
			if string(message[1:]) != "ok" {
				t.Fatalf("stdout = %q", message[1:])
			}
			sawOutput = true
		case channelErr:
			sawStatus = strings.Contains(string(message[1:]), `"status":"Success"`)
		}
	}
	if !sawOutput || !sawStatus {
		t.Fatalf("sawOutput=%v sawStatus=%v", sawOutput, sawStatus)
	}
}
