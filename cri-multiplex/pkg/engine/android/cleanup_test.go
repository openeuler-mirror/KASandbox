package android

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/cri-multiplex/pkg/engine"
	"github.com/cri-multiplex/pkg/engine/util"
)

func newCleanupTestStore(t *testing.T) engine.StateStore {
	t.Helper()
	store, err := engine.NewJSONStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewJSONStateStore: %v", err)
	}
	return store
}

func newCleanupTestEngine(t *testing.T, cfg Config) *Engine {
	t.Helper()
	e, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func TestCleanupAndroidNotFoundIsIdempotent(t *testing.T) {
	store := newCleanupTestStore(t)
	stateDir := t.TempDir()
	artifactsDir := t.TempDir()
	e := newCleanupTestEngine(t, Config{Enabled: true, StateDir: stateDir, ArtifactsDir: artifactsDir, Deps: engine.Deps{Store: store}})
	e.cniManager = &fakeCNIManager{delErr: os.ErrNotExist}
	e.ops.stopCVD = func(*Engine, context.Context, *AndroidSandboxRecord) error { return syscall.ESRCH }
	rec := &AndroidSandboxRecord{
		CRISandboxID: "android-pod", PodUID: "android-pod", State: androidSandboxUnknown,
		WorkDir: filepath.Join(stateDir, "android-pod"), ArtifactsDir: artifactsDir,
		LaunchPID: 999999, LaunchPGID: 999999,
		CNIRecord: &util.CNIRecord{SandboxID: "android-pod", NetNSPath: "/missing/android-pod"},
	}
	e.pods[rec.CRISandboxID] = rec
	if err := saveAndroidPodTo(store, androidPodStateFromRecords(rec, nil)); err != nil {
		t.Fatal(err)
	}

	for _, reason := range []string{"test", "repeat"} {
		if err := e.CleanupSandbox(context.Background(), rec.CRISandboxID, reason); err != nil {
			t.Fatalf("cleanup %s: %v", reason, err)
		}
	}
	pods, _ := loadAndroidPodsFrom(t, store)
	if len(pods) != 0 {
		t.Fatalf("android state remains: %+v", pods)
	}
}

func TestAndroidReconcileProtectsPendingResources(t *testing.T) {
	stateDir := t.TempDir()
	netNSDir := t.TempDir()
	sandboxID := "android-pending-sandbox"
	pendingDir := filepath.Join(stateDir, sandboxID)
	orphanDir := filepath.Join(stateDir, "android-orphan-sandbox")
	for path, id := range map[string]string{pendingDir: sandboxID, orphanDir: "android-orphan-sandbox"} {
		if err := util.WriteResourceOwner(path, util.ResourceOwner{Owner: "cri-multiplex", Runtime: string(engine.EngineTypeAndroid), SandboxID: id}); err != nil {
			t.Fatal(err)
		}
	}

	e := newCleanupTestEngine(t, Config{
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
	if err := e.Reconcile(context.Background(), engine.ReconcileOptions{}); err != nil {
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
	e := newCleanupTestEngine(t, Config{Enabled: true, StateDir: t.TempDir(), ArtifactsDir: t.TempDir(), Deps: engine.Deps{Store: store}})
	pod := &AndroidSandboxRecord{CRISandboxID: "exit-pod", PodUID: "exit-pod", State: androidSandboxRunning}
	container := &AndroidContainerRecord{ContainerID: "exit-pod-c", CRISandboxID: "exit-pod", State: androidContainerRunning}
	e.pods[pod.CRISandboxID] = pod
	e.containers[container.ContainerID] = container

	e.handleUnexpectedCVDExit(pod.CRISandboxID, nil)
	if pod.State != androidSandboxUnknown || container.State != androidContainerExited {
		t.Fatalf("unexpected exit state: pod=%s container=%s", pod.State, container.State)
	}
}

func TestAndroidReconcileKeepsUnknownOwnerDirectory(t *testing.T) {
	stateDir := t.TempDir()
	unknown := filepath.Join(stateDir, "android-similar")
	if err := os.MkdirAll(unknown, 0o755); err != nil {
		t.Fatal(err)
	}
	e := newCleanupTestEngine(t, Config{StateDir: stateDir, ArtifactsDir: filepath.Join(t.TempDir(), "cf17")})
	if err := e.Reconcile(context.Background(), engine.ReconcileOptions{}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, err := os.Stat(unknown); err != nil {
		t.Fatalf("unknown owner directory was removed: %v", err)
	}
}

func loadAndroidPodsFrom(t *testing.T, store engine.StateStore) ([]AndroidPodState, error) {
	t.Helper()
	var pods []AndroidPodState
	_, err := store.LoadBackendState("android.pods", &pods)
	return pods, err
}

func saveAndroidPodTo(store engine.StateStore, pods ...AndroidPodState) error {
	var existing []AndroidPodState
	if _, err := store.LoadBackendState("android.pods", &existing); err != nil {
		return err
	}
	for _, p := range pods {
		existing = upsertAndroidPodState(existing, p)
	}
	return store.SaveBackendState("android.pods", existing)
}
