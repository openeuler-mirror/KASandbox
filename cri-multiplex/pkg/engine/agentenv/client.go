package agentenv

import (
	"context"

	runtimeapi "github.com/cri-multiplex/pkg/agentenv/runtime/v1"
	"google.golang.org/grpc"
)

const (
	LifecycleServiceName = "agentenv.runtime.v1.SandboxLifecycleService"
	AdminServiceName     = "agentenv.runtime.v1.SandboxAdminService"
	ExecServiceName      = "agentenv.runtime.v1.SandboxExecService"
)

// 客户端复用协议生成代码，仅在 engine 内适配流式接口。
type LifecycleClient struct {
	runtimeapi.SandboxLifecycleServiceClient
}

func NewLifecycleClient(cc grpc.ClientConnInterface) *LifecycleClient {
	return &LifecycleClient{runtimeapi.NewSandboxLifecycleServiceClient(cc)}
}

type AdminClient struct {
	runtimeapi.SandboxAdminServiceClient
}

func NewAdminClient(cc grpc.ClientConnInterface) *AdminClient {
	return &AdminClient{runtimeapi.NewSandboxAdminServiceClient(cc)}
}

type ExecClient struct {
	runtimeapi.SandboxExecServiceClient
}

func NewExecClient(cc grpc.ClientConnInterface) *ExecClient {
	return &ExecClient{runtimeapi.NewSandboxExecServiceClient(cc)}
}

// ProcessEventStream 让 engine 和测试桩只依赖实际使用的接收方法。
type ProcessEventStream interface {
	Recv() (*runtimeapi.SandboxProcessConnectResponse, error)
}

func (c *ExecClient) ConnectProcess(ctx context.Context, in *runtimeapi.SandboxProcessConnectRequest, opts ...grpc.CallOption) (ProcessEventStream, error) {
	return c.SandboxExecServiceClient.ConnectProcess(ctx, in, opts...)
}
