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
//
// deferRules (SANDBOX_PROXY_CA_AUTO guest injection path, §7.4.2): spawn +
// ready check run synchronously regardless of the apply mode, but redirect
// rule installation is deferred to the caller — the rules must only go in
// after the CA cert has been injected into the guest trust store (which needs
// a running envd). The caller then runs inject + rule install synchronously
// (forced immediate semantics: a "rules installed but CA not injected" window
// would fail every guest HTTPS handshake, so injection failure must fail the
// creation and roll back via the cleanup stack).
func (f *Factory) startEgressProxy(
	ctx context.Context,
	cleanup *Cleanup,
	netCfg network.Config,
	slot *network.Slot,
	config Config,
	runtime RuntimeMetadata,
	sandboxDir string,
	egressMode network.EgressMode,
	deferRules bool,
) error {
	if egressMode != network.EgressModePerSandbox {
		return nil
	}

	// deferRules 强制 immediate 语义：代理起不来/规则装不上都直接创建失败，
	// 不允许「规则已装、CA 未注入」的半配置沙箱运行。
	failFast := deferRules || netCfg.SandboxProxyApply == network.EgressProxyApplyImmediate

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
		if failFast {
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

	runStartup := func(ctx context.Context, installRules bool) error {
		if err := manager.StartAll(ctx); err != nil {
			return fmt.Errorf("start egress proxy: %w", err)
		}
		readyAt := time.Now()

		var rulesMillis int64
		if installRules {
			rulesStart := time.Now()
			if err := slot.InstallEgressProxyRulesWithRetry(ctx); err != nil {
				return fmt.Errorf("install egress proxy rules: %w", err)
			}
			rulesMillis = time.Since(rulesStart).Milliseconds()

			egressProxySpawnCounter.Add(ctx, 1)
		}

		logger.L().Info(ctx, "egress proxy up",
			zap.String("sandbox_id", runtime.SandboxID),
			zap.String("namespace_id", slot.NamespaceID()),
			zap.Int64("proxy_spawn_ms", timing.spawnMillis()),
			zap.Int64("proxy_ready_ms", readyAt.Sub(start).Milliseconds()),
			zap.Int64("proxy_rules_ms", rulesMillis),
			zap.Bool("rules_deferred", !installRules),
		)

		return nil
	}

	if failFast {
		// immediate：同步装规则；deferRules：规则延后到 CA 注入完成之后。
		return runStartup(ctx, !deferRules)
	}

	go func() {
		// Detach from the (per-request) creation ctx: proxy bring-up is
		// decoupled from the create return path, but still bounded.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), network.EgressProxyApplyTimeout)
		defer cancel()

		if err := runStartup(ctx, true); err != nil {
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

// installDeferredEgressProxyRules installs the redirect rules that
// startEgressProxy deferred for the CA injection path, with the same retry +
// metrics semantics as the synchronous apply mode. Must run after the guest
// CA injection succeeded.
func installDeferredEgressProxyRules(ctx context.Context, slot *network.Slot, runtime RuntimeMetadata) error {
	rulesStart := time.Now()
	if err := slot.InstallEgressProxyRulesWithRetry(ctx); err != nil {
		egressProxyFailCounter.Add(ctx, 1)

		return fmt.Errorf("install egress proxy rules: %w", err)
	}

	egressProxySpawnCounter.Add(ctx, 1)
	logger.L().Info(ctx, "egress proxy rules installed after guest CA injection",
		zap.String("sandbox_id", runtime.SandboxID),
		zap.String("namespace_id", slot.NamespaceID()),
		zap.Int64("proxy_rules_ms", time.Since(rulesStart).Milliseconds()),
	)

	return nil
}

// injectEgressProxyCAAndInstallRules 是 §7.4.2 在创建/恢复路径上的收尾：
// 等 envd 就绪后把代理 CA 注入 guest 信任库，成功后再安装延后的 REDIRECT
// 规则。任一步失败即返回错误，创建/恢复失败并走 cleanup 回滚。
func (f *Factory) injectEgressProxyCAAndInstallRules(
	ctx context.Context,
	sbx *Sandbox,
	netCfg network.Config,
	slot *network.Slot,
	runtime RuntimeMetadata,
) error {
	injectStart := time.Now()
	if err := sbx.injectEgressProxyCA(ctx, netCfg); err != nil {
		return fmt.Errorf("inject egress proxy CA into guest: %w", err)
	}

	logger.L().Info(ctx, "egress proxy CA injected, installing deferred redirect rules",
		zap.String("sandbox_id", runtime.SandboxID),
		zap.Int64("ca_inject_ms", time.Since(injectStart).Milliseconds()),
	)

	return installDeferredEgressProxyRules(ctx, slot, runtime)
}
