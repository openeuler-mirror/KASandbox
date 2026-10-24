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

	"github.com/cri-multiplex/pkg/engine/util"
)

// Reconcile 把 android 后端的运行态与持久态对齐：运行时、路由、state/artifacts 孤儿、
// 进程组孤儿与 netns 孤儿。
func (e *AndroidEngine) Reconcile(ctx context.Context, opts ReconcileOptions) error {
	if e == nil {
		return nil
	}
	var errs []error
	if err := e.reconcileRuntimeState(ctx, opts); err != nil {
		errs = append(errs, err)
	}
	if err := e.reconcileRouteTable(); err != nil {
		errs = append(errs, err)
	}
	if err := e.reconcileOwnedDirectories(opts); err != nil {
		errs = append(errs, err)
	}
	if err := e.reconcileOrphanProcesses(ctx, opts); err != nil {
		errs = append(errs, err)
	}
	if err := e.reconcileOrphanNetNS(opts); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// reconcileRuntimeState 回收"已停止/已删除/进程已消失"的 android sandbox。
func (e *AndroidEngine) reconcileRuntimeState(ctx context.Context, opts ReconcileOptions) error {
	if e.stateStore == nil {
		return nil
	}
	states, err := e.stateStore.LoadAndroidPods()
	if err != nil {
		return err
	}

	var errs []error
	for _, state := range states {
		if !pastGrace(state.UpdatedAt, opts.GracePeriod, e.currentTime()) {
			continue
		}
		missingProcess := (state.State == androidSandboxRunning || state.State == androidSandboxVMStarting ||
			state.State == androidSandboxUnknown) && !androidProcessAlive(state.LaunchPID, state.LaunchPGID)
		if state.State != androidSandboxRemoved && state.State != androidSandboxStopped && !missingProcess {
			continue
		}
		if err := e.CleanupSandbox(ctx, state.SandboxID, "missing-android-process"); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// reconcileRouteTable 只修复 android 自己的路由，绝不触碰其它后端。
func (e *AndroidEngine) reconcileRouteTable() error {
	if e.stateStore == nil {
		return nil
	}
	routes, err := e.stateStore.LoadRoutes()
	if err != nil {
		return err
	}
	pods, err := e.stateStore.LoadAndroidPods()
	if err != nil {
		return err
	}

	wanted := make(map[string]RouteRecord)
	for _, pod := range pods {
		if pod.SandboxID == "" || pod.State == androidSandboxRemoved {
			continue
		}
		wanted["pod\x00"+pod.SandboxID] = RouteRecord{Kind: "pod", ID: pod.SandboxID, Engine: EngineTypeAndroid}
		if pod.ContainerID != "" {
			key := "container\x00" + pod.ContainerID
			wanted[key] = RouteRecord{
				Kind:      "container",
				ID:        pod.ContainerID,
				Engine:    EngineTypeAndroid,
				SandboxID: pod.SandboxID,
			}
		}
	}

	present := make(map[string]bool, len(wanted))
	var errs []error
	for _, route := range routes {
		if route.Engine != EngineTypeAndroid {
			continue
		}
		key := route.Kind + "\x00" + route.ID
		if _, ok := wanted[key]; !ok {
			log.Printf("[AndroidEngine] pruning orphan route kind=%s id=%s", route.Kind, route.ID)
			if err := e.stateStore.DeleteRoute(route.Kind, route.ID); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		present[key] = true
	}
	for key, rec := range wanted {
		if present[key] {
			continue
		}
		if err := e.stateStore.SaveRoute(rec); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// reconcileOwnedDirectories 回收带 cri-multiplex owner 标记、且对应 sandbox 已消失的 workdir 与 artifacts。
func (e *AndroidEngine) reconcileOwnedDirectories(opts ReconcileOptions) error {
	if e.cfg.StateDir == "" {
		return nil
	}
	active := map[string]bool{}
	if e.stateStore != nil {
		states, err := e.stateStore.LoadAndroidPods()
		if err != nil {
			return err
		}
		for _, state := range states {
			active[state.SandboxID] = true
		}
	}

	var errs []error
	entries, err := os.ReadDir(e.cfg.StateDir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(e.cfg.StateDir, entry.Name())
		owner, ownerErr := util.ReadResourceOwner(path)
		if ownerErr != nil {
			if errors.Is(ownerErr, util.ErrOwnershipUnknown) {
				log.Printf("[AndroidEngine] skip unknown owner path=%s", path)
				continue
			}
			errs = append(errs, ownerErr)
			continue
		}
		if owner.Runtime != string(EngineTypeAndroid) || active[owner.SandboxID] || e.isPendingSandbox(owner.SandboxID) {
			continue
		}
		if opts.DryRun {
			log.Printf("[AndroidEngine] dry-run orphan workdir path=%s", path)
			continue
		}
		if err := removeOwnedAndroidPath(path, owner, e.cfg, false, false); err != nil {
			errs = append(errs, err)
		}
	}
	if artifactsErr := e.reconcileOwnedArtifacts(active, opts); artifactsErr != nil {
		errs = append(errs, artifactsErr)
	}
	return errors.Join(errs...)
}

func (e *AndroidEngine) reconcileOwnedArtifacts(active map[string]bool, opts ReconcileOptions) error {
	root := filepath.Clean(e.cfg.ArtifactsDir)
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
		owner, ownerErr := util.ReadResourceOwner(path)
		if ownerErr != nil {
			if errors.Is(ownerErr, util.ErrOwnershipUnknown) {
				log.Printf("[AndroidEngine] skip unknown owner path=%s", path)
				continue
			}
			errs = append(errs, ownerErr)
			continue
		}
		if owner.Runtime != string(EngineTypeAndroid) || active[owner.SandboxID] || e.isPendingSandbox(owner.SandboxID) {
			continue
		}
		if opts.DryRun {
			log.Printf("[AndroidEngine] dry-run orphan artifacts path=%s", path)
			continue
		}
		if err := removeOwnedAndroidPath(path, owner, e.cfg, true, false); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// reconcileOrphanProcesses 回收 environ 里带 CRI_MULTIPLEX_SANDBOX_ID、但 sandbox 已不在内存的进程组。
func (e *AndroidEngine) reconcileOrphanProcesses(ctx context.Context, opts ReconcileOptions) error {
	active := map[string]bool{}
	e.mu.Lock()
	for sandboxID := range e.pods {
		active[sandboxID] = true
	}
	e.mu.Unlock()

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
		if opts.DryRun {
			log.Printf("[AndroidEngine] dry-run orphan process sandbox=%s pid=%d", sandboxID, pid)
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

// reconcileOrphanNetNS 删除没有对应 sandbox 的 android netns，跳过活跃的与 pending 的。
func (e *AndroidEngine) reconcileOrphanNetNS(opts ReconcileOptions) error {
	if !e.cfg.CNI.Enabled {
		return nil
	}
	active := map[string]bool{}
	if e.stateStore != nil {
		pods, err := e.stateStore.LoadAndroidPods()
		if err != nil {
			return err
		}
		for _, pod := range pods {
			if pod.CNIRecord == nil {
				continue
			}
			active[pod.CNIRecord.NetNSName] = true
			active[filepath.Base(pod.CNIRecord.NetNSPath)] = true
		}
	}

	prefix := defaultString(e.cfg.CNI.NetNSPrefix, "android-")
	entries, err := os.ReadDir(e.cfg.CNI.NetNSDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var errs []error
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || active[name] || e.isPendingNetNS(name) {
			continue
		}
		if opts.DryRun {
			log.Printf("[AndroidEngine] dry-run orphan netns name=%s", name)
			continue
		}
		if err := e.hostOps.DeleteNamedNetNS(name); err != nil && !isCleanupNotFound(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (e *AndroidEngine) currentTime() time.Time {
	if e.now == nil {
		return time.Now()
	}
	return e.now()
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
