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
		call   func(runtimeapi.SandboxLifecycleServiceClient) error
	}{
		{
			name:   "create",
			method: "/agentenv.runtime.v1.SandboxLifecycleService/Create",
			call: func(client runtimeapi.SandboxLifecycleServiceClient) error {
				_, err := client.Create(context.Background(), &runtimeapi.SandboxCreateRequest{})
				return err
			},
		},
		{
			name:   "get",
			method: "/agentenv.runtime.v1.SandboxLifecycleService/Get",
			call: func(client runtimeapi.SandboxLifecycleServiceClient) error {
				_, err := client.Get(context.Background(), &runtimeapi.SandboxGetRequest{})
				return err
			},
		},
		{
			name:   "delete",
			method: "/agentenv.runtime.v1.SandboxLifecycleService/Delete",
			call: func(client runtimeapi.SandboxLifecycleServiceClient) error {
				_, err := client.Delete(context.Background(), &runtimeapi.SandboxDeleteRequest{})
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := &recordingClientConn{}
			if err := tt.call(runtimeapi.NewSandboxLifecycleServiceClient(conn)); err != nil {
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
	if _, err := runtimeapi.NewSandboxAdminServiceClient(conn).Capabilities(context.Background(), &emptypb.Empty{}); err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if conn.method != "/agentenv.runtime.v1.SandboxAdminService/Capabilities" {
		t.Fatalf("method = %q", conn.method)
	}
}
