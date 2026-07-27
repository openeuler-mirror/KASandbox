package hostservice

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

// Manager orchestrates a set of host-side service processes for one sandbox.
//
// The Manager starts services in slice order (callers should arrange the slice
// in dependency order — deps first). Stop is performed in reverse order so
// that dependents are torn down before their dependencies. Suspend/Resume
// operate over all entries in slice order.
//
// nil-safe: a nil *Manager behaves as a no-op for every lifecycle method,
// which keeps Linux/Windows sandbox paths branch-free.
type Manager struct {
	mu       sync.Mutex
	services []Service
	entries  []*procEntry
	cancel   context.CancelFunc
}

// NewManager builds a Manager over the given services. The slice is consumed
// in order; callers control the startup order by the position in the slice.
// Returns nil when services is empty so callers can short-circuit downstream
// nil-checks.
func NewManager(services []Service) *Manager {
	if len(services) == 0 {
		return nil
	}
	return &Manager{services: services}
}

// StartAll starts every service in slice order, waiting for each one's
// ReadyCheck (if any) before starting the next. On any failure, already
// started services are stopped in reverse order and the manager resets to
// its pre-Start state. Safe to call only once per Manager instance.
func (m *Manager) StartAll(ctx context.Context) error {
	if m == nil {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.entries != nil {
		return fmt.Errorf("host service manager already started")
	}
	if len(m.services) == 0 {
		return nil
	}

	// Host services belong to the sandbox, not to the gRPC request that created
	// it. The manager's own cancel function controls their lifetime.
	svcCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	m.cancel = cancel

	entries := make([]*procEntry, 0, len(m.services))
	rollback := func() {
		cancel()
		for i := len(entries) - 1; i >= 0; i-- {
			_ = stopProc(ctx, entries[i], stopGracePeriodSec)
		}
		m.closeServiceFiles()
		m.entries = nil
		m.cancel = nil
	}

	for i, svc := range m.services {
		entry, err := startService(svcCtx, svc)
		if err != nil {
			rollback()
			return fmt.Errorf("start host service %s: %w", svc.Name, err)
		}
		entries = append(entries, entry)

		if svc.ReadyCheck != nil {
			if err := waitReady(ctx, svc.ReadyCheck, readyCheckTimeout); err != nil {
				logger.L().Error(ctx, "host service ready check failed",
					zap.String("service", svc.Name),
					zap.Error(err),
				)
				rollback()
				return fmt.Errorf("host service %s not ready: %w", svc.Name, err)
			}
		}

		logger.L().Info(ctx, "host service started and ready",
			zap.String("service", svc.Name),
			zap.Int("index", i),
		)
	}

	m.entries = entries

	for _, entry := range entries {
		entry := entry
		go monitorAndRestart(svcCtx, entry, func(ctx context.Context, old *procEntry) (*procEntry, error) {
			newEntry, err := startService(ctx, old.service)
			if err != nil {
				return nil, err
			}
			m.mu.Lock()
			for i, p := range m.entries {
				if p == old {
					m.entries[i] = newEntry
					break
				}
			}
			m.mu.Unlock()
			return newEntry, nil
		})
	}

	logger.L().Info(ctx, "host service manager started",
		zap.Int("service_count", len(m.services)),
	)
	return nil
}

// StopAll stops all services in reverse slice order (dependents before deps),
// sending SIGTERM then SIGKILL after the grace period. Idempotent: calling on
// a not-started or already-stopped Manager is a no-op.
func (m *Manager) StopAll(ctx context.Context) error {
	if m == nil {
		return nil
	}

	m.mu.Lock()
	entries := m.entries
	cancel := m.cancel
	m.entries = nil
	m.cancel = nil
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if len(entries) == 0 {
		return nil
	}

	var errs []error
	for i := len(entries) - 1; i >= 0; i-- {
		if err := stopProc(ctx, entries[i], stopGracePeriodSec); err != nil {
			errs = append(errs, fmt.Errorf("stop host service %s: %w", entries[i].service.Name, err))
		}
	}
	m.closeServiceFiles()
	return errors.Join(errs...)
}

// closeServiceFiles releases the parent-owned listener/socket descriptors.
// Children inherit their own descriptors at exec, while the parent copies are
// deliberately retained until StopAll so RestartOnCrash can reuse them.
func (m *Manager) closeServiceFiles() {
	for i := range m.services {
		m.services[i].CloseParentResources()
	}
}

// SuspendAll SIGSTOPs every running service so its in-flight connection state
// freezes in lockstep with a paused VMM. Used by Sandbox.Pause / Shutdown
// before the guest is snapshotted.
func (m *Manager) SuspendAll() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	var errs []error
	for _, entry := range m.entries {
		if err := suspendProc(entry); err != nil {
			errs = append(errs, fmt.Errorf("suspend host service %s: %w", entry.service.Name, err))
		}
	}
	return errors.Join(errs...)
}

// ResumeAll SIGCONTs every suspended service. Mirrors SuspendAll. Note: e2b's
// current Pause() flow destroys the sandbox after snapshotting, so this is
// reserved for future live suspend/resume support.
func (m *Manager) ResumeAll() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	var errs []error
	for _, entry := range m.entries {
		if err := resumeProc(entry); err != nil {
			errs = append(errs, fmt.Errorf("resume host service %s: %w", entry.service.Name, err))
		}
	}
	return errors.Join(errs...)
}

// readyCheckTimeout is how long StartAll waits for a single service's
// ReadyCheck to succeed.
const readyCheckTimeout = 30 * time.Second

// stopGracePeriodSec is the wait between SIGTERM and SIGKILL in stopProc.
const stopGracePeriodSec = 10

// waitReady polls a ReadyCheck until it succeeds, the context is cancelled,
// or the timeout elapses.
func waitReady(ctx context.Context, check ReadyCheck, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := check.Check(ctx); err == nil {
			return nil
		} else if time.Now().After(deadline) {
			return fmt.Errorf("ready check %s timed out after %s", check, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
