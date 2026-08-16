package engine

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newAdminOpsTestEngine(t *testing.T, client *fakeSandboxServiceClient) *grpcE2BEngine {
	t.Helper()
	e := newTestGRPCE2BEngine(client)
	e.stateStore = newStateStoreTestStore(t)
	e.nodeName = "node-a"
	e.tracker.Add("cri-a", &podInfo{
		sandboxID:    "cri-a",
		e2bSandboxID: "e2b-a",
		podUID:       "uid-a",
		name:         "pod-a",
		namespace:    "ns-a",
		createdAt:    time.Now(),
		state:        stateRunning,
		templateID:   "tmpl-a",
		buildID:      "build-a",
		executionID:  "exec-a",
		teamID:       "team-a",
		nodeName:     "node-a",
		cniRecord:    &CNIRecord{SandboxID: "cri-a", NetNSPath: "/var/run/netns/e2b-cri-a", IfName: "eth0", PodIP: "10.0.0.10", Gateway: "10.0.0.1", DNS: []string{"10.96.0.10"}},
		portMappings: []PortMapping{{HostPort: 20001, SandboxPort: 49983}},
	})
	return e
}

func adminOp(id string) E2BOperation {
	return E2BOperation{
		OperationID:  id,
		CRISandboxID: "cri-a",
		E2BSandboxID: "e2b-a",
		TeamID:       "team-a",
		TemplateID:   "tmpl-snap",
		BuildID:      "build-snap",
	}
}

func loadOp(t *testing.T, e *grpcE2BEngine, opID string) E2BOperation {
	t.Helper()
	ops, err := e.stateStore.LoadE2BOperations()
	if err != nil {
		t.Fatalf("LoadE2BOperations: %v", err)
	}
	op, ok := ops[opID]
	if !ok {
		t.Fatalf("operation %s not persisted: %+v", opID, ops)
	}
	return op
}

func TestAdminPauseSuccess(t *testing.T) {
	client := &fakeSandboxServiceClient{}
	e := newAdminOpsTestEngine(t, client)

	startedAt, finishedAt, err := e.AdminPause(context.Background(), adminOp("op-1"), 0)
	if err != nil {
		t.Fatalf("AdminPause: %v", err)
	}
	if startedAt.IsZero() || finishedAt.Before(startedAt) {
		t.Fatalf("bad timestamps: started=%v finished=%v", startedAt, finishedAt)
	}
	if client.pauseCalls != 1 || client.lastPause.SandboxId != "e2b-a" ||
		client.lastPause.TemplateId != "tmpl-snap" || client.lastPause.BuildId != "build-snap" {
		t.Fatalf("pause request mismatch: calls=%d req=%+v", client.pauseCalls, client.lastPause)
	}
	pod, _ := e.tracker.Get("cri-a")
	if pod.state != statePaused {
		t.Fatalf("pod state = %v, want paused", pod.state)
	}
	op := loadOp(t, e, "op-1")
	if op.State != OperationStateSucceeded || op.Action != "Pause" || op.Error != "" {
		t.Fatalf("op record mismatch: %+v", op)
	}
	// pod 状态变化应持久化
	pods, _ := e.stateStore.LoadE2BPods()
	if len(pods) != 1 || pods[0].State != statePaused {
		t.Fatalf("persisted pods mismatch: %+v", pods)
	}
}

func TestAdminPauseOperationIDRetryReturnsRecordedResult(t *testing.T) {
	client := &fakeSandboxServiceClient{}
	e := newAdminOpsTestEngine(t, client)

	if _, _, err := e.AdminPause(context.Background(), adminOp("op-1"), 0); err != nil {
		t.Fatalf("AdminPause: %v", err)
	}
	// 重试相同 operation_id：直接成功，不再调 orchestrator；
	// 注意此时 pod 已是 Paused 状态，证明走的是幂等路径而非重新校验状态。
	startedAt, finishedAt, err := e.AdminPause(context.Background(), adminOp("op-1"), 0)
	if err != nil {
		t.Fatalf("retry AdminPause: %v", err)
	}
	if client.pauseCalls != 1 {
		t.Fatalf("pause calls = %d, want 1", client.pauseCalls)
	}
	if startedAt.IsZero() || finishedAt.IsZero() {
		t.Fatalf("retry should return recorded timestamps: %v / %v", startedAt, finishedAt)
	}
}

func TestAdminPauseLockConflictAborted(t *testing.T) {
	client := &fakeSandboxServiceClient{}
	e := newAdminOpsTestEngine(t, client)

	mu, locked := e.tryLockSandbox("cri-a")
	if !locked {
		t.Fatal("failed to pre-acquire sandbox lock")
	}
	defer mu.Unlock()

	if _, _, err := e.AdminPause(context.Background(), adminOp("op-1"), 0); status.Code(err) != codes.Aborted {
		t.Fatalf("code = %v, want Aborted", status.Code(err))
	}
	if client.pauseCalls != 0 {
		t.Fatalf("pause calls = %d, want 0", client.pauseCalls)
	}
}

func TestAdminPauseValidationErrors(t *testing.T) {
	client := &fakeSandboxServiceClient{}
	e := newAdminOpsTestEngine(t, client)

	// CRI ID 不存在
	missing := adminOp("op-missing")
	missing.CRISandboxID = "cri-missing"
	if _, _, err := e.AdminPause(context.Background(), missing, 0); status.Code(err) != codes.NotFound {
		t.Fatalf("missing pod code = %v, want NotFound", status.Code(err))
	}

	// team 不符
	wrongTeam := adminOp("op-team")
	wrongTeam.TeamID = "team-b"
	if _, _, err := e.AdminPause(context.Background(), wrongTeam, 0); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("team mismatch code = %v, want FailedPrecondition", status.Code(err))
	}

	// 双向索引不一致
	wrongE2B := adminOp("op-e2b")
	wrongE2B.E2BSandboxID = "e2b-other"
	if _, _, err := e.AdminPause(context.Background(), wrongE2B, 0); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("e2b mismatch code = %v, want FailedPrecondition", status.Code(err))
	}

	// template/build 必填
	noBuild := adminOp("op-invalid")
	noBuild.TemplateID = ""
	if _, _, err := e.AdminPause(context.Background(), noBuild, 0); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing template code = %v, want InvalidArgument", status.Code(err))
	}

	if client.pauseCalls != 0 {
		t.Fatalf("pause calls = %d, want 0", client.pauseCalls)
	}
}

func TestAdminPauseOrchestratorErrorRecordsFailed(t *testing.T) {
	client := &fakeSandboxServiceClient{pauseErr: status.Error(codes.Internal, "boom")}
	e := newAdminOpsTestEngine(t, client)

	if _, _, err := e.AdminPause(context.Background(), adminOp("op-1"), 0); status.Code(err) != codes.Internal {
		t.Fatalf("code = %v, want Internal", status.Code(err))
	}
	op := loadOp(t, e, "op-1")
	if op.State != OperationStateFailed || op.Error == "" {
		t.Fatalf("op record mismatch: %+v", op)
	}
	pod, _ := e.tracker.Get("cri-a")
	if pod.state != stateRunning {
		t.Fatalf("pod state = %v, want running after failed pause", pod.state)
	}

	// Failed 允许重试
	client.pauseErr = nil
	if _, _, err := e.AdminPause(context.Background(), adminOp("op-1"), 0); err != nil {
		t.Fatalf("retry after failure: %v", err)
	}
	if client.pauseCalls != 2 {
		t.Fatalf("pause calls = %d, want 2", client.pauseCalls)
	}
	if op := loadOp(t, e, "op-1"); op.State != OperationStateSucceeded {
		t.Fatalf("op record after retry = %+v, want Succeeded", op)
	}
}

func TestAdminCheckpointSuccess(t *testing.T) {
	client := &fakeSandboxServiceClient{}
	e := newAdminOpsTestEngine(t, client)

	op := adminOp("op-ckpt")
	op.TemplateID = "" // checkpoint 无 template_id
	vmResumed, startedAt, finishedAt, err := e.AdminCheckpoint(context.Background(), op, 60)
	if err != nil {
		t.Fatalf("AdminCheckpoint: %v", err)
	}
	if !vmResumed {
		t.Fatal("vmResumed should be true")
	}
	if startedAt.IsZero() || finishedAt.Before(startedAt) {
		t.Fatalf("bad timestamps: %v / %v", startedAt, finishedAt)
	}
	if client.checkpointCalls != 1 || client.lastCheckpoint.SandboxId != "e2b-a" || client.lastCheckpoint.BuildId != "build-snap" {
		t.Fatalf("checkpoint request mismatch: calls=%d req=%+v", client.checkpointCalls, client.lastCheckpoint)
	}
	pod, _ := e.tracker.Get("cri-a")
	if pod.state != stateRunning {
		t.Fatalf("pod state = %v, want running after checkpoint", pod.state)
	}
	rec := loadOp(t, e, "op-ckpt")
	if rec.State != OperationStateSucceeded || rec.Action != "Checkpoint" {
		t.Fatalf("op record mismatch: %+v", rec)
	}

	// 幂等重试
	vmResumed, _, _, err = e.AdminCheckpoint(context.Background(), op, 60)
	if err != nil || !vmResumed {
		t.Fatalf("retry checkpoint: resumed=%v err=%v", vmResumed, err)
	}
	if client.checkpointCalls != 1 {
		t.Fatalf("checkpoint calls = %d, want 1", client.checkpointCalls)
	}
}

func TestAdminGetRuntime(t *testing.T) {
	client := &fakeSandboxServiceClient{}
	e := newAdminOpsTestEngine(t, client)

	// 制造一条进行中的 operation
	if err := e.stateStore.SaveE2BOperation(E2BOperation{
		OperationID: "op-active", Action: "Pause", CRISandboxID: "cri-a", E2BSandboxID: "e2b-a",
		State: OperationStateRunning, StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.stateStore.SaveE2BOperation(E2BOperation{
		OperationID: "op-done", Action: "Pause", CRISandboxID: "cri-a", E2BSandboxID: "e2b-a",
		State: OperationStateSucceeded, StartedAt: time.Now(), FinishedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		criID string
		e2bID string
	}{
		{name: "by cri id", criID: "cri-a"},
		{name: "by e2b id", e2bID: "e2b-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info, active, err := e.AdminGetRuntime(tc.criID, tc.e2bID)
			if err != nil {
				t.Fatalf("AdminGetRuntime: %v", err)
			}
			if info.CRISandboxID != "cri-a" || info.E2BSandboxID != "e2b-a" || info.TeamID != "team-a" ||
				info.PodName != "pod-a" || info.PodNamespace != "ns-a" || info.PodUID != "uid-a" ||
				info.NodeName != "node-a" || info.TemplateID != "tmpl-a" || info.BuildID != "build-a" ||
				info.ExecutionID != "exec-a" || info.RuntimeState != "Running" {
				t.Fatalf("runtime info mismatch: %+v", info)
			}
			if info.CNI == nil || info.CNI.PodIP != "10.0.0.10" || info.CNI.NetNSPath == "" {
				t.Fatalf("cni mismatch: %+v", info.CNI)
			}
			if len(info.HostPorts) != 1 || info.HostPorts[0].HostPort != 20001 {
				t.Fatalf("hostports mismatch: %+v", info.HostPorts)
			}
			if len(active) != 1 || active[0].OperationID != "op-active" {
				t.Fatalf("active ops mismatch: %+v", active)
			}
		})
	}

	if _, _, err := e.AdminGetRuntime("", ""); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty target code = %v, want InvalidArgument", status.Code(err))
	}
	if _, _, err := e.AdminGetRuntime("cri-missing", ""); status.Code(err) != codes.NotFound {
		t.Fatalf("missing code = %v, want NotFound", status.Code(err))
	}
}
