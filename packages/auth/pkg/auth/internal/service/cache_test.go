package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/auth/pkg/types"
	authqueries "github.com/e2b-dev/infra/packages/db/pkg/auth/queries"
)

func TestAuthCacheWithoutRedis(t *testing.T) {
	t.Parallel()

	// Without Redis every lookup goes to the store.
	c := newAuthCache(nil)
	teamID := uuid.New()
	calls := 0

	for range 2 {
		team, err := c.GetOrSet(t.Context(), "hashed-key", func(context.Context, string) (*types.Team, error) {
			calls++

			return &types.Team{Team: &authqueries.Team{ID: teamID}}, nil
		})
		require.NoError(t, err)
		assert.Equal(t, teamID, team.ID)
	}
	assert.Equal(t, 2, calls)

	assert.NotPanics(t, func() { c.Invalidate(t.Context(), "hashed-key") })
	require.NoError(t, c.Close(t.Context()))
}
