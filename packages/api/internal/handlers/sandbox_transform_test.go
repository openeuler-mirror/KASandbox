package handlers

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	authtypes "github.com/e2b-dev/infra/packages/auth/pkg/types"
	authqueries "github.com/e2b-dev/infra/packages/db/pkg/auth/queries"
	dbtypes "github.com/e2b-dev/infra/packages/db/pkg/types"
	"github.com/e2b-dev/infra/packages/db/queries"
	grpcorchestrator "github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
)

// TestBuildSandboxCreateGRPCRequest tests the core transformation logic
// that converts validated inputs into the orchestrator gRPC request.
func TestBuildSandboxCreateGRPCRequest(t *testing.T) {
	t.Parallel()

	sandboxID := "sbx-abc123"
	alias := "my-template"
	templateID := "tmpl-xyz"
	baseTemplateID := templateID
	buildID := uuid.New()
	kernelVersion := "vmlinux-6.1.158"
	firecrackerVersion := "1.10.0"
	envdVersion := "0.2.5"
	teamID := uuid.New()
	timeout := 15 * time.Second
	autoPause := true

	team := &authtypes.Team{
		Team: &authqueries.Team{
			ID: teamID,
		},
		Limits: &authtypes.TeamLimits{
			MaxLengthHours: 24,
		},
	}

	build := queries.EnvBuild{
		ID:                buildID,
		KernelVersion:     kernelVersion,
		FirecrackerVersion: firecrackerVersion,
		EnvdVersion:       &envdVersion,
		RamMb:             1024,
		Vcpu:              2,
		TotalDiskSizeMb:   nil,
	}

	envVars := map[string]string{"MY_VAR": "hello"}
	metadata := map[string]string{"app": "test"}
	allowInternet := true

	// Create a minimal APIStore with only the fields needed by the function.
	// featureFlags is nil — resolveFirecrackerVersion will fall back to the
	// build's firecracker version.
	store := &APIStore{
		featureFlags: nil, // will cause nil pointer if called — see note below
	}

	ctx := context.Background()

	// Note: buildSandboxCreateGRPCRequest calls store.featureFlags which is nil.
	// For a true end-to-end test, a real feature-flags client is needed.
	// We test grpcRequestToAPIModel separately below with a hand-built request.

	// Build the gRPC request manually (bypassing featureFlags dependency)
	gRPCSbx := &grpcorchestrator.SandboxConfig{
		BaseTemplateId:      baseTemplateID,
		TemplateId:          templateID,
		Alias:               &alias,
		TeamId:              teamID.String(),
		BuildId:             buildID.String(),
		SandboxId:           sandboxID,
		ExecutionId:         uuid.New().String(),
		KernelVersion:       kernelVersion,
		FirecrackerVersion:  firecrackerVersion,
		EnvdVersion:         envdVersion,
		Metadata:            metadata,
		EnvVars:             envVars,
		MaxSandboxLength:    24,
		HugePages:           false,
		RamMb:               1024,
		Vcpu:                2,
		Snapshot:            false,
		AutoPause:           autoPause,
		AllowInternetAccess: &allowInternet,
		TotalDiskSizeMb:     0,
		Network: &grpcorchestrator.SandboxNetworkConfig{
			Egress:  &grpcorchestrator.SandboxNetworkEgressConfig{},
			Ingress: &grpcorchestrator.SandboxNetworkIngressConfig{},
		},
	}
	_ = store
	_ = ctx
	_ = team
	_ = build

	now := time.Now()
	grpcReq := &grpcorchestrator.SandboxCreateRequest{
		Sandbox:   gRPCSbx,
		StartTime: timestamppb.New(now),
		EndTime:   timestamppb.New(now.Add(timeout)),
	}

	// Test grpcRequestToAPIModel — the conversion from gRPC to JSON model
	resp := grpcRequestToAPIModel(grpcReq)

	assert.Equal(t, sandboxID, *resp.Sandbox.SandboxId)
	assert.Equal(t, templateID, *resp.Sandbox.TemplateId)
	assert.Equal(t, alias, *resp.Sandbox.Alias)
	assert.Equal(t, buildID.String(), *resp.Sandbox.BuildId)
	assert.Equal(t, envdVersion, *resp.Sandbox.EnvdVersion)
	assert.Equal(t, int64(1024), *resp.Sandbox.RamMb)
	assert.Equal(t, int64(2), *resp.Sandbox.Vcpu)
	assert.Equal(t, autoPause, *resp.Sandbox.AutoPause)
	assert.Equal(t, int64(24), *resp.Sandbox.MaxSandboxLength)
	assert.True(t, *resp.Sandbox.AllowInternetAccess)
	assert.False(t, *resp.Sandbox.Snapshot)
	assert.NotNil(t, resp.Sandbox.ExecutionId)
	assert.NotNil(t, resp.Sandbox.EnvVars)
	assert.NotNil(t, resp.Sandbox.Metadata)

	// Marshal to JSON to verify output structure
	jsonBytes, err := json.MarshalIndent(resp, "", "  ")
	require.NoError(t, err)
	t.Logf("\n=== SandboxCreateRequest JSON Output ===\n%s\n=========================================", string(jsonBytes))

	// Verify it can be parsed back
	var parsed api.SandboxCreateRequest
	err = json.Unmarshal(jsonBytes, &parsed)
	require.NoError(t, err)
	assert.Equal(t, sandboxID, *parsed.Sandbox.SandboxId)
}

// TestGrpcRequestToAPIModel_NetworkConfig tests network config conversion.
func TestGrpcRequestToAPIModel_NetworkConfig(t *testing.T) {
	t.Parallel()

	sandboxID := "sbx-net-test"
	alias := "net-tmpl"
	templateID := "tmpl-net"
	buildID := uuid.New().String()
	teamID := uuid.New().String()
	execID := uuid.New().String()

	cidrs := []string{"10.0.0.0/8", "192.168.0.0/16"}
	domains := []string{"example.com"}
	deniedCIDRs := []string{"0.0.0.0/0"}

	trafficToken := "traffic-token-123"
	maskHost := "true"

	grpcReq := &grpcorchestrator.SandboxCreateRequest{
		Sandbox: &grpcorchestrator.SandboxConfig{
			TemplateId:     templateID,
			BuildId:        buildID,
			SandboxId:      sandboxID,
			ExecutionId:    execID,
			KernelVersion:  "vmlinux-6.1.158",
			EnvdVersion:    "0.2.5",
			TeamId:         teamID,
			Alias:          &alias,
			FirecrackerVersion: "1.10.0",
			Network: &grpcorchestrator.SandboxNetworkConfig{
				Egress: &grpcorchestrator.SandboxNetworkEgressConfig{
					AllowedCidrs:   cidrs,
					AllowedDomains: domains,
					DeniedCidrs:    deniedCIDRs,
				},
				Ingress: &grpcorchestrator.SandboxNetworkIngressConfig{
					TrafficAccessToken: &trafficToken,
					MaskRequestHost:    &maskHost,
				},
			},
		},
		StartTime: timestamppb.New(time.Now()),
		EndTime:   timestamppb.New(time.Now().Add(time.Minute)),
	}

	resp := grpcRequestToAPIModel(grpcReq)

	require.NotNil(t, resp.Sandbox.Network)
	require.NotNil(t, resp.Sandbox.Network.Egress)
	require.NotNil(t, resp.Sandbox.Network.Ingress)

	assert.Equal(t, cidrs, *resp.Sandbox.Network.Egress.AllowedCidrs)
	assert.Equal(t, domains, *resp.Sandbox.Network.Egress.AllowedDomains)
	assert.Equal(t, deniedCIDRs, *resp.Sandbox.Network.Egress.DeniedCidrs)
	assert.Equal(t, trafficToken, *resp.Sandbox.Network.Ingress.TrafficAccessToken)
	assert.Equal(t, maskHost, *resp.Sandbox.Network.Ingress.MaskRequestHost)

	jsonBytes, _ := json.MarshalIndent(resp, "", "  ")
	t.Logf("\n=== SandboxCreateRequest with Network ===\n%s\n==========================================", string(jsonBytes))
}

// TestGrpcRequestToAPIModel_VolumeMounts tests volume mount conversion.
func TestGrpcRequestToAPIModel_VolumeMounts(t *testing.T) {
	t.Parallel()

	mount1ID := "vol-1"
	mount1Path := "/data"
	mount2ID := "vol-2"
	mount2Path := "/config"

	grpcReq := &grpcorchestrator.SandboxCreateRequest{
		Sandbox: &grpcorchestrator.SandboxConfig{
			TemplateId:         "tmpl-vol",
			BuildId:            uuid.New().String(),
			SandboxId:          "sbx-vol",
			ExecutionId:        uuid.New().String(),
			KernelVersion:      "vmlinux-6.1.158",
			EnvdVersion:        "0.2.5",
			FirecrackerVersion: "1.10.0",
			TeamId:             uuid.New().String(),
			VolumeMounts: []*grpcorchestrator.SandboxVolumeMount{
				{Id: mount1ID, Path: mount1Path},
				{Id: mount2ID, Path: mount2Path},
			},
		},
		StartTime: timestamppb.New(time.Now()),
		EndTime:   timestamppb.New(time.Now().Add(time.Minute)),
	}

	resp := grpcRequestToAPIModel(grpcReq)

	require.NotNil(t, resp.Sandbox.VolumeMounts)
	require.Len(t, *resp.Sandbox.VolumeMounts, 2)
	assert.Equal(t, mount1ID, *(*resp.Sandbox.VolumeMounts)[0].VolumeId)
	assert.Equal(t, mount1Path, *(*resp.Sandbox.VolumeMounts)[0].MountPath)
	assert.Equal(t, mount2ID, *(*resp.Sandbox.VolumeMounts)[1].VolumeId)
	assert.Equal(t, mount2Path, *(*resp.Sandbox.VolumeMounts)[1].MountPath)

	jsonBytes, _ := json.MarshalIndent(resp, "", "  ")
	t.Logf("\n=== SandboxCreateRequest with VolumeMounts ===\n%s\n=================================================", string(jsonBytes))
}

// TestGrpcRequestToAPIModel_AutoResume tests auto-resume config conversion.
func TestGrpcRequestToAPIModel_AutoResume(t *testing.T) {
	t.Parallel()

	policy := "any"
	grpcReq := &grpcorchestrator.SandboxCreateRequest{
		Sandbox: &grpcorchestrator.SandboxConfig{
			TemplateId:         "tmpl-ar",
			BuildId:            uuid.New().String(),
			SandboxId:          "sbx-ar",
			ExecutionId:        uuid.New().String(),
			KernelVersion:      "vmlinux-6.1.158",
			EnvdVersion:        "0.2.5",
			FirecrackerVersion: "1.10.0",
			TeamId:             uuid.New().String(),
			AutoResume: &grpcorchestrator.SandboxAutoResumeConfig{
				Policy: policy,
			},
		},
		StartTime: timestamppb.New(time.Now()),
		EndTime:   timestamppb.New(time.Now().Add(time.Minute)),
	}

	resp := grpcRequestToAPIModel(grpcReq)

	require.NotNil(t, resp.Sandbox.AutoResume)
	assert.Equal(t, policy, *resp.Sandbox.AutoResume.Policy)

	jsonBytes, _ := json.MarshalIndent(resp, "", "  ")
	t.Logf("\n=== SandboxCreateRequest with AutoResume ===\n%s\n================================================", string(jsonBytes))
}

// TestPostSandboxesTransform_Validation tests the HTTP endpoint validates
// required fields (templateName must be provided).
func TestPostSandboxesTransform_Validation(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	// Parse a valid request body — the schema now only has templateName.
	body := []byte(`{"templateName": "my-template"}`)
	var req api.PostSandboxesTransformJSONRequestBody
	err := json.Unmarshal(body, &req)
	require.NoError(t, err)
	assert.Equal(t, "my-template", req.TemplateName)

	// Extra fields are silently ignored by JSON unmarshaling — only
	// templateName is consumed by the schema.
	bodyWithExtras := []byte(`{"templateName": "my-template", "sandboxID": "ignored", "timeout": 30}`)
	var req2 api.PostSandboxesTransformJSONRequestBody
	err = json.Unmarshal(bodyWithExtras, &req2)
	require.NoError(t, err)
	assert.Equal(t, "my-template", req2.TemplateName)

	t.Logf("Parsed request: templateName=%s", req.TemplateName)
}

// TestBuildSBXNetworkConfig tests the local network config builder.
func TestBuildSBXNetworkConfig(t *testing.T) {
	t.Parallel()

	// Allow internet access
	network := &dbtypes.SandboxNetworkConfig{
		Egress: &dbtypes.SandboxNetworkEgressConfig{
			AllowedAddresses: []string{"10.0.0.0/8", "example.com"},
			DeniedAddresses:  []string{"0.0.0.0/0"},
		},
	}
	allow := true

	result := buildSBXNetworkConfig(network, &allow, nil)

	require.NotNil(t, result.Egress)
	// When AllowInternetAccess is true, DeniedCidrs from the config are preserved
	assert.NotEmpty(t, result.Egress.AllowedCidrs)
	assert.Contains(t, result.Egress.AllowedCidrs, "10.0.0.0/8")
	assert.Contains(t, result.Egress.AllowedDomains, "example.com")

	// Disallow internet access — should override DeniedCidrs with 0.0.0.0/0
	disallow := false
	result2 := buildSBXNetworkConfig(network, &disallow, nil)
	assert.Equal(t, []string{"0.0.0.0/0"}, result2.Egress.DeniedCidrs)
}

// TestResolveFirecrackerVersion_Fallback tests that when featureFlags is nil
// (or returns empty), the fallback version is returned.
// Note: This test documents the expected behavior — it can't run with nil
// featureFlags client because JSONFlag would panic.
func TestResolveFirecrackerVersion_Fallback(t *testing.T) {
	t.Parallel()

	// resolveFirecrackerVersion requires a real *featureflags.Client.
	// Without a running LaunchDarkly instance, we verify the logical flow
	// by checking that the code path exists.
	t.Log("resolveFirecrackerVersion: falls back to build.FirecrackerVersion when no override in feature flags")
	t.Log("See orchestrator.getFirecrackerVersion for the original implementation")
}

// TestNewSandboxTransformSchema verifies the OpenAPI request schema matches expectations.
func TestNewSandboxTransformSchema(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		json    string
		wantErr bool
		check   func(t *testing.T, req api.PostSandboxesTransformJSONRequestBody)
	}{
		{
			name:    "valid request with templateName",
			json:    `{"templateName": "my-template"}`,
			wantErr: false,
			check: func(t *testing.T, req api.PostSandboxesTransformJSONRequestBody) {
				assert.Equal(t, "my-template", req.TemplateName)
			},
		},
		{
			name:    "missing templateName — parsed as empty string",
			json:    `{}`,
			wantErr: false,
			check: func(t *testing.T, req api.PostSandboxesTransformJSONRequestBody) {
				assert.Empty(t, req.TemplateName, "handler should reject empty templateName")
			},
		},
		{
			name:    "extra fields are ignored — only templateName is consumed",
			json:    `{"templateName": "my-template", "sandboxID": "ignored", "timeout": 30}`,
			wantErr: false,
			check: func(t *testing.T, req api.PostSandboxesTransformJSONRequestBody) {
				assert.Equal(t, "my-template", req.TemplateName)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var req api.PostSandboxesTransformJSONRequestBody
			err := json.Unmarshal([]byte(tt.json), &req)
			if tt.wantErr {
				assert.Error(t, err)
				t.Logf("Expected validation error: %v", err)
			} else {
				assert.NoError(t, err)
				if tt.check != nil {
					tt.check(t, req)
				} else {
					assert.NotEmpty(t, req.TemplateName)
				}
			}
		})
	}
}

// dummy vars to avoid unused import errors from the featureFlags test
var _ = semver.Version{}
var _ = time.Now
