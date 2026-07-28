package process

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	"github.com/e2b-dev/infra/packages/envd/internal/platform"
	rpc "github.com/e2b-dev/infra/packages/envd/internal/services/spec/process"
)

func (s *Service) SendSignal(
	_ context.Context,
	req *connect.Request[rpc.SendSignalRequest],
) (*connect.Response[rpc.SendSignalResponse], error) {
	handler, err := s.getProcess(req.Msg.GetProcess())
	if err != nil {
		return nil, err
	}

	signal, ok := platform.ProcessSignal(req.Msg.GetSignal())
	if !ok {
		return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("invalid signal: %s", req.Msg.GetSignal()))
	}

	err = handler.SendSignal(signal)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("error sending signal: %w", err))
	}

	return connect.NewResponse(&rpc.SendSignalResponse{}), nil
}
