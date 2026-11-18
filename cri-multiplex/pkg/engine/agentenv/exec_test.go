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
	startProcess   func(context.Context, *runtimeapi.SandboxProcessStartRequest) (*runtimeapi.SandboxProcessStartResponse, error)
	getProcess     func(context.Context, *runtimeapi.SandboxProcessGetRequest) (*runtimeapi.SandboxProcessGetResponse, error)
	connectProcess func(context.Context, *runtimeapi.SandboxProcessConnectRequest) (ProcessEventStream, error)
	stopProcess    func(context.Context, *runtimeapi.SandboxProcessStopRequest) (*runtimeapi.SandboxProcessStopResponse, error)
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

func (f *fakeExec) StopProcess(ctx context.Context, req *runtimeapi.SandboxProcessStopRequest, _ ...grpc.CallOption) (*runtimeapi.SandboxProcessStopResponse, error) {
	if f.stopProcess == nil {
		return &runtimeapi.SandboxProcessStopResponse{Stopped: true, ExitCode: 0}, nil
	}
	return f.stopProcess(ctx, req)
}

// 即使 Runtime 未连接，CRI 命令入口也必须明确返回不支持。
func TestCRIExecutionUsesSDK(t *testing.T) {
	e := &Engine{}
	_, syncErr := e.ExecSync(context.Background(), &runtime.ExecSyncRequest{ContainerId: "c", Cmd: []string{"true"}})
	_, execErr := e.Exec(context.Background(), &runtime.ExecRequest{ContainerId: "c", Cmd: []string{"true"}})
	_, attachErr := e.Attach(context.Background(), &runtime.AttachRequest{ContainerId: "c"})
	for name, err := range map[string]error{"ExecSync": syncErr, "Exec": execErr, "Attach": attachErr} {
		if status.Code(err) != codes.Unimplemented {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if e.streamingServer != nil {
		t.Fatal("命令入口不应启动 streaming server")
	}
}
