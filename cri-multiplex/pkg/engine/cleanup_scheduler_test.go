package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type stubCleaner struct {
	mu    sync.Mutex
	calls int
	err   error
	block bool
}

func (s *stubCleaner) CleanupSandbox(ctx context.Context, sandboxID, reason string) error {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if s.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return s.err
}

func (s *stubCleaner) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func testCleanupRegistry(t *testing.T, typ EngineType, cleaner Cleaner) *Registry {
	t.Helper()
	reg := NewRegistry("")
	if err := reg.Register(BackendModule{
		Descriptor: BackendDescriptor{Type: typ, Handler: string(typ)},
		Engine:     &stubEngine{typ: typ},
		Cleaner:    cleaner,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := reg.Freeze(); err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	return reg
}

func TestScheduleCallsModuleCleaner(t *testing.T) {
	store := newCleanupTestStore(t)
	cleaner := &stubCleaner{}
	m := NewCleanupManager(store, testCleanupRegistry(t, EngineTypeE2B, cleaner), CleanupConfig{Enabled: true})

	result := m.Schedule(context.Background(), EngineTypeE2B, "sb1", "sandbox", "test")
	if !result.Succeeded || result.Err != nil {
		t.Fatalf("Schedule = %+v", result)
	}
	if cleaner.callCount() != 1 {
		t.Fatalf("cleaner calls = %d", cleaner.callCount())
	}
	if tasks, _ := store.LoadCleanupTasks(); len(tasks) != 0 {
		t.Fatalf("cleanup tasks = %+v", tasks)
	}
}

func TestScheduleWithoutCleanerIsSuccess(t *testing.T) {
	store := newCleanupTestStore(t)
	m := NewCleanupManager(store, testCleanupRegistry(t, EngineTypeE2B, nil), CleanupConfig{Enabled: true})
	if result := m.Schedule(context.Background(), EngineTypeE2B, "sb1", "sandbox", "test"); !result.Succeeded {
		t.Fatalf("Schedule = %+v, want success", result)
	}
}

func TestScheduleUnknownBackendRetainsTask(t *testing.T) {
	store := newCleanupTestStore(t)
	m := NewCleanupManager(store, testCleanupRegistry(t, EngineTypeE2B, &stubCleaner{}), CleanupConfig{Enabled: true, MaxRetries: 3})

	result := m.Schedule(context.Background(), EngineType("cube"), "sb1", "sandbox", "test")
	if result.Err == nil {
		t.Fatal("unknown backend must report an error")
	}
	tasks, err := store.LoadCleanupTasks()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Runtime != EngineType("cube") || tasks[0].Attempts != 1 {
		t.Fatalf("cleanup tasks = %+v", tasks)
	}
}

func TestScheduleAppliesCleanupTimeout(t *testing.T) {
	store := newCleanupTestStore(t)
	cleaner := &stubCleaner{block: true, err: errors.New("unreachable")}
	m := NewCleanupManager(store, testCleanupRegistry(t, EngineTypeE2B, cleaner), CleanupConfig{Enabled: true, CleanupTimeout: 10 * time.Millisecond})

	start := time.Now()
	if result := m.Schedule(context.Background(), EngineTypeE2B, "sb1", "sandbox", "test"); result.Err == nil {
		t.Fatal("blocked cleaner should time out")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Schedule ignored CleanupTimeout: %s", elapsed)
	}
}

func TestScheduleConcurrentFailuresKeepAttempts(t *testing.T) {
	store := newCleanupTestStore(t)
	cleaner := &stubCleaner{err: errors.New("still broken")}
	m := NewCleanupManager(store, testCleanupRegistry(t, EngineTypeE2B, cleaner), CleanupConfig{Enabled: true, MaxRetries: 100})

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Schedule(context.Background(), EngineTypeE2B, "sb1", "sandbox", "test")
		}()
	}
	wg.Wait()

	tasks, err := store.LoadCleanupTasks()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Attempts != 10 {
		t.Fatalf("concurrent failures lost attempts: %+v", tasks)
	}
}
