package nodemanager

import (
	"context"

	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
)

func (n *Node) SandboxCreate(ctx context.Context, sbxRequest *orchestrator.SandboxCreateRequest) (*orchestrator.SandboxCreateResponse, error) {
	client, ctx := n.GetSandboxCreateCtx(ctx, sbxRequest)
	res, err := client.Sandbox.Create(ctx, sbxRequest)
	if err != nil {
		return nil, err
	}

	return res, nil
}
