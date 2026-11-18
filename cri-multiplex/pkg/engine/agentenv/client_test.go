package agentenv

import (
	"context"
	runtimeapi "github.com/cri-multiplex/pkg/agentenv/runtime/v1"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

type recordingClientConn struct {
	method string
	reply  proto.Message
	stream string
}

type recordingClientStream struct {
	grpc.ClientStream
}

func (s *recordingClientStream) SendMsg(any) error { return nil }
func (s *recordingClientStream) CloseSend() error  { return nil }

func (c *recordingClientConn) Invoke(_ context.Context, method string, _ any, reply any, _ ...grpc.CallOption) error {
	c.method = method
	if c.reply == nil {
		return nil
	}
	proto.Merge(reply.(proto.Message), c.reply)
	return nil
}

func (c *recordingClientConn) NewStream(_ context.Context, _ *grpc.StreamDesc, method string, _ ...grpc.CallOption) (grpc.ClientStream, error) {
	c.stream = method
	return &recordingClientStream{}, nil
}

func TestLifecycleClientUsesContractMethodNames(t *testing.T) {
	tests := []struct {
		name   string
		method string
		call   func(*LifecycleClient) error
	}{
		{
			name:   "create",
			method: "/agentenv.runtime.v1.SandboxLifecycleService/Create",
			call: func(client *LifecycleClient) error {
				_, err := client.Create(context.Background(), &runtimeapi.SandboxCreateRequest{})
				return err
			},
		},
		{
			name:   "get",
			method: "/agentenv.runtime.v1.SandboxLifecycleService/Get",
			call: func(client *LifecycleClient) error {
				_, err := client.Get(context.Background(), &runtimeapi.SandboxGetRequest{})
				return err
			},
		},
		{
			name:   "delete",
			method: "/agentenv.runtime.v1.SandboxLifecycleService/Delete",
			call: func(client *LifecycleClient) error {
				_, err := client.Delete(context.Background(), &runtimeapi.SandboxDeleteRequest{})
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := &recordingClientConn{}
			if err := tt.call(NewLifecycleClient(conn)); err != nil {
				t.Fatalf("call failed: %v", err)
			}
			if conn.method != tt.method {
				t.Fatalf("method = %q, want %q", conn.method, tt.method)
			}
		})
	}
}

func TestAdminClientUsesContractMethodNames(t *testing.T) {
	conn := &recordingClientConn{}
	if _, err := NewAdminClient(conn).Capabilities(context.Background(), &emptypb.Empty{}); err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if conn.method != "/agentenv.runtime.v1.SandboxAdminService/Capabilities" {
		t.Fatalf("method = %q", conn.method)
	}
}

func TestExecClientUsesContractMethodName(t *testing.T) {
	conn := &recordingClientConn{}
	if _, err := NewExecClient(conn).ExecSync(context.Background(), &runtimeapi.SandboxExecSyncRequest{}); err != nil {
		t.Fatalf("ExecSync: %v", err)
	}
	if conn.method != "/agentenv.runtime.v1.SandboxExecService/ExecSync" {
		t.Fatalf("method = %q", conn.method)
	}
}
func TestExecClientConnectProcessUsesServerStreamMethodName(t *testing.T) {
	conn := &recordingClientConn{}
	stream, err := NewExecClient(conn).ConnectProcess(context.Background(), &runtimeapi.SandboxProcessConnectRequest{})
	if err != nil {
		t.Fatalf("ConnectProcess: %v", err)
	}
	if stream == nil {
		t.Fatal("ConnectProcess returned a nil stream")
	}
	const expected = "/agentenv.runtime.v1.SandboxExecService/ConnectProcess"
	if conn.stream != expected {
		t.Fatalf("stream method = %q, want %q", conn.stream, expected)
	}
}

func TestExecClientProcessMethodsUseContractMethodNames(t *testing.T) {
	tests := []struct {
		name   string
		method string
		call   func(*ExecClient) error
	}{
		{
			name:   "start-process",
			method: "/agentenv.runtime.v1.SandboxExecService/StartProcess",
			call: func(client *ExecClient) error {
				_, err := client.StartProcess(context.Background(), &runtimeapi.SandboxProcessStartRequest{})
				return err
			},
		},
		{
			name:   "get-process",
			method: "/agentenv.runtime.v1.SandboxExecService/GetProcess",
			call: func(client *ExecClient) error {
				_, err := client.GetProcess(context.Background(), &runtimeapi.SandboxProcessGetRequest{})
				return err
			},
		},
		{
			name:   "send-input",
			method: "/agentenv.runtime.v1.SandboxExecService/SendProcessInput",
			call: func(client *ExecClient) error {
				_, err := client.SendProcessInput(context.Background(), &runtimeapi.SandboxProcessInputRequest{})
				return err
			},
		},
		{
			name:   "resize-process",
			method: "/agentenv.runtime.v1.SandboxExecService/ResizeProcess",
			call: func(client *ExecClient) error {
				_, err := client.ResizeProcess(context.Background(), &runtimeapi.SandboxProcessResizeRequest{})
				return err
			},
		},
		{
			name:   "close-stdin",
			method: "/agentenv.runtime.v1.SandboxExecService/CloseProcessStdin",
			call: func(client *ExecClient) error {
				_, err := client.CloseProcessStdin(context.Background(), &runtimeapi.SandboxProcessCloseStdinRequest{})
				return err
			},
		},
		{
			name:   "stop-process",
			method: "/agentenv.runtime.v1.SandboxExecService/StopProcess",
			call: func(client *ExecClient) error {
				_, err := client.StopProcess(context.Background(), &runtimeapi.SandboxProcessStopRequest{})
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := &recordingClientConn{}
			if err := tt.call(NewExecClient(conn)); err != nil {
				t.Fatalf("call failed: %v", err)
			}
			if conn.method != tt.method {
				t.Fatalf("method = %q, want %q", conn.method, tt.method)
			}
		})
	}
}
