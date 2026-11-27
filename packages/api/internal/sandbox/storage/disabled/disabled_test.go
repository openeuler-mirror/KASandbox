package disabled

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/api/internal/sandbox/sandboxtypes"
	catalog "github.com/e2b-dev/infra/packages/shared/pkg/sandbox-catalog"
)

func TestStorageTracksNothing(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	teamID := uuid.New()
	s := Storage{}

	require.ErrorIs(t, s.Add(ctx, sandboxtypes.Sandbox{SandboxID: "sbx", TeamID: teamID}), ErrRedisDisabled)

	_, err := s.Get(ctx, teamID, "sbx")
	require.ErrorIs(t, err, sandboxtypes.ErrNotFound)
	require.ErrorIs(t, err, ErrRedisDisabled)

	require.ErrorIs(t, s.Remove(ctx, teamID, "sbx"), sandboxtypes.ErrNotFound)

	_, err = s.Update(ctx, teamID, "sbx", func(sbx sandboxtypes.Sandbox) (sandboxtypes.Sandbox, error) { return sbx, nil })
	require.ErrorIs(t, err, sandboxtypes.ErrNotFound)

	_, _, _, err = s.StartRemoving(ctx, teamID, "sbx", sandboxtypes.RemoveOpts{})
	require.ErrorIs(t, err, sandboxtypes.ErrNotFound)

	_, err = s.RestoreRunning(ctx, teamID, "sbx", sandboxtypes.StateRunning, time.Second)
	require.ErrorIs(t, err, sandboxtypes.ErrNotFound)

	require.ErrorIs(t, s.WaitForStateChange(ctx, teamID, "sbx"), sandboxtypes.ErrNotFound)

	items, err := s.TeamItems(ctx, teamID, []sandboxtypes.State{sandboxtypes.StateRunning})
	require.NoError(t, err)
	assert.Empty(t, items)

	expired, err := s.ExpiredItems(ctx)
	require.NoError(t, err)
	assert.Empty(t, expired)

	counts, err := s.TeamsWithSandboxCount(ctx)
	require.NoError(t, err)
	assert.NotNil(t, counts)
	assert.Empty(t, counts)
}

func TestStorageReconcileReportsNoOrphans(t *testing.T) {
	t.Parallel()

	// Sandboxes scheduled outside the api are never in the store; they must not
	// be reported as orphans, or the api would kill them.
	nodeSandboxes := []sandboxtypes.NodeSandbox{{}, {}}
	assert.Empty(t, Storage{}.Reconcile(t.Context(), nodeSandboxes, "node-1"))
}

func TestReservationsRefuseStarts(t *testing.T) {
	t.Parallel()

	finish, wait, err := Reservations{}.Reserve(t.Context(), uuid.New(), "sbx", 10)
	require.ErrorIs(t, err, ErrRedisDisabled)
	assert.Nil(t, finish)
	assert.Nil(t, wait)

	require.NoError(t, Reservations{}.Release(t.Context(), uuid.New(), "sbx"))
}

func TestCatalogIsEmpty(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	c := Catalog{}

	_, err := c.GetSandbox(ctx, "sbx")
	require.ErrorIs(t, err, catalog.ErrSandboxNotFound)
	require.ErrorIs(t, c.StoreSandbox(ctx, "sbx", &catalog.SandboxInfo{}, time.Minute), ErrRedisDisabled)
	require.NoError(t, c.DeleteSandbox(ctx, "sbx", "exec"))
	require.NoError(t, c.Close(ctx))
}
