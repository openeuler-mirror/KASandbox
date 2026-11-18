package engine

import (
	"errors"
	"testing"
)

type failingStateStore struct {
	StateStore
	saveErr   error
	deleteErr error
}

func (f *failingStateStore) SaveRoute(RouteRecord) error      { return f.saveErr }
func (f *failingStateStore) DeleteRoute(string, string) error { return f.deleteErr }

func TestRouteTableDeleteSandboxUsesRecordedSandboxID(t *testing.T) {
	store, err := NewJSONStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewJSONStateStore: %v", err)
	}
	rt := NewRouteTable(store)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(rt.SetPod("sb1", EngineTypeE2B))
	must(rt.SetContainer("sb1-app", "sb1", EngineTypeE2B))
	must(rt.SetContainer("sb1-sidecar", "sb1", EngineTypeE2B))
	must(rt.SetContainer("sb1-c", "", EngineTypeE2B)) // 旧记录：无 SandboxID，靠前缀兜底
	must(rt.SetContainer("ctr-b", "sb2", EngineTypeE2B))

	must(rt.DeleteSandbox("sb1"))
	for _, id := range []string{"sb1-app", "sb1-sidecar", "sb1-c"} {
		if _, ok := rt.ContainerBackend(id); ok {
			t.Fatalf("container route %s survived DeleteSandbox", id)
		}
	}
	if _, ok := rt.PodBackend("sb1"); ok {
		t.Fatal("pod route survived DeleteSandbox")
	}
	if _, ok := rt.ContainerBackend("ctr-b"); !ok {
		t.Fatal("unrelated container route was deleted")
	}
	routes, err := store.LoadRoutes()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range routes {
		if r.Kind == "container" && (r.ID == "sb1-app" || r.ID == "sb1-sidecar" || r.ID == "sb1-c") {
			t.Fatalf("persisted route survived: %+v", r)
		}
	}
}

func TestRouteTablePersistFailureIsReportedButKeepsMemory(t *testing.T) {
	rt := NewRouteTable(&failingStateStore{saveErr: errors.New("disk full")})
	if err := rt.SetPod("sb1", EngineTypeE2B); err == nil {
		t.Fatal("SetPod hid persistence failure")
	}
	if _, ok := rt.PodBackend("sb1"); !ok {
		t.Fatal("in-memory route missing after persistence failure")
	}
}

func TestRouteTableRestoreKeepsSandboxIDAndLegacyRecords(t *testing.T) {
	store, err := NewJSONStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewJSONStateStore: %v", err)
	}
	for _, rec := range []RouteRecord{
		{Kind: "container", ID: "sb1-c2", Engine: EngineTypeE2B},
		{Kind: "container", ID: "new-c", Engine: EngineTypeE2B, SandboxID: "sb1"},
		{Kind: "pod", ID: "sb1", Engine: EngineTypeE2B},
		{Kind: "bogus", ID: "x", Engine: EngineTypeE2B},
	} {
		if err := store.SaveRoute(rec); err != nil {
			t.Fatal(err)
		}
	}
	rt := NewRouteTable(store)
	if err := rt.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if err := rt.DeleteSandbox("sb1"); err != nil {
		t.Fatalf("DeleteSandbox: %v", err)
	}
	if _, ok := rt.ContainerBackend("new-c"); ok {
		t.Fatal("container with recorded SandboxID not deleted")
	}
	if _, ok := rt.ContainerBackend("sb1-c2"); ok {
		t.Fatal("legacy prefixed container not deleted")
	}
}
