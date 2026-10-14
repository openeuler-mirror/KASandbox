package perftrace

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// capture 把包级输出换成内存缓冲，测试结束后恢复；用到它的测试不能并行。
func capture(t *testing.T, on, detailed bool) *[]string {
	t.Helper()

	var mu sync.Mutex
	lines := []string{}
	oldEnabled, oldVerbose, oldEmit := enabled, verbose, emit
	enabled, verbose = on, detailed
	emit = func(_ context.Context, line string) {
		mu.Lock()
		defer mu.Unlock()

		lines = append(lines, line)
	}
	t.Cleanup(func() { enabled, verbose, emit = oldEnabled, oldVerbose, oldEmit })

	return &lines
}

func summary(t *testing.T, line string) record {
	t.Helper()

	payload, ok := strings.CutPrefix(line, LogPrefix)
	require.True(t, ok)

	var rec record
	require.NoError(t, json.Unmarshal([]byte(payload), &rec))

	return rec
}

func TestSummaryNestsPhasesByContext(t *testing.T) {
	lines := capture(t, true, false)
	received := time.Now().Add(-5 * time.Millisecond)
	ctx, rec := Begin(context.Background(), "api", "CreateSandbox", "", received)
	rec.AddPhase("middleware", received, received.Add(3*time.Millisecond))
	rec.SetSandboxID("sbx-1")

	parentCtx, endParent := Phase(ctx, "resume VM")
	var wg sync.WaitGroup
	for _, name := range []string{"configured fc", "wait uffd-socket-ready"} {
		wg.Go(func() {
			_, end := Phase(parentCtx, name)
			end()
		})
	}
	wg.Wait()
	endParent()
	endParent()

	_, endOpen := Phase(ctx, "wait envd ready")
	rec.End(false)
	// End 之后结束或开始的阶段、重复的 End 都不再产生输出
	endOpen()
	lateCtx, endLate := Phase(ctx, "late")
	endLate()
	rec.End(true)

	assert.Equal(t, ctx, lateCtx)
	require.Len(t, *lines, 1)
	got := summary(t, (*lines)[0])
	assert.Equal(t, "api", got.Component)
	assert.Equal(t, "CreateSandbox", got.Op)
	assert.Equal(t, "sbx-1", got.SandboxID)
	assert.False(t, got.OK)
	assert.GreaterOrEqual(t, got.TotalUs, int64(5000))
	require.Len(t, got.Phases, 5)

	byName := map[string]phase{}
	for _, p := range got.Phases {
		byName[p.Name] = p
	}
	assert.Equal(t, int64(3000), byName["middleware"].DurUs)
	parent := byName["resume VM"]
	assert.Equal(t, 0, parent.Parent)
	assert.GreaterOrEqual(t, parent.DurUs, int64(0))
	assert.Equal(t, parent.ID, byName["configured fc"].Parent)
	assert.Equal(t, parent.ID, byName["wait uffd-socket-ready"].Parent)
	assert.Equal(t, int64(-1), byName["wait envd ready"].DurUs)
}

func TestVerboseEmitsLinePerPhase(t *testing.T) {
	lines := capture(t, true, true)
	ctx, rec := Begin(context.Background(), "orchestrator", "ResumeSandbox", "sbx-1", time.Now())
	_, end := Phase(ctx, "wait envd ready")
	end()
	rec.End(true)

	require.Len(t, *lines, 2)
	assert.Regexp(t, `^\[ResumeSandbox\] wait envd ready cost: \d+\.\d{3} ms, traceID=$`, (*lines)[0])
	assert.True(t, summary(t, (*lines)[1]).OK)
}

func TestDisabledAndMissingRecorderAreNoops(t *testing.T) {
	lines := capture(t, false, true)
	ctx, rec := Begin(context.Background(), "api", "CreateSandbox", "sbx", time.Now())
	assert.Nil(t, rec)

	rec.SetSandboxID("x")
	rec.AddPhase("x", time.Now(), time.Now())
	phaseCtx, end := Phase(ctx, "anything")
	end()
	rec.End(true)

	assert.Equal(t, ctx, phaseCtx)
	assert.Empty(t, *lines)
}
