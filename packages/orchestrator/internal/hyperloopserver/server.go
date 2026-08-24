package hyperloopserver

import (
	"context"
	"fmt"
	"net"
	"net/http"

	limits "github.com/gin-contrib/size"
	"github.com/gin-gonic/gin"
	middleware "github.com/oapi-codegen/gin-middleware"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/hyperloopserver/contracts"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/hyperloopserver/handlers"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox"
	"github.com/e2b-dev/infra/packages/shared/pkg/env"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

const maxUploadLimit = 1 << 28 // 256 MiB

func NewHyperloopServer(ctx context.Context, port uint16, logger logger.Logger, sandboxes *sandbox.Map) (*http.Server, error) {
	// Sandbox log forwarding is optional. Only enable it for a valid HTTP(S)
	// collector URL; an empty value disables it silently, while a non-empty but
	// malformed value is disabled with a warning so a misconfiguration is
	// visible at startup rather than as a failed request per log line.
	sandboxCollectorAddr, ok := env.ValidLogsCollectorAddress()
	if !ok && env.LogsCollectorAddress() != "" {
		logger.Warn(ctx, "LOGS_COLLECTOR_ADDRESS is set but not a valid http(s) URL; sandbox log forwarding disabled",
			zap.String("value", env.LogsCollectorAddress()))
	}
	store := handlers.NewHyperloopStore(logger, sandboxes, sandboxCollectorAddr)
	swagger, err := contracts.GetSwagger()
	if err != nil {
		return nil, fmt.Errorf("error getting swagger spec: %w", err)
	}

	engine := gin.New()
	engine.Use(
		gin.Recovery(),
		limits.RequestSizeLimiter(maxUploadLimit),
		middleware.OapiRequestValidatorWithOptions(swagger, &middleware.Options{}),
	)

	server := &http.Server{
		Handler: engine,
		Addr:    fmt.Sprintf("0.0.0.0:%d", port),

		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	contracts.RegisterHandlersWithOptions(engine, store, contracts.GinServerOptions{})

	return server, nil
}
