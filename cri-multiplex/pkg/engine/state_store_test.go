package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newStateStoreTestStore(t *testing.T) *JSONStateStore {
	t.Helper()
	store, err := NewJSONStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewJSONStateStore: %v", err)
	}
	return store
}

func TestE2BOperationRoundTrip(t *testing.T) {
	store := newStateStoreTestStore(t)
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
	if err := store.SaveE2BOperation(op); err != nil {
		t.Fatalf("SaveE2BOperation: %v", err)
	}

	// 重新打开同一文件，验证落盘内容可恢复
	reopened, err := NewJSONStateStore(filepath.Dir(store.path))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	ops, err := reopened.LoadE2BOperations()
	if err != nil {
		t.Fatalf("LoadE2BOperations: %v", err)
	}
	got, ok := ops["op-1"]
	if !ok {
		t.Fatalf("operation not persisted: %+v", ops)
	}
	if got != op {
		t.Fatalf("operation mismatch:\n got %+v\nwant %+v", got, op)
	}

	if err := reopened.DeleteE2BOperation("op-1"); err != nil {
		t.Fatalf("DeleteE2BOperation: %v", err)
	}
	ops, _ = reopened.LoadE2BOperations()
	if len(ops) != 0 {
		t.Fatalf("operations should be empty after delete: %+v", ops)
	}
}

func TestInFlightE2BOperationsFailOnRestart(t *testing.T) {
	dir := t.TempDir()
	startedAt := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	state := persistedState{
		Version: 1,
		E2BOperations: map[string]E2BOperation{
			"op-running": {
				OperationID: "op-running",
				State:       OperationStateRunning,
				StartedAt:   startedAt,
			},
			"op-pending": {
				OperationID: "op-pending",
				State:       legacyOperationStatePending,
				StartedAt:   startedAt,
			},
			"op-success": {
				OperationID: "op-success",
				State:       OperationStateSucceeded,
				StartedAt:   startedAt,
				FinishedAt:  startedAt.Add(time.Second),
			},
		},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := NewJSONStateStore(dir)
	if err != nil {
		t.Fatalf("NewJSONStateStore: %v", err)
	}
	ops, err := store.LoadE2BOperations()
	if err != nil {
		t.Fatalf("LoadE2BOperations: %v", err)
	}
	for _, id := range []string{"op-running", "op-pending"} {
		got := ops[id]
		if got.State != OperationStateFailed || got.Error != operationRestartError || got.FinishedAt.IsZero() {
			t.Fatalf("%s not failed after restart: %+v", id, got)
		}
	}
	if got := ops["op-success"]; got.State != OperationStateSucceeded || !got.FinishedAt.Equal(startedAt.Add(time.Second)) {
		t.Fatalf("terminal operation should be unchanged: %+v", got)
	}

	reopened, err := NewJSONStateStore(dir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	ops, err = reopened.LoadE2BOperations()
	if err != nil {
		t.Fatalf("LoadE2BOperations after reopen: %v", err)
	}
	if ops["op-running"].State != OperationStateFailed || ops["op-pending"].State != OperationStateFailed {
		t.Fatalf("recovered operation state was not persisted: %+v", ops)
	}
}

func TestE2BPodStateNewFieldsRoundTrip(t *testing.T) {
	store := newStateStoreTestStore(t)
	pod := E2BPodState{
		SandboxID:    "cri-a",
		E2BSandboxID: "e2b-a",
		TemplateID:   "tmpl-a",
		BuildID:      "build-a",
		ExecutionID:  "exec-a",
		TeamID:       "team-a",
		NodeName:     "node-a",
	}
	if err := store.SaveE2BPod(pod); err != nil {
		t.Fatalf("SaveE2BPod: %v", err)
	}
	pods, err := store.LoadE2BPods()
	if err != nil {
		t.Fatalf("LoadE2BPods: %v", err)
	}
	if len(pods) != 1 {
		t.Fatalf("pods = %+v", pods)
	}
	got := pods[0]
	if got.ExecutionID != "exec-a" || got.TeamID != "team-a" || got.NodeName != "node-a" {
		t.Fatalf("new fields mismatch: %+v", got)
	}

	// podInfo 双向转换也要带新字段
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
	store, err := NewJSONStateStore(dir)
	if err != nil {
		t.Fatalf("NewJSONStateStore: %v", err)
	}
	pods, err := store.LoadE2BPods()
	if err != nil || len(pods) != 1 {
		t.Fatalf("LoadE2BPods: pods=%+v err=%v", pods, err)
	}
	if pods[0].SandboxID != "cri-old" || pods[0].ExecutionID != "" || pods[0].TeamID != "" || pods[0].NodeName != "" {
		t.Fatalf("legacy pod mismatch: %+v", pods[0])
	}
	ops, err := store.LoadE2BOperations()
	if err != nil || len(ops) != 0 {
		t.Fatalf("legacy store should have no operations: ops=%+v err=%v", ops, err)
	}
}

func TestE2BOperationJSONFieldNames(t *testing.T) {
	data, err := json.Marshal(E2BOperation{OperationID: "op", Action: "Pause", State: OperationStateRunning})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"operation_id", "action", "state"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("json key %q missing in %s", key, data)
		}
	}
}
