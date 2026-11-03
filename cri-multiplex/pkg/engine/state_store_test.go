package engine

import (
	"encoding/json"
	"testing"
)

func newStateStoreTestStore(t *testing.T) *JSONStateStore {
	t.Helper()
	store, err := NewJSONStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewJSONStateStore: %v", err)
	}
	return store
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
