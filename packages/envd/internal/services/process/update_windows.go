//go:build windows

package process

import (
	"context"

	"connectrpc.com/connect"

	rpc "github.com/e2b-dev/infra/packages/envd/internal/services/spec/process"
)

func (s *Service) Update(_ context.Context, _ *connect.Request[rpc.UpdateRequest]) (*connect.Response[rpc.UpdateResponse], error) {
	// TODO: Implement Windows PTY resize support.
	return connect.NewResponse(&rpc.UpdateResponse{}), nil
}
