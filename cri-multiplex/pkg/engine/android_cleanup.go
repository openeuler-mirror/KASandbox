package engine

import (
	"context"
	"errors"
	"os"
	"strings"
)

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
