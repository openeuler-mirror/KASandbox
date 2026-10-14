// Package perftrace 记录一次请求内各阶段的耗时，请求结束时输出一行汇总日志：
//
//	[PerfTrace] {"component":"orchestrator","op":"ResumeSandbox","sandbox_id":"...",
//	  "start_unix_us":...,"total_us":...,"ok":true,"phases":[{"i":1,"p":0,"n":"resume VM","s":806,"d":18495},...]}
//
// 阶段的父子关系随 ctx 传递：i 为阶段编号，p 为父阶段编号（0 表示请求本身），
// s、d 为相对请求开始的起始偏移和时长（微秒），d 为 -1 表示请求结束时该阶段仍未完成。
//
// 环境变量 PERF_TRACE=0 关闭记录；PERF_TRACE_VERBOSE=1 时每个阶段结束额外输出一行
// "[<op>] <阶段> cost: <毫秒> ms, traceID=<id>"。
package perftrace

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

// LogPrefix 是汇总日志行的固定前缀。
const LogPrefix = "[PerfTrace] "

var (
	enabled = os.Getenv("PERF_TRACE") != "0"
	verbose = os.Getenv("PERF_TRACE_VERBOSE") == "1"
	emit    = func(ctx context.Context, line string) { logger.L().Info(ctx, line) }
)

type phase struct {
	ID      int    `json:"i"`
	Parent  int    `json:"p"`
	Name    string `json:"n"`
	StartUs int64  `json:"s"`
	DurUs   int64  `json:"d"`
}

type record struct {
	Component   string  `json:"component"`
	Op          string  `json:"op"`
	SandboxID   string  `json:"sandbox_id"`
	TraceID     string  `json:"trace_id,omitempty"`
	StartUnixUs int64   `json:"start_unix_us"`
	TotalUs     int64   `json:"total_us"`
	OK          bool    `json:"ok"`
	Phases      []phase `json:"phases"`
}

// Recorder 收集一次请求的阶段耗时，由 Begin 创建；nil Recorder 的方法都是空操作。
type Recorder struct {
	start time.Time

	mu    sync.Mutex
	rec   record
	ended bool
}

// scope 是 ctx 里携带的"当前所在阶段"。
type scope struct {
	rec   *Recorder
	phase int
}

type scopeKey struct{}

// Begin 开始记录一次请求，start 为请求的起点。sandboxID 用于跨组件关联，可稍后用 SetSandboxID 补充。
func Begin(ctx context.Context, component, op, sandboxID string, start time.Time) (context.Context, *Recorder) {
	if !enabled {
		return ctx, nil
	}

	r := &Recorder{start: start, rec: record{
		Component:   component,
		Op:          op,
		SandboxID:   sandboxID,
		StartUnixUs: start.UnixMicro(),
		Phases:      []phase{},
	}}
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		r.rec.TraceID = sc.TraceID().String()
	}

	return context.WithValue(ctx, scopeKey{}, scope{rec: r}), r
}

func (r *Recorder) SetSandboxID(sandboxID string) {
	if r == nil {
		return
	}

	r.mu.Lock()
	r.rec.SandboxID = sandboxID
	r.mu.Unlock()
}

// AddPhase 补记一个已经结束的顶层阶段。
func (r *Recorder) AddPhase(name string, start, end time.Time) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.ended {
		r.rec.Phases = append(r.rec.Phases, phase{
			ID:      len(r.rec.Phases) + 1,
			Name:    name,
			StartUs: start.Sub(r.start).Microseconds(),
			DurUs:   end.Sub(start).Microseconds(),
		})
	}
}

// End 结束记录并输出汇总行，只有第一次调用生效。
func (r *Recorder) End(ok bool) {
	if r == nil {
		return
	}

	r.mu.Lock()
	if r.ended {
		r.mu.Unlock()

		return
	}
	r.ended = true
	r.rec.TotalUs = time.Since(r.start).Microseconds()
	r.rec.OK = ok
	line, err := json.Marshal(r.rec)
	// Recorder 会随 ctx 被长生命周期的 goroutine 持有，输出后不再保留阶段数据
	r.rec.Phases = nil
	r.mu.Unlock()

	if err == nil {
		emit(context.Background(), LogPrefix+string(line))
	}
}

// Phase 开始一个阶段，返回带有该阶段的 ctx 和结束函数；用返回的 ctx 再开的阶段是它的子阶段。
// ctx 里没有 Recorder 或记录已结束时不做任何事。
func Phase(ctx context.Context, name string) (context.Context, func()) {
	sc, _ := ctx.Value(scopeKey{}).(scope)
	r := sc.rec
	if r == nil {
		return ctx, func() {}
	}

	r.mu.Lock()
	if r.ended {
		r.mu.Unlock()

		return ctx, func() {}
	}
	id := len(r.rec.Phases) + 1
	r.rec.Phases = append(r.rec.Phases, phase{
		ID:      id,
		Parent:  sc.phase,
		Name:    name,
		StartUs: time.Since(r.start).Microseconds(),
		DurUs:   -1,
	})
	r.mu.Unlock()

	return context.WithValue(ctx, scopeKey{}, scope{rec: r, phase: id}), func() { r.endPhase(ctx, id) }
}

func (r *Recorder) endPhase(ctx context.Context, id int) {
	r.mu.Lock()
	if r.ended || r.rec.Phases[id-1].DurUs >= 0 {
		r.mu.Unlock()

		return
	}
	p := &r.rec.Phases[id-1]
	p.DurUs = time.Since(r.start).Microseconds() - p.StartUs
	name, durUs := p.Name, p.DurUs
	r.mu.Unlock()

	if verbose {
		emit(ctx, fmt.Sprintf("[%s] %s cost: %.3f ms, traceID=%s", r.rec.Op, name, float64(durUs)/1000, r.rec.TraceID))
	}
}
