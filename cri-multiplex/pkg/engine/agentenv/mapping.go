package agentenv

import (
	"context"
	"fmt"
	"log"
)

// RestoreState 恢复本地 Pod 投影。
func (e *Engine) RestoreState(context.Context) error {
	if e == nil || e.store == nil {
		return nil
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
