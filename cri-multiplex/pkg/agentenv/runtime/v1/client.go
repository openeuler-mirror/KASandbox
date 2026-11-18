package v1

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

const (
	LifecycleServiceName = "agentenv.runtime.v1.SandboxLifecycleService"
	AdminServiceName     = "agentenv.runtime.v1.SandboxAdminService"
	ExecServiceName      = "agentenv.runtime.v1.SandboxExecService"
)

// LifecycleClient 是 AgentENV 内部生命周期契约的轻量 gRPC client。
// 方法名与服务路径必须和 runtime.proto 的 service 定义保持一致。
type LifecycleClient struct {
	cc grpc.ClientConnInterface
}

func NewLifecycleClient(cc grpc.ClientConnInterface) *LifecycleClient {
	return &LifecycleClient{cc: cc}
}

func (c *LifecycleClient) Create(ctx context.Context, in *SandboxCreateRequest, opts ...grpc.CallOption) (*SandboxCreateResponse, error) {
	out := new(SandboxCreateResponse)
	err := c.cc.Invoke(ctx, "/"+LifecycleServiceName+"/Create", in, out, opts...)
	return out, err
}

func (c *LifecycleClient) Get(ctx context.Context, in *SandboxGetRequest, opts ...grpc.CallOption) (*SandboxInfo, error) {
	out := new(SandboxInfo)
	err := c.cc.Invoke(ctx, "/"+LifecycleServiceName+"/Get", in, out, opts...)
	return out, err
}

func (c *LifecycleClient) List(ctx context.Context, in *SandboxListRequest, opts ...grpc.CallOption) (*SandboxListResponse, error) {
	out := new(SandboxListResponse)
	err := c.cc.Invoke(ctx, "/"+LifecycleServiceName+"/List", in, out, opts...)
	return out, err
}

func (c *LifecycleClient) Update(ctx context.Context, in *SandboxUpdateRequest, opts ...grpc.CallOption) (*SandboxInfo, error) {
	out := new(SandboxInfo)
	err := c.cc.Invoke(ctx, "/"+LifecycleServiceName+"/Update", in, out, opts...)
	return out, err
}

func (c *LifecycleClient) Delete(ctx context.Context, in *SandboxDeleteRequest, opts ...grpc.CallOption) (*OperationStatus, error) {
	out := new(OperationStatus)
	err := c.cc.Invoke(ctx, "/"+LifecycleServiceName+"/Delete", in, out, opts...)
	return out, err
}

// AdminClient 是内部管理契约的轻量 gRPC client；公共 Admin v2 不直接使用它。
type AdminClient struct {
	cc grpc.ClientConnInterface
}

func NewAdminClient(cc grpc.ClientConnInterface) *AdminClient {
	return &AdminClient{cc: cc}
}

func (c *AdminClient) Pause(ctx context.Context, in *SandboxPauseRequest, opts ...grpc.CallOption) (*SandboxPauseResponse, error) {
	out := new(SandboxPauseResponse)
	err := c.cc.Invoke(ctx, "/"+AdminServiceName+"/Pause", in, out, opts...)
	return out, err
}

func (c *AdminClient) Resume(ctx context.Context, in *SandboxResumeRequest, opts ...grpc.CallOption) (*SandboxResumeResponse, error) {
	out := new(SandboxResumeResponse)
	err := c.cc.Invoke(ctx, "/"+AdminServiceName+"/Resume", in, out, opts...)
	return out, err
}

func (c *AdminClient) Checkpoint(ctx context.Context, in *SandboxCheckpointRequest, opts ...grpc.CallOption) (*CheckpointResult, error) {
	out := new(CheckpointResult)
	err := c.cc.Invoke(ctx, "/"+AdminServiceName+"/Checkpoint", in, out, opts...)
	return out, err
}

func (c *AdminClient) Remediate(ctx context.Context, in *SandboxRemediationRequest, opts ...grpc.CallOption) (*SandboxRemediationResponse, error) {
	out := new(SandboxRemediationResponse)
	err := c.cc.Invoke(ctx, "/"+AdminServiceName+"/Remediate", in, out, opts...)
	return out, err
}

func (c *AdminClient) GetRuntime(ctx context.Context, in *SandboxGetRuntimeRequest, opts ...grpc.CallOption) (*SandboxRuntimeInfo, error) {
	out := new(SandboxRuntimeInfo)
	err := c.cc.Invoke(ctx, "/"+AdminServiceName+"/GetRuntime", in, out, opts...)
	return out, err
}

func (c *AdminClient) GetOperation(ctx context.Context, in *SandboxOperationGetRequest, opts ...grpc.CallOption) (*OperationStatus, error) {
	out := new(OperationStatus)
	err := c.cc.Invoke(ctx, "/"+AdminServiceName+"/GetOperation", in, out, opts...)
	return out, err
}

func (c *AdminClient) GetStats(ctx context.Context, in *SandboxStatsRequest, opts ...grpc.CallOption) (*SandboxStatsResponse, error) {
	out := new(SandboxStatsResponse)
	err := c.cc.Invoke(ctx, "/"+AdminServiceName+"/GetStats", in, out, opts...)
	return out, err
}

func (c *AdminClient) Capabilities(ctx context.Context, in *emptypb.Empty, opts ...grpc.CallOption) (*SandboxCapabilities, error) {
	out := new(SandboxCapabilities)
	err := c.cc.Invoke(ctx, "/"+AdminServiceName+"/Capabilities", in, out, opts...)
	return out, err
}

// ExecClient 通过内部 gRPC 契约执行 CRI ExecSync；数据面由 Runtime 转入 Node Proxy。
type ExecClient struct {
	cc grpc.ClientConnInterface
}

func NewExecClient(cc grpc.ClientConnInterface) *ExecClient {
	return &ExecClient{cc: cc}
}

func (c *ExecClient) ExecSync(ctx context.Context, in *SandboxExecSyncRequest, opts ...grpc.CallOption) (*SandboxExecSyncResponse, error) {
	out := new(SandboxExecSyncResponse)
	err := c.cc.Invoke(ctx, "/"+ExecServiceName+"/ExecSync", in, out, opts...)
	return out, err
}
func (c *ExecClient) StartProcess(ctx context.Context, in *SandboxProcessStartRequest, opts ...grpc.CallOption) (*SandboxProcessStartResponse, error) {
	out := new(SandboxProcessStartResponse)
	err := c.cc.Invoke(ctx, "/"+ExecServiceName+"/StartProcess", in, out, opts...)
	return out, err
}

func (c *ExecClient) GetProcess(ctx context.Context, in *SandboxProcessGetRequest, opts ...grpc.CallOption) (*SandboxProcessGetResponse, error) {
	out := new(SandboxProcessGetResponse)
	err := c.cc.Invoke(ctx, "/"+ExecServiceName+"/GetProcess", in, out, opts...)
	return out, err
}

func (c *ExecClient) StopProcess(ctx context.Context, in *SandboxProcessStopRequest, opts ...grpc.CallOption) (*SandboxProcessStopResponse, error) {
	out := new(SandboxProcessStopResponse)
	err := c.cc.Invoke(ctx, "/"+ExecServiceName+"/StopProcess", in, out, opts...)
	return out, err
}

// ProcessEventStream 是 ConnectProcess 的服务端流。
type ProcessEventStream interface {
	Recv() (*SandboxProcessConnectResponse, error)
}

type processEventStream struct {
	stream grpc.ClientStream
}

func (s *processEventStream) Recv() (*SandboxProcessConnectResponse, error) {
	out := new(SandboxProcessConnectResponse)
	if err := s.stream.RecvMsg(out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *ExecClient) ConnectProcess(ctx context.Context, in *SandboxProcessConnectRequest, opts ...grpc.CallOption) (ProcessEventStream, error) {
	desc := &grpc.StreamDesc{ServerStreams: true}
	stream, err := c.cc.NewStream(ctx, desc, "/"+ExecServiceName+"/ConnectProcess", opts...)
	if err != nil {
		return nil, err
	}
	if err := stream.SendMsg(in); err != nil {
		return nil, err
	}
	if err := stream.CloseSend(); err != nil {
		return nil, err
	}
	return &processEventStream{stream: stream}, nil
}

func (c *ExecClient) SendProcessInput(ctx context.Context, in *SandboxProcessInputRequest, opts ...grpc.CallOption) (*SandboxProcessInputResponse, error) {
	out := new(SandboxProcessInputResponse)
	err := c.cc.Invoke(ctx, "/"+ExecServiceName+"/SendProcessInput", in, out, opts...)
	return out, err
}

func (c *ExecClient) ResizeProcess(ctx context.Context, in *SandboxProcessResizeRequest, opts ...grpc.CallOption) (*SandboxProcessResizeResponse, error) {
	out := new(SandboxProcessResizeResponse)
	err := c.cc.Invoke(ctx, "/"+ExecServiceName+"/ResizeProcess", in, out, opts...)
	return out, err
}

func (c *ExecClient) CloseProcessStdin(ctx context.Context, in *SandboxProcessCloseStdinRequest, opts ...grpc.CallOption) (*SandboxProcessCloseStdinResponse, error) {
	out := new(SandboxProcessCloseStdinResponse)
	err := c.cc.Invoke(ctx, "/"+ExecServiceName+"/CloseProcessStdin", in, out, opts...)
	return out, err
}
