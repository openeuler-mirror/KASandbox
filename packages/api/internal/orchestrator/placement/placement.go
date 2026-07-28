package placement

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/e2b-dev/infra/packages/api/internal/orchestrator/nodemanager"
	"github.com/e2b-dev/infra/packages/api/internal/utils"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/machineinfo"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
)

var tracer = otel.Tracer("github.com/e2b-dev/infra/packages/api/internal/orchestrator/placement")

var errSandboxCreateFailed = fmt.Errorf("failed to create a new sandbox, if the problem persists, contact us")

// Algorithm defines the interface for sandbox placement strategies.
// Implementations should choose an optimal node based on available resources
// and current load distribution.
type Algorithm interface {
	chooseNode(ctx context.Context, nodes []*nodemanager.Node, nodesExcluded map[string]struct{}, requested nodemanager.SandboxResources, buildMachineInfo machineinfo.MachineInfo) (*nodemanager.Node, error)
}

// logSandboxCreateRequest 分层打印 SandboxCreateRequest 的所有字段
func logSandboxCreateRequest(ctx context.Context, req *orchestrator.SandboxCreateRequest) {
	if req == nil {
		logger.L().Info(ctx, "Sandbox create request is nil")
		return
	}

	sbx := req.GetSandbox()
	if sbx == nil {
		logger.L().Info(ctx, "Sandbox create request has nil sandbox")
		return
	}

	fields := []zap.Field{
		logger.WithSandboxID(sbx.GetSandboxId()),
		zap.String("base_template_id", sbx.GetBaseTemplateId()),
		zap.String("template_id", sbx.GetTemplateId()),
		zap.String("team_id", sbx.GetTeamId()),
		zap.String("build_id", sbx.GetBuildId()),
		zap.String("execution_id", sbx.GetExecutionId()),
		zap.String("kernel_version", sbx.GetKernelVersion()),
		zap.String("firecracker_version", sbx.GetFirecrackerVersion()),
		zap.String("envd_version", sbx.GetEnvdVersion()),
		zap.String("envd_access_token", sbx.GetEnvdAccessToken()),
		zap.Int64("vcpu", sbx.GetVcpu()),
		zap.Int64("ram_mb", sbx.GetRamMb()),
		zap.Int64("max_sandbox_length_hours", sbx.GetMaxSandboxLength()),
		zap.Int64("total_disk_size_mb", sbx.GetTotalDiskSizeMb()),
		zap.Bool("huge_pages", sbx.GetHugePages()),
		zap.Bool("snapshot", sbx.GetSnapshot()),
		zap.Bool("auto_pause", sbx.GetAutoPause()),
		zap.Bool("allow_internet_access", sbx.GetAllowInternetAccess()),
		zap.Any("metadata", sbx.GetMetadata()),
		zap.Any("env_vars", sbx.GetEnvVars()),
	}

	if req.StartTime != nil {
		fields = append(fields, zap.Time("start_time", req.StartTime.AsTime()))
	}
	if req.EndTime != nil {
		fields = append(fields, zap.Time("end_time", req.EndTime.AsTime()))
	}

	// Alias
	if sbx.Alias != nil {
		fields = append(fields, zap.Stringp("alias", sbx.Alias))
	} else {
		fields = append(fields, zap.String("alias", "<nil>"))
	}

	// Network
	net := sbx.GetNetwork()
	if net != nil {
		netMap := map[string]interface{}{}
		if egress := net.GetEgress(); egress != nil {
			netMap["egress"] = map[string]interface{}{
				"allowed_cidrs":   egress.GetAllowedCidrs(),
				"denied_cidrs":    egress.GetDeniedCidrs(),
				"allowed_domains": egress.GetAllowedDomains(),
			}
		} else {
			netMap["egress"] = nil
		}
		if ingress := net.GetIngress(); ingress != nil {
			ingressMap := map[string]interface{}{}
			if ingress.TrafficAccessToken != nil {
				ingressMap["traffic_access_token"] = *ingress.TrafficAccessToken
			} else {
				ingressMap["traffic_access_token"] = nil
			}
			if ingress.MaskRequestHost != nil {
				ingressMap["mask_request_host"] = *ingress.MaskRequestHost
			} else {
				ingressMap["mask_request_host"] = nil
			}
			netMap["ingress"] = ingressMap
		} else {
			netMap["ingress"] = nil
		}
		fields = append(fields, zap.Any("network", netMap))
	} else {
		fields = append(fields, zap.String("network", "<nil>"))
	}

	// VolumeMounts
	vms := sbx.GetVolumeMounts()
	if len(vms) > 0 {
		vmList := make([]map[string]string, 0, len(vms))
		for _, vm := range vms {
			vmList = append(vmList, map[string]string{
				"id":   vm.GetId(),
				"path": vm.GetPath(),
				"type": vm.GetType(),
				"name": vm.GetName(),
			})
		}
		fields = append(fields, zap.Any("volume_mounts", vmList))
	} else {
		fields = append(fields, zap.String("volume_mounts", "[]"))
	}

	// AutoResume
	ar := sbx.GetAutoResume()
	if ar != nil {
		fields = append(fields, zap.String("auto_resume_policy", ar.GetPolicy()))
	} else {
		fields = append(fields, zap.String("auto_resume", "<nil>"))
	}

	logger.L().Info(ctx, "Sandbox create request details", fields...)
}

func PlaceSandbox(ctx context.Context, algorithm Algorithm, clusterNodes []*nodemanager.Node, preferredNode *nodemanager.Node, sbxRequest *orchestrator.SandboxCreateRequest, buildMachineInfo machineinfo.MachineInfo) (*nodemanager.Node, error) {
	ctx, span := tracer.Start(ctx, "place-sandbox")
	defer span.End()
	logSandboxCreateRequest(ctx, sbxRequest)

	nodesExcluded := make(map[string]struct{})
	var err error

	var node *nodemanager.Node
	if preferredNode != nil {
		node = preferredNode
	}

	attempt := 0
	for attempt < maxRetries {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("request timed out during %d. attempt", attempt+1)
		default:
			// Continue
		}

		if node != nil {
			telemetry.ReportEvent(ctx, "Placing sandbox on the preferred node", telemetry.WithNodeID(node.ID))
		} else {
			if len(nodesExcluded) >= len(clusterNodes) {
				return nil, fmt.Errorf("no nodes available")
			}

			node, err = algorithm.chooseNode(ctx, clusterNodes, nodesExcluded, nodemanager.SandboxResources{CPUs: sbxRequest.GetSandbox().GetVcpu(), MiBMemory: sbxRequest.GetSandbox().GetRamMb()}, buildMachineInfo)
			if err != nil {
				return nil, err
			}

			telemetry.ReportEvent(ctx, "Placing sandbox on the node", telemetry.WithNodeID(node.ID))
		}

		node.PlacementMetrics.StartPlacing(sbxRequest.GetSandbox().GetSandboxId(), nodemanager.SandboxResources{
			CPUs:      sbxRequest.GetSandbox().GetVcpu(),
			MiBMemory: sbxRequest.GetSandbox().GetRamMb(),
		})

		ctx, span := tracer.Start(ctx, "create-sandbox")
		span.SetAttributes(
			telemetry.WithNodeID(node.ID),
			telemetry.WithClusterID(node.ClusterID),
		)
		err = node.SandboxCreate(ctx, sbxRequest)
		span.End()
		if err == nil {
			node.PlacementMetrics.Success(sbxRequest.GetSandbox().GetSandboxId())

			return node, nil
		}

		failedNode := node
		node = nil

		st, ok := status.FromError(err)
		statusCode := codes.Internal
		if ok {
			statusCode = st.Code()
		}

		switch statusCode {
		case codes.ResourceExhausted:
			failedNode.PlacementMetrics.Skip(sbxRequest.GetSandbox().GetSandboxId())
			logger.L().Warn(ctx, "Node exhausted, trying another node", logger.WithSandboxID(sbxRequest.GetSandbox().GetSandboxId()), logger.WithNodeID(failedNode.ID))
		default:
			nodesExcluded[failedNode.ID] = struct{}{}
			failedNode.PlacementMetrics.Fail(sbxRequest.GetSandbox().GetSandboxId())
			logger.L().Error(ctx, "Failed to create sandbox", logger.WithSandboxID(sbxRequest.GetSandbox().GetSandboxId()), logger.WithNodeID(failedNode.ID), zap.Int("attempt", attempt+1), zap.Error(utils.UnwrapGRPCError(err)))
			attempt++
		}
	}

	return nil, errSandboxCreateFailed
}
