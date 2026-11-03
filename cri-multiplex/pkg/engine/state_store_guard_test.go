package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBackendStateRejectsReservedSections(t *testing.T) {
	store, err := NewJSONStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewJSONStateStore: %v", err)
	}
	for _, path := range []string{"version", "routes", "cleanup_tasks", "routes.extra", "cleanup_tasks.x"} {
		if err := store.SaveBackendState(path, map[string]string{"k": "v"}); err == nil {
			t.Fatalf("SaveBackendState(%q) accepted a reserved section", path)
		}
		if _, err := store.LoadBackendState(path, &map[string]string{}); err == nil {
			t.Fatalf("LoadBackendState(%q) accepted a reserved section", path)
		}
		if err := store.DeleteBackendState(path); err == nil {
			t.Fatalf("DeleteBackendState(%q) accepted a reserved section", path)
		}
	}
}

func TestCorruptStateFileIsQuarantinedAndDegradedToEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("{not-json"), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := NewJSONStateStore(dir)
	if err != nil {
		t.Fatalf("NewJSONStateStore must tolerate corrupt state and start empty, got %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("corrupt state.json should be quarantined, stat err = %v", err)
	}
	backups, err := filepath.Glob(path + ".corrupt-*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("backups = %v, err = %v", backups, err)
	}
	routes, err := store.LoadRoutes()
	if err != nil || len(routes) != 0 {
		t.Fatalf("degraded store should be empty, routes=%v err=%v", routes, err)
	}
	// 降级后必须能继续持久化：下一次保存要重建 state.json。
	if err := store.SaveRoute(RouteRecord{Kind: "pod", ID: "p1", Engine: EngineTypeE2B}); err != nil {
		t.Fatalf("SaveRoute after degrade: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state.json should be recreated after first save: %v", err)
	}
}
