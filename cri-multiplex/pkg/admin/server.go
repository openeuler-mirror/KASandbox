package admin

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cri-multiplex/pkg/engine"
)

// Engine 是 admin server 依赖的 engine 管理操作小接口（由 grpcE2BEngine 实现），
// 避免把管理接口混入 MuxServer CRI 路由代码。
type Engine interface {
	AdminPause(ctx context.Context, op engine.E2BOperation, timeoutSeconds uint64) (startedAt, finishedAt time.Time, err error)
	AdminCheckpoint(ctx context.Context, op engine.E2BOperation, timeoutSeconds uint64) (vmResumed bool, startedAt, finishedAt time.Time, err error)
	AdminGetRuntime(criID, e2bID string) (*engine.RuntimeInfo, []engine.E2BOperation, error)
}

type Server struct {
	UnimplementedE2BSandboxAdminServiceServer
	sockPath string
	eng      Engine
	listener net.Listener
	grpcSrv  *grpc.Server
}

func NewServer(sockPath string, eng Engine) *Server {
	return &Server{sockPath: sockPath, eng: eng}
}

func (s *Server) Start() error {
	// 父目录可能不存在（如 /run/cri-multiplex 在 tmpfs 重启后丢失），先创建。
	if dir := filepath.Dir(s.sockPath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(s.sockPath); err != nil {
		return err
	}
	listener, err := net.Listen("unix", s.sockPath)
	if err != nil {
		return err
	}
	if err := os.Chmod(s.sockPath, 0o660); err != nil {
		_ = listener.Close()
		return err
	}
	s.listener = listener
	s.grpcSrv = grpc.NewServer()
	RegisterE2BSandboxAdminServiceServer(s.grpcSrv, s)
	go func() {
		_ = s.grpcSrv.Serve(listener)
	}()
	return nil
}

func (s *Server) Stop() {
	if s.grpcSrv != nil {
		s.grpcSrv.GracefulStop()
	}
}

func (s *Server) PauseSandbox(ctx context.Context, req *PauseSandboxRequest) (*PauseSandboxResponse, error) {
	if req.GetOperationId() == "" || req.GetCriSandboxId() == "" || req.GetE2BSandboxId() == "" || req.GetTeamId() == "" {
		return nil, status.Error(codes.InvalidArgument, "operation_id, cri_sandbox_id, e2b_sandbox_id and team_id are required")
	}
	if req.GetTemplateId() == "" || req.GetBuildId() == "" {
		return nil, status.Error(codes.InvalidArgument, "template_id and build_id are required")
	}
	startedAt, finishedAt, err := s.eng.AdminPause(ctx, engine.E2BOperation{
		OperationID:  req.GetOperationId(),
		CRISandboxID: req.GetCriSandboxId(),
		E2BSandboxID: req.GetE2BSandboxId(),
		TeamID:       req.GetTeamId(),
		TemplateID:   req.GetTemplateId(),
		BuildID:      req.GetBuildId(),
	}, req.GetTimeoutSeconds())
	if err != nil {
		return nil, err
	}
	return &PauseSandboxResponse{
		SnapshotPersisted: true,
		StartedAtUnix:     startedAt.Unix(),
		FinishedAtUnix:    finishedAt.Unix(),
	}, nil
}

func (s *Server) CheckpointSandbox(ctx context.Context, req *CheckpointSandboxRequest) (*CheckpointSandboxResponse, error) {
	if req.GetOperationId() == "" || req.GetCriSandboxId() == "" || req.GetE2BSandboxId() == "" || req.GetTeamId() == "" {
		return nil, status.Error(codes.InvalidArgument, "operation_id, cri_sandbox_id, e2b_sandbox_id and team_id are required")
	}
	if req.GetBuildId() == "" {
		return nil, status.Error(codes.InvalidArgument, "build_id is required")
	}
	vmResumed, startedAt, finishedAt, err := s.eng.AdminCheckpoint(ctx, engine.E2BOperation{
		OperationID:  req.GetOperationId(),
		CRISandboxID: req.GetCriSandboxId(),
		E2BSandboxID: req.GetE2BSandboxId(),
		TeamID:       req.GetTeamId(),
		BuildID:      req.GetBuildId(),
	}, req.GetTimeoutSeconds())
	if err != nil {
		return nil, err
	}
	return &CheckpointSandboxResponse{
		SnapshotPersisted: true,
		VmResumed:         vmResumed,
		StartedAtUnix:     startedAt.Unix(),
		FinishedAtUnix:    finishedAt.Unix(),
	}, nil
}

func (s *Server) GetSandboxRuntime(ctx context.Context, req *GetSandboxRuntimeRequest) (*GetSandboxRuntimeResponse, error) {
	var criID, e2bID string
	switch target := req.GetTarget().(type) {
	case *GetSandboxRuntimeRequest_CriSandboxId:
		criID = target.CriSandboxId
	case *GetSandboxRuntimeRequest_E2BSandboxId:
		e2bID = target.E2BSandboxId
	}
	if criID == "" && e2bID == "" {
		return nil, status.Error(codes.InvalidArgument, "cri_sandbox_id or e2b_sandbox_id is required")
	}
	info, activeOps, err := s.eng.AdminGetRuntime(criID, e2bID)
	if err != nil {
		return nil, err
	}
	return runtimeInfoToProto(info, activeOps), nil
}

func runtimeInfoToProto(info *engine.RuntimeInfo, activeOps []engine.E2BOperation) *GetSandboxRuntimeResponse {
	resp := &GetSandboxRuntimeResponse{
		CriSandboxId: info.CRISandboxID,
		E2BSandboxId: info.E2BSandboxID,
		TeamId:       info.TeamID,
		PodName:      info.PodName,
		PodNamespace: info.PodNamespace,
		PodUid:       info.PodUID,
		NodeName:     info.NodeName,
		TemplateId:   info.TemplateID,
		BuildId:      info.BuildID,
		ExecutionId:  info.ExecutionID,
		RuntimeState: info.RuntimeState,
	}
	if info.CNI != nil {
		resp.Cni = &CNIRecord{
			NetnsPath:  info.CNI.NetNSPath,
			IfName:     info.CNI.IfName,
			PodIp:      info.CNI.PodIP,
			Gateway:    info.CNI.Gateway,
			DnsServers: append([]string(nil), info.CNI.DNS...),
		}
	}
	for _, m := range info.HostPorts {
		if m.HostPort <= 0 || m.SandboxPort <= 0 {
			continue
		}
		resp.Hostports = append(resp.Hostports, &HostPortMapping{
			HostPort:    uint32(m.HostPort),
			SandboxPort: uint32(m.SandboxPort),
		})
	}
	for _, op := range activeOps {
		resp.ActiveOperations = append(resp.ActiveOperations, &OperationRecord{
			OperationId:    op.OperationID,
			Action:         op.Action,
			CriSandboxId:   op.CRISandboxID,
			E2BSandboxId:   op.E2BSandboxID,
			TemplateId:     op.TemplateID,
			BuildId:        op.BuildID,
			State:          op.State,
			Error:          op.Error,
			StartedAtUnix:  op.StartedAt.Unix(),
			FinishedAtUnix: op.FinishedAt.Unix(),
		})
	}
	return resp
}
