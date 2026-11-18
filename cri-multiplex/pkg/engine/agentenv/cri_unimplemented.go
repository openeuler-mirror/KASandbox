package agentenv

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func unimplemented(method string) error {
	return status.Errorf(codes.Unimplemented, "AgentENV CRI method %s is not implemented", method)
}

func (e *Engine) GetContainerEvents(context.Context, *runtime.GetEventsRequest, ...grpc.CallOption) (runtime.RuntimeService_GetContainerEventsClient, error) {
	return nil, unimplemented("GetContainerEvents")
}
