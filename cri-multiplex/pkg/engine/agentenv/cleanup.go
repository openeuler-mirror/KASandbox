package agentenv

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	runtimeapi "github.com/cri-multiplex/pkg/agentenv/runtime/v1"
)

const cleanupOperationPrefix = "cleanup:"

// CleanupSandbox 按 CRI ID 或产品 ID 清理 AgentENV sandbox；不存在视为成功。
func (e *Engine) CleanupSandbox(ctx context.Context, sandboxID, _ string) error {
	if sandboxID == "" {
		return nil
	}
	ids := e.sandboxOperationIDs(sandboxID)
	unlock, err := e.lockSandbox(ctx, ids...)
	if err != nil {
		return status.FromContextError(err).Err()
	}
	defer unlock()
	// 获取 sandbox 锁后重新读取映射，避免使用加锁前的过期快照。
	mapping, found := e.routes.PodRoute(sandboxID)
	if !found {
		return nil
	}
	record, hasRecord := e.getPod(mapping.CRISandboxID)
	if err := e.ensureClients(); err != nil {
		return e.mapRuntimeError(err)
	}
	if hasRecord {
		return e.rollbackPodCreate(ctx, record)
	}

	_, err = e.lifecycle.Delete(ctx, &runtimeapi.SandboxDeleteRequest{
		OperationId:     cleanupOperationPrefix + mapping.NativeSandboxID,
		NativeSandboxId: mapping.NativeSandboxID,
	})
	if err != nil && status.Code(e.mapRuntimeError(err)) != codes.NotFound {
		return e.mapRuntimeError(err)
	}
	return e.routes.DeleteSandbox(sandboxID)
}
