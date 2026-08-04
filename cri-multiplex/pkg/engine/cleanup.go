package engine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/cri-multiplex/pkg/orchestrator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	cleanupResourceE2BSandbox     = "e2b-sandbox"
	cleanupResourceAndroidSandbox = "android-sandbox"
)

type CleanupConfig struct {
	Enabled     bool
	Interval    time.Duration
	GracePeriod time.Duration
	MaxRetries  int
	DryRun      bool
}

type CleanupResult struct {
	Succeeded bool
	Err       error
}

type CleanupManager struct {
	stateStore StateStore
	e2b        *grpcE2BEngine
	android    *AndroidEngine
	cfg        CleanupConfig
	now        func() time.Time
	hostOps    hostResourceOps

	cleanupE2BFunc     func(context.Context, string) error
	cleanupAndroidFunc func(context.Context, string) error

	cancel context.CancelFunc
}

func NewCleanupManager(store StateStore, e2b E2BEngine, android *AndroidEngine, cfg CleanupConfig) *CleanupManager {
	if cfg.Interval <= 0 {
		cfg.Interval = 60 * time.Second
	}
	if cfg.GracePeriod < 0 {
		cfg.GracePeriod = 0
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 10
	}
	m := &CleanupManager{stateStore: store, android: android, cfg: cfg, now: time.Now, hostOps: defaultHostResourceOps()}
	if concrete, ok := e2b.(*grpcE2BEngine); ok {
		m.e2b = concrete
		concrete.cleanupManager = m
	}
	if android != nil {
		android.cleanupManager = m
	}
	m.cleanupE2BFunc = m.cleanupE2BSandbox
	m.cleanupAndroidFunc = m.cleanupAndroidSandbox
	return m
}

func (m *CleanupManager) CleanupE2BSandbox(ctx context.Context, sandboxID, reason string) CleanupResult {
	return m.runSandboxCleanup(ctx, EngineTypeE2B, sandboxID, cleanupResourceE2BSandbox, reason, m.cleanupE2BFunc)
}

func (m *CleanupManager) CleanupAndroidSandbox(ctx context.Context, sandboxID, reason string) CleanupResult {
	return m.runSandboxCleanup(ctx, EngineTypeAndroid, sandboxID, cleanupResourceAndroidSandbox, reason, m.cleanupAndroidFunc)
}

func (m *CleanupManager) CleanupRoute(_ context.Context, kind, id string) CleanupResult {
	if m == nil || m.stateStore == nil || id == "" {
		return CleanupResult{Succeeded: true}
	}
	if err := m.stateStore.DeleteRoute(kind, id); err != nil {
		return CleanupResult{Err: err}
	}
	return CleanupResult{Succeeded: true}
}

func (m *CleanupManager) runSandboxCleanup(
	ctx context.Context,
	runtimeType EngineType,
	sandboxID, resourceType, reason string,
	cleanup func(context.Context, string) error,
) CleanupResult {
	if m == nil || sandboxID == "" || cleanup == nil {
		return CleanupResult{Succeeded: true}
	}
	log.Printf("[Cleanup] start runtime=%s sandbox=%s reason=%s", runtimeType, sandboxID, reason)
	if m.cfg.DryRun {
		log.Printf("[Cleanup] dry-run runtime=%s sandbox=%s reason=%s", runtimeType, sandboxID, reason)
		return CleanupResult{Succeeded: true}
	}
	err := cleanup(ctx, sandboxID)
	if isCleanupNotFound(err) {
		err = nil
	}
	taskID := cleanupTaskID(runtimeType, sandboxID, resourceType)
	if err == nil {
		if m.stateStore != nil {
			if deleteErr := m.stateStore.DeleteCleanupTask(taskID); deleteErr != nil {
				return CleanupResult{Err: deleteErr}
			}
		}
		log.Printf("[Cleanup] done runtime=%s sandbox=%s", runtimeType, sandboxID)
		return CleanupResult{Succeeded: true}
	}
	if saveErr := m.saveRetryTask(CleanupTask{
		ID:           taskID,
		Runtime:      runtimeType,
		SandboxID:    sandboxID,
		ResourceType: resourceType,
		ResourceID:   sandboxID,
		Reason:       reason,
	}, err); saveErr != nil {
		err = errors.Join(err, fmt.Errorf("persist cleanup task: %w", saveErr))
	}
	log.Printf("[Cleanup] retry runtime=%s sandbox=%s resource=%s err=%v", runtimeType, sandboxID, resourceType, err)
	return CleanupResult{Err: err}
}

func cleanupTaskID(runtimeType EngineType, sandboxID, resourceType string) string {
	return fmt.Sprintf("%s:%s:%s", runtimeType, resourceType, sandboxID)
}

func (m *CleanupManager) saveRetryTask(task CleanupTask, cleanupErr error) error {
	if m == nil || m.stateStore == nil {
		return nil
	}
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
		var result CleanupResult
		switch task.Runtime {
		case EngineTypeE2B:
			result = m.CleanupE2BSandbox(ctx, task.SandboxID, "retry")
		case EngineTypeAndroid:
			result = m.CleanupAndroidSandbox(ctx, task.SandboxID, "retry")
		default:
			errs = append(errs, fmt.Errorf("unknown cleanup task runtime %q", task.Runtime))
			continue
		}
		if result.Err != nil {
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

func (m *CleanupManager) cleanupE2BSandbox(ctx context.Context, sandboxID string) error {
	if m.e2b == nil {
		return nil
	}
	return m.e2b.cleanupSandboxResources(ctx, sandboxID)
}

func (m *CleanupManager) cleanupAndroidSandbox(ctx context.Context, sandboxID string) error {
	if m.android == nil {
		return nil
	}
	return m.android.cleanupSandboxResources(ctx, sandboxID)
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

func (e *grpcE2BEngine) cleanupSandboxResources(ctx context.Context, sandboxID string) error {
	pod, ok := e.tracker.Get(sandboxID)
	if !ok && e.stateStore != nil {
		states, err := e.stateStore.LoadE2BPods()
		if err != nil {
			return err
		}
		for _, state := range states {
			if state.SandboxID == sandboxID {
				pod = podInfoFromPersistedState(state)
				ok = true
				break
			}
		}
	}
	if !ok {
		return e.deleteE2BStateAndRoutes(sandboxID, sandboxID+"-c")
	}

	var errs []error
	if pod.hostIP != "" {
		mappings := append([]PortMapping(nil), pod.portMappings...)
		if len(mappings) == 0 && pod.hostPort > 0 {
			mappings = []PortMapping{{HostPort: pod.hostPort, SandboxPort: envdSandboxPort}}
		}
		for _, mapping := range mappings {
			collectCleanupError(&errs, "hostport", e.hostPortOps.cleanup(e.nodeIP, mapping.HostPort, pod.hostIP, mapping.SandboxPort))
		}
	}
	if err := e.ensureConn(); err != nil {
		collectCleanupError(&errs, "orchestrator connection", err)
	} else {
		_, err := e.client.Delete(ctx, &orchestrator.SandboxDeleteRequest{SandboxId: pod.envdSandboxID()})
		collectCleanupError(&errs, "orchestrator delete", err)
	}
	if pod.cniRecord != nil && e.cniManager != nil {
		collectCleanupError(&errs, "cni del", e.cniManager.Del(ctx, pod.cniRecord, pod.toPodSandboxConfig()))
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	ports := make([]int, 0, len(pod.portMappings))
	for _, mapping := range pod.portMappings {
		ports = append(ports, mapping.SandboxPort)
	}
	if len(ports) == 0 && pod.hostPort > 0 {
		ports = append(ports, envdSandboxPort)
	}
	e.hostPortManager.ReleasePorts(sandboxID, ports)
	containerID := sandboxID + "-c"
	if pod.containerName == "" {
		containerID = ""
	}
	if err := e.deleteE2BStateAndRoutes(sandboxID, containerID); err != nil {
		return err
	}
	e.tracker.Delete(sandboxID)
	return nil
}

func (e *grpcE2BEngine) deleteE2BStateAndRoutes(sandboxID, containerID string) error {
	if e.stateStore == nil {
		e.tracker.Delete(sandboxID)
		return nil
	}
	var errs []error
	collectCleanupError(&errs, "pod route", e.stateStore.DeleteRoute("pod", sandboxID))
	if containerID != "" {
		collectCleanupError(&errs, "container route", e.stateStore.DeleteRoute("container", containerID))
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return e.stateStore.DeleteE2BPod(sandboxID)
}

func (e *AndroidEngine) cleanupSandboxResources(ctx context.Context, sandboxID string) error {
	e.mu.Lock()
	rec := e.pods[sandboxID]
	container := e.firstContainerLocked(sandboxID)
	e.mu.Unlock()
	if rec == nil && e.stateStore != nil {
		states, err := e.stateStore.LoadAndroidPods()
		if err != nil {
			return err
		}
		for _, state := range states {
			if state.SandboxID == sandboxID {
				rec = androidSandboxRecordFromState(state)
				container = androidContainerRecordFromState(state)
				break
			}
		}
	}
	if rec == nil {
		return e.deleteAndroidStateAndRoutes(sandboxID, "")
	}

	e.mu.Lock()
	e.expectedStops[sandboxID] = true
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.expectedStops, sandboxID)
		e.mu.Unlock()
	}()

	var errs []error
	if e.ops.cleanupGuestNetwork != nil {
		e.ops.cleanupGuestNetwork(e, ctx, rec)
	}
	if e.ops.stopCVD != nil {
		collectCleanupError(&errs, "android process group", e.ops.stopCVD(e, ctx, rec))
	}
	if rec.CNIRecord != nil && e.cniManager != nil {
		collectCleanupError(&errs, "cni del", e.cniManager.Del(ctx, rec.CNIRecord, rec.toPodSandboxConfig()))
	}
	owner := androidResourceOwner(rec)
	collectCleanupError(&errs, "android workdir", removeOwnedAndroidPath(rec.WorkDir, owner, e.cfg, false, true))
	if rec.ArtifactsDir != "" && filepathClean(rec.ArtifactsDir) != filepathClean(e.cfg.ArtifactsDir) {
		collectCleanupError(&errs, "android artifacts", removeOwnedAndroidPath(rec.ArtifactsDir, owner, e.cfg, true, true))
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	containerID := ""
	if container != nil {
		containerID = container.ContainerID
	}
	if err := e.deleteAndroidStateAndRoutes(sandboxID, containerID); err != nil {
		return err
	}
	e.mu.Lock()
	delete(e.portOwners, rec.ADBPort)
	delete(e.portOwners, rec.WebRTCPort)
	delete(e.instanceOwners, rec.BaseInstanceNum)
	for id, item := range e.containers {
		if item.CRISandboxID == sandboxID {
			delete(e.containers, id)
		}
	}
	delete(e.pods, sandboxID)
	e.mu.Unlock()
	return nil
}

func (e *AndroidEngine) deleteAndroidStateAndRoutes(sandboxID, containerID string) error {
	if e.stateStore == nil {
		return nil
	}
	var errs []error
	collectCleanupError(&errs, "pod route", e.stateStore.DeleteRoute("pod", sandboxID))
	if containerID != "" {
		collectCleanupError(&errs, "container route", e.stateStore.DeleteRoute("container", containerID))
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return e.stateStore.DeleteAndroidPod(sandboxID)
}

func filepathClean(path string) string {
	if path == "" {
		return ""
	}
	return strings.TrimRight(path, string(os.PathSeparator))
}
