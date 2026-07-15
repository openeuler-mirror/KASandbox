package grpc

import (
	"context"
	"time"

	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/logging"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/recovery"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/selector"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
)

func NewGRPCServer(tel *telemetry.Client) *grpc.Server {
	opts := []logging.Option{
		logging.WithLogOnEvents(logging.StartCall, logging.PayloadReceived, logging.PayloadSent, logging.FinishCall),
		logging.WithLevels(logging.DefaultServerCodeToLevel),
		logging.WithFieldsFromContext(logging.ExtractFields),
	}

	ignoredLoggingRoutes := logger.WithoutRoutes(
		logger.HealthCheckRoute,
		"/TemplateService/TemplateBuildStatus",
		"/TemplateService/HealthStatus",
		"/InfoService/ServiceInfo",
	)

	return grpc.NewServer(
		grpc.MaxConcurrentStreams(1000),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             5 * time.Second, // Minimum time between pings from client
			PermitWithoutStream: true,            // Allow pings even when no active streams
		}),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    15 * time.Second, // Server sends keepalive pings every 15s
			Timeout: 5 * time.Second,  // Wait 5s for response before considering dead
                        MaxConnectionAge:  0, // 0 = 不强制关闭活跃连接
                        MaxConnectionIdle: 0, // 0 = 不强制关闭空闲连接
		}),
		grpc.StatsHandler(
			NewStatsWrapper(
				otelgrpc.NewServerHandler(
					otelgrpc.WithTracerProvider(tel.TracerProvider),
					otelgrpc.WithMeterProvider(tel.MeterProvider),
					otelgrpc.WithMetricAttributesFn(extractIsResume),
				))),
		grpc.ChainUnaryInterceptor(
			recovery.UnaryServerInterceptor(),
			selector.UnaryServerInterceptor(
				logging.UnaryServerInterceptor(logger.GRPCLogger(logger.L()), opts...),
				ignoredLoggingRoutes,
			),
		),
		grpc.ChainStreamInterceptor(
			selector.StreamServerInterceptor(
				logging.StreamServerInterceptor(logger.GRPCLogger(logger.L()), opts...),
				ignoredLoggingRoutes,
			),
		),
	)
}

func extractIsResume(ctx context.Context) []attribute.KeyValue {
	if holder, ok := ctx.Value(isResumeHolderKey{}).(*IsResumeHolder); ok {
		return []attribute.KeyValue{
			attribute.Bool("sandbox.resume", holder.Value),
		}
	}

	return nil
}
