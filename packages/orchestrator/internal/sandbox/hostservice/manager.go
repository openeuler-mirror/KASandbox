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

// Manager orchestrates a set of host-side service processes. A manager may
// own either host-wide shared services or services scoped to one sandbox.
//
// The Manager starts services in slice order (callers should arrange the slice
// in dependency order — deps first). Stop is performed in reverse order so
// that dependents are torn down before their dependencies.
//
// nil-safe: a nil *Manager behaves as a no-op for every lifecycle method,
// which keeps Linux/Windows sandbox paths branch-free.
type Manager struct {
	mu                sync.Mutex
	services          []Service
	readyCheckTimeout time.Duration
	entries           []*procEntry
	cancel            context.CancelFunc
	monitorWG         sync.WaitGroup
	stopping          bool
	stopDone          chan struct{}
	stopErr           error
}

// NewManager builds a Manager over the given services. The slice is consumed
// in order; callers control the startup order by the position in the slice.
// Returns nil when services is empty so callers can short-circuit downstream
// nil-checks.
func NewManager(services []Service, readyCheckTimeout time.Duration) *Manager {
	if len(services) == 0 {
		return nil
	}
	return &Manager{
		services:          services,
		readyCheckTimeout: readyCheckTimeout,
	}
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
	if m.stopping {
		return fmt.Errorf("host service manager is stopping")
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
			if err := waitReady(ctx, svc.ReadyCheck, m.readyCheckTimeout, entry); err != nil {
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
		m.monitorWG.Add(1)
		go func() {
			defer m.monitorWG.Done()
			monitorAndRestart(svcCtx, entry, func(ctx context.Context, old *procEntry) (*procEntry, error) {
				m.mu.Lock()
				defer m.mu.Unlock()

				if m.stopping {
					return nil, context.Canceled
				}
				if err := ctx.Err(); err != nil {
					return nil, err
				}

				entryIndex := -1
				for i, candidate := range m.entries {
					if candidate == old {
						entryIndex = i
						break
					}
				}
				if entryIndex < 0 {
					return nil, fmt.Errorf("host service %s entry is no longer managed", old.service.Name)
				}

				newEntry, err := startService(ctx, old.service)
				if err != nil {
					return nil, err
				}
				m.entries[entryIndex] = newEntry
				return newEntry, nil
			})
		}()
	}

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
	if m.stopping {
		done := m.stopDone
		m.mu.Unlock()
		if done != nil {
			<-done
		}
		m.mu.Lock()
		err := m.stopErr
		m.mu.Unlock()
		return err
	}
	if len(m.entries) == 0 {
		m.mu.Unlock()
		return nil
	}

	m.stopping = true
	m.stopDone = make(chan struct{})
	done := m.stopDone
	cancel := m.cancel
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	m.monitorWG.Wait()

	m.mu.Lock()
	entries := m.entries
	m.entries = nil
	m.cancel = nil
	m.mu.Unlock()

	var errs []error
	for i := len(entries) - 1; i >= 0; i-- {
		if err := stopProc(ctx, entries[i], stopGracePeriodSec); err != nil {
			errs = append(errs, fmt.Errorf("stop host service %s: %w", entries[i].service.Name, err))
		}
	}
	m.closeServiceFiles()
	stopErr := errors.Join(errs...)

	m.mu.Lock()
	m.stopErr = stopErr
	close(done)
	m.mu.Unlock()

	return stopErr
}

// closeServiceFiles releases the parent-owned listener/socket descriptors.
// Children inherit their own descriptors at exec, while the parent copies are
// deliberately retained until StopAll so RestartOnCrash can reuse them.
func (m *Manager) closeServiceFiles() {
	for i := range m.services {
		m.services[i].CloseParentResources()
	}
}

// stopGracePeriodSec is the wait between SIGTERM and SIGKILL in stopProc.
const stopGracePeriodSec = 10

// waitReady polls a ReadyCheck until it succeeds, the context is cancelled,
// or the timeout elapses.
func waitReady(ctx context.Context, check ReadyCheck, timeout time.Duration, entry *procEntry) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if err := check.Check(ctx); err == nil {
			select {
			case <-entry.done:
				if exitErr := entry.getWaitErr(); exitErr != nil {
					return fmt.Errorf("process exited while waiting for readiness: %w", exitErr)
				}
				return fmt.Errorf("process exited while waiting for readiness")
			default:
			}
			return nil
		} else {
			lastErr = err
			if time.Now().After(deadline) {
				return fmt.Errorf("ready check %s timed out after %s: %w", check, timeout, lastErr)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-entry.done:
			if exitErr := entry.getWaitErr(); exitErr != nil {
				return fmt.Errorf("process exited while waiting for readiness: %w", exitErr)
			}
			return fmt.Errorf("process exited while waiting for readiness")
		case <-time.After(200 * time.Millisecond):
		}
	}
}
