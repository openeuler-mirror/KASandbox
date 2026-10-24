package engine

import (
	"context"
	"time"
)

// Cleaner 是后端可选能力：按 sandbox 维度回收资源，必须幂等（资源不存在返回 nil）。
type Cleaner interface {
	CleanupSandbox(ctx context.Context, sandboxID, reason string) error
}

// ReconcileOptions 由 CleanupManager 统一下发，避免同一份策略在多个 Config 里各存一份。
type ReconcileOptions struct {
	GracePeriod time.Duration
	DryRun      bool
}

// Reconciler 是后端可选能力：把运行态与持久态对齐（对账）。
type Reconciler interface {
	Reconcile(ctx context.Context, opts ReconcileOptions) error
}

// StateRestorer 是后端可选能力：从持久化状态恢复内存态（含路由派生）。
type StateRestorer interface {
	RestoreState(ctx context.Context) error
}

// HealthChecker 是后端可选能力：健康检查结果只用于日志，不影响 CRI Status 的 readiness。
type HealthChecker interface {
	Health(ctx context.Context) error
}

// CleanupScheduler 由 CleanupManager 实现，注入 Mux 以便路由持久化失败时排入清理。
type CleanupScheduler interface {
	Schedule(ctx context.Context, backend EngineType, sandboxID, resourceType, reason string) CleanupResult
}
