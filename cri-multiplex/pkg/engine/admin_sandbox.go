package engine

import (
	"context"
	"log"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/cri-multiplex/pkg/orchestrator"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// adminSandboxNamespace 是 admin Create 路径下 podInfo 的固定 namespace。
const adminSandboxNamespace = "e2b-admin"

// AdminCreate 是 admin socket 上 E2BSandboxService.Create 的引擎实现：
// 入参为 orchestrator SandboxCreateRequest，内部走与 RunPodSandbox 完全共用的
// 创建生命周期（createE2BSandbox），CNI/HostPort/tracker/stateStore 全部继承。
// config.sandbox_id 必填，同时作为 cri id 与 e2b sandbox id。
func (e *grpcE2BEngine) AdminCreate(ctx context.Context, req *orchestrator.SandboxCreateRequest) (*orchestrator.SandboxCreateResponse, error) {
	cfg := req.GetSandbox()
	if cfg == nil {
		return nil, status.Error(codes.InvalidArgument, "sandbox config is required")
	}
	sandboxID := cfg.GetSandboxId()
	if sandboxID == "" {
		return nil, status.Error(codes.InvalidArgument, "config.sandbox_id is required")
	}
	if cfg.GetTemplateId() == "" || cfg.GetBuildId() == "" || cfg.GetTeamId() == "" {
		return nil, status.Error(codes.InvalidArgument, "config.template_id, config.build_id and config.team_id are required")
	}
	log.Printf("[GrpcE2BEngine] AdminCreate: sandbox=%s alias=%s", sandboxID, strDeref(cfg.Alias))
	e.trackRunPodStart()
	defer e.trackRunPodEnd()
	if err := e.ensureConn(); err != nil {
		return nil, mapE2BError(err)
	}
	if existing, ok := e.tracker.Get(sandboxID); ok && existing.state != stateRemoved {
		// 与 RunPodSandbox 一致的幂等语义：tracker 命中且 orchestrator 侧存在
		// 则视为同一次创建的幂等重试；orchestrator 侧不存在则清残留后重建。
		exists, err := e.orchestratorSandboxExists(ctx, existing.envdSandboxID())
		if err != nil {
			return nil, status.Errorf(codes.Unavailable, "verify existing sandbox %s in orchestrator: %v", existing.envdSandboxID(), err)
		}
		if exists {
			log.Printf("[GrpcE2BEngine] AdminCreate: sandbox %s already exists, returning idempotently", sandboxID)
			return &orchestrator.SandboxCreateResponse{HostIp: existing.hostIP}, nil
		}
		log.Printf("[GrpcE2BEngine] AdminCreate: sandbox %s missing in orchestrator, cleaning stale record before re-create", sandboxID)
		e.cleanupStalePodResources(existing)
	}
	// MaxSandboxLength 缺省时回落到与 CRI 路径一致的默认值（24h），
	// end_time 未设置时 createE2BSandbox 按 startTime + MaxSandboxLength 计算。
	if cfg.MaxSandboxLength <= 0 {
		cfg.MaxSandboxLength = defaultSandboxConfig.MaxSandboxLength
	}
	alias := strDeref(cfg.Alias)
	if alias == "" {
		alias = sandboxID
	}
	startTime := time.Now()
	if t := req.GetStartTime(); t != nil {
		startTime = t.AsTime()
	}
	var endTime time.Time
	if t := req.GetEndTime(); t != nil {
		endTime = t.AsTime()
	}
	return e.createE2BSandbox(ctx, e2bCreateParams{
		sandboxID: sandboxID,
		cfg:       cfg,
		metadata:  cfg.Metadata,
		labels:    nil,
		podUID:    sandboxID,
		name:      alias,
		namespace: adminSandboxNamespace,
		startTime: startTime,
		endTime:   endTime,
		cniConfig: &runtime.PodSandboxConfig{
			Metadata: &runtime.PodSandboxMetadata{Name: sandboxID},
		},
	})
}

// AdminDelete 是 Stop+Remove 合一的完整清理：阻塞式获取 sandbox 操作锁
// （等待进行中的 Pause/Checkpoint 结束，随 ctx 取消）后执行
// cleanupSandboxResources 的完整序列（orchestrator Delete / HostPort 释放 /
// CNI DEL / stateStore / podRoutes / tracker 摘除）。幂等：sandbox 不存在返回 OK。
func (e *grpcE2BEngine) AdminDelete(ctx context.Context, req *orchestrator.SandboxDeleteRequest) (*emptypb.Empty, error) {
	sandboxID := req.GetSandboxId()
	if sandboxID == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox_id is required")
	}
	mu, err := e.lockSandbox(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	defer mu.Unlock()
	if err := e.cleanupSandboxResources(ctx, sandboxID); err != nil {
		return nil, mapE2BError(err)
	}
	log.Printf("[GrpcE2BEngine] AdminDelete succeeded: sandbox=%s", sandboxID)
	return &emptypb.Empty{}, nil
}

// AdminUpdate 薄转发 orchestrator Update（改 TTL），不触碰本地状态。
func (e *grpcE2BEngine) AdminUpdate(ctx context.Context, req *orchestrator.SandboxUpdateRequest) (*emptypb.Empty, error) {
	if err := e.ensureConn(); err != nil {
		return nil, mapE2BError(err)
	}
	resp, err := e.client.Update(ctx, req)
	if err != nil {
		return nil, mapE2BError(err)
	}
	return resp, nil
}

// AdminList 薄转发 orchestrator List，不与本地 stateStore 合并。
func (e *grpcE2BEngine) AdminList(ctx context.Context) (*orchestrator.SandboxListResponse, error) {
	if err := e.ensureConn(); err != nil {
		return nil, mapE2BError(err)
	}
	resp, err := e.client.List(ctx, &emptypb.Empty{})
	if err != nil {
		return nil, mapE2BError(err)
	}
	return resp, nil
}

// AdminListCachedBuilds 薄转发 orchestrator ListCachedBuilds，不触碰本地状态。
func (e *grpcE2BEngine) AdminListCachedBuilds(ctx context.Context) (*orchestrator.SandboxListCachedBuildsResponse, error) {
	if err := e.ensureConn(); err != nil {
		return nil, mapE2BError(err)
	}
	resp, err := e.client.ListCachedBuilds(ctx, &emptypb.Empty{})
	if err != nil {
		return nil, mapE2BError(err)
	}
	return resp, nil
}
