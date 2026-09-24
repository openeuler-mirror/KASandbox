package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cri-multiplex/pkg/orchestrator"
)

func newCleanupTestStore(t *testing.T) *JSONStateStore {
	t.Helper()
	store, err := NewJSONStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewJSONStateStore: %v", err)
	}
	return store
}

func TestCleanupE2BNotFoundIsIdempotent(t *testing.T) {
	store := newCleanupTestStore(t)
	client := &fakeSandboxServiceClient{deleteErr: status.Error(codes.NotFound, "sandbox not found")}
	e := newTestGRPCE2BEngine(client)
	e.stateStore = store
	e.cniManager = &fakeCNIManager{delErr: os.ErrNotExist}
	e.hostPortOps.cleanup = func(string, int, string, int) error {
		return errors.New("Bad rule (does a matching rule exist in that chain?)")
	}
	pod := &podInfo{
		sandboxID: "e2b-pod", e2bSandboxID: "remote-e2b-pod", state: stateRunning,
		hostIP: "192.0.2.20", cniEnabled: true,
		cniRecord:    &CNIRecord{SandboxID: "e2b-pod", NetNSPath: "/missing/e2b-pod"},
		portMappings: []PortMapping{{HostPort: 21000, SandboxPort: 49983}},
	}
	e.tracker.Add(pod.sandboxID, pod)
	if err := store.SaveE2BPod(pod.toPersistedState()); err != nil {
		t.Fatal(err)
	}
	_ = store.SaveRoute(RouteRecord{Kind: "pod", ID: pod.sandboxID, Engine: EngineTypeE2B})

	m := NewCleanupManager(store, e, nil, CleanupConfig{Enabled: true})
	if result := m.CleanupE2BSandbox(context.Background(), pod.sandboxID, "test"); !result.Succeeded || result.Err != nil {
		t.Fatalf("cleanup result = %+v", result)
	}
	if result := m.CleanupE2BSandbox(context.Background(), pod.sandboxID, "repeat"); !result.Succeeded || result.Err != nil {
		t.Fatalf("repeat cleanup result = %+v", result)
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
		CNIRecord: &CNIRecord{SandboxID: "android-pod", NetNSPath: "/missing/android-pod"},
	}
	e.pods[rec.CRISandboxID] = rec
	if err := store.SaveAndroidPod(androidPodStateFromRecords(rec, nil)); err != nil {
		t.Fatal(err)
	}

	m := NewCleanupManager(store, nil, e, CleanupConfig{Enabled: true})
	for _, reason := range []string{"test", "repeat"} {
		if result := m.CleanupAndroidSandbox(context.Background(), rec.CRISandboxID, reason); !result.Succeeded || result.Err != nil {
			t.Fatalf("cleanup %s result = %+v", reason, result)
		}
	}
	pods, _ := store.LoadAndroidPods()
	if len(pods) != 0 {
		t.Fatalf("android state remains: %+v", pods)
	}
}

func TestHostPortCommentCleanupOnlyDeletesOwnedRules(t *testing.T) {
	owned := hostPortRuleComment("192.0.2.10", 20000, "10.0.0.2", 49983)
	data := []byte("*nat\n" +
		"-A PREROUTING -p tcp --dport 20000 -m comment --comment \"" + owned + "\" -j DNAT --to-destination 10.0.0.2:49983\n" +
		"-A PREROUTING -p tcp --dport 20001 -j DNAT --to-destination 10.0.0.3:49983\nCOMMIT\n")
	var deleted []iptablesOwnerRule
	m := &CleanupManager{cfg: CleanupConfig{}, now: time.Now, hostOps: hostResourceOps{
		listIPTables: func(context.Context) ([]byte, error) { return data, nil },
		deleteIPTables: func(_ context.Context, rule iptablesOwnerRule) error {
			deleted = append(deleted, rule)
			return nil
		},
	}}
	if err := m.scanHostPortRules(context.Background()); err != nil {
		t.Fatalf("scanHostPortRules: %v", err)
	}
	if len(deleted) != 1 || deleted[0].Comment != owned {
		t.Fatalf("deleted rules = %+v", deleted)
	}
}

func TestHostResourceParsersOnlyReturnOwnedRules(t *testing.T) {
	iptablesData := []byte("*filter\n" +
		"-A FORWARD -m comment --comment \"cri-multiplex:hostport:a\" -j ACCEPT\n" +
		"-A FORWARD -m comment --comment \"someone-else\" -j ACCEPT\nCOMMIT\n")
	iptablesRules := parseIPTablesOwnerRules(iptablesData)
	if len(iptablesRules) != 1 || iptablesRules[0].Comment != "cri-multiplex:hostport:a" {
		t.Fatalf("iptables owner rules = %+v", iptablesRules)
	}

	nftData := []byte("table ip nat {\n chain prerouting {\n" +
		"tcp dport 20000 counter comment \"cri-multiplex:hostport:a\" # handle 12\n" +
		"tcp dport 20001 counter comment \"someone-else\" # handle 13\n }\n}\n")
	nftRules := parseNFTablesOwnerRules(nftData)
	if len(nftRules) != 1 || nftRules[0].Handle != "12" || nftRules[0].Comment != "cri-multiplex:hostport:a" {
		t.Fatalf("nftables owner rules = %+v", nftRules)
	}
}

func TestPendingE2BNetNSIsProtectedFromOrphanScan(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"e2b-pending", "e2b-orphan"} {
		if err := os.WriteFile(filepath.Join(root, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	e := &grpcE2BEngine{
		cniConfig:    CNIConfig{Enabled: true, NetNSDir: root, NetNSPrefix: "e2b-"},
		pendingNetNS: map[string]int{"e2b-pending": 1},
	}
	var deleted []string
	m := &CleanupManager{e2b: e, hostOps: hostResourceOps{
		deleteNamedNetNS: func(name string) error {
			deleted = append(deleted, name)
			return nil
		},
	}}
	if err := m.scanOrphanNetNS(); err != nil {
		t.Fatalf("scanOrphanNetNS: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != "e2b-orphan" {
		t.Fatalf("deleted netns = %v", deleted)
	}
}

func TestPendingAndroidResourcesAreProtectedFromOrphanScan(t *testing.T) {
	stateDir := t.TempDir()
	netNSDir := t.TempDir()
	sandboxID := "android-pending-sandbox"
	pendingDir := filepath.Join(stateDir, sandboxID)
	orphanDir := filepath.Join(stateDir, "android-orphan-sandbox")
	for path, id := range map[string]string{pendingDir: sandboxID, orphanDir: "android-orphan-sandbox"} {
		if err := writeResourceOwner(path, ResourceOwner{Owner: "cri-multiplex", Runtime: EngineTypeAndroid, SandboxID: id}); err != nil {
			t.Fatal(err)
		}
	}

	e := NewAndroidEngine(AndroidConfig{
		StateDir:     stateDir,
		ArtifactsDir: filepath.Join(t.TempDir(), "cf17"),
		CNI:          CNIConfig{Enabled: true, NetNSDir: netNSDir, NetNSPrefix: "android-"},
	})
	e.markPendingSandbox(sandboxID)
	defer e.unmarkPendingSandbox(sandboxID)
	pendingNetNS := "android-" + shortID(sandboxID)
	orphanNetNS := "android-orphan"
	for _, name := range []string{pendingNetNS, orphanNetNS} {
		if err := os.WriteFile(filepath.Join(netNSDir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var deleted []string
	m := &CleanupManager{android: e, hostOps: hostResourceOps{
		deleteNamedNetNS: func(name string) error {
			deleted = append(deleted, name)
			return nil
		},
	}}
	if err := m.scanOwnedAndroidDirectories(); err != nil {
		t.Fatalf("scanOwnedAndroidDirectories: %v", err)
	}
	if _, err := os.Stat(pendingDir); err != nil {
		t.Fatalf("pending workdir was removed: %v", err)
	}
	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Fatalf("orphan workdir still exists: %v", err)
	}
	if err := m.scanOrphanNetNS(); err != nil {
		t.Fatalf("scanOrphanNetNS: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != orphanNetNS {
		t.Fatalf("deleted netns = %v", deleted)
	}
}

func TestAndroidUnexpectedExitMarksStateAndEnqueuesCleanup(t *testing.T) {
	store := newCleanupTestStore(t)
	e := NewAndroidEngine(AndroidConfig{Enabled: true, StateDir: t.TempDir(), ArtifactsDir: t.TempDir(), StateStore: store})
	pod := &AndroidSandboxRecord{CRISandboxID: "exit-pod", PodUID: "exit-pod", State: androidSandboxRunning}
	container := &AndroidContainerRecord{ContainerID: "exit-pod-c", CRISandboxID: "exit-pod", State: androidContainerRunning}
	e.pods[pod.CRISandboxID] = pod
	e.containers[container.ContainerID] = container
	called := make(chan string, 1)
	m := NewCleanupManager(store, nil, e, CleanupConfig{Enabled: true})
	m.cleanupAndroidFunc = func(_ context.Context, sandboxID string) error {
		called <- sandboxID
		return nil
	}

	e.handleUnexpectedCVDExit(pod.CRISandboxID, nil)
	select {
	case sandboxID := <-called:
		if sandboxID != pod.CRISandboxID {
			t.Fatalf("cleanup sandbox = %s", sandboxID)
		}
	case <-time.After(time.Second):
		t.Fatal("unexpected exit did not enqueue cleanup")
	}
	if pod.State != androidSandboxUnknown || container.State != androidContainerExited {
		t.Fatalf("unexpected exit state: pod=%s container=%s", pod.State, container.State)
	}
}

func TestRouteStateReconcileRepairsBothDirections(t *testing.T) {
	store := newCleanupTestStore(t)
	if err := store.SaveE2BPod(E2BPodState{SandboxID: "live", State: stateRunning, ContainerName: "app"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRoute(RouteRecord{Kind: "pod", ID: "orphan", Engine: EngineTypeAndroid}); err != nil {
		t.Fatal(err)
	}
	m := &CleanupManager{stateStore: store}
	if err := m.reconcileRoutes(); err != nil {
		t.Fatalf("reconcileRoutes: %v", err)
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
}

func TestCleanupRetryTaskDeletedAfterSuccess(t *testing.T) {
	store := newCleanupTestStore(t)
	now := time.Now()
	calls := 0
	m := &CleanupManager{
		stateStore: store,
		cfg:        CleanupConfig{MaxRetries: 3},
		now:        func() time.Time { return now },
	}
	m.cleanupE2BFunc = func(context.Context, string) error {
		calls++
		if calls == 1 {
			return errors.New("temporary cleanup failure")
		}
		return nil
	}
	first := m.CleanupE2BSandbox(context.Background(), "retry-pod", "test")
	if first.Err == nil {
		t.Fatal("first cleanup should fail")
	}
	tasks, _ := store.LoadCleanupTasks()
	if len(tasks) != 1 || tasks[0].Attempts != 1 {
		t.Fatalf("cleanup tasks after failure = %+v", tasks)
	}
	now = tasks[0].NextRetryAt.Add(time.Millisecond)
	if err := m.RetryPending(context.Background()); err != nil {
		t.Fatalf("RetryPending: %v", err)
	}
	tasks, _ = store.LoadCleanupTasks()
	if len(tasks) != 0 || calls != 2 {
		t.Fatalf("retry did not converge: calls=%d tasks=%+v", calls, tasks)
	}
}

func TestUnknownOwnerDirectoryIsNotDeleted(t *testing.T) {
	stateDir := t.TempDir()
	unknown := filepath.Join(stateDir, "android-similar")
	if err := os.MkdirAll(unknown, 0o755); err != nil {
		t.Fatal(err)
	}
	e := NewAndroidEngine(AndroidConfig{StateDir: stateDir, ArtifactsDir: filepath.Join(t.TempDir(), "cf17")})
	m := &CleanupManager{android: e}
	if err := m.scanOwnedAndroidDirectories(); err != nil {
		t.Fatalf("scanOwnedAndroidDirectories: %v", err)
	}
	if _, err := os.Stat(unknown); err != nil {
		t.Fatalf("unknown owner directory was removed: %v", err)
	}
}

func TestCleanupSandboxResourcesWaitsForSandboxOpLock(t *testing.T) {
	e := &grpcE2BEngine{}
	mu, locked := e.tryLockSandbox("sb-oplock")
	if !locked {
		t.Fatal("failed to pre-acquire sandbox op lock")
	}
	defer mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := e.cleanupSandboxResources(ctx, "sb-oplock"); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("cleanupSandboxResources err = %v, want DeadlineExceeded while op lock is held", err)
	}
}

func TestCreateE2BSandboxTakesSandboxOpLock(t *testing.T) {
	e := &grpcE2BEngine{}
	mu, locked := e.tryLockSandbox("sb-oplock")
	if !locked {
		t.Fatal("failed to pre-acquire sandbox op lock")
	}
	defer mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := e.createE2BSandbox(ctx, e2bCreateParams{sandboxID: "sb-oplock"}); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("createE2BSandbox err = %v, want DeadlineExceeded while op lock is held", err)
	}
}

func TestPendingNetNSMarkRefcount(t *testing.T) {
	e := &grpcE2BEngine{}
	e.markPendingNetNS("e2b-refcount")
	e.markPendingNetNS("e2b-refcount")
	e.unmarkPendingNetNS("e2b-refcount")
	if !e.isPendingNetNS("e2b-refcount") {
		t.Fatal("pending mark lost while another same-name mark is still held")
	}
	e.unmarkPendingNetNS("e2b-refcount")
	if e.isPendingNetNS("e2b-refcount") {
		t.Fatal("pending mark should be removed after refcount reaches zero")
	}
}

func TestCreateE2BSandboxReusesInflightResult(t *testing.T) {
	e := &grpcE2BEngine{}
	want := &orchestrator.SandboxCreateResponse{ClientId: "client-1"}
	ic := &inflightCreate{done: make(chan struct{}), resp: want}
	close(ic.done)
	e.inflightCreates.Store("sb-dedup", ic)

	got, err := e.createE2BSandbox(context.Background(), e2bCreateParams{sandboxID: "sb-dedup"})
	if err != nil || got != want {
		t.Fatalf("createE2BSandbox = (%v, %v), want reused in-flight result (%v, nil)", got, err, want)
	}
}

func TestCreateE2BSandboxDuplicateWaitsForLeader(t *testing.T) {
	e := &grpcE2BEngine{}
	// 占住 op lock：leader 会在 lockSandbox 等到自己的 ctx 超时；follower 应在
	// singleflight 上等 leader 并复用其结果，而不是自己也去抢锁。
	mu, locked := e.tryLockSandbox("sb-dedup")
	if !locked {
		t.Fatal("failed to pre-acquire sandbox op lock")
	}
	defer mu.Unlock()

	leaderCtx, cancelLeader := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelLeader()
	leaderErr := make(chan error, 1)
	go func() {
		_, err := e.createE2BSandbox(leaderCtx, e2bCreateParams{sandboxID: "sb-dedup"})
		leaderErr <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := e.inflightCreates.Load("sb-dedup"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("leader did not register in-flight create")
		}
		time.Sleep(time.Millisecond)
	}

	followerCtx, cancelFollower := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelFollower()
	start := time.Now()
	_, err := e.createE2BSandbox(followerCtx, e2bCreateParams{sandboxID: "sb-dedup"})
	elapsed := time.Since(start)

	if lErr := <-leaderErr; status.Code(lErr) != codes.DeadlineExceeded {
		t.Fatalf("leader err = %v, want DeadlineExceeded", lErr)
	}
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("follower err = %v, want leader's DeadlineExceeded reused", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("follower did not reuse leader result, blocked for %v", elapsed)
	}
}
