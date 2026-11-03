package e2b

import (
	"context"
	"errors"

	"github.com/cri-multiplex/pkg/engine/util"
	"github.com/cri-multiplex/pkg/orchestrator"
)

func (e *Engine) cleanupSandboxResources(ctx context.Context, sandboxID string) error {
	pod, ok := e.tracker.Get(sandboxID)
	if !ok && e.stateStore != nil {
		states, err := e.loadPods()
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
		mappings := append([]util.PortMapping(nil), pod.portMappings...)
		if len(mappings) == 0 && pod.hostPort > 0 {
			mappings = []util.PortMapping{{HostPort: pod.hostPort, SandboxPort: envdSandboxPort}}
		}
		for _, mapping := range mappings {
			collectCleanupError(&errs, "hostport", e.hostPortOps.Cleanup(e.nodeIP, mapping.HostPort, pod.hostIP, mapping.SandboxPort))
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

func (e *Engine) deleteE2BStateAndRoutes(sandboxID, containerID string) error {
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
	return e.deletePod(sandboxID)
}
