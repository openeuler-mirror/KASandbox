package agentenv

import (
	"context"
	"io"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	runtimeapi "github.com/cri-multiplex/pkg/agentenv/runtime/v1"
)

type fakeExec struct {
	execSync          func(context.Context, *runtimeapi.SandboxExecSyncRequest) (*runtimeapi.SandboxExecSyncResponse, error)
	startProcess      func(context.Context, *runtimeapi.SandboxProcessStartRequest) (*runtimeapi.SandboxProcessStartResponse, error)
	getProcess        func(context.Context, *runtimeapi.SandboxProcessGetRequest) (*runtimeapi.SandboxProcessGetResponse, error)
	connectProcess    func(context.Context, *runtimeapi.SandboxProcessConnectRequest) (ProcessEventStream, error)
	sendProcessInput  func(context.Context, *runtimeapi.SandboxProcessInputRequest) (*runtimeapi.SandboxProcessInputResponse, error)
	resizeProcess     func(context.Context, *runtimeapi.SandboxProcessResizeRequest) (*runtimeapi.SandboxProcessResizeResponse, error)
	closeProcessStdin func(context.Context, *runtimeapi.SandboxProcessCloseStdinRequest) (*runtimeapi.SandboxProcessCloseStdinResponse, error)
	stopProcess       func(context.Context, *runtimeapi.SandboxProcessStopRequest) (*runtimeapi.SandboxProcessStopResponse, error)
}

type fakeProcessEventStream struct {
	events []*runtimeapi.SandboxProcessConnectResponse
	index  int
}

func (s *fakeProcessEventStream) Recv() (*runtimeapi.SandboxProcessConnectResponse, error) {
	if s.index >= len(s.events) {
		return nil, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (f *fakeExec) ExecSync(ctx context.Context, req *runtimeapi.SandboxExecSyncRequest, _ ...grpc.CallOption) (*runtimeapi.SandboxExecSyncResponse, error) {
	if f.execSync == nil {
		return &runtimeapi.SandboxExecSyncResponse{}, nil
	}
	return f.execSync(ctx, req)
}

func (f *fakeExec) StartProcess(ctx context.Context, req *runtimeapi.SandboxProcessStartRequest, _ ...grpc.CallOption) (*runtimeapi.SandboxProcessStartResponse, error) {
	if f.startProcess == nil {
		return &runtimeapi.SandboxProcessStartResponse{Pid: 4242}, nil
	}
	return f.startProcess(ctx, req)
}

func (f *fakeExec) GetProcess(ctx context.Context, req *runtimeapi.SandboxProcessGetRequest, _ ...grpc.CallOption) (*runtimeapi.SandboxProcessGetResponse, error) {
	if f.getProcess == nil {
		return &runtimeapi.SandboxProcessGetResponse{Pid: req.GetPid(), Running: true}, nil
	}
	return f.getProcess(ctx, req)
}

func (f *fakeExec) ConnectProcess(ctx context.Context, req *runtimeapi.SandboxProcessConnectRequest, _ ...grpc.CallOption) (ProcessEventStream, error) {
	if f.connectProcess == nil {
		return &fakeProcessEventStream{}, nil
	}
	return f.connectProcess(ctx, req)
}

func (f *fakeExec) SendProcessInput(ctx context.Context, req *runtimeapi.SandboxProcessInputRequest, _ ...grpc.CallOption) (*runtimeapi.SandboxProcessInputResponse, error) {
	if f.sendProcessInput == nil {
		return &runtimeapi.SandboxProcessInputResponse{}, nil
	}
	return f.sendProcessInput(ctx, req)
}

func (f *fakeExec) ResizeProcess(ctx context.Context, req *runtimeapi.SandboxProcessResizeRequest, _ ...grpc.CallOption) (*runtimeapi.SandboxProcessResizeResponse, error) {
	if f.resizeProcess == nil {
		return &runtimeapi.SandboxProcessResizeResponse{}, nil
	}
	return f.resizeProcess(ctx, req)
}

func (f *fakeExec) CloseProcessStdin(ctx context.Context, req *runtimeapi.SandboxProcessCloseStdinRequest, _ ...grpc.CallOption) (*runtimeapi.SandboxProcessCloseStdinResponse, error) {
	if f.closeProcessStdin == nil {
		return &runtimeapi.SandboxProcessCloseStdinResponse{}, nil
	}
	return f.closeProcessStdin(ctx, req)
}

func (f *fakeExec) StopProcess(ctx context.Context, req *runtimeapi.SandboxProcessStopRequest, _ ...grpc.CallOption) (*runtimeapi.SandboxProcessStopResponse, error) {
	if f.stopProcess == nil {
		return &runtimeapi.SandboxProcessStopResponse{Stopped: true, ExitCode: 0}, nil
	}
	return f.stopProcess(ctx, req)
}

func TestExecSyncUsesContainerRuntimeFactsAndNativeID(t *testing.T) {
	instance, _, _ := newReadyContainerTestEngine(t)
	create := criCreateContainerRequest("cri-1", "app", "agentenv.dev/app:v1", 0)
	create.Config.Envs = []*runtime.KeyValue{
		{Key: "APP_ENV", Value: "test"},
		{Key: "EMPTY", Value: ""},
	}
	create.Config.WorkingDir = "/workspace"
	created, err := instance.CreateContainer(context.Background(), create)
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if _, err := instance.StartContainer(context.Background(), &runtime.StartContainerRequest{ContainerId: created.GetContainerId()}); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}

	instance.exec = &fakeExec{execSync: func(_ context.Context, req *runtimeapi.SandboxExecSyncRequest) (*runtimeapi.SandboxExecSyncResponse, error) {
		if req.GetNativeSandboxId() != "019a0000-0000-7000-8000-000000000201" {
			t.Fatalf("native id = %q", req.GetNativeSandboxId())
		}
		if got := req.GetCommand(); len(got) != 3 || got[0] != "/bin/sh" || got[2] != "exit 7" {
			t.Fatalf("command = %#v", got)
		}
		if req.GetEnvs()["APP_ENV"] != "test" {
			t.Fatalf("env = %#v", req.GetEnvs())
		}
		if req.GetCwd() != "/workspace" || req.GetTimeoutSeconds() != 12 {
			t.Fatalf("cwd=%q timeout=%d", req.GetCwd(), req.GetTimeoutSeconds())
		}
		return &runtimeapi.SandboxExecSyncResponse{Stdout: []byte("ok"), Stderr: []byte("warn"), ExitCode: 7}, nil
	}}

	response, err := instance.ExecSync(context.Background(), &runtime.ExecSyncRequest{
		ContainerId: created.GetContainerId(),
		Cmd:         []string{"/bin/sh", "-c", "exit 7"},
		Timeout:     12,
	})
	if err != nil {
		t.Fatalf("ExecSync: %v", err)
	}
	if string(response.GetStdout()) != "ok" || string(response.GetStderr()) != "warn" || response.GetExitCode() != 7 {
		t.Fatalf("response = %+v", response)
	}
}

func TestExecSyncRequiresRunningContainerAndValidRequest(t *testing.T) {
	instance, _, _ := newReadyContainerTestEngine(t)
	created, err := instance.CreateContainer(context.Background(), criCreateContainerRequest("cri-1", "app", "agentenv.dev/app:v1", 0))
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	instance.exec = &fakeExec{execSync: func(context.Context, *runtimeapi.SandboxExecSyncRequest) (*runtimeapi.SandboxExecSyncResponse, error) {
		t.Fatal("runtime must not be called for invalid request or non-running container")
		return nil, nil
	}}

	if _, err := instance.ExecSync(context.Background(), &runtime.ExecSyncRequest{ContainerId: created.GetContainerId(), Cmd: []string{"true"}}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("created container code = %v, want FailedPrecondition", status.Code(err))
	}
	if _, err := instance.ExecSync(context.Background(), &runtime.ExecSyncRequest{ContainerId: created.GetContainerId()}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty command code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestExecSyncMapsRuntimeErrors(t *testing.T) {
	instance, _, _ := newReadyContainerTestEngine(t)
	created, err := instance.CreateContainer(context.Background(), criCreateContainerRequest("cri-1", "app", "agentenv.dev/app:v1", 0))
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if _, err := instance.StartContainer(context.Background(), &runtime.StartContainerRequest{ContainerId: created.GetContainerId()}); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	instance.exec = &fakeExec{execSync: func(context.Context, *runtimeapi.SandboxExecSyncRequest) (*runtimeapi.SandboxExecSyncResponse, error) {
		return nil, status.Error(codes.DeadlineExceeded, "exec timed out")
	}}
	if _, err := instance.ExecSync(context.Background(), &runtime.ExecSyncRequest{ContainerId: created.GetContainerId(), Cmd: []string{"sleep", "10"}}); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("runtime error code = %v, want DeadlineExceeded", status.Code(err))
	}
}
