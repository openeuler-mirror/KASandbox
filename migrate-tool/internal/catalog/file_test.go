package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileSnapshotRejectsTrailingJSONValue(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(filename, []byte(`{"schema_version":"test","teams":[],"templates":[],"aliases":[],"builds":[],"assignments":[]} {}`), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog, err := NewFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	_, err = catalog.Snapshot(t.Context())
	if err == nil || !strings.Contains(err.Error(), "unexpected JSON value after catalog") {
		t.Fatalf("Snapshot() error = %v, want trailing JSON error", err)
	}
}
