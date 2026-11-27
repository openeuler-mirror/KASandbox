package server

import (
	"context"

	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
)

// ListCachedBuilds lists the builds in the local template cache; KASandbox's
// cri-multiplex reports them as CRI images.
func (s *Server) ListCachedBuilds(ctx context.Context, _ *emptypb.Empty) (*orchestrator.SandboxListCachedBuildsResponse, error) {
	_, childSpan := tracer.Start(ctx, "list-cached-templates")
	defer childSpan.End()

	var builds []*orchestrator.CachedBuildInfo

	for key, item := range s.templateCache.Items() {
		builds = append(builds, &orchestrator.CachedBuildInfo{
			BuildId:        key,
			ExpirationTime: timestamppb.New(item.ExpiresAt()),
		})
	}

	return &orchestrator.SandboxListCachedBuildsResponse{
		Builds: builds,
	}, nil
}
