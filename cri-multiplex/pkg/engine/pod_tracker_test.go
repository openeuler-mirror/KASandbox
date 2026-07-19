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
