package agentenv

import (
	"context"
	"fmt"
	"log"

	"github.com/cri-multiplex/pkg/engine"
)

// BindCRISandbox 将 CRI PodSandbox ID 绑定到已有 product/native 映射，
// 返回 native UUID 供 CRI 后续操作使用。
func (e *Engine) BindCRISandbox(productSandboxID, criSandboxID string) (string, error) {
	if productSandboxID == "" || criSandboxID == "" {
		return "", fmt.Errorf("agentenv mapping: product and CRI sandbox ids are required")
	}
	unlock, err := e.lockSandbox(context.Background(), productSandboxID, criSandboxID)
	if err != nil {
		return "", fmt.Errorf("agentenv mapping: lock sandbox: %w", err)
	}
	defer unlock()
	route, found, err := e.routes.BindPodRoute(productSandboxID, criSandboxID, engine.EngineTypeAgentENV)
	if err != nil {
		return "", fmt.Errorf("agentenv mapping: bind CRI sandbox: %w", err)
	}
	if !found {
		return "", fmt.Errorf("agentenv mapping: product %s has no native mapping", productSandboxID)
	}
	return route.NativeSandboxID, nil
}

// RestoreState 恢复 RouteTable 与本地 Pod 投影。
func (e *Engine) RestoreState(context.Context) error {
	if e == nil || e.store == nil {
		return nil
	}
	if err := e.routes.Restore(); err != nil {
		return fmt.Errorf("agentenv mapping: restore route table: %w", err)
	}
	if err := e.restorePods(); err != nil {
		return err
	}
	for _, record := range e.listPods() {
		if record.State != criStateReady {
			continue
		}
		if err := e.ensurePodNetwork(record); err != nil {
			record.State = criStateNotReady
			record.LastError = err.Error()
			if persistErr := e.putPod(record); persistErr != nil {
				return fmt.Errorf("agentenv mapping: persist unavailable pod %s network: %w",
					record.CRISandboxID, persistErr)
			}
			log.Printf("[AgentENV] pod %s network unavailable during restore: %v", record.CRISandboxID, err)
		}
	}
	return nil
}
