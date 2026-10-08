package telemetry

import (
	"context"
	"fmt"
	"os"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.20.0"
)

var otelCollectorGRPCEndpoint = os.Getenv("OTEL_COLLECTOR_GRPC_ENDPOINT")

// HasCollectorEndpoint 报告是否配置了 OTLP collector endpoint
// （OTEL_COLLECTOR_GRPC_ENDPOINT）。未配置时导出路径必须走 noop：
// 空 endpoint 会覆盖 SDK 默认地址，创建出的真实 exporter 每次导出
// 都失败并刷 "failed to upload metrics" 错误日志。
func HasCollectorEndpoint() bool {
	return otelCollectorGRPCEndpoint != ""
}

func GetResource(ctx context.Context, nodeID, serviceName, serviceCommit, serviceVersion, serviceInstanceID string) (*resource.Resource, error) {
	attributes := []attribute.KeyValue{
		semconv.ServiceName(serviceName),
		semconv.ServiceVersion(fmt.Sprintf("%s-%s", serviceVersion, serviceCommit)),
		semconv.ServiceInstanceID(serviceInstanceID),
		semconv.TelemetrySDKName("otel"),
		semconv.HostID(nodeID),
		semconv.TelemetrySDKLanguageGo,
	}

	hostname, err := os.Hostname()
	if err == nil {
		attributes = append(attributes, semconv.HostName(hostname))
	}

	res, err := resource.New(
		ctx,
		resource.WithSchemaURL(semconv.SchemaURL),
		resource.WithAttributes(attributes...),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	return res, nil
}
