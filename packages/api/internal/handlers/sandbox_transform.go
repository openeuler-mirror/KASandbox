package handlers

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	templatecache "github.com/e2b-dev/infra/packages/api/internal/cache/templates"
	"github.com/e2b-dev/infra/packages/api/internal/sandbox"
	"github.com/e2b-dev/infra/packages/api/internal/utils"
	"github.com/e2b-dev/infra/packages/auth/pkg/auth"
	authtypes "github.com/e2b-dev/infra/packages/auth/pkg/types"
	dbtypes "github.com/e2b-dev/infra/packages/db/pkg/types"
	"github.com/e2b-dev/infra/packages/db/queries"
	"github.com/e2b-dev/infra/packages/shared/pkg/clusters"
	featureflags "github.com/e2b-dev/infra/packages/shared/pkg/feature-flags"
	grpcorchestrator "github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	"github.com/e2b-dev/infra/packages/shared/pkg/id"
	sbxlogger "github.com/e2b-dev/infra/packages/shared/pkg/logger/sandbox"
	sandbox_network "github.com/e2b-dev/infra/packages/shared/pkg/sandbox-network"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
	sharedUtils "github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// PostSandboxesTransform transforms a sandbox-create request into the gRPC
// parameters required to call the orchestrator's SandboxService.Create RPC.
//
// The request body only contains the template name (ID or alias). All other
// sandbox parameters (sandbox ID, timeout, env vars, network, volumes, etc.)
// are populated with sensible defaults by the server, and the fully-formed
// orchestrator gRPC SandboxCreateRequest payload is returned to the caller.
//
// The caller is expected to use these parameters to invoke the orchestrator
// gRPC service directly — this endpoint does NOT create the sandbox.
func (a *APIStore) PostSandboxesTransform(c *gin.Context) {
	ctx := c.Request.Context()

	teamInfo := auth.MustGetTeamInfo(c)
	c.Set("teamID", teamInfo.Team.ID.String())

	body, err := utils.ParseBody[api.PostSandboxesTransformJSONRequestBody](ctx, c)
	if err != nil {
		a.sendAPIStoreError(c, http.StatusBadRequest, fmt.Sprintf("Error when parsing request: %s", err))
		telemetry.ReportCriticalError(ctx, "error when parsing request", err)
		return
	}

	if body.TemplateName == "" {
		a.sendAPIStoreError(c, http.StatusBadRequest, "templateName is required")
		return
	}

	// Generate the sandbox ID server-side — callers of the transform endpoint
	// don't need to provide one.
	sandboxID := id.Generate()
	c.Set("instanceID", sandboxID)

	sbxlogger.E(&sbxlogger.SandboxMetadata{
		SandboxID: sandboxID,
		TeamID:    teamInfo.Team.ID.String(),
	}).Debug(ctx, "Transforming sandbox create request")

	// Resolve template alias → template ID + build
	identifier, tag, err := id.ParseName(body.TemplateName)
	if err != nil {
		a.sendAPIStoreError(c, http.StatusBadRequest, fmt.Sprintf("Invalid template reference: %s", err))
		telemetry.ReportError(ctx, "invalid template reference", err)
		return
	}

	clusterID := clusters.WithClusterFallback(teamInfo.Team.ClusterID)
	aliasInfo, err := a.templateCache.ResolveAlias(ctx, identifier, teamInfo.Team.Slug)
	if err != nil {
		apiErr := templatecache.ErrorToAPIError(err, identifier)
		telemetry.ReportErrorByCode(ctx, apiErr.Code, "error when resolving template alias", apiErr.Err)
		a.sendAPIStoreError(c, apiErr.Code, apiErr.ClientMsg)
		return
	}

	env, build, err := a.templateCache.Get(ctx, aliasInfo.TemplateID, tag, teamInfo.Team.ID, clusterID)
	if err != nil {
		apiErr := templatecache.ErrorToAPIError(err, aliasInfo.TemplateID)
		telemetry.ReportErrorByCode(ctx, apiErr.Code, "error when getting template", apiErr.Err, telemetry.WithTemplateID(aliasInfo.TemplateID))
		a.sendAPIStoreError(c, apiErr.Code, apiErr.ClientMsg)
		return
	}

	c.Set("envID", env.TemplateID)
	setTemplateNameMetric(ctx, c, a.featureFlags, env.TemplateID, env.Names)

	alias := firstAlias(env.Aliases)
	telemetry.SetAttributes(ctx,
		telemetry.WithTemplateID(env.TemplateID),
		telemetry.WithSandboxID(sandboxID),
	)

	// Use defaults for everything else — the transform endpoint only needs
	// the template name; the caller can post-process the returned payload
	// if it needs to override any field before calling the orchestrator.
	timeout := sandbox.SandboxTimeoutDefault
	autoPause := sandbox.AutoPauseDefault
	var envVars map[string]string
	var metadata map[string]string
	var autoResume *dbtypes.SandboxAutoResumeConfig
	var envdAccessToken *string
	var allowInternetAccess *bool
	var network *dbtypes.SandboxNetworkConfig
	var sbxVolumeMounts []*grpcorchestrator.SandboxVolumeMount

	// Validate default timeout against the team's max length.
	if timeout > time.Duration(teamInfo.Limits.MaxLengthHours)*time.Hour {
		a.sendAPIStoreError(c, http.StatusBadRequest, fmt.Sprintf("Default timeout exceeds team max length of %d hours", teamInfo.Limits.MaxLengthHours))
		return
	}

	// Build the gRPC SandboxCreateRequest (without dispatching it)
	grpcReq, buildErr := a.buildSandboxCreateGRPCRequest(
		ctx,
		sandboxID,
		alias,
		teamInfo,
		*build,
		env.TemplateID,
		env.TemplateID, // baseTemplateID
		timeout,
		envVars,
		metadata,
		autoPause,
		autoResume,
		envdAccessToken,
		allowInternetAccess,
		network,
		sbxVolumeMounts,
	)
	if buildErr != nil {
		a.sendAPIStoreError(c, buildErr.Code, buildErr.ClientMsg)
		return
	}

	// Convert gRPC request to the API response model and return
	resp := grpcRequestToAPIModel(grpcReq)
	c.JSON(http.StatusOK, resp)
}

// buildSandboxCreateGRPCRequest constructs the orchestrator gRPC SandboxCreateRequest
// from the validated input parameters. This is the "transformation" that the
// transform endpoint provides — it does NOT call the orchestrator.
func (a *APIStore) buildSandboxCreateGRPCRequest(
	ctx context.Context,
	sandboxID, alias string,
	team *authtypes.Team,
	build queries.EnvBuild,
	templateID, baseTemplateID string,
	timeout time.Duration,
	envVars, metadata map[string]string,
	autoPause bool,
	autoResume *dbtypes.SandboxAutoResumeConfig,
	envdAccessToken *string,
	allowInternetAccess *bool,
	network *dbtypes.SandboxNetworkConfig,
	volumeMounts []*grpcorchestrator.SandboxVolumeMount,
) (*grpcorchestrator.SandboxCreateRequest, *api.APIError) {
	// Resolve firecracker version
	fcSemver, err := sandbox.NewVersionInfo(build.FirecrackerVersion)
	if err != nil {
		return nil, &api.APIError{
			Code:      http.StatusInternalServerError,
			ClientMsg: "Failed to get build information for the template",
			Err:       fmt.Errorf("failed to get fcSemver for firecracker version '%s': %w", build.FirecrackerVersion, err),
		}
	}

	hasHugePages := fcSemver.HasHugePages()
	firecrackerVersion := resolveFirecrackerVersion(ctx, a.featureFlags, fcSemver.Version(), build.FirecrackerVersion)

	// Generate traffic access token if public access is disabled
	var trafficAccessToken *string
	if network != nil && network.Ingress != nil && network.Ingress.AllowPublicAccess != nil && !*network.Ingress.AllowPublicAccess {
		accessToken, err := a.accessTokenGenerator.GenerateTrafficAccessToken(sandboxID)
		if err != nil {
			return nil, &api.APIError{
				Code:      http.StatusInternalServerError,
				ClientMsg: "Failed to create traffic access token",
				Err:       fmt.Errorf("failed to create traffic access token for sandbox %s: %w", sandboxID, err),
			}
		}
		trafficAccessToken = &accessToken
	}

	sbxNetwork := buildSBXNetworkConfig(network, allowInternetAccess, trafficAccessToken)

	var orchAutoResume *grpcorchestrator.SandboxAutoResumeConfig
	if autoResume != nil {
		orchAutoResume = &grpcorchestrator.SandboxAutoResumeConfig{
			Policy: string(autoResume.Policy),
		}
	}

	executionID := uuid.New().String()
	startTime := time.Now()
	endTime := startTime.Add(timeout)

	req := &grpcorchestrator.SandboxCreateRequest{
		Sandbox: &grpcorchestrator.SandboxConfig{
			BaseTemplateId:      baseTemplateID,
			TemplateId:          templateID,
			Alias:               &alias,
			TeamId:              team.ID.String(),
			BuildId:             build.ID.String(),
			SandboxId:           sandboxID,
			ExecutionId:         executionID,
			KernelVersion:       build.KernelVersion,
			FirecrackerVersion:  firecrackerVersion,
			EnvdVersion:         *build.EnvdVersion,
			Metadata:            metadata,
			EnvVars:             envVars,
			EnvdAccessToken:     envdAccessToken,
			MaxSandboxLength:    team.Limits.MaxLengthHours,
			HugePages:           hasHugePages,
			RamMb:               build.RamMb,
			Vcpu:                build.Vcpu,
			Snapshot:            false,
			AutoPause:           autoPause,
			AutoResume:          orchAutoResume,
			AllowInternetAccess: allowInternetAccess,
			Network:             sbxNetwork,
			TotalDiskSizeMb:     sharedUtils.FromPtr(build.TotalDiskSizeMb),
			VolumeMounts:        volumeMounts,
		},
		StartTime: timestamppb.New(startTime),
		EndTime:   timestamppb.New(endTime),
	}

	return req, nil
}

// grpcRequestToAPIModel converts the gRPC SandboxCreateRequest to the API
// response model (api.SandboxCreateRequest) for JSON serialization.
func grpcRequestToAPIModel(req *grpcorchestrator.SandboxCreateRequest) *api.SandboxCreateRequest {
	resp := &api.SandboxCreateRequest{
		StartTime: req.StartTime.AsTime(),
		EndTime:   req.EndTime.AsTime(),
		Sandbox:   api.SandboxConfigGrpc{},
	}

	sbx := req.Sandbox
	if sbx == nil {
		return resp
	}

	resp.Sandbox.TemplateId = &sbx.TemplateId
	resp.Sandbox.BuildId = &sbx.BuildId
	resp.Sandbox.KernelVersion = &sbx.KernelVersion
	resp.Sandbox.FirecrackerVersion = &sbx.FirecrackerVersion
	resp.Sandbox.HugePages = &sbx.HugePages
	resp.Sandbox.SandboxId = &sbx.SandboxId
	resp.Sandbox.Alias = sbx.Alias
	resp.Sandbox.EnvdVersion = &sbx.EnvdVersion
	resp.Sandbox.Vcpu = &sbx.Vcpu
	resp.Sandbox.RamMb = &sbx.RamMb
	resp.Sandbox.TeamId = &sbx.TeamId
	resp.Sandbox.MaxSandboxLength = &sbx.MaxSandboxLength
	resp.Sandbox.TotalDiskSizeMb = &sbx.TotalDiskSizeMb
	resp.Sandbox.Snapshot = &sbx.Snapshot
	resp.Sandbox.BaseTemplateId = &sbx.BaseTemplateId
	resp.Sandbox.AutoPause = &sbx.AutoPause
	resp.Sandbox.EnvdAccessToken = sbx.EnvdAccessToken
	resp.Sandbox.ExecutionId = &sbx.ExecutionId
	resp.Sandbox.AllowInternetAccess = sbx.AllowInternetAccess

	if sbx.EnvVars != nil {
		envVars := api.EnvVars(sbx.EnvVars)
		resp.Sandbox.EnvVars = &envVars
	}
	if sbx.Metadata != nil {
		metadata := api.SandboxMetadata(sbx.Metadata)
		resp.Sandbox.Metadata = &metadata
	}

	if sbx.Network != nil {
		net := api.SandboxNetworkConfigGrpc{
			Egress:  &api.SandboxNetworkEgressConfigGrpc{},
			Ingress: &api.SandboxNetworkIngressConfigGrpc{},
		}
		if sbx.Network.Egress != nil {
			net.Egress.AllowedCidrs = &sbx.Network.Egress.AllowedCidrs
			net.Egress.AllowedDomains = &sbx.Network.Egress.AllowedDomains
			net.Egress.DeniedCidrs = &sbx.Network.Egress.DeniedCidrs
		}
		if sbx.Network.Ingress != nil {
			net.Ingress.TrafficAccessToken = sbx.Network.Ingress.TrafficAccessToken
			net.Ingress.MaskRequestHost = sbx.Network.Ingress.MaskRequestHost
		}
		resp.Sandbox.Network = &net
	}

	if len(sbx.VolumeMounts) > 0 {
		mounts := make([]api.SandboxVolumeMountGrpc, 0, len(sbx.VolumeMounts))
		for _, m := range sbx.VolumeMounts {
			mounts = append(mounts, api.SandboxVolumeMountGrpc{
				VolumeId:  &m.Id,
				MountPath: &m.Path,
			})
		}
		resp.Sandbox.VolumeMounts = &mounts
	}

	if sbx.AutoResume != nil {
		policy := sbx.AutoResume.Policy
		resp.Sandbox.AutoResume = &api.SandboxAutoResumeConfigGrpc{
			Policy: &policy,
		}
	}

	return resp
}

// resolveFirecrackerVersion returns the firecracker version from feature flags,
// falling back to the provided fallback if no override is configured.
func resolveFirecrackerVersion(ctx context.Context, ff *featureflags.Client, version semver.Version, fallback string) string {
	firecrackerVersions := ff.JSONFlag(ctx, featureflags.FirecrackerVersions).AsValueMap()
	fcVersion, ok := firecrackerVersions.Get(fmt.Sprintf("v%d.%d", version.Major(), version.Minor())).AsOptionalString().Get()
	if !ok {
		return fallback
	}
	return fcVersion
}

// buildSBXNetworkConfig constructs the orchestrator gRPC network configuration
// from the input parameters. Mirrors orchestrator.buildNetworkConfig.
func buildSBXNetworkConfig(network *dbtypes.SandboxNetworkConfig, allowInternetAccess *bool, trafficAccessToken *string) *grpcorchestrator.SandboxNetworkConfig {
	orchNetwork := &grpcorchestrator.SandboxNetworkConfig{
		Egress: &grpcorchestrator.SandboxNetworkEgressConfig{},
		Ingress: &grpcorchestrator.SandboxNetworkIngressConfig{
			TrafficAccessToken: trafficAccessToken,
		},
	}

	if network != nil && network.Egress != nil {
		allowedAddresses, allowedDomains := sandbox_network.ParseAddressesAndDomains(network.Egress.AllowedAddresses)
		if len(allowedDomains) > 0 {
			allowedAddresses = append(allowedAddresses, sandbox_network.DefaultNameserver)
		}
		orchNetwork.Egress.AllowedCidrs = sandbox_network.AddressStringsToCIDRs(allowedAddresses)
		orchNetwork.Egress.AllowedDomains = allowedDomains
		orchNetwork.Egress.DeniedCidrs = sandbox_network.AddressStringsToCIDRs(network.Egress.DeniedAddresses)
	}

	if network != nil && network.Ingress != nil {
		orchNetwork.Ingress.MaskRequestHost = network.Ingress.MaskRequestHost
	}

	if allowInternetAccess != nil && !*allowInternetAccess {
		orchNetwork.Egress.DeniedCidrs = []string{sandbox_network.AllInternetTrafficCIDR}
	}

	return orchNetwork
}
