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

func TestRouteTablePersistsProductAndNativeIDs(t *testing.T) {
	store, err := NewJSONStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewJSONStateStore: %v", err)
	}
	rt := NewRouteTable(store)
	if err := rt.SetPodRoute("cri-1", EngineTypeE2B, "product-1", "019a-native-uuid"); err != nil {
		t.Fatalf("SetPodRoute: %v", err)
	}

	restored := NewRouteTable(store)
	if err := restored.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	route, ok := restored.PodRoute("cri-1")
	if !ok {
		t.Fatal("pod route was not restored")
	}
	if route.Backend != EngineTypeE2B ||
		route.ProductSandboxID != "product-1" ||
		route.NativeSandboxID != "019a-native-uuid" {
		t.Fatalf("restored pod route mismatch: %+v", route)
	}
}
func TestRouteTableSetPodPreservesAgentEnvIDs(t *testing.T) {
	store, err := NewJSONStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewJSONStateStore: %v", err)
	}
	rt := NewRouteTable(store)
	if err := rt.SetPodRoute("cri-1", EngineTypeAgentENV, "product-1", "native-1"); err != nil {
		t.Fatalf("SetPodRoute: %v", err)
	}
	if err := rt.SetPod("cri-1", EngineTypeAgentENV); err != nil {
		t.Fatalf("SetPod: %v", err)
	}
	route, ok := rt.PodRouteByCRI("cri-1")
	if !ok || route.ProductSandboxID != "product-1" || route.NativeSandboxID != "native-1" {
		t.Fatalf("SetPod erased AgentENV IDs: %+v ok=%v", route, ok)
	}
}

func TestRouteTableBindPodRouteAddsCRIWithoutDuplicateRecord(t *testing.T) {
	store, err := NewJSONStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewJSONStateStore: %v", err)
	}
	rt := NewRouteTable(store)
	if err := rt.PutPodRoute(PodRoute{
		Backend:          EngineTypeAgentENV,
		ProductSandboxID: "product-1",
		NativeSandboxID:  "native-1",
	}); err != nil {
		t.Fatalf("PutPodRoute: %v", err)
	}
	bound, found, err := rt.BindPodRoute("product-1", "cri-1", EngineTypeAgentENV)
	if err != nil {
		t.Fatalf("BindPodRoute: %v", err)
	}
	if !found || bound.CRISandboxID != "cri-1" {
		t.Fatalf("bound route mismatch: %+v found=%v", bound, found)
	}

	restored := NewRouteTable(store)
	if err := restored.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for name, lookup := range map[string]func() (PodRoute, bool){
		"CRI":     func() (PodRoute, bool) { return restored.PodRouteByCRI("cri-1") },
		"product": func() (PodRoute, bool) { return restored.PodRouteByProduct("product-1") },
		"native":  func() (PodRoute, bool) { return restored.PodRouteByNative("native-1") },
	} {
		route, ok := lookup()
		if !ok || route.CRISandboxID != "cri-1" || route.ProductSandboxID != "product-1" || route.NativeSandboxID != "native-1" {
			t.Fatalf("%s mapping mismatch: %+v ok=%v", name, route, ok)
		}
	}
	count := 0
	for _, rec := range restored.Snapshot() {
		if rec.Kind == "pod" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("pod route count = %d, want 1", count)
	}
}

func TestRouteTableRejectsConflictingMappings(t *testing.T) {
	rt := NewRouteTable(nil)
	if err := rt.PutPodRoute(PodRoute{
		Backend:          EngineTypeAgentENV,
		ProductSandboxID: "product-1",
		NativeSandboxID:  "native-1",
	}); err != nil {
		t.Fatalf("seed route: %v", err)
	}
	if err := rt.PutPodRoute(PodRoute{
		Backend:          EngineTypeAgentENV,
		ProductSandboxID: "product-1",
		NativeSandboxID:  "native-2",
	}); err == nil {
		t.Fatal("conflicting native ID was accepted")
	}
}

func TestRouteTableDeleteByProductRemovesAllIndexesAndContainers(t *testing.T) {
	rt := NewRouteTable(nil)
	if err := rt.PutPodRoute(PodRoute{
		Backend:          EngineTypeAgentENV,
		CRISandboxID:     "cri-1",
		ProductSandboxID: "product-1",
		NativeSandboxID:  "native-1",
	}); err != nil {
		t.Fatalf("PutPodRoute: %v", err)
	}
	if err := rt.SetContainer("container-1", "cri-1", EngineTypeAgentENV); err != nil {
		t.Fatalf("SetContainer: %v", err)
	}
	if err := rt.DeleteSandbox("product-1"); err != nil {
		t.Fatalf("DeleteSandbox: %v", err)
	}
	for name, lookup := range map[string]func() (PodRoute, bool){
		"CRI":     func() (PodRoute, bool) { return rt.PodRouteByCRI("cri-1") },
		"product": func() (PodRoute, bool) { return rt.PodRouteByProduct("product-1") },
		"native":  func() (PodRoute, bool) { return rt.PodRouteByNative("native-1") },
	} {
		if _, ok := lookup(); ok {
			t.Fatalf("%s index survived delete", name)
		}
	}
	if _, ok := rt.ContainerBackend("container-1"); ok {
		t.Fatal("container route survived delete")
	}
}

type countingStateStore struct {
	StateStore
	saveCalls int
}

func (s *countingStateStore) SaveRoute(rec RouteRecord) error {
	s.saveCalls++
	return s.StateStore.SaveRoute(rec)
}

type fallbackRouteStateStore struct {
	StateStore
	routeCalls   int
	backendCalls int
}

func (s *fallbackRouteStateStore) SaveRoute(rec RouteRecord) error {
	s.routeCalls++
	return s.StateStore.SaveRoute(rec)
}

func (s *fallbackRouteStateStore) SaveBackendState(path string, value any) error {
	s.backendCalls++
	return s.StateStore.SaveBackendState(path, value)
}

func TestRouteTableCombinedPutFallsBackForLegacyStore(t *testing.T) {
	base, err := NewJSONStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := &fallbackRouteStateStore{StateStore: base}
	rt := NewRouteTable(store)
	route := PodRoute{
		Backend:          EngineTypeAgentENV,
		CRISandboxID:     "cri-fallback",
		ProductSandboxID: "product-fallback",
		NativeSandboxID:  "native-fallback",
	}
	if err := rt.PutPodRouteAndBackendState(route, "agentenv.pods", []string{"fallback"}); err != nil {
		t.Fatalf("PutPodRouteAndBackendState: %v", err)
	}
	if store.routeCalls != 1 || store.backendCalls != 1 {
		t.Fatalf("fallback calls route=%d backend=%d, want 1 each", store.routeCalls, store.backendCalls)
	}
	if restored, ok := rt.PodRouteByCRI("cri-fallback"); !ok || restored.NativeSandboxID != "native-fallback" {
		t.Fatalf("route memory mismatch: %+v ok=%v", restored, ok)
	}
}

func TestRouteTableIdenticalPutDoesNotPersistAgain(t *testing.T) {
	base, err := NewJSONStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewJSONStateStore: %v", err)
	}
	store := &countingStateStore{StateStore: base}
	rt := NewRouteTable(store)
	route := PodRoute{
		Backend:          EngineTypeAgentENV,
		CRISandboxID:     "cri-idempotent",
		ProductSandboxID: "product-idempotent",
		NativeSandboxID:  "native-idempotent",
	}
	if err := rt.PutPodRoute(route); err != nil {
		t.Fatalf("first PutPodRoute: %v", err)
	}
	if err := rt.PutPodRoute(route); err != nil {
		t.Fatalf("second PutPodRoute: %v", err)
	}
	if store.saveCalls != 1 {
		t.Fatalf("identical route persisted %d times, want 1", store.saveCalls)
	}
}
