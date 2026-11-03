package e2b

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cri-multiplex/pkg/engine"
)

func newStateTestEngine(t *testing.T) (*Engine, engine.StateStore) {
	t.Helper()
	store := newTestStateStore(t)
	return &Engine{stateStore: store, tracker: newPodTracker(), client: &fakeSandboxServiceClient{}}, store
}

func TestE2BOperationRoundTrip(t *testing.T) {
	e, store := newStateTestEngine(t)
	op := E2BOperation{
		OperationID:  "op-1",
		Action:       "Pause",
		CRISandboxID: "cri-a",
		E2BSandboxID: "e2b-a",
		TeamID:       "team-a",
		TemplateID:   "tmpl-a",
		BuildID:      "build-a",
		State:        OperationStateSucceeded,
		StartedAt:    time.Now().Add(-time.Second).Truncate(time.Millisecond),
		FinishedAt:   time.Now().Truncate(time.Millisecond),
	}
	if err := e.persistOperation(op); err != nil {
		t.Fatalf("persistOperation: %v", err)
	}

	reopened, err := engine.NewJSONStateStore(filepath.Dir(storeFilePath(t, store)))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	reopenedEngine := &Engine{stateStore: reopened}
	ops, err := reopenedEngine.loadOperations()
	if err != nil {
		t.Fatalf("loadOperations: %v", err)
	}
	got, ok := ops["op-1"]
	if !ok || got != op {
		t.Fatalf("operation mismatch: got %+v want %+v", got, op)
	}
}

func storeFilePath(t *testing.T, store engine.StateStore) string {
	t.Helper()
	jsonStore, ok := store.(*engine.JSONStateStore)
	if !ok {
		t.Fatalf("unexpected store type %T", store)
	}
	return jsonStore.PathForTest()
}

func TestInFlightE2BOperationsFailOnRestart(t *testing.T) {
	dir := t.TempDir()
	startedAt := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	state := map[string]any{
		"version": 1,
		"e2b_operations": map[string]E2BOperation{
			"op-running": {OperationID: "op-running", State: OperationStateRunning, StartedAt: startedAt},
			"op-pending": {OperationID: "op-pending", State: legacyOperationStatePending, StartedAt: startedAt},
			"op-success": {OperationID: "op-success", State: OperationStateSucceeded, StartedAt: startedAt, FinishedAt: startedAt.Add(time.Second)},
		},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := engine.NewJSONStateStore(dir)
	if err != nil {
		t.Fatalf("NewJSONStateStore: %v", err)
	}
	e := &Engine{stateStore: store, tracker: newPodTracker(), client: &fakeSandboxServiceClient{}}
	if err := e.RestoreState(context.Background()); err != nil {
		t.Fatalf("RestoreState: %v", err)
	}

	ops, err := e.loadOperations()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"op-running", "op-pending"} {
		op := ops[id]
		if op.State != OperationStateFailed || op.Error != operationRestartError || op.FinishedAt.IsZero() {
			t.Fatalf("%s not failed: %+v", id, op)
		}
	}
	if success := ops["op-success"]; success.State != OperationStateSucceeded || success.FinishedAt.IsZero() {
		t.Fatalf("completed operation was mutated: %+v", success)
	}
}

func TestE2BPodStateNewFieldsRoundTrip(t *testing.T) {
	e, _ := newStateTestEngine(t)
	pod := E2BPodState{
		SandboxID:    "cri-a",
		E2BSandboxID: "e2b-a",
		TemplateID:   "tmpl-a",
		BuildID:      "build-a",
		ExecutionID:  "exec-a",
		TeamID:       "team-a",
		NodeName:     "node-a",
	}
	if err := e.savePod(pod); err != nil {
		t.Fatalf("savePod: %v", err)
	}
	pods, err := e.loadPods()
	if err != nil || len(pods) != 1 {
		t.Fatalf("loadPods: pods=%+v err=%v", pods, err)
	}
	got := pods[0]
	if got.ExecutionID != "exec-a" || got.TeamID != "team-a" || got.NodeName != "node-a" {
		t.Fatalf("new fields mismatch: %+v", got)
	}

	info := podInfoFromPersistedState(got)
	if info.executionID != "exec-a" || info.teamID != "team-a" || info.nodeName != "node-a" {
		t.Fatalf("podInfo mismatch: %+v", info)
	}
	back := info.toPersistedState()
	if back.ExecutionID != "exec-a" || back.TeamID != "team-a" || back.NodeName != "node-a" {
		t.Fatalf("persisted state mismatch: %+v", back)
	}
}

func TestLegacyStateJSONWithoutNewFields(t *testing.T) {
	dir := t.TempDir()
	legacy := `{
	  "version": 1,
	  "e2b": {
	    "pods": [{
	      "sandbox_id": "cri-old",
	      "e2b_sandbox_id": "e2b-old",
	      "pod_uid": "uid-old",
	      "name": "pod-old",
	      "namespace": "ns",
	      "created_at": "2024-01-01T00:00:00Z",
	      "state": 0,
	      "template_id": "tmpl",
	      "build_id": "build",
	      "image_ref": "",
	      "container_stdin": false,
	      "container_tty": false,
	      "container_state": 0,
	      "container_created_at": "2024-01-01T00:00:00Z",
	      "container_started_at": "2024-01-01T00:00:00Z",
	      "container_finished_at": "2024-01-01T00:00:00Z",
	      "container_exit_code": 0,
	      "cni_enabled": false,
	      "updated_at": "2024-01-01T00:00:00Z"
	    }]
	  },
	  "android": {}
	}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := engine.NewJSONStateStore(dir)
	if err != nil {
		t.Fatalf("NewJSONStateStore: %v", err)
	}
	e := &Engine{stateStore: store}
	pods, err := e.loadPods()
	if err != nil || len(pods) != 1 {
		t.Fatalf("loadPods: pods=%+v err=%v", pods, err)
	}
	if pods[0].SandboxID != "cri-old" || pods[0].ExecutionID != "" || pods[0].TeamID != "" || pods[0].NodeName != "" {
		t.Fatalf("legacy pod mismatch: %+v", pods[0])
	}
	ops, err := e.loadOperations()
	if err != nil || len(ops) != 0 {
		t.Fatalf("legacy store should have no operations: ops=%+v err=%v", ops, err)
	}
}
