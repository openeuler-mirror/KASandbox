package sandbox

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/hostservice"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/network"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

var (
	egressProxySpawnCounter = utils.Must(meter.Int64Counter("orchestrator.sandbox.egress_proxy.spawned",
		metric.WithDescription("Number of per-sandbox egress proxy processes started and ready with redirect rules installed."),
		metric.WithUnit("{proxy}"),
	))
	egressProxyFailCounter = utils.Must(meter.Int64Counter("orchestrator.sandbox.egress_proxy.failed",
		metric.WithDescription("Number of per-sandbox egress proxy startup failures (build/spawn/ready/rules)."),
		metric.WithUnit("{proxy}"),
	))
)

// timingReadyCheck wraps the egress proxy ReadyCheck to split the startup
// latency into spawn (until the first probe attempt, i.e. right after the
// process exec) and ready (until the first successful probe) segments.
type timingReadyCheck struct {
	inner hostservice.ReadyCheck
	start time.Time

	mu        sync.Mutex
	firstCall time.Time
}

func (t *timingReadyCheck) Check(ctx context.Context) error {
	t.mu.Lock()
	if t.firstCall.IsZero() {
		t.firstCall = time.Now()
	}
	t.mu.Unlock()

	return t.inner.Check(ctx)
}

func (t *timingReadyCheck) String() string { return t.inner.String() }

func (t *timingReadyCheck) spawnMillis() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.firstCall.IsZero() {
		return 0
	}

	return t.firstCall.Sub(t.start).Milliseconds()
}

// startEgressProxy wires the per-sandbox netns egress proxy lifecycle for a
// sandbox resolved to egress-mode=per-sandbox; any other mode short-circuits
// with nil. It must be called after the slot cleanup is registered (the
// manager's StopAll cleanup is LIFO-ordered before it, so the proxy process
// group is reaped before the netns is removed) and before the VMM is created.
//
// Apply modes (SANDBOX_PROXY_APPLY):
//   - "immediate": spawn + ready check + redirect rule install run
//     synchronously; any failure fails the sandbox creation and the cleanup
//     stack rolls back (kill proxy → remove netns → release slot).
//   - "ready" (default): the same sequence runs in a background goroutine
//     detached from the create request (bounded by EgressProxyApplyTimeout);
//     the final failure is only logged — boot-time traffic egresses directly
//     and a broken proxy never slows down creation. Both orders are
//     fail-close (an unlistened REDIRECT target RSTs connections), so this is
//     deliberate design semantics, not a race window.
func (f *Factory) startEgressProxy(
	ctx context.Context,
	cleanup *Cleanup,
	netCfg network.Config,
	slot *network.Slot,
	config Config,
	runtime RuntimeMetadata,
	sandboxDir string,
	egressMode network.EgressMode,
) error {
	if egressMode != network.EgressModePerSandbox {
		return nil
	}

	start := time.Now()

	svc, err := hostservice.BuildEgressProxyService(
		hostservice.EgressProxyConfig{
			Binary:     netCfg.SandboxProxyBinary,
			Addon:      netCfg.SandboxProxyAddon,
			ConfDir:    netCfg.SandboxProxyConfDir,
			Port:       netCfg.SandboxProxyListenPort,
			Restart:    netCfg.SandboxProxyRestart,
			LogDir:     netCfg.SandboxProxyLogDir,
			Upstream:   netCfg.SandboxProxyUpstream,
			ExtraArgs:  netCfg.SandboxProxyExtraArgs,
			SandboxDir: sandboxDir,
		},
		slot.NamespaceID(),
		runtime.SandboxID,
		config.EgressIdentity,
	)
	if err != nil {
		if netCfg.SandboxProxyApply == network.EgressProxyApplyImmediate {
			return fmt.Errorf("build egress proxy service: %w", err)
		}
		egressProxyFailCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("phase", "build")))
		logger.L().Error(ctx, "failed to build egress proxy service, sandbox egresses directly",
			zap.Error(err),
			zap.String("sandbox_id", runtime.SandboxID),
		)

		return nil
	}

	timing := &timingReadyCheck{inner: svc.ReadyCheck, start: start}
	svc.ReadyCheck = timing

	manager := hostservice.NewManager([]hostservice.Service{svc}, f.config.ReadyCheckTimeout)
	cleanup.Add(ctx, manager.StopAll)

	runStartup := func(ctx context.Context) error {
		if err := manager.StartAll(ctx); err != nil {
			return fmt.Errorf("start egress proxy: %w", err)
		}
		readyAt := time.Now()

		rulesStart := time.Now()
		if err := slot.InstallEgressProxyRulesWithRetry(ctx); err != nil {
			return fmt.Errorf("install egress proxy rules: %w", err)
		}

		egressProxySpawnCounter.Add(ctx, 1)
		logger.L().Info(ctx, "egress proxy up",
			zap.String("sandbox_id", runtime.SandboxID),
			zap.String("namespace_id", slot.NamespaceID()),
			zap.Int64("proxy_spawn_ms", timing.spawnMillis()),
			zap.Int64("proxy_ready_ms", readyAt.Sub(start).Milliseconds()),
			zap.Int64("proxy_rules_ms", time.Since(rulesStart).Milliseconds()),
		)

		return nil
	}

	if netCfg.SandboxProxyApply == network.EgressProxyApplyImmediate {
		return runStartup(ctx)
	}

	go func() {
		// Detach from the (per-request) creation ctx: proxy bring-up is
		// decoupled from the create return path, but still bounded.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), network.EgressProxyApplyTimeout)
		defer cancel()

		if err := runStartup(ctx); err != nil {
			egressProxyFailCounter.Add(ctx, 1)
			logger.L().Error(ctx, "egress proxy startup failed, sandbox egresses directly",
				zap.Error(err),
				zap.String("sandbox_id", runtime.SandboxID),
				zap.String("namespace_id", slot.NamespaceID()),
			)
		}
	}()

	return nil
}
