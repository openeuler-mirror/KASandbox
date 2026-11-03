package engine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const defaultCleanupTimeout = 2 * time.Minute

type CleanupConfig struct {
	Enabled     bool
	Interval    time.Duration
	GracePeriod time.Duration
	MaxRetries  int
	DryRun      bool
	// CleanupTimeout 限制单次 Schedule 调后端 Cleaner 的时长，默认 2 分钟。
	CleanupTimeout time.Duration
}

type CleanupResult struct {
	Succeeded bool
	Err       error
}

type CleanupManager struct {
	stateStore StateStore
	reg        *Registry
	cfg        CleanupConfig
	now        func() time.Time
	// taskMu 串行化 cleanup_tasks 的 load-modify-save，避免并发 Schedule 丢 Attempts 更新
	// （等效 singleflight；Reconcile 仍由单协程驱动）。
	taskMu sync.Mutex

	cancel context.CancelFunc
}

func NewCleanupManager(store StateStore, reg *Registry, cfg CleanupConfig) *CleanupManager {
	if cfg.Interval <= 0 {
		cfg.Interval = 60 * time.Second
	}
	if cfg.GracePeriod < 0 {
		cfg.GracePeriod = 0
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 10
	}
	if cfg.CleanupTimeout <= 0 {
		cfg.CleanupTimeout = defaultCleanupTimeout
	}
	return &CleanupManager{
		stateStore: store,
		reg:        reg,
		cfg:        cfg,
		now:        time.Now,
	}
}

// NewCleanupManagerLegacy 用旧的两引擎签名构造临时注册表，供过渡期与测试使用。
func NewCleanupManagerLegacy(store StateStore, e2b, android RuntimeEngine, cfg CleanupConfig) *CleanupManager {
	reg := NewRegistry("")
	for _, eng := range []RuntimeEngine{e2b, android} {
		if eng == nil {
			continue
		}
		mod := BackendModule{Descriptor: BackendDescriptor{Type: eng.Type(), Handler: string(eng.Type())}, Engine: eng}
		if cleaner, ok := eng.(Cleaner); ok {
			mod.Cleaner = cleaner
		}
		if reconciler, ok := eng.(Reconciler); ok {
			mod.Reconciler = reconciler
		}
		if restorer, ok := eng.(StateRestorer); ok {
			mod.Restorer = restorer
		}
		if err := reg.Register(mod); err != nil {
			log.Printf("[Cleanup] WARNING: register legacy backend %s: %v", eng.Type(), err)
		}
	}
	if err := reg.Freeze(); err != nil {
		log.Printf("[Cleanup] WARNING: freeze legacy registry: %v", err)
	}
	return NewCleanupManager(store, reg, cfg)
}

// Schedule 通过注册表里的 Cleaner 回收一个 sandbox 的后端资源。
// 约定：未知后端与清理失败都必须把任务留在 cleanup_tasks 里，成功才删任务。
func (m *CleanupManager) Schedule(ctx context.Context, backend EngineType, sandboxID, resourceType, reason string) CleanupResult {
	if m == nil || sandboxID == "" {
		return CleanupResult{Succeeded: true}
	}
	log.Printf("[Cleanup] start backend=%s sandbox=%s resource=%s reason=%s", backend, sandboxID, resourceType, reason)
	if m.cfg.DryRun {
		log.Printf("[Cleanup] dry-run backend=%s sandbox=%s resource=%s reason=%s", backend, sandboxID, resourceType, reason)
		return CleanupResult{Succeeded: true}
	}

	taskID := cleanupTaskID(backend, sandboxID, resourceType)
	mod, ok := m.module(backend)
	if !ok {
		// 未知后端的任务必须保留：后端上线后还能重试，且不能误删别的后端资源。
		err := fmt.Errorf("cleanup backend %q is not registered", backend)
		if saveErr := m.saveRetryTask(CleanupTask{
			ID:           taskID,
			Runtime:      backend,
			SandboxID:    sandboxID,
			ResourceType: resourceType,
			Reason:       reason,
		}, err); saveErr != nil {
			err = errors.Join(err, fmt.Errorf("persist cleanup task: %w", saveErr))
		}
		log.Printf("[Cleanup] retry backend=%s sandbox=%s resource=%s err=%v", backend, sandboxID, resourceType, err)
		return CleanupResult{Err: err}
	}
	if mod.Cleaner == nil {
		return CleanupResult{Succeeded: true}
	}

	cleanupCtx, cancel := context.WithTimeout(ctx, m.cfg.CleanupTimeout)
	err := mod.Cleaner.CleanupSandbox(cleanupCtx, sandboxID, reason)
	cancel()
	if isCleanupNotFound(err) {
		err = nil
	}
	if err == nil {
		if deleteErr := m.deleteTask(taskID); deleteErr != nil {
			return CleanupResult{Err: deleteErr}
		}
		log.Printf("[Cleanup] done backend=%s sandbox=%s", backend, sandboxID)
		return CleanupResult{Succeeded: true}
	}
	if saveErr := m.saveRetryTask(CleanupTask{
		ID:           taskID,
		Runtime:      backend,
		SandboxID:    sandboxID,
		ResourceType: resourceType,
		Reason:       reason,
	}, err); saveErr != nil {
		err = errors.Join(err, fmt.Errorf("persist cleanup task: %w", saveErr))
	}
	log.Printf("[Cleanup] retry backend=%s sandbox=%s resource=%s err=%v", backend, sandboxID, resourceType, err)
	return CleanupResult{Err: err}
}

func (m *CleanupManager) module(backend EngineType) (*BackendModule, bool) {
	if m == nil || m.reg == nil {
		return nil, false
	}
	return m.reg.Get(backend)
}

// Reconcile 先重试到期任务，再让每个后端自己的 Reconciler 做对账。
func (m *CleanupManager) Reconcile(ctx context.Context) error {
	if m == nil || !m.cfg.Enabled || m.reg == nil {
		return nil
	}
	var errs []error
	if err := m.RetryPending(ctx); err != nil {
		errs = append(errs, err)
	}
	opts := ReconcileOptions{GracePeriod: m.cfg.GracePeriod, DryRun: m.cfg.DryRun}
	for _, mod := range m.reg.Modules() {
		if mod.Reconciler == nil {
			continue
		}
		if err := mod.Reconciler.Reconcile(ctx, opts); err != nil {
			errs = append(errs, fmt.Errorf("%s reconcile: %w", mod.Descriptor.Type, err))
		}
	}
	return errors.Join(errs...)
}

func cleanupTaskID(backend EngineType, sandboxID, resourceType string) string {
	return fmt.Sprintf("%s:%s:%s", backend, resourceType, sandboxID)
}

func (m *CleanupManager) saveRetryTask(task CleanupTask, cleanupErr error) error {
	if m == nil || m.stateStore == nil {
		return nil
	}
	m.taskMu.Lock()
	defer m.taskMu.Unlock()

	now := m.now()
	existing, err := m.stateStore.LoadCleanupTasks()
	if err != nil {
		return err
	}
	for _, item := range existing {
		if item.ID == task.ID {
			task = item
			break
		}
	}
	task.Attempts++
	task.LastError = cleanupErr.Error()
	if task.CreatedAt.IsZero() {
		task.CreatedAt = now
	}
	if task.Attempts < m.cfg.MaxRetries {
		delay := time.Second << min(task.Attempts-1, 6)
		task.NextRetryAt = now.Add(delay)
	} else {
		task.NextRetryAt = time.Time{}
	}
	return m.stateStore.SaveCleanupTask(task)
}

func (m *CleanupManager) deleteTask(taskID string) error {
	if m == nil || m.stateStore == nil {
		return nil
	}
	m.taskMu.Lock()
	defer m.taskMu.Unlock()

	return m.stateStore.DeleteCleanupTask(taskID)
}

func (m *CleanupManager) RetryPending(ctx context.Context) error {
	if m == nil || m.stateStore == nil {
		return nil
	}
	tasks, err := m.stateStore.LoadCleanupTasks()
	if err != nil {
		return err
	}
	now := m.now()
	var errs []error
	for _, task := range tasks {
		if task.Attempts >= m.cfg.MaxRetries || (!task.NextRetryAt.IsZero() && task.NextRetryAt.After(now)) {
			continue
		}
		if result := m.Schedule(ctx, task.Runtime, task.SandboxID, task.ResourceType, "retry"); result.Err != nil {
			errs = append(errs, result.Err)
		}
	}
	return errors.Join(errs...)
}

func (m *CleanupManager) Start(ctx context.Context) {
	if m == nil || !m.cfg.Enabled {
		return
	}
	periodicCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	go func() {
		ticker := time.NewTicker(m.cfg.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-periodicCtx.Done():
				return
			case <-ticker.C:
				reconcileCtx, cancel := context.WithTimeout(periodicCtx, minDuration(m.cfg.Interval, 15*time.Second))
				if err := m.Reconcile(reconcileCtx); err != nil {
					log.Printf("[Cleanup] periodic reconcile warning: %v", err)
				}
				cancel()
			}
		}
	}()
}

func (m *CleanupManager) Close() {
	if m != nil && m.cancel != nil {
		m.cancel()
	}
}

func isCleanupNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return true
	}
	if status.Code(err) == codes.NotFound {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "no such file") || strings.Contains(msg, "no chain/target/match") ||
		strings.Contains(msg, "bad rule")
}

func collectCleanupError(errs *[]error, resource string, err error) {
	if err == nil || isCleanupNotFound(err) {
		return
	}
	*errs = append(*errs, fmt.Errorf("%s: %w", resource, err))
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
