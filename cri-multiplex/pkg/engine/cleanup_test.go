package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cri-multiplex/pkg/engine/util"
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
