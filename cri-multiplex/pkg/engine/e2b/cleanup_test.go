package e2b

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cri-multiplex/pkg/engine"
	"github.com/cri-multiplex/pkg/engine/util"
)

func newTestEngine(client *fakeSandboxServiceClient) *Engine {
	return &Engine{
		client:          client,
		tracker:         newPodTracker(),
		hostPortOps:     util.DefaultHostPortMappingOps(),
		hostOps:         util.DefaultHostResourceOps(),
		stateStore:      nil,
		nodeIP:          "192.0.2.1",
		pendingNetNS:    map[string]struct{}{},
		imageCache:      map[string]*e2bImageMeta{},
		streamingReqs:   map[string]*execStreamRequest{},
		attachReqs:      map[string]*attachStreamRequest{},
		hostPortManager: util.NewHostPortManager(20000, 29999),
	}
}

func newCleanupTestStore(t *testing.T) engine.StateStore {
	t.Helper()
	store, err := engine.NewJSONStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewJSONStateStore: %v", err)
	}
	return store
}

func TestCleanupE2BNotFoundIsIdempotent(t *testing.T) {
	store := newCleanupTestStore(t)
	client := &fakeSandboxServiceClient{deleteErr: status.Error(codes.NotFound, "sandbox not found")}
	e := newTestEngine(client)
	e.stateStore = store
	e.cniManager = &fakeCNIManager{delErr: os.ErrNotExist}
	e.hostPortOps.Cleanup = func(string, int, string, int) error {
		return errors.New("Bad rule (does a matching rule exist in that chain?)")
	}
	pod := &podInfo{
		sandboxID: "e2b-pod", e2bSandboxID: "remote-e2b-pod", state: stateRunning,
		hostIP: "192.0.2.20", cniEnabled: true,
		cniRecord:    &util.CNIRecord{SandboxID: "e2b-pod", NetNSPath: "/missing/e2b-pod"},
		portMappings: []util.PortMapping{{HostPort: 21000, SandboxPort: 49983}},
	}
	e.tracker.Add(pod.sandboxID, pod)
	if err := saveE2BPodTo(store, pod.toPersistedState()); err != nil {
		t.Fatal(err)
	}
	_ = store.SaveRoute(engine.RouteRecord{Kind: "pod", ID: pod.sandboxID, Engine: engine.EngineTypeE2B})

	for _, reason := range []string{"test", "repeat"} {
		if err := e.CleanupSandbox(context.Background(), pod.sandboxID, reason); err != nil {
			t.Fatalf("cleanup %s: %v", reason, err)
		}
	}
	pods, _ := loadPodsFrom(t, store)
	routes, _ := store.LoadRoutes()
	if len(pods) != 0 || len(routes) != 0 {
		t.Fatalf("state remains after cleanup: pods=%+v routes=%+v", pods, routes)
	}
}

func TestE2BReconcileDeletesOrphanHostPortRules(t *testing.T) {
	owned := util.HostPortRuleComment("192.0.2.10", 20000, "10.0.0.2", 49983)
	data := []byte("*nat\n" +
		"-A PREROUTING -p tcp --dport 20000 -m comment --comment \"" + owned + "\" -j DNAT --to-destination 10.0.0.2:49983\n" +
		"-A PREROUTING -p tcp --dport 20001 -j DNAT --to-destination 10.0.0.3:49983\nCOMMIT\n")
	var deleted []util.IPTablesOwnerRule
	e := &Engine{hostOps: util.HostResourceOps{
		ListIPTables: func(context.Context) ([]byte, error) { return data, nil },
		DeleteIPTables: func(_ context.Context, rule util.IPTablesOwnerRule) error {
			deleted = append(deleted, rule)
			return nil
		},
	}}
	if err := e.Reconcile(context.Background(), engine.ReconcileOptions{}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(deleted) != 1 || deleted[0].Comment != owned {
		t.Fatalf("deleted rules = %+v", deleted)
	}
}

func TestE2BReconcileProtectsPendingNetNS(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"e2b-pending", "e2b-orphan"} {
		if err := os.WriteFile(filepath.Join(root, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var deleted []string
	e := &Engine{
		cniConfig:    util.CNIConfig{Enabled: true, NetNSDir: root, NetNSPrefix: "e2b-"},
		pendingNetNS: map[string]struct{}{"e2b-pending": {}},
		hostOps: util.HostResourceOps{
			DeleteNamedNetNS: func(name string) error {
				deleted = append(deleted, name)
				return nil
			},
		},
	}
	if err := e.Reconcile(context.Background(), engine.ReconcileOptions{}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != "e2b-orphan" {
		t.Fatalf("deleted netns = %v", deleted)
	}
}

func TestE2BReconcileRebuildsAndPrunesOwnRoutes(t *testing.T) {
	store := newCleanupTestStore(t)
	if err := saveE2BPodTo(store, E2BPodState{
		SandboxID: "live", State: stateRunning, ContainerName: "app", UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRoute(engine.RouteRecord{Kind: "pod", ID: "orphan", Engine: engine.EngineTypeE2B}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRoute(engine.RouteRecord{Kind: "pod", ID: "android-live", Engine: engine.EngineTypeAndroid}); err != nil {
		t.Fatal(err)
	}

	e := &Engine{stateStore: store, client: &fakeSandboxServiceClient{}}
	if err := e.Reconcile(context.Background(), engine.ReconcileOptions{GracePeriod: time.Hour}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	routes, _ := store.LoadRoutes()
	got := map[string]engine.EngineType{}
	for _, route := range routes {
		got[route.Kind+":"+route.ID] = route.Engine
	}
	if got["pod:live"] != engine.EngineTypeE2B || got["container:live-c"] != engine.EngineTypeE2B {
		t.Fatalf("missing rebuilt routes: %+v", got)
	}
	if _, ok := got["pod:orphan"]; ok {
		t.Fatalf("orphan route not removed: %+v", got)
	}
	if got["pod:android-live"] != engine.EngineTypeAndroid {
		t.Fatalf("android route was touched by the e2b reconciler: %+v", got)
	}
}
