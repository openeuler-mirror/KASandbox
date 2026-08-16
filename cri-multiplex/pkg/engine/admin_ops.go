package engine

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cri-multiplex/pkg/orchestrator"
)

const defaultAdminOpTimeout = 300 * time.Second

// RuntimeInfo 是 AdminGetRuntime 返回的导出视图（podInfo 字段未导出，
// admin 包只能消费该结构体做 proto 映射）。
type RuntimeInfo struct {
	CRISandboxID string
	E2BSandboxID string
	TeamID       string
	PodName      string
	PodNamespace string
	PodUID       string
	NodeName     string
	TemplateID   string
	BuildID      string
	ExecutionID  string
	RuntimeState string
	CNI          *CNIRecord
	HostPorts    []PortMapping
}

func runtimeStateString(s e2bState) string {
	switch s {
	case stateRunning:
		return "Running"
	case stateStopped:
		return "Stopping"
	case statePaused:
		return "Paused"
	case stateRemoved:
		return "Removing"
	default:
		return "Unknown"
	}
}

// tryLockSandbox 获取 sandbox 级 operation lock（TryLock 语义，拿不到立即返回 false）
func (e *grpcE2BEngine) tryLockSandbox(criSandboxID string) (*sync.Mutex, bool) {
	m, _ := e.opLocks.LoadOrStore(criSandboxID, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	return mu, mu.TryLock()
}

func (e *grpcE2BEngine) lookupOperation(operationID string) (E2BOperation, bool) {
	if e.stateStore == nil || operationID == "" {
		return E2BOperation{}, false
	}
	ops, err := e.stateStore.LoadE2BOperations()
	if err != nil {
		log.Printf("[GrpcE2BEngine] WARNING: load e2b operations failed: %v", err)
		return E2BOperation{}, false
	}
	op, ok := ops[operationID]
	return op, ok
}

func (e *grpcE2BEngine) saveOperation(op E2BOperation) {
	if e.stateStore == nil {
		return
	}
	if err := e.stateStore.SaveE2BOperation(op); err != nil {
		log.Printf("[GrpcE2BEngine] WARNING: persist e2b operation %s failed: %v", op.OperationID, err)
	}
}

// validateAdminOpTarget 校验 Pod 存在、双向索引一致、team ownership。
// admin.sock 仅节点本地可达，"Pod 在本节点"由 tracker 命中天然保证。
func (e *grpcE2BEngine) validateAdminOpTarget(op E2BOperation) (*podInfo, error) {
	pod, ok := e.tracker.Get(op.CRISandboxID)
	if !ok || pod.state == stateRemoved {
		return nil, status.Errorf(codes.NotFound, "sandbox %s not found on this node", op.CRISandboxID)
	}
	if pod.e2bSandboxID != op.E2BSandboxID {
		return nil, status.Errorf(codes.FailedPrecondition,
			"e2b sandbox id mismatch: pod %s has %q, request has %q", op.CRISandboxID, pod.e2bSandboxID, op.E2BSandboxID)
	}
	if back, ok := e.tracker.GetByE2B(op.E2BSandboxID); !ok || back.sandboxID != op.CRISandboxID {
		return nil, status.Errorf(codes.FailedPrecondition,
			"e2b/cri sandbox index inconsistent for %s / %s", op.CRISandboxID, op.E2BSandboxID)
	}
	if pod.teamID != op.TeamID {
		return nil, status.Errorf(codes.FailedPrecondition,
			"team mismatch: pod %s belongs to team %q, request has %q", op.CRISandboxID, pod.teamID, op.TeamID)
	}
	if pod.state != stateRunning {
		return nil, status.Errorf(codes.FailedPrecondition,
			"sandbox %s state %s does not allow the operation", op.CRISandboxID, runtimeStateString(pod.state))
	}
	return pod, nil
}

// beginAdminOperation 做 operation_id 幂等检查、sandbox 锁、写入 Running 记录。
// 返回 (lock, startedAt, done)；done 为 nil 表示命中幂等/冲突，直接按返回错误处理。
func (e *grpcE2BEngine) beginAdminOperation(op E2BOperation) (*sync.Mutex, E2BOperation, error) {
	if op.OperationID == "" || op.CRISandboxID == "" || op.E2BSandboxID == "" {
		return nil, op, status.Error(codes.InvalidArgument, "operation_id, cri_sandbox_id and e2b_sandbox_id are required")
	}
	if rec, ok := e.lookupOperation(op.OperationID); ok {
		switch rec.State {
		case OperationStateSucceeded:
			return nil, rec, errOperationAlreadySucceeded
		case OperationStateRunning, OperationStatePending:
			return nil, rec, status.Errorf(codes.Aborted, "operation %s is already %s", op.OperationID, rec.State)
		}
		// Failed：允许重试，覆盖记录重新开始
	}
	mu, locked := e.tryLockSandbox(op.CRISandboxID)
	if !locked {
		return nil, op, status.Errorf(codes.Aborted, "sandbox %s has a conflicting operation in progress", op.CRISandboxID)
	}
	op.State = OperationStateRunning
	op.Error = ""
	op.StartedAt = time.Now()
	op.FinishedAt = time.Time{}
	e.saveOperation(op)
	return mu, op, nil
}

var errOperationAlreadySucceeded = errors.New("operation already succeeded")

// finishAdminOperation 写回 Succeeded/Failed 终态并释放 sandbox 锁。
func (e *grpcE2BEngine) finishAdminOperation(mu *sync.Mutex, op E2BOperation, opErr error) E2BOperation {
	op.FinishedAt = time.Now()
	if opErr != nil {
		op.State = OperationStateFailed
		op.Error = opErr.Error()
	} else {
		op.State = OperationStateSucceeded
		op.Error = ""
	}
	e.saveOperation(op)
	mu.Unlock()
	return op
}

func adminOpContext(ctx context.Context, timeoutSeconds uint64) (context.Context, context.CancelFunc) {
	timeout := defaultAdminOpTimeout
	if timeoutSeconds > 0 {
		timeout = time.Duration(timeoutSeconds) * time.Second
	}
	return context.WithTimeout(ctx, timeout)
}

// mapAdminOpError 把 orchestrator 调用错误映射为 admin 错误码。
func mapAdminOpError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return status.Errorf(codes.DeadlineExceeded, "operation timed out: %v", err)
	}
	return status.Errorf(codes.Internal, "orchestrator call failed: %v", err)
}

// AdminPause 为指定 E2B sandbox 执行 Pause：调 orchestrator Pause 拍快照并等待
// 持久化，成功后本地状态置为 Paused。相同 operation_id 重试直接返回已有结果。
func (e *grpcE2BEngine) AdminPause(ctx context.Context, op E2BOperation, timeoutSeconds uint64) (time.Time, time.Time, error) {
	op.Action = "Pause"
	if op.TemplateID == "" || op.BuildID == "" {
		return time.Time{}, time.Time{}, status.Error(codes.InvalidArgument, "template_id and build_id are required")
	}
	mu, rec, err := e.beginAdminOperation(op)
	if err != nil {
		if errors.Is(err, errOperationAlreadySucceeded) {
			return rec.StartedAt, rec.FinishedAt, nil
		}
		return time.Time{}, time.Time{}, err
	}

	pod, err := e.validateAdminOpTarget(op)
	if err != nil {
		rec = e.finishAdminOperation(mu, rec, err)
		return rec.StartedAt, rec.FinishedAt, err
	}

	opCtx, cancel := adminOpContext(ctx, timeoutSeconds)
	defer cancel()
	_, callErr := e.client.Pause(opCtx, &orchestrator.SandboxPauseRequest{
		SandboxId:  pod.envdSandboxID(),
		TemplateId: op.TemplateID,
		BuildId:    op.BuildID,
	})
	if callErr != nil {
		mapped := mapAdminOpError(opCtx, callErr)
		rec = e.finishAdminOperation(mu, rec, mapped)
		return rec.StartedAt, rec.FinishedAt, mapped
	}

	pod.state = statePaused
	e.persistPodState(pod)
	rec = e.finishAdminOperation(mu, rec, nil)
	log.Printf("[GrpcE2BEngine] AdminPause succeeded: cri_id=%s e2b_id=%s operation=%s", op.CRISandboxID, op.E2BSandboxID, op.OperationID)
	return rec.StartedAt, rec.FinishedAt, nil
}

// AdminCheckpoint 为指定 E2B sandbox 执行 Checkpoint：拍快照、等待持久化、VM
// 原地恢复继续运行（orchestrator Checkpoint RPC 返回即视为已 resume）。
// 成功后 pod 保持 Running。
func (e *grpcE2BEngine) AdminCheckpoint(ctx context.Context, op E2BOperation, timeoutSeconds uint64) (bool, time.Time, time.Time, error) {
	op.Action = "Checkpoint"
	if op.BuildID == "" {
		return false, time.Time{}, time.Time{}, status.Error(codes.InvalidArgument, "build_id is required")
	}
	mu, rec, err := e.beginAdminOperation(op)
	if err != nil {
		if errors.Is(err, errOperationAlreadySucceeded) {
			return true, rec.StartedAt, rec.FinishedAt, nil
		}
		return false, time.Time{}, time.Time{}, err
	}

	pod, err := e.validateAdminOpTarget(op)
	if err != nil {
		rec = e.finishAdminOperation(mu, rec, err)
		return false, rec.StartedAt, rec.FinishedAt, err
	}

	opCtx, cancel := adminOpContext(ctx, timeoutSeconds)
	defer cancel()
	_, callErr := e.client.Checkpoint(opCtx, &orchestrator.SandboxCheckpointRequest{
		SandboxId: pod.envdSandboxID(),
		BuildId:   op.BuildID,
	})
	if callErr != nil {
		mapped := mapAdminOpError(opCtx, callErr)
		rec = e.finishAdminOperation(mu, rec, mapped)
		return false, rec.StartedAt, rec.FinishedAt, mapped
	}

	rec = e.finishAdminOperation(mu, rec, nil)
	log.Printf("[GrpcE2BEngine] AdminCheckpoint succeeded: cri_id=%s e2b_id=%s operation=%s", op.CRISandboxID, op.E2BSandboxID, op.OperationID)
	return true, rec.StartedAt, rec.FinishedAt, nil
}

// AdminGetRuntime 返回节点运行事实；active operations = op store 中该 CRI ID
// 且 State 为 Running/Pending 的记录。
func (e *grpcE2BEngine) AdminGetRuntime(criID, e2bID string) (*RuntimeInfo, []E2BOperation, error) {
	if criID == "" && e2bID == "" {
		return nil, nil, status.Error(codes.InvalidArgument, "cri_sandbox_id or e2b_sandbox_id is required")
	}
	var pod *podInfo
	var ok bool
	if criID != "" {
		pod, ok = e.tracker.Get(criID)
	} else {
		pod, ok = e.tracker.GetByE2B(e2bID)
	}
	if !ok || pod.state == stateRemoved {
		return nil, nil, status.Errorf(codes.NotFound, "sandbox (cri_id=%q e2b_id=%q) not found on this node", criID, e2bID)
	}

	info := &RuntimeInfo{
		CRISandboxID: pod.sandboxID,
		E2BSandboxID: pod.e2bSandboxID,
		TeamID:       pod.teamID,
		PodName:      pod.name,
		PodNamespace: pod.namespace,
		PodUID:       pod.podUID,
		NodeName:     pod.nodeName,
		TemplateID:   pod.templateID,
		BuildID:      pod.buildID,
		ExecutionID:  pod.executionID,
		RuntimeState: runtimeStateString(pod.state),
		CNI:          cloneCNIRecord(pod.cniRecord),
		HostPorts:    append([]PortMapping(nil), pod.portMappings...),
	}

	var active []E2BOperation
	if e.stateStore != nil {
		ops, err := e.stateStore.LoadE2BOperations()
		if err != nil {
			log.Printf("[GrpcE2BEngine] WARNING: load e2b operations failed: %v", err)
		}
		for _, op := range ops {
			if op.CRISandboxID == pod.sandboxID && (op.State == OperationStateRunning || op.State == OperationStatePending) {
				active = append(active, op)
			}
		}
	}
	return info, active, nil
}
