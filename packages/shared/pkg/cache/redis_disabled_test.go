package cache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newDisabledCache[V any]() *RedisCache[V] {
	return NewRedisCache[V](RedisConfig[V]{
		TTL:             30 * time.Second,
		RefreshInterval: time.Second,
		RedisPrefix:     "test:disabled",
	})
}

func TestRedisCacheWithoutRedis_GetOrSetCallsCallback(t *testing.T) {
	t.Parallel()

	rc := newDisabledCache[testValue]()
	calls := 0

	for range 3 {
		v, err := rc.GetOrSet(t.Context(), "k", func(_ context.Context, key string) (testValue, error) {
			calls++

			return testValue{ID: key, Name: "from-db"}, nil
		})
		require.NoError(t, err)
		assert.Equal(t, testValue{ID: "k", Name: "from-db"}, v)
	}

	// Nothing is cached without Redis, every call reads the source.
	assert.Equal(t, 3, calls)
}

func TestRedisCacheWithoutRedis_GetOrSetReturnsCallbackError(t *testing.T) {
	t.Parallel()

	rc := newDisabledCache[*testValue]()
	errDB := errors.New("db down")

	v, err := rc.GetOrSet(t.Context(), "k", func(context.Context, string) (*testValue, error) {
		return nil, errDB
	})
	require.ErrorIs(t, err, errDB)
	assert.Nil(t, v)
}

func TestRedisCacheWithoutRedis_GetOrSetNilPointerValue(t *testing.T) {
	t.Parallel()

	rc := newDisabledCache[*testValue]()

	v, err := rc.GetOrSet(t.Context(), "k", func(context.Context, string) (*testValue, error) {
		return nil, nil
	})
	require.NoError(t, err)
	assert.Nil(t, v)
}

func TestRedisCacheWithoutRedis_GetOrSetDeduplicatesConcurrentCalls(t *testing.T) {
	t.Parallel()

	rc := newDisabledCache[testValue]()
	var calls atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{})

	const n = 10
	var wg sync.WaitGroup
	results := make([]testValue, n)
	for i := range n {
		wg.Go(func() {
			v, err := rc.GetOrSet(t.Context(), "k", func(context.Context, string) (testValue, error) {
				if calls.Add(1) == 1 {
					close(started)
				}
				<-release

				return testValue{ID: "k"}, nil
			})
			assert.NoError(t, err)
			results[i] = v
		})
	}

	<-started
	// Give the other callers time to join the in-flight call.
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	assert.Equal(t, int32(1), calls.Load())
	for _, v := range results {
		assert.Equal(t, testValue{ID: "k"}, v)
	}
}

func TestRedisCacheWithoutRedis_CallbackSurvivesCallerCancel(t *testing.T) {
	t.Parallel()

	rc := newDisabledCache[testValue]()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	v, err := rc.GetOrSet(ctx, "k", func(ctx context.Context, _ string) (testValue, error) {
		// Like a cache miss with Redis, the callback runs detached from the
		// caller's cancellation, bounded by RefreshTimeout.
		if err := ctx.Err(); err != nil {
			return testValue{}, err
		}
		_, hasDeadline := ctx.Deadline()
		assert.True(t, hasDeadline)

		return testValue{ID: "k"}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, testValue{ID: "k"}, v)
}

func TestRedisCacheWithoutRedis_WritesAreNoops(t *testing.T) {
	t.Parallel()

	rc := newDisabledCache[testValue]()

	assert.NotPanics(t, func() {
		rc.Set(t.Context(), "k", testValue{ID: "k"})
		rc.Delete(t.Context(), "k")
		assert.Nil(t, rc.DeleteByPrefix(t.Context(), "k"))
		assert.NoError(t, rc.Close(t.Context()))
	})
}
