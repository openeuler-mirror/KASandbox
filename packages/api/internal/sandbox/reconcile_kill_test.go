package sandbox

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
)

// orphanStorage reports every sandbox it is given as an orphan. Only Reconcile
// is used; the embedded interface leaves the other methods unimplemented.
type orphanStorage struct {
	Storage

	calls atomic.Int64
}

func (s *orphanStorage) Reconcile(_ context.Context, sbxs []NodeSandbox, _ string) []NodeSandbox {
	s.calls.Add(1)

	return sbxs
}

func TestReconcile_KillsOrphansOnlyWhenEnabled(t *testing.T) {
	for _, tc := range []struct {
		env   string
		kills int64
	}{
		{"", 0},
		{"false", 0},
		{"garbage", 0},
		{"true", 2},
		{" 1 ", 2},
	} {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv(KillOrphanSandboxesEnv, tc.env)

			storage := &orphanStorage{}
			var kills atomic.Int64
			store := NewStore(storage, nil, Callbacks{
				KillOrphanSandbox: func(context.Context, NodeSandbox) { kills.Add(1) },
			})

			store.Reconcile(t.Context(), []NodeSandbox{{SandboxID: "a"}, {SandboxID: "b"}}, "node-1")

			assert.Equal(t, tc.kills, kills.Load())
			assert.Equal(t, tc.kills/2, storage.calls.Load(), "the store is only consulted when orphans may be killed")
		})
	}
}
