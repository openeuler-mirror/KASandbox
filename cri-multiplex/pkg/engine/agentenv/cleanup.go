package agentenv

import (
	"context"
	"google.golang.org/grpc/status"
)

// CleanupSandbox 只清理本后端已记录的 Pod，不接管其他 Sandbox。
func (e *Engine) CleanupSandbox(ctx context.Context, sandboxID, _ string) error {
	if sandboxID == "" {
		return nil
	}
	unlock, err := e.lockSandbox(ctx, sandboxID)
	if err != nil {
		return status.FromContextError(err).Err()
	}
	defer unlock()
	record, found := e.getPod(sandboxID)
	if !found {
		return nil
	}
	if err := e.ensureClients(); err != nil {
		return e.mapRuntimeError(err)
	}
	return e.rollbackPodCreate(ctx, record)
}
