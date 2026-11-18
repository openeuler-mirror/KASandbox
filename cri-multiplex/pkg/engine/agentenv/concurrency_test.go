package agentenv

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	runtimeapi "github.com/cri-multiplex/pkg/agentenv/runtime/v1"
)

func TestRunPodSandboxDifferentSandboxesRunConcurrently(t *testing.T) {
	entered := make(chan string, 2)
	release := make(chan struct{})
	lifecycle := &fakeLifecycle{create: func(_ context.Context, req *runtimeapi.SandboxCreateRequest) (*runtimeapi.SandboxCreateResponse, error) {
		productID := req.GetSpec().GetProductSandboxId()
		entered <- productID
		<-release
		return &runtimeapi.SandboxCreateResponse{Sandbox: &runtimeapi.SandboxInfo{
			NativeSandboxId:  "native-" + productID,
			ProductSandboxId: productID,
			Phase:            runtimeapi.SandboxPhase_RUNNING,
		}}, nil
	}}
	instance, _, _ := newCRITestEngine(t, lifecycle)

	newRequest := func(uid, productID string) *runtime.RunPodSandboxRequest {
		req := criRunRequest(map[string]string{
			annTemplate:  "template-1",
			annSandboxID: productID,
		})
		req.GetConfig().GetMetadata().Uid = uid
		req.GetConfig().GetMetadata().Name = uid
		return req
	}

	errs := make(chan error, 2)
	go func() {
		_, err := instance.RunPodSandbox(context.Background(), newRequest("cri-a", "product-a"))
		errs <- err
	}()
	go func() {
		_, err := instance.RunPodSandbox(context.Background(), newRequest("cri-b", "product-b"))
		errs <- err
	}()

	seen := make(map[string]bool)
	for len(seen) < 2 {
		select {
		case productID := <-entered:
			seen[productID] = true
		case <-time.After(2 * time.Second):
			close(release)
			t.Fatalf("only %d sandbox(es) reached Runtime Create; different sandboxes are still serialized", len(seen))
		}
	}
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("RunPodSandbox: %v", err)
		}
	}
}

type blockingStateStore struct {
	*memoryStateStore
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	calls   atomic.Int32
}

func (s *blockingStateStore) SaveBackendState(path string, value any) error {
	s.calls.Add(1)
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return s.memoryStateStore.SaveBackendState(path, value)
}

func TestPodPersistenceDoesNotHoldStateReadLock(t *testing.T) {
	store := &blockingStateStore{
		memoryStateStore: newMemoryStateStore(),
		entered:          make(chan struct{}),
		release:          make(chan struct{}),
	}
	routes := newRouteTable(store)
	instance := newWithClients(
		Config{RuntimeAddress: "passthrough:///agentenv-test", NodeName: "node-1", NodeIP: "10.0.0.1"},
		store,
		routes,
		&fakeLifecycle{},
		&fakeAdmin{},
	)

	putErr := make(chan error, 1)
	go func() {
		putErr <- instance.putPod(podRecord{
			CRISandboxID:     "cri-lock",
			ProductSandboxID: "product-lock",
			State:            criStateCreating,
		})
	}()

	select {
	case <-store.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("state persistence did not start")
	}

	readDone := make(chan error, 1)
	go func() {
		record, ok := instance.getPod("cri-lock")
		if !ok {
			readDone <- fmt.Errorf("pod record not found while persistence was blocked")
			return
		}
		if record.ProductSandboxID != "product-lock" {
			readDone <- fmt.Errorf("unexpected pod record: %+v", record)
			return
		}
		readDone <- nil
	}()

	select {
	case err := <-readDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("getPod blocked while SaveBackendState was in progress; disk I/O still holds podMu")
	}

	close(store.release)
	if err := <-putErr; err != nil {
		t.Fatalf("putPod: %v", err)
	}
}

func TestConcurrentPodMutationsCoalesceStateSnapshots(t *testing.T) {
	store := &blockingStateStore{
		memoryStateStore: newMemoryStateStore(),
		entered:          make(chan struct{}),
		release:          make(chan struct{}),
	}
	routes := newRouteTable(store)
	instance := newWithClients(
		Config{RuntimeAddress: "passthrough:///agentenv-test", NodeName: "node-1", NodeIP: "10.0.0.1"},
		store,
		routes,
		&fakeLifecycle{},
		&fakeAdmin{},
	)

	firstErr := make(chan error, 1)
	go func() {
		firstErr <- instance.putPod(podRecord{
			CRISandboxID:     "cri-0",
			ProductSandboxID: "product-0",
			State:            criStateCreating,
		})
	}()
	select {
	case <-store.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first state persistence did not start")
	}

	const extra = 20
	errs := make(chan error, extra)
	for i := 0; i < extra; i++ {
		go func(i int) {
			errs <- instance.putPod(podRecord{
				CRISandboxID:     fmt.Sprintf("cri-%d", i+1),
				ProductSandboxID: fmt.Sprintf("product-%d", i+1),
				State:            criStateCreating,
			})
		}(i)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		count := 0
		for i := 0; i <= extra; i++ {
			if _, ok := instance.getPod(fmt.Sprintf("cri-%d", i)); ok {
				count++
			}
		}
		if count == extra+1 {
			break
		}
		if time.Now().After(deadline) {
			close(store.release)
			t.Fatalf("only %d/%d mutations became visible before release", count, extra+1)
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(store.release)

	if err := <-firstErr; err != nil {
		t.Fatalf("first putPod: %v", err)
	}
	for i := 0; i < extra; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("putPod %d: %v", i, err)
		}
	}
	if calls := store.calls.Load(); calls > 3 {
		t.Fatalf("state snapshots = %d, want <= 3 for %d mutations", calls, extra+1)
	}
}
