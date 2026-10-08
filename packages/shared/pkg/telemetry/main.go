package telemetry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/metric"
	noopMetric "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	noopTrace "go.opentelemetry.io/otel/trace/noop"
)

const metricExportPeriod = 15 * time.Second

type Client struct {
	MetricExporter  sdkmetric.Exporter
	MeterProvider   metric.MeterProvider
	SpanExporter    sdktrace.SpanExporter
	TracerProvider  trace.TracerProvider
	TracePropagator propagation.TextMapPropagator
	LogsProvider    LogProvider
}

func New(ctx context.Context, nodeID, serviceName, serviceCommit, serviceVersion, serviceInstanceID string) (*Client, error) {
	if otelCollectorGRPCEndpoint == "" {
		return NewNoopClient(), nil
	}

	// Setup metrics
	metricsExporter, err := NewMeterExporter(ctx, otlpmetricgrpc.WithAggregationSelector(func(kind sdkmetric.InstrumentKind) sdkmetric.Aggregation {
		if kind == sdkmetric.InstrumentKindHistogram {
			// Defaults from https://github.com/open-telemetry/opentelemetry-specification/blob/main/specification/metrics/sdk.md#base2-exponential-bucket-histogram-aggregation
			return sdkmetric.AggregationBase2ExponentialHistogram{
				MaxSize:  160,
				MaxScale: 20,
				NoMinMax: false,
			}
		}

		return sdkmetric.DefaultAggregationSelector(kind)
	}))
	if err != nil {
		return nil, fmt.Errorf("failed to create metrics exporter: %w", err)
	}

	res, err := GetResource(ctx, nodeID, serviceName, serviceCommit, serviceVersion, serviceInstanceID)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	meterProvider, err := NewMeterProvider(metricsExporter, metricExportPeriod, res)
	if err != nil {
		return nil, fmt.Errorf("failed to create metrics provider: %w", err)
	}
	otel.SetMeterProvider(meterProvider)

	// Setup logging
	logProvider, err := NewLogProvider(ctx, res)
	if err != nil {
		return nil, fmt.Errorf("failed to create log provider: %w", err)
	}

	// Setup tracing
	spanExporter, err := NewSpanExporter(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create span exporter: %w", err)
	}

	tracerProvider := NewTracerProvider(spanExporter, res)
	otel.SetTracerProvider(tracerProvider)

	// There's probably not a reason why not to set the trace propagator globally, it's used in SDKs
	propagator := NewTextPropagator()
	otel.SetTextMapPropagator(propagator)

	return &Client{
		MetricExporter:  metricsExporter,
		MeterProvider:   meterProvider,
		SpanExporter:    spanExporter,
		TracerProvider:  tracerProvider,
		TracePropagator: propagator,
		LogsProvider:    logProvider,
	}, nil
}

func (t *Client) Shutdown(ctx context.Context) error {
	var errs []error
	if t.MetricExporter != nil {
		if err := t.MetricExporter.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if t.SpanExporter != nil {
		if err := t.SpanExporter.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if t.LogsProvider != nil {
		if err := t.LogsProvider.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// ShutdownWithTimeout 在独立协程中执行 shutdown，超时后直接放弃等待并返回
// 错误。OTLP gRPC exporter 的 Shutdown 可能被连接重连退避阻塞、不遵守传入
// 的 ctx（实测 collector 不可达时偶发阻塞 19s+，超出 ctx deadline 数倍）。
// 仅用于进程退出路径：放弃等待后进程随即退出，泄漏的协程会被回收。
func ShutdownWithTimeout(timeout time.Duration, shutdown func(ctx context.Context) error) error {
	done := make(chan error, 1)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		done <- shutdown(ctx)
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("shutdown did not finish within %s, abandoned (OTLP collector likely unreachable)", timeout)
	}
}

func NewNoopClient() *Client {
	return &Client{
		MetricExporter:  &noopMetricExporter{},
		MeterProvider:   noopMetric.MeterProvider{},
		SpanExporter:    &noopSpanExporter{},
		TracerProvider:  noopTrace.NewTracerProvider(),
		TracePropagator: propagation.NewCompositeTextMapPropagator(),
		LogsProvider:    NewNoopLogProvider(),
	}
}
