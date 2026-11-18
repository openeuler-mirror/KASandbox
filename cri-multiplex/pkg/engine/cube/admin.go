package cube

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cri-multiplex/pkg/engine"
)

const envdPort = "49983"

// AdminPauseSandbox 把运行中的沙箱暂停。
// 快照 ID 由本节点分配并写入本地索引，响应只回报是否持久化。
func (e *Engine) AdminPauseSandbox(ctx context.Context, ref engine.SandboxRef, opts engine.AdminOpOptions) (*engine.AdminOpResult, error) {
	started := time.Now()
	pod, err := e.prepareAdmin(ref, opts, "pause")
	if err != nil {
		return nil, err
	}
	if pod.LastAdminOpID == opts.OperationID && pod.LastAdminAction == "pause" && pod.State == podPaused {
		return adminResult(started, true, false), nil
	}
	ctx, cancel := bound(ctx, opts.Timeout)
	defer cancel()
	snap, err := e.Pause(ctx, pod.CRISandboxID)
	if err != nil {
		return nil, err
	}
	if err := e.rememberOp(pod.CRISandboxID, opts.OperationID, "pause"); err != nil {
		return nil, err
	}
	return adminResult(started, snap != "", false), nil
}

// AdminCheckpointSandbox 对运行中的沙箱做 CommitSandbox。
// 调用方不拿回快照 ID；本节点把 snap-* 记在本地索引里。
func (e *Engine) AdminCheckpointSandbox(ctx context.Context, ref engine.SandboxRef, opts engine.AdminOpOptions) (*engine.AdminOpResult, error) {
	started := time.Now()
	pod, err := e.prepareAdmin(ref, opts, "checkpoint")
	if err != nil {
		return nil, err
	}
	if pod.LastAdminOpID == opts.OperationID && pod.LastAdminAction == "checkpoint" && pod.CheckpointSnapshotID != "" {
		return adminResult(started, true, true), nil
	}
	locked, err := e.begin(pod.CRISandboxID)
	if err != nil {
		return nil, err
	}
	defer e.end(pod.CRISandboxID)
	if locked == nil {
		return nil, status.Errorf(codes.NotFound, "sandbox %s not found", pod.CRISandboxID)
	}
	if locked.State != podRunning {
		return nil, status.Errorf(codes.FailedPrecondition, "sandbox %s is %s", locked.CRISandboxID, locked.State)
	}
	snap, err := newSnapshotID()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "allocate snapshot id: %v", err)
	}
	ctx, cancel := bound(ctx, opts.Timeout)
	defer cancel()
	if _, err := e.client.Commit(ctx, locked.SandboxID, snap, locked.Backend); err != nil {
		if isTimeout(err) {
			return nil, status.Error(codes.Unknown, "cube checkpoint timed out")
		}
		return nil, err
	}
	locked.CheckpointSnapshotID = snap
	locked.LastAdminOpID = opts.OperationID
	locked.LastAdminAction = "checkpoint"
	if err := e.commit(*locked); err != nil {
		return nil, err
	}
	// CommitSandbox 不停机；vm_resumed=true 表示源仍 Running。
	return adminResult(started, true, true), nil
}

// AdminGetSandboxRuntime 返回本节点上的 cube 运行事实。
// PodIP 是 Cubelet 分配的 sandboxIP，envd 端口放在 Extra。
func (e *Engine) AdminGetSandboxRuntime(_ context.Context, ref engine.SandboxRef) (*engine.SandboxRuntime, error) {
	if err := e.ensureEnabled(); err != nil {
		return nil, err
	}
	pod, err := e.resolve(ref)
	if err != nil {
		return nil, err
	}
	start := pod.CreatedAt
	return &engine.SandboxRuntime{
		Backend:      string(engine.EngineTypeCube),
		CRISandboxID: pod.CRISandboxID,
		SandboxID:    pod.SandboxID,
		State:        pod.State,
		TemplateID:   pod.TemplateID,
		PodName:      pod.Name,
		PodNamespace: pod.Namespace,
		PodUID:       pod.PodUID,
		NodeName:     pod.OriginNode,
		PodIP:        pod.SandboxIP,
		HostIP:       e.cfg.Deps.NodeIP,
		Extra: map[string]string{
			"envd_port":              envdPort,
			"pause_snapshot_id":      pod.PauseSnapshotID,
			"checkpoint_snapshot_id": pod.CheckpointSnapshotID,
		},
		StartTime: &start,
	}, nil
}

func (e *Engine) prepareAdmin(ref engine.SandboxRef, opts engine.AdminOpOptions, action string) (CubePodState, error) {
	if err := e.ensureEnabled(); err != nil {
		return CubePodState{}, err
	}
	if opts.OperationID == "" {
		return CubePodState{}, status.Errorf(codes.InvalidArgument, "cube %s requires operation_id", action)
	}
	if e.client == nil {
		return CubePodState{}, status.Error(codes.Unavailable, "cubelet client is not connected")
	}
	return e.resolve(ref)
}

func (e *Engine) resolve(ref engine.SandboxRef) (CubePodState, error) {
	if ref.CRISandboxID != "" {
		pod, ok := e.get(ref.CRISandboxID)
		if !ok {
			return CubePodState{}, status.Errorf(codes.NotFound, "sandbox %s not found", ref.CRISandboxID)
		}
		if ref.SandboxID != "" && pod.SandboxID != ref.SandboxID {
			return CubePodState{}, status.Errorf(codes.FailedPrecondition, "sandbox %s does not match cri %s", ref.SandboxID, ref.CRISandboxID)
		}
		return pod, nil
	}
	if ref.SandboxID == "" {
		return CubePodState{}, status.Error(codes.InvalidArgument, "cri_sandbox_id or sandbox_id is required")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, pod := range e.pods {
		if pod.SandboxID == ref.SandboxID {
			return clonePod(*pod), nil
		}
	}
	return CubePodState{}, status.Errorf(codes.NotFound, "sandbox %s not found", ref.SandboxID)
}

func (e *Engine) rememberOp(criID, operationID, action string) error {
	pod, ok := e.get(criID)
	if !ok {
		return status.Errorf(codes.NotFound, "sandbox %s not found", criID)
	}
	pod.LastAdminOpID = operationID
	pod.LastAdminAction = action
	return e.commit(pod)
}

func bound(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

func adminResult(started time.Time, persisted, resumed bool) *engine.AdminOpResult {
	return &engine.AdminOpResult{
		SnapshotPersisted: persisted,
		VMResumed:         resumed,
		StartedAt:         started,
		FinishedAt:        time.Now(),
	}
}
