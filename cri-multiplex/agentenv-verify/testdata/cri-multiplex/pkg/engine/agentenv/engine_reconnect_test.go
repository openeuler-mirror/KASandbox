package agentenv

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	runtimeapi "github.com/cri-multiplex/pkg/agentenv/runtime/v1"
	"github.com/cri-multiplex/pkg/engine"
)

type reconnectCapabilitiesServer struct{}

func (*reconnectCapabilitiesServer) Capabilities(context.Context, *emptypb.Empty) (*runtimeapi.SandboxCapabilities, error) {
	return &runtimeapi.SandboxCapabilities{}, nil
}

type reconnectCapabilitiesService interface{}

var reconnectCapabilitiesServiceDesc = grpc.ServiceDesc{
	ServiceName: "agentenv.runtime.v1.SandboxAdminService",
	HandlerType: (*reconnectCapabilitiesService)(nil),
	Methods: []grpc.MethodDesc{{
		MethodName: "Capabilities",
		Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
			in := new(emptypb.Empty)
			if err := dec(in); err != nil {
				return nil, err
			}
			if interceptor == nil {
				return srv.(*reconnectCapabilitiesServer).Capabilities(ctx, in)
			}
			info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + "agentenv.runtime.v1.SandboxAdminService" + "/Capabilities"}
			handler := func(ctx context.Context, req any) (any, error) {
				return srv.(*reconnectCapabilitiesServer).Capabilities(ctx, req.(*emptypb.Empty))
			}
			return interceptor(ctx, in, info, handler)
		},
	}},
}

func startReconnectCapabilitiesServer(t *testing.T, address string) (string, *grpc.Server) {
	t.Helper()
	if address == "" {
		address = "127.0.0.1:0"
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("listen %s: %v", address, err)
	}
	server := grpc.NewServer()
	server.RegisterService(&reconnectCapabilitiesServiceDesc, &reconnectCapabilitiesServer{})
	go func() {
		_ = server.Serve(listener)
	}()
	return listener.Addr().String(), server
}

func TestHealthRecoversAfterRuntimeGRPCRestart(t *testing.T) {
	address, server := startReconnectCapabilitiesServer(t, "")
	defer server.Stop()

	store := newMemoryStateStore()
	instance, err := New(
		Config{RuntimeAddress: address, NodeName: "node-1", NodeIP: "10.0.0.1"},
		engine.Deps{Store: store},
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer instance.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := instance.Health(ctx); err != nil {
		cancel()
		t.Fatalf("initial Health: %v", err)
	}
	cancel()

	server.Stop()
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	err = instance.Health(ctx)
	cancel()
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("Health while runtime is down = %v, want Unavailable", err)
	}

	_, server = startReconnectCapabilitiesServer(t, address)
	defer server.Stop()
	started := time.Now()
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	err = instance.Health(ctx)
	cancel()
	if err != nil {
		t.Fatalf("Health after runtime restart: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("runtime reconnect took %s, want <= 3s", elapsed)
	}
}
