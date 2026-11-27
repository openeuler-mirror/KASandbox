package checkpoint

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

var (
	meter = otel.GetMeterProvider().Meter("orchestrator.internal.checkpoint")

	checkpointCalls    = utils.Must(telemetry.GetCounter(meter, telemetry.SandboxCheckpointCalls))
	checkpointDuration = utils.Must(telemetry.GetHistogram(meter, telemetry.SandboxCheckpointDurationHistogramName))
)

// callStats carries what only the handler knows about how an operation went,
// so the caller that times it can report it in the same place.
//
// It exists for the degradation paths. A checkpoint that had to capture full
// memory, or a clone that fell back to copying the base byte by byte, costs
// orders of magnitude more than the intended path while still succeeding —
// exactly the kind of regression that hides in a latency average until
// somebody reads the logs. Reporting it as an attribute makes it answerable
// from a dashboard.
type callStats struct {
	attrs []attribute.KeyValue
}

func (c *callStats) set(key string, value string) {
	c.attrs = append(c.attrs, attribute.String(key, value))
}

func (c *callStats) setBool(key string, value bool) {
	c.attrs = append(c.attrs, attribute.Bool(key, value))
}

// record reports the outcome and cost of one checkpoint operation.
func record(ctx context.Context, operation string, start time.Time, err error, stats *callStats) {
	attrs := make([]attribute.KeyValue, 0, len(stats.attrs)+2)
	attrs = append(attrs,
		attribute.String("operation", operation),
		attribute.Bool("success", err == nil),
	)
	attrs = append(attrs, stats.attrs...)

	set := metric.WithAttributes(attrs...)

	checkpointCalls.Add(ctx, 1, set)
	checkpointDuration.Record(ctx, time.Since(start).Milliseconds(), set)
}
