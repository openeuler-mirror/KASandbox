package engine

import (
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"
	"time"
)

// Opt-in standalone probe. It only creates synthetic state in a fresh
// temporary directory on the chosen filesystem; it never opens live state.
func TestStateStoreFilesystemProbe(t *testing.T) {
	root := os.Getenv("CRI_STATE_PROBE_ROOT")
	if root == "" {
		t.Skip("set CRI_STATE_PROBE_ROOT to opt into the filesystem probe")
	}
	if !filepath.IsAbs(root) {
		t.Fatal("CRI_STATE_PROBE_ROOT must be an existing absolute directory")
	}
	dir, err := os.MkdirTemp(root, "cri-state-probe-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	store, err := NewJSONStateStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	var writes, writeNanos atomic.Int64
	write := store.writeSnapshot
	store.writeSnapshot = func(data []byte) error {
		start := time.Now()
		err := write(data)
		writeNanos.Add(int64(time.Since(start)))
		writes.Add(1)
		return err
	}
	timings, wall := runConcurrentStateSaves(t, store, 50)
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	t.Logf("mode=batched concurrency=50 mutations=100 snapshot_writes=%d wall_ms=%.3f snapshot_io_total_ms=%.3f",
		writes.Load(), ms(wall), ms(time.Duration(writeNanos.Load())))
	for _, op := range []string{"SaveE2BPod", "SaveRoute", "PodAndRoute"} {
		values := make([]float64, len(timings))
		var total float64
		for i, timing := range timings {
			d := timing.pod + timing.route
			if op == "SaveE2BPod" {
				d = timing.pod
			} else if op == "SaveRoute" {
				d = timing.route
			}
			values[i] = ms(d)
			total += values[i]
		}
		sort.Float64s(values)
		t.Logf("op=%s n=%d avg_ms=%.3f p95_ms=%.3f max_ms=%.3f",
			op, len(values), total/float64(len(values)), values[(95*len(values)+99)/100-1], values[len(values)-1])
	}
	t.Log("disk verification: all 50 pods and 50 routes present; synthetic persistence workload, not a VM benchmark")
}
