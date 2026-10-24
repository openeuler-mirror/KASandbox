package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestBackendStateLayoutPreservesLegacySections(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONStateStore(dir)
	if err != nil {
		t.Fatalf("NewJSONStateStore: %v", err)
	}
	if err := store.SaveE2BPod(E2BPodState{SandboxID: "sb1"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveE2BOperation(E2BOperation{OperationID: "op1", Action: "Pause"}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("state.json is not an object: %v", err)
	}
	if _, ok := raw["e2b"]; !ok {
		t.Fatalf("missing top-level e2b section: %s", data)
	}
	if _, ok := raw["e2b_operations"]; !ok {
		t.Fatalf("missing legacy top-level e2b_operations section: %s", data)
	}
	var e2b map[string]json.RawMessage
	if err := json.Unmarshal(raw["e2b"], &e2b); err != nil {
		t.Fatal(err)
	}
	if _, ok := e2b["pods"]; !ok {
		t.Fatalf("missing e2b.pods: %s", data)
	}
}

func TestBackendStateRoundTripAndDelete(t *testing.T) {
	store, err := NewJSONStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewJSONStateStore: %v", err)
	}
	type payload struct {
		Value string `json:"value"`
	}
	if err := store.SaveBackendState("cube.state", payload{Value: "v1"}); err != nil {
		t.Fatal(err)
	}
	var got payload
	ok, err := store.LoadBackendState("cube.state", &got)
	if err != nil || !ok || got.Value != "v1" {
		t.Fatalf("LoadBackendState = (%v, %v, %+v)", ok, err, got)
	}
	if err := store.DeleteBackendState("cube.state"); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.LoadBackendState("cube.state", &got); err != nil || ok {
		t.Fatalf("state survived delete: ok=%v err=%v", ok, err)
	}
}

func TestBackendStateKeepsUnknownSections(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"e2b":{"pods":[{"sandbox_id":"legacy"}]},"e2b_operations":{"op1":{"operation_id":"op1"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewJSONStateStore(dir)
	if err != nil {
		t.Fatalf("NewJSONStateStore: %v", err)
	}
	if err := store.SaveBackendState("android.pods", []any{}); err != nil {
		t.Fatal(err)
	}
	var pods []E2BPodState
	if ok, err := store.LoadBackendState("e2b.pods", &pods); err != nil || !ok || len(pods) != 1 || pods[0].SandboxID != "legacy" {
		t.Fatalf("legacy e2b.pods lost: ok=%v err=%v pods=%+v", ok, err, pods)
	}
}
