package engine

import (
	"fmt"
	"sync"
	"testing"
)

func TestPodTrackerAddGetDelete(t *testing.T) {
	tracker := newPodTracker()
	info := &podInfo{sandboxID: "pod-a", state: stateRunning}

	tracker.Add("pod-a", info)
	got, ok := tracker.Get("pod-a")
	if !ok || got != info {
		t.Fatalf("Get returned (%v, %v), want original info", got, ok)
	}

	tracker.Delete("pod-a")
	if _, ok := tracker.Get("pod-a"); ok {
		t.Fatal("pod should be deleted")
	}
}

func TestPodTrackerListSkipsRemoved(t *testing.T) {
	tracker := newPodTracker()
	tracker.Add("running", &podInfo{sandboxID: "running", state: stateRunning})
	tracker.Add("stopped", &podInfo{sandboxID: "stopped", state: stateStopped})
	tracker.Add("removed", &podInfo{sandboxID: "removed", state: stateRemoved})

	items := tracker.List()
	if len(items) != 2 {
		t.Fatalf("List returned %d items, want 2", len(items))
	}
	for _, item := range items {
		if item.sandboxID == "removed" {
			t.Fatal("List should not return removed pod")
		}
	}
}

func TestPodTrackerBidirectionalIndex(t *testing.T) {
	tracker := newPodTracker()
	tracker.Add("pod-a", &podInfo{sandboxID: "pod-a", e2bSandboxID: "e2b-a", state: stateRunning})
	tracker.Add("pod-b", &podInfo{sandboxID: "pod-b", state: stateRunning}) // 无 e2b ID，不索引

	got, ok := tracker.GetByE2B("e2b-a")
	if !ok || got.sandboxID != "pod-a" {
		t.Fatalf("GetByE2B returned (%+v, %v), want pod-a", got, ok)
	}
	if _, ok := tracker.GetByE2B("pod-b"); ok {
		t.Fatal("pod without e2bSandboxID should not be indexed")
	}

	// 覆盖 Add 时旧反查被清掉
	tracker.Add("pod-a", &podInfo{sandboxID: "pod-a", e2bSandboxID: "e2b-a2", state: stateRunning})
	if _, ok := tracker.GetByE2B("e2b-a"); ok {
		t.Fatal("stale e2b index should be removed on re-add")
	}
	got, ok = tracker.GetByE2B("e2b-a2")
	if !ok || got.sandboxID != "pod-a" {
		t.Fatalf("GetByE2B after re-add returned (%+v, %v), want pod-a", got, ok)
	}

	tracker.Delete("pod-a")
	if _, ok := tracker.GetByE2B("e2b-a2"); ok {
		t.Fatal("e2b index should be removed on delete")
	}
}

func TestPodTrackerConcurrentAccess(t *testing.T) {
	tracker := newPodTracker()
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("pod-%d", i)
			tracker.Add(id, &podInfo{sandboxID: id, state: stateRunning})
			_, _ = tracker.Get(id)
			_ = tracker.List()
			if i%2 == 0 {
				tracker.Delete(id)
			}
		}(i)
	}
	wg.Wait()
}
