package engine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"google.golang.org/protobuf/types/known/emptypb"
)

func (m *CleanupManager) Reconcile(ctx context.Context) error {
	if m == nil || !m.cfg.Enabled {
		return nil
	}
	var errs []error
	if err := m.RetryPending(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := m.reconcileRuntimeState(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := m.reconcileRoutes(); err != nil {
		errs = append(errs, err)
	}
	if err := m.scanOwnedAndroidDirectories(); err != nil {
		errs = append(errs, err)
	}
	if err := m.scanOwnedAndroidProcesses(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := m.scanOrphanNetNS(); err != nil {
		errs = append(errs, err)
	}
	if err := m.scanHostPortRules(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (m *CleanupManager) reconcileRuntimeState(ctx context.Context) error {
	var errs []error
	if m.e2b != nil && m.stateStore != nil {
		states, err := m.stateStore.LoadE2BPods()
		if err != nil {
			errs = append(errs, err)
		} else if len(states) > 0 {
			active, listErr := m.activeE2BSandboxes(ctx)
			if listErr != nil {
				log.Printf("[Cleanup] e2b reconcile deferred: orchestrator list failed: %v", listErr)
			} else {
				for _, state := range states {
					if !m.pastGrace(state.UpdatedAt) {
						continue
					}
					if state.State == stateRemoved || state.State == stateStopped {
						if result := m.CleanupE2BSandbox(ctx, state.SandboxID, "non-active-state"); result.Err != nil {
							errs = append(errs, result.Err)
						}
						continue
					}
					if state.State != stateRunning || active[state.E2BSandboxID] || active[state.SandboxID] {
						continue
					}
					if result := m.CleanupE2BSandbox(ctx, state.SandboxID, "missing-orchestrator-sandbox"); result.Err != nil {
						errs = append(errs, result.Err)
					}
				}
			}
		}
	}

	if m.android != nil && m.stateStore != nil {
		states, err := m.stateStore.LoadAndroidPods()
		if err != nil {
			errs = append(errs, err)
		} else {
			for _, state := range states {
				missingProcess := (state.State == androidSandboxRunning || state.State == androidSandboxVMStarting || state.State == androidSandboxUnknown) &&
					!androidProcessAlive(state.LaunchPID, state.LaunchPGID)
				if state.State == androidSandboxRemoved || state.State == androidSandboxStopped || missingProcess {
					if result := m.CleanupAndroidSandbox(ctx, state.SandboxID, "missing-android-process"); result.Err != nil {
						errs = append(errs, result.Err)
					}
				}
			}
		}
	}
	return errors.Join(errs...)
}

func (m *CleanupManager) activeE2BSandboxes(ctx context.Context) (map[string]bool, error) {
	active := map[string]bool{}
	if err := m.e2b.ensureConn(); err != nil {
		return active, err
	}
	resp, err := m.e2b.client.List(ctx, &emptypb.Empty{})
	if err != nil {
		return active, err
	}
	for id := range activeSandboxIDs(resp.Sandboxes) {
		active[id] = true
	}
	return active, nil
}

func (m *CleanupManager) pastGrace(updatedAt time.Time) bool {
	return updatedAt.IsZero() || !updatedAt.Add(m.cfg.GracePeriod).After(m.now())
}

func (m *CleanupManager) reconcileRoutes() error {
	if m.stateStore == nil {
		return nil
	}
	routes, err := m.stateStore.LoadRoutes()
	if err != nil {
		return err
	}
	e2bPods, err := m.stateStore.LoadE2BPods()
	if err != nil {
		return err
	}
	androidPods, err := m.stateStore.LoadAndroidPods()
	if err != nil {
		return err
	}
	valid := make(map[string]EngineType)
	for _, pod := range e2bPods {
		if pod.SandboxID == "" || pod.State == stateRemoved {
			continue
		}
		valid["pod\x00"+pod.SandboxID] = EngineTypeE2B
		if pod.ContainerName != "" {
			valid["container\x00"+pod.SandboxID+"-c"] = EngineTypeE2B
		}
	}
	for _, pod := range androidPods {
		if pod.SandboxID == "" || pod.State == androidSandboxRemoved {
			continue
		}
		valid["pod\x00"+pod.SandboxID] = EngineTypeAndroid
		if pod.ContainerID != "" {
			valid["container\x00"+pod.ContainerID] = EngineTypeAndroid
		}
	}

	present := make(map[string]bool)
	var errs []error
	for _, route := range routes {
		key := route.Kind + "\x00" + route.ID
		if route.Engine == EngineTypeContainer {
			present[key] = true
			continue
		}
		if expected, ok := valid[key]; !ok || expected != route.Engine {
			log.Printf("[Cleanup] pruning orphan route kind=%s id=%s engine=%s", route.Kind, route.ID, route.Engine)
			if err := m.stateStore.DeleteRoute(route.Kind, route.ID); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		present[key] = true
	}
	for key, runtimeType := range valid {
		if present[key] {
			continue
		}
		parts := strings.SplitN(key, "\x00", 2)
		if err := m.stateStore.SaveRoute(RouteRecord{Kind: parts[0], ID: parts[1], Engine: runtimeType}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *CleanupManager) scanOwnedAndroidDirectories() error {
	if m.android == nil || m.android.cfg.StateDir == "" {
		return nil
	}
	entries, err := os.ReadDir(m.android.cfg.StateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	active := map[string]bool{}
	if m.stateStore != nil {
		states, loadErr := m.stateStore.LoadAndroidPods()
		if loadErr != nil {
			return loadErr
		}
		for _, state := range states {
			active[state.SandboxID] = true
		}
	}
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(m.android.cfg.StateDir, entry.Name())
		owner, ownerErr := readResourceOwner(path)
		if ownerErr != nil {
			if errors.Is(ownerErr, ErrOwnershipUnknown) {
				log.Printf("[Cleanup] skip unknown owner path=%s", path)
				continue
			}
			errs = append(errs, ownerErr)
			continue
		}
		if owner.Runtime != EngineTypeAndroid || active[owner.SandboxID] || m.android.isPendingSandbox(owner.SandboxID) {
			continue
		}
		if m.cfg.DryRun {
			log.Printf("[Cleanup] dry-run orphan android workdir path=%s", path)
			continue
		}
		if err := removeOwnedAndroidPath(path, owner, m.android.cfg, false, false); err != nil {
			errs = append(errs, err)
		}
	}
	if artifactsErr := m.scanOwnedAndroidArtifacts(active); artifactsErr != nil {
		errs = append(errs, artifactsErr)
	}
	return errors.Join(errs...)
}

func (m *CleanupManager) scanOwnedAndroidArtifacts(active map[string]bool) error {
	root := filepath.Clean(m.android.cfg.ArtifactsDir)
	parent := filepath.Dir(root)
	prefix := filepath.Base(root) + "-"
	entries, err := os.ReadDir(parent)
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		path := filepath.Join(parent, entry.Name())
		owner, ownerErr := readResourceOwner(path)
		if ownerErr != nil {
			if errors.Is(ownerErr, ErrOwnershipUnknown) {
				log.Printf("[Cleanup] skip unknown owner path=%s", path)
				continue
			}
			errs = append(errs, ownerErr)
			continue
		}
		if owner.Runtime != EngineTypeAndroid || active[owner.SandboxID] || m.android.isPendingSandbox(owner.SandboxID) {
			continue
		}
		if m.cfg.DryRun {
			log.Printf("[Cleanup] dry-run orphan android artifacts path=%s", path)
			continue
		}
		if err := removeOwnedAndroidPath(path, owner, m.android.cfg, true, false); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *CleanupManager) scanOwnedAndroidProcesses(ctx context.Context) error {
	if m.android == nil {
		return nil
	}
	active := map[string]bool{}
	m.android.mu.Lock()
	for sandboxID := range m.android.pods {
		active[sandboxID] = true
	}
	m.android.mu.Unlock()
	procEntries, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range procEntries {
		if !entry.IsDir() {
			continue
		}
		environ, readErr := os.ReadFile(filepath.Join("/proc", entry.Name(), "environ"))
		if readErr != nil {
			continue
		}
		sandboxID := ownerEnvValue(environ, "CRI_MULTIPLEX_SANDBOX_ID")
		if sandboxID == "" || active[sandboxID] {
			continue
		}
		var pid int
		if _, scanErr := fmt.Sscanf(entry.Name(), "%d", &pid); scanErr != nil || pid <= 0 {
			continue
		}
		if m.cfg.DryRun {
			log.Printf("[Cleanup] dry-run orphan android process sandbox=%s pid=%d", sandboxID, pid)
			continue
		}
		pgid, pgErr := syscall.Getpgid(pid)
		if pgErr != nil && !errors.Is(pgErr, syscall.ESRCH) {
			errs = append(errs, pgErr)
			continue
		}
		if pgid > 0 {
			if killErr := syscall.Kill(-pgid, syscall.SIGTERM); killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
				errs = append(errs, killErr)
			}
		} else if killErr := syscall.Kill(pid, syscall.SIGTERM); killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
			errs = append(errs, killErr)
		}
		select {
		case <-ctx.Done():
			return errors.Join(append(errs, ctx.Err())...)
		default:
		}
	}
	return errors.Join(errs...)
}

func ownerEnvValue(environ []byte, key string) string {
	prefix := key + "="
	for _, item := range strings.Split(string(environ), "\x00") {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimPrefix(item, prefix)
		}
	}
	return ""
}

func (m *CleanupManager) scanOrphanNetNS() error {
	active := map[string]bool{}
	if m.stateStore != nil {
		e2bPods, err := m.stateStore.LoadE2BPods()
		if err != nil {
			return err
		}
		for _, pod := range e2bPods {
			if pod.CNIRecord != nil {
				active[pod.CNIRecord.NetNSName] = true
				active[filepath.Base(pod.CNIRecord.NetNSPath)] = true
			}
		}
		androidPods, err := m.stateStore.LoadAndroidPods()
		if err != nil {
			return err
		}
		for _, pod := range androidPods {
			if pod.CNIRecord != nil {
				active[pod.CNIRecord.NetNSName] = true
				active[filepath.Base(pod.CNIRecord.NetNSPath)] = true
			}
		}
	}
	type scanRoot struct{ dir, prefix string }
	roots := map[scanRoot]bool{}
	if m.e2b != nil && m.e2b.cniConfig.Enabled {
		roots[scanRoot{m.e2b.cniConfig.NetNSDir, defaultString(m.e2b.cniConfig.NetNSPrefix, "e2b-")}] = true
	}
	if m.android != nil && m.android.cfg.CNI.Enabled {
		roots[scanRoot{m.android.cfg.CNI.NetNSDir, defaultString(m.android.cfg.CNI.NetNSPrefix, "android-")}] = true
	}
	var errs []error
	for root := range roots {
		entries, err := os.ReadDir(root.dir)
		if err != nil {
			if !os.IsNotExist(err) {
				errs = append(errs, err)
			}
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasPrefix(name, root.prefix) || active[name] ||
				(m.e2b != nil && m.e2b.isPendingNetNS(name)) ||
				(m.android != nil && m.android.isPendingNetNS(name)) {
				continue
			}
			if m.cfg.DryRun {
				log.Printf("[Cleanup] dry-run orphan netns name=%s", name)
				continue
			}
			if err := m.hostOps.deleteNamedNetNS(name); err != nil && !isCleanupNotFound(err) {
				errs = append(errs, fmt.Errorf("delete netns %s: %w", name, err))
			}
		}
	}
	return errors.Join(errs...)
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func (m *CleanupManager) scanHostPortRules(ctx context.Context) error {
	active := map[string]bool{}
	if m.stateStore != nil {
		pods, err := m.stateStore.LoadE2BPods()
		if err != nil {
			return err
		}
		for _, pod := range pods {
			if pod.HostIP == "" {
				continue
			}
			nodeIP := ""
			if m.e2b != nil {
				nodeIP = m.e2b.nodeIP
			}
			for _, mapping := range pod.PortMappings {
				active[hostPortRuleComment(nodeIP, mapping.HostPort, pod.HostIP, mapping.SandboxPort)] = true
			}
			if len(pod.PortMappings) == 0 && pod.HostPort > 0 {
				active[hostPortRuleComment(nodeIP, pod.HostPort, pod.HostIP, envdSandboxPort)] = true
			}
		}
	}
	var errs []error
	if m.hostOps.listIPTables != nil {
		data, err := m.hostOps.listIPTables(ctx)
		if err == nil {
			for _, rule := range parseIPTablesOwnerRules(data) {
				if active[rule.Comment] {
					continue
				}
				if m.cfg.DryRun {
					log.Printf("[Cleanup] dry-run orphan iptables rule comment=%s", rule.Comment)
					continue
				}
				if err := m.hostOps.deleteIPTables(ctx, rule); err != nil && !isCleanupNotFound(err) {
					errs = append(errs, err)
				}
			}
		} else if !isCommandUnavailable(err) {
			errs = append(errs, err)
		}
	}
	if m.hostOps.listNFTables != nil {
		data, err := m.hostOps.listNFTables(ctx)
		if err == nil {
			for _, rule := range parseNFTablesOwnerRules(data) {
				if active[rule.Comment] {
					continue
				}
				if m.cfg.DryRun {
					log.Printf("[Cleanup] dry-run orphan nftables rule comment=%s", rule.Comment)
					continue
				}
				if err := m.hostOps.deleteNFTables(ctx, rule); err != nil && !isCleanupNotFound(err) {
					errs = append(errs, err)
				}
			}
		} else if !isCommandUnavailable(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func isCommandUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return errors.Is(err, os.ErrNotExist) || strings.Contains(msg, "executable file not found") ||
		strings.Contains(msg, "operation not permitted") || strings.Contains(msg, "permission denied")
}
