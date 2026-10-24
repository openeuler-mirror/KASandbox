package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/cri-multiplex/pkg/engine/util"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newCleanupTestStore(t *testing.T) *JSONStateStore {
	t.Helper()
	store, err := NewJSONStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewJSONStateStore: %v", err)
	}
	return store
}

// newCleanupManagerForTests 用注册表构造 CleanupManager，供直接调用 scan*/reconcile* 的用例使用。
func newCleanupManagerForTests(t *testing.T, engines ...RuntimeEngine) *CleanupManager {
	t.Helper()
	reg := NewRegistry("")
	for _, eng := range engines {
		if eng == nil {
			continue
		}
		if err := reg.Register(BackendModule{
			Descriptor: BackendDescriptor{Type: eng.Type(), Handler: string(eng.Type())},
			Engine:     eng,
		}); err != nil {
			t.Fatalf("register %s: %v", eng.Type(), err)
		}
	}
	if err := reg.Freeze(); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	return NewCleanupManager(nil, reg, CleanupConfig{})
}

func TestCleanupE2BNotFoundIsIdempotent(t *testing.T) {
	store := newCleanupTestStore(t)
	client := &fakeSandboxServiceClient{deleteErr: status.Error(codes.NotFound, "sandbox not found")}
	e := newTestGRPCE2BEngine(client)
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
	if err := store.SaveE2BPod(pod.toPersistedState()); err != nil {
		t.Fatal(err)
	}
	_ = store.SaveRoute(RouteRecord{Kind: "pod", ID: pod.sandboxID, Engine: EngineTypeE2B})

	for _, reason := range []string{"test", "repeat"} {
		if err := e.CleanupSandbox(context.Background(), pod.sandboxID, reason); err != nil {
			t.Fatalf("cleanup %s: %v", reason, err)
		}
	}
	pods, _ := store.LoadE2BPods()
	routes, _ := store.LoadRoutes()
	if len(pods) != 0 || len(routes) != 0 {
		t.Fatalf("state remains after cleanup: pods=%+v routes=%+v", pods, routes)
	}
}

func TestCleanupAndroidNotFoundIsIdempotent(t *testing.T) {
	store := newCleanupTestStore(t)
	stateDir := t.TempDir()
	artifactsDir := t.TempDir()
	e := NewAndroidEngine(AndroidConfig{Enabled: true, StateDir: stateDir, ArtifactsDir: artifactsDir, StateStore: store})
	e.cniManager = &fakeCNIManager{delErr: os.ErrNotExist}
	e.ops.stopCVD = func(*AndroidEngine, context.Context, *AndroidSandboxRecord) error { return syscall.ESRCH }
	rec := &AndroidSandboxRecord{
		CRISandboxID: "android-pod", PodUID: "android-pod", State: androidSandboxUnknown,
		WorkDir: filepath.Join(stateDir, "android-pod"), ArtifactsDir: artifactsDir,
		LaunchPID: 999999, LaunchPGID: 999999,
		CNIRecord: &util.CNIRecord{SandboxID: "android-pod", NetNSPath: "/missing/android-pod"},
	}
	e.pods[rec.CRISandboxID] = rec
	if err := store.SaveAndroidPod(androidPodStateFromRecords(rec, nil)); err != nil {
		t.Fatal(err)
	}

	for _, reason := range []string{"test", "repeat"} {
		if err := e.CleanupSandbox(context.Background(), rec.CRISandboxID, reason); err != nil {
			t.Fatalf("cleanup %s: %v", reason, err)
		}
	}
	pods, _ := store.LoadAndroidPods()
	if len(pods) != 0 {
		t.Fatalf("android state remains: %+v", pods)
	}
}

func TestE2BReconcileDeletesOrphanHostPortRules(t *testing.T) {
	owned := util.HostPortRuleComment("192.0.2.10", 20000, "10.0.0.2", 49983)
	data := []byte("*nat\n" +
		"-A PREROUTING -p tcp --dport 20000 -m comment --comment \"" + owned + "\" -j DNAT --to-destination 10.0.0.2:49983\n" +
		"-A PREROUTING -p tcp --dport 20001 -j DNAT --to-destination 10.0.0.3:49983\nCOMMIT\n")
	var deleted []util.IPTablesOwnerRule
	e := &grpcE2BEngine{hostOps: util.HostResourceOps{
		ListIPTables: func(context.Context) ([]byte, error) { return data, nil },
		DeleteIPTables: func(_ context.Context, rule util.IPTablesOwnerRule) error {
			deleted = append(deleted, rule)
			return nil
		},
	}}
	if err := e.Reconcile(context.Background(), ReconcileOptions{}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(deleted) != 1 || deleted[0].Comment != owned {
		t.Fatalf("deleted rules = %+v", deleted)
	}
}

func TestHostResourceParsersOnlyReturnOwnedRules(t *testing.T) {
	iptablesData := []byte("*filter\n" +
		"-A FORWARD -m comment --comment \"cri-multiplex:hostport:a\" -j ACCEPT\n" +
		"-A FORWARD -m comment --comment \"someone-else\" -j ACCEPT\nCOMMIT\n")
	iptablesRules := util.ParseIPTablesOwnerRules(iptablesData)
	if len(iptablesRules) != 1 || iptablesRules[0].Comment != "cri-multiplex:hostport:a" {
		t.Fatalf("iptables owner rules = %+v", iptablesRules)
	}

	nftData := []byte("table ip nat {\n chain prerouting {\n" +
		"tcp dport 20000 counter comment \"cri-multiplex:hostport:a\" # handle 12\n" +
		"tcp dport 20001 counter comment \"someone-else\" # handle 13\n }\n}\n")
	nftRules := util.ParseNFTablesOwnerRules(nftData)
	if len(nftRules) != 1 || nftRules[0].Handle != "12" || nftRules[0].Comment != "cri-multiplex:hostport:a" {
		t.Fatalf("nftables owner rules = %+v", nftRules)
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
	e := &grpcE2BEngine{
		cniConfig:    util.CNIConfig{Enabled: true, NetNSDir: root, NetNSPrefix: "e2b-"},
		pendingNetNS: map[string]struct{}{"e2b-pending": {}},
		hostOps: util.HostResourceOps{
			DeleteNamedNetNS: func(name string) error {
				deleted = append(deleted, name)
				return nil
			},
		},
	}
	if err := e.Reconcile(context.Background(), ReconcileOptions{}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != "e2b-orphan" {
		t.Fatalf("deleted netns = %v", deleted)
	}
}

func TestAndroidReconcileProtectsPendingResources(t *testing.T) {
	stateDir := t.TempDir()
	netNSDir := t.TempDir()
	sandboxID := "android-pending-sandbox"
	pendingDir := filepath.Join(stateDir, sandboxID)
	orphanDir := filepath.Join(stateDir, "android-orphan-sandbox")
	for path, id := range map[string]string{pendingDir: sandboxID, orphanDir: "android-orphan-sandbox"} {
		if err := util.WriteResourceOwner(path, util.ResourceOwner{Owner: "cri-multiplex", Runtime: string(EngineTypeAndroid), SandboxID: id}); err != nil {
			t.Fatal(err)
		}
	}

	e := NewAndroidEngine(AndroidConfig{
		StateDir:     stateDir,
		ArtifactsDir: filepath.Join(t.TempDir(), "cf17"),
		CNI:          util.CNIConfig{Enabled: true, NetNSDir: netNSDir, NetNSPrefix: "android-"},
	})
	e.markPendingSandbox(sandboxID)
	defer e.unmarkPendingSandbox(sandboxID)
	pendingNetNS := "android-" + util.ShortID(sandboxID)
	orphanNetNS := "android-orphan"
	for _, name := range []string{pendingNetNS, orphanNetNS} {
		if err := os.WriteFile(filepath.Join(netNSDir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var deleted []string
	e.hostOps = util.HostResourceOps{
		DeleteNamedNetNS: func(name string) error {
			deleted = append(deleted, name)
			return nil
		},
	}
	if err := e.Reconcile(context.Background(), ReconcileOptions{}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, err := os.Stat(pendingDir); err != nil {
		t.Fatalf("pending workdir was removed: %v", err)
	}
	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Fatalf("orphan workdir still exists: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != orphanNetNS {
		t.Fatalf("deleted netns = %v", deleted)
	}
}

func TestAndroidUnexpectedExitMarksState(t *testing.T) {
	store := newCleanupTestStore(t)
	e := NewAndroidEngine(AndroidConfig{Enabled: true, StateDir: t.TempDir(), ArtifactsDir: t.TempDir(), StateStore: store})
	pod := &AndroidSandboxRecord{CRISandboxID: "exit-pod", PodUID: "exit-pod", State: androidSandboxRunning}
	container := &AndroidContainerRecord{ContainerID: "exit-pod-c", CRISandboxID: "exit-pod", State: androidContainerRunning}
	e.pods[pod.CRISandboxID] = pod
	e.containers[container.ContainerID] = container

	e.handleUnexpectedCVDExit(pod.CRISandboxID, nil)
	if pod.State != androidSandboxUnknown || container.State != androidContainerExited {
		t.Fatalf("unexpected exit state: pod=%s container=%s", pod.State, container.State)
	}
}

func TestE2BReconcileRebuildsAndPrunesOwnRoutes(t *testing.T) {
	store := newCleanupTestStore(t)
	if err := store.SaveE2BPod(E2BPodState{
		SandboxID: "live", State: stateRunning, ContainerName: "app", UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRoute(RouteRecord{Kind: "pod", ID: "orphan", Engine: EngineTypeE2B}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRoute(RouteRecord{Kind: "pod", ID: "android-live", Engine: EngineTypeAndroid}); err != nil {
		t.Fatal(err)
	}

	e := &grpcE2BEngine{stateStore: store, client: &fakeSandboxServiceClient{}}
	if err := e.Reconcile(context.Background(), ReconcileOptions{GracePeriod: time.Hour}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	routes, _ := store.LoadRoutes()
	got := map[string]EngineType{}
	for _, route := range routes {
		got[route.Kind+":"+route.ID] = route.Engine
	}
	if got["pod:live"] != EngineTypeE2B || got["container:live-c"] != EngineTypeE2B {
		t.Fatalf("missing rebuilt routes: %+v", got)
	}
	if _, ok := got["pod:orphan"]; ok {
		t.Fatalf("orphan route not removed: %+v", got)
	}
	if got["pod:android-live"] != EngineTypeAndroid {
		t.Fatalf("android route was touched by the e2b reconciler: %+v", got)
	}
}

func TestCleanupManagerReconcileRunsModuleReconcilers(t *testing.T) {
	store := newCleanupTestStore(t)
	var called bool
	reg := NewRegistry("")
	if err := reg.Register(BackendModule{
		Descriptor: BackendDescriptor{Type: EngineTypeE2B, Handler: "e2b"},
		Engine:     &stubEngine{typ: EngineTypeE2B},
		Reconciler: reconcileFunc(func(context.Context, ReconcileOptions) error { called = true; return nil }),
	}); err != nil {
		t.Fatal(err)
	}
	_ = reg.Freeze()
	m := NewCleanupManager(store, reg, CleanupConfig{Enabled: true})
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !called {
		t.Fatal("module reconciler was not called")
	}
}

type reconcileFunc func(context.Context, ReconcileOptions) error

func (f reconcileFunc) Reconcile(ctx context.Context, opts ReconcileOptions) error {
	return f(ctx, opts)
}

func TestCleanupRetryTaskDeletedAfterSuccess(t *testing.T) {
	store := newCleanupTestStore(t)
	now := time.Now()
	cleaner := &stubCleaner{err: errors.New("temporary cleanup failure")}
	m := NewCleanupManager(store, testCleanupRegistry(t, EngineTypeE2B, cleaner), CleanupConfig{MaxRetries: 3})
	m.now = func() time.Time { return now }

	first := m.Schedule(context.Background(), EngineTypeE2B, "retry-pod", "sandbox", "test")
	if first.Err == nil {
		t.Fatal("first cleanup should fail")
	}
	tasks, _ := store.LoadCleanupTasks()
	if len(tasks) != 1 || tasks[0].Attempts != 1 {
		t.Fatalf("cleanup tasks after failure = %+v", tasks)
	}

	cleaner.err = nil
	now = tasks[0].NextRetryAt.Add(time.Millisecond)
	if err := m.RetryPending(context.Background()); err != nil {
		t.Fatalf("RetryPending: %v", err)
	}
	tasks, _ = store.LoadCleanupTasks()
	if len(tasks) != 0 || cleaner.callCount() != 2 {
		t.Fatalf("retry did not converge: calls=%d tasks=%+v", cleaner.callCount(), tasks)
	}
}

func TestAndroidReconcileKeepsUnknownOwnerDirectory(t *testing.T) {
	stateDir := t.TempDir()
	unknown := filepath.Join(stateDir, "android-similar")
	if err := os.MkdirAll(unknown, 0o755); err != nil {
		t.Fatal(err)
	}
	e := NewAndroidEngine(AndroidConfig{StateDir: stateDir, ArtifactsDir: filepath.Join(t.TempDir(), "cf17")})
	if err := e.Reconcile(context.Background(), ReconcileOptions{}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, err := os.Stat(unknown); err != nil {
		t.Fatalf("unknown owner directory was removed: %v", err)
	}
}
