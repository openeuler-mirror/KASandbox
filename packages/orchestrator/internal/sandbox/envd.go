package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/envd"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/vmm"
	"github.com/e2b-dev/infra/packages/shared/pkg/consts"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

const (
	loopDelay = 5 * time.Millisecond
)

type responseBodyWithConn struct {
	io.ReadCloser
	conn net.Conn
}

func (b *responseBodyWithConn) Close() error {
	bodyErr := b.ReadCloser.Close()
	connErr := b.conn.Close()
	if bodyErr != nil {
		return bodyErr
	}

	return connErr
}

// doAndroidInitRequest sends the small init request with one TCP write.
// StratoVirt's Android virtio-net path can delay responses indefinitely when
// net/http splits the headers and body across separate writes during cold
// boot. A single write avoids that transport-specific deadlock while keeping
// the normal instrumented HTTP transport for every other guest OS.
func (s *Sandbox) doAndroidInitRequest(
	ctx context.Context,
	method, address string,
	body []byte,
) (*http.Response, error) {
	u, err := url.Parse(address)
	if err != nil {
		return nil, fmt.Errorf("parse Android envd URL: %w", err)
	}

	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, err
	}

	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			conn.Close()
			return nil, err
		}
	}

	var request bytes.Buffer
	fmt.Fprintf(&request, "%s %s HTTP/1.1\r\n", method, u.RequestURI())
	fmt.Fprintf(&request, "Host: %s\r\n", u.Host)
	request.WriteString("Content-Type: application/json\r\n")
	fmt.Fprintf(&request, "Content-Length: %d\r\n", len(body))
	request.WriteString("Connection: close\r\n")
	if s.Config.Envd.AccessToken != nil {
		fmt.Fprintf(&request, "X-Access-Token: %s\r\n", *s.Config.Envd.AccessToken)
	}
	request.WriteString("\r\n")
	request.Write(body)

	written, err := conn.Write(request.Bytes())
	if err != nil {
		conn.Close()
		return nil, err
	}
	if written != request.Len() {
		conn.Close()
		return nil, io.ErrShortWrite
	}

	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: method})
	if err != nil {
		conn.Close()
		return nil, err
	}
	response.Body = &responseBodyWithConn{ReadCloser: response.Body, conn: conn}

	return response, nil
}

// doRequestWithInfiniteRetries does a request with infinite retries until the context is done.
// The parent context should have a deadline or a timeout.
func (s *Sandbox) doRequestWithInfiniteRetries(
	ctx context.Context,
	method,
	address string,
) (*http.Response, int64, error) {
	requestCount := int64(0)

	jsonBody := &envd.PostInitJSONBody{
		EnvVars:        s.Config.Envd.Vars,
		HyperloopIP:    s.config.NetworkConfig.OrchestratorInSandboxIPAddress,
		AccessToken:    utils.DerefOrDefault(s.Config.Envd.AccessToken, ""),
		DefaultUser:    utils.DerefOrDefault(s.Config.Envd.DefaultUser, ""),
		DefaultWorkdir: utils.DerefOrDefault(s.Config.Envd.DefaultWorkdir, ""),
		VolumeMounts:   s.convertMounts(s.Config.VolumeMounts),
	}

	for {
		jsonBody.Timestamp = time.Now()

		body, err := json.Marshal(jsonBody)
		if err != nil {
			return nil, requestCount, err
		}

		requestCount++
		reqCtx, cancel := context.WithTimeout(ctx, s.internalConfig.EnvdInitRequestTimeout)
		request, err := http.NewRequestWithContext(reqCtx, method, address, bytes.NewReader(body))
		if err != nil {
			cancel()

			return nil, requestCount, err
		}
		request.Header.Set("Content-Type", "application/json")

		// make sure request to already authorized envd will not fail
		// this can happen in sandbox resume and in some edge cases when previous request was success, but we continued
		if s.Config.Envd.AccessToken != nil {
			request.Header.Set("X-Access-Token", *s.Config.Envd.AccessToken)
		}

		var response *http.Response
		if s.Config.VMMConfig.OsType.OrDefault() == vmm.OsAndroid {
			response, err = s.doAndroidInitRequest(reqCtx, method, address, body)
		} else {
			response, err = sandboxHttpClient.Do(request)
		}
		cancel()

		if err == nil {
			return response, requestCount, nil
		}

		logger.L().Debug(ctx, "failed to do request to envd, retrying",
			logger.WithSandboxID(s.Runtime.SandboxID),
			logger.WithEnvdVersion(s.Config.Envd.Version),
			zap.Int64("timeout_ms", s.internalConfig.EnvdInitRequestTimeout.Milliseconds()),
			zap.Error(err))

		select {
		case <-ctx.Done():
			return nil, requestCount, fmt.Errorf("%w with cause: %w", ctx.Err(), context.Cause(ctx))
		case <-time.After(loopDelay):
		}
	}
}

func (s *Sandbox) convertMounts(mounts []VolumeMountConfig) []envd.VolumeMount {
	results := make([]envd.VolumeMount, 0, len(mounts))

	for _, mount := range mounts {
		results = append(results, envd.VolumeMount{
			NfsTarget: fmt.Sprintf("%s:/%s", s.config.NetworkConfig.OrchestratorInSandboxIPAddress, mount.Name),
			Path:      mount.Path,
		})
	}

	return results
}

func (s *Sandbox) initEnvd(ctx context.Context) (e error) {
	ctx, span := tracer.Start(ctx, "envd-init", trace.WithAttributes(telemetry.WithEnvdVersion(s.Config.Envd.Version)))
	traceID := span.SpanContext().TraceID().String()
	defer func() {
		if e != nil {
			span.SetStatus(codes.Error, e.Error())
		}

		span.End()
	}()

	attributes := []attribute.KeyValue{telemetry.WithEnvdVersion(s.Config.Envd.Version), attribute.Int64("timeout_ms", s.internalConfig.EnvdInitRequestTimeout.Milliseconds())}
	attributesFail := append(attributes, attribute.Bool("success", false))
	attributesSuccess := append(attributes, attribute.Bool("success", true))

	address := fmt.Sprintf("http://%s:%d/init", s.Slot.HostIPString(), consts.DefaultEnvdServerPort)

	reqStart := time.Now()
	response, count, err := s.doRequestWithInfiniteRetries(ctx, http.MethodPost, address)
	if err != nil {
		envdInitCalls.Add(ctx, count, metric.WithAttributes(attributesFail...))

		return fmt.Errorf("failed to init envd: %w", err)
	}
	zap.L().Sugar().Infof("[ResumeSandbox] start envd do request cost: %.3f ms, traceID=%s", time.Since(reqStart).Seconds()*1000, traceID)

	if count > 1 {
		// Track failed envd init calls
		envdInitCalls.Add(ctx, count-1, metric.WithAttributes(attributesFail...))
	}

	// Track successful envd init
	envdInitCalls.Add(ctx, 1, metric.WithAttributes(attributesSuccess...))

	defer response.Body.Close()
	readStart := time.Now()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("failed to read envd init response body: %w", err)
	}
	zap.L().Sugar().Infof("[ResumeSandbox] read envd response cost: %.3f ms, traceID=%s", time.Since(readStart).Seconds()*1000, traceID)

	if response.StatusCode != http.StatusNoContent {
		logger.L().Error(ctx, "envd init request failed",
			logger.WithSandboxID(s.Runtime.SandboxID),
			logger.WithEnvdVersion(s.Config.Envd.Version),
			zap.Int("status_code", response.StatusCode),
			zap.String("response_body", utils.Truncate(string(body), 100)),
		)

		return fmt.Errorf("unexpected status code: %d", response.StatusCode)
	}

	span.SetStatus(codes.Ok, fmt.Sprintf("envd init returned %d", response.StatusCode))

	return nil
}
