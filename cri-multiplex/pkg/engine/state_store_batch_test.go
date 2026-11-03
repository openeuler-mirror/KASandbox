package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type blockedStateWrite struct {
	data    []byte
	release chan error
}

// Blocks each real atomic write until the test allows it to complete. Pending
// saves must neither block other state mutations nor return success early.
func blockStateWrites(t *testing.T, store *JSONStateStore) <-chan blockedStateWrite {
	t.Helper()
	writes := make(chan blockedStateWrite, 100)
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	write := store.writeSnapshot
	store.writeSnapshot = func(data []byte) error {
		gate := blockedStateWrite{data: append([]byte(nil), data...), release: make(chan error, 1)}
		writes <- gate
		select {
		case err := <-gate.release:
			if err != nil {
				return err
			}
			return write(data)
		case <-stop:
			return errors.New("test finished")
		}
	}
	return writes
}

func awaitStateWrite(t *testing.T, writes <-chan blockedStateWrite) blockedStateWrite {
	t.Helper()
	select {
	case w := <-writes:
		return w
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot writer did not start")
		return blockedStateWrite{}
	}
}

func awaitStateResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("state operation did not complete")
		return nil
	}
}

// e2bSection 从 persistedState 的 "e2b" section 里取出一个子键，避免测试直接依赖 typed 字段。
func e2bSection[T any](t *testing.T, s persistedState, key string) T {
	t.Helper()
	var out T
	raw, ok := s.Sections["e2b"]
	if !ok {
		return out
	}
	var children map[string]json.RawMessage
	if err := json.Unmarshal(raw, &children); err != nil {
		t.Fatalf("decode e2b section: %v", err)
	}
	value, ok := children[key]
	if !ok {
		return out
	}
	if err := json.Unmarshal(value, &out); err != nil {
		t.Fatalf("decode e2b.%s: %v", key, err)
	}
	return out
}

func awaitStateMutation(t *testing.T, store *JSONStateStore, ready func(persistedState) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if store.mu.TryLock() {
			ok := ready(store.state)
			store.mu.Unlock()
			if ok {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("state mutations remained blocked behind snapshot I/O")
}

func TestStateStoreConcurrentSavesShareFlush(t *testing.T) {
	store := newStateStoreTestStore(t)
	writes := blockStateWrites(t, store)
	firstDone := make(chan error, 1)
	go func() { firstDone <- store.SaveRoute(RouteRecord{Kind: "pod", ID: "first"}) }()
	first := awaitStateWrite(t, writes)

	done := make(chan error, 49)
	for i := 0; i < 49; i++ {
		go func(i int) {
			done <- store.SaveRoute(RouteRecord{Kind: "pod", ID: fmt.Sprint(i)})
		}(i)
	}
	awaitStateMutation(t, store, func(s persistedState) bool { return len(s.Routes) == 50 })
	select {
	case err := <-done:
		t.Fatalf("save returned before its snapshot was written: %v", err)
	default:
	}
	first.release <- nil
	second := awaitStateWrite(t, writes)
	// The first caller must not wait for later batches to drain.
	if err := awaitStateResult(t, firstDone); err != nil {
		t.Fatal(err)
	}
	var snapshot persistedState
	if err := json.Unmarshal(second.data, &snapshot); err != nil || len(snapshot.Routes) != 50 {
		t.Fatalf("pending callers did not share a complete snapshot: routes=%d err=%v", len(snapshot.Routes), err)
	}
	select {
	case err := <-done:
		t.Fatalf("save returned before its own batch completed: %v", err)
	default:
	}
	second.release <- nil
	for i := 0; i < 49; i++ {
		if err := awaitStateResult(t, done); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-writes:
		t.Fatal("50 concurrent saves required more than two snapshot writes")
	default:
	}
	reopened, err := NewJSONStateStore(filepath.Dir(store.path))
	if err != nil {
		t.Fatal(err)
	}
	routes, err := reopened.LoadRoutes()
	if err != nil || len(routes) != 50 {
		t.Fatalf("recovery lost completed writes: routes=%d err=%v", len(routes), err)
	}
}

func TestStateStoreBatchFailureAndRetry(t *testing.T) {
	store := newStateStoreTestStore(t)
	writes := blockStateWrites(t, store)
	firstDone := make(chan error, 1)
	go func() { firstDone <- store.SaveRoute(RouteRecord{Kind: "pod", ID: "first"}) }()
	first := awaitStateWrite(t, writes)
	done := make(chan error, 5)
	for i := 0; i < 5; i++ {
		go func(i int) { done <- store.SaveBackendState(fmt.Sprintf("probe_image_%d", i), i) }(i)
	}
	awaitStateMutation(t, store, func(s persistedState) bool { return len(s.Sections) == 5 })
	first.release <- nil
	failed := awaitStateWrite(t, writes)
	if err := awaitStateResult(t, firstDone); err != nil {
		t.Fatal(err)
	}
	diskErr := errors.New("simulated sync failure")
	failed.release <- diskErr
	for i := 0; i < 5; i++ {
		if err := awaitStateResult(t, done); !errors.Is(err, diskErr) {
			t.Fatalf("batch caller received %v, want sync failure", err)
		}
	}
	go func() { firstDone <- store.SaveBackendState("e2b.images", []string{"retry"}) }()
	retry := awaitStateWrite(t, writes)
	retry.release <- nil
	if err := awaitStateResult(t, firstDone); err != nil {
		t.Fatal(err)
	}
}

func TestStateStoreDeleteDuringFlushDoesNotResurrect(t *testing.T) {
	store := newStateStoreTestStore(t)
	writes := blockStateWrites(t, store)
	firstDone := make(chan error, 1)
	go func() { firstDone <- store.SaveBackendState("e2b.pods", []map[string]any{{"sandbox_id": "deleted"}}) }()
	first := awaitStateWrite(t, writes)
	deleteDone := make(chan error, 1)
	go func() { deleteDone <- store.DeleteBackendState("e2b.pods") }()
	awaitStateMutation(t, store, func(s persistedState) bool { return len(e2bSection[[]map[string]any](t, s, "pods")) == 0 })
	first.release <- nil
	second := awaitStateWrite(t, writes)
	second.release <- nil
	if err := awaitStateResult(t, firstDone); err != nil {
		t.Fatal(err)
	}
	if err := awaitStateResult(t, deleteDone); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	var saved persistedState
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if pods := e2bSection[[]map[string]any](t, saved, "pods"); len(pods) != 0 {
		t.Fatalf("deleted pod resurrected: pods=%v", pods)
	}
}

func TestStateStoreConcurrentPodRouteRecovery(t *testing.T) {
	store := newStateStoreTestStore(t)
	var writes atomic.Int64
	write := store.writeSnapshot
	store.writeSnapshot = func(data []byte) error {
		writes.Add(1)
		return write(data)
	}
	runConcurrentStateSaves(t, store, 50)
	t.Logf("100 mutations persisted in %d atomic snapshot writes", writes.Load())
}

type stateProbeTiming struct {
	pod, route time.Duration
}

func runConcurrentStateSaves(t *testing.T, store *JSONStateStore, count int) ([]stateProbeTiming, time.Duration) {
	t.Helper()
	start := make(chan struct{})
	errs := make(chan error, count)
	timings := make([]stateProbeTiming, count)
	var wg sync.WaitGroup
	var upsertMu sync.Mutex
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			id := fmt.Sprintf("state-probe-%03d", i)
			started := time.Now()
			if err := upsertE2BPodForTest(store, &upsertMu, map[string]any{
				"sandbox_id": id, "e2b_sandbox_id": id, "state": 0,
				"envd_access_token": "secret-test-token",
			}); err != nil {
				errs <- err
				return
			}
			timings[i].pod = time.Since(started)
			started = time.Now()
			err := store.SaveRoute(RouteRecord{Kind: "pod", ID: id, Engine: EngineTypeE2B})
			timings[i].route = time.Since(started)
			errs <- err
		}(i)
	}
	started := time.Now()
	close(start)
	wg.Wait()
	wall := time.Since(started)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	var saved persistedState
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	savedPods := e2bSection[[]map[string]any](t, saved, "pods")
	if len(saved.Routes) != count || len(savedPods) != count {
		t.Fatalf("persisted routes=%d pods=%d, want %d each", len(saved.Routes), len(savedPods), count)
	}
	ids := make(map[string]bool, count)
	for _, p := range savedPods {
		if id, ok := p["sandbox_id"].(string); ok {
			ids[id] = true
		}
	}
	for _, r := range saved.Routes {
		if !ids[r.ID] {
			t.Fatalf("route %s has no persisted pod", r.ID)
		}
	}
	return timings, wall
}

// upsertE2BPodForTest 在测试里保留旧 typed 方法的 upsert 语义：
// KV 只有单次调用原子性，读改写需要调用方自己串行化。
func upsertE2BPodForTest(store *JSONStateStore, mu *sync.Mutex, pod map[string]any) error {
	mu.Lock()
	defer mu.Unlock()
	var pods []map[string]any
	if _, err := store.LoadBackendState("e2b.pods", &pods); err != nil {
		return err
	}
	return store.SaveBackendState("e2b.pods", append(pods, pod))
}
