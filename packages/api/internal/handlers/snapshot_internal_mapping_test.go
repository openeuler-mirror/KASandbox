package handlers

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apispec "github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/db/pkg/types"
	"github.com/e2b-dev/infra/packages/db/queries"
)

// The tests in this file need no database: they cover the DTO mapping and the
// request checks that run before any query.

func TestRuntimeMetadataToUpsertParams(t *testing.T) {
	t.Parallel()

	teamID := uuid.New()
	body := newRuntimeSnapshotBody(teamID, "base-env", "node-a")
	body.Metadata = &map[string]string{"k": "v"}
	body.AutoResume = &map[string]interface{}{"policy": "any"}
	body.VolumeMounts = &[]map[string]interface{}{{"id": "vol-1", "type": "nfs", "name": "data", "path": "/data"}}

	params, err := runtimeMetadataToUpsertParams("sbx-1", body)
	require.NoError(t, err)

	assert.NotEmpty(t, params.TemplateID)
	assert.Equal(t, teamID, params.TeamID)
	assert.Nil(t, params.ClusterID)
	assert.Equal(t, "sbx-1", params.SandboxID)
	assert.Equal(t, "base-env", params.BaseTemplateID)
	assert.Equal(t, types.JSONBStringMap{"k": "v"}, params.Metadata)
	assert.True(t, params.StartedAt.Valid)
	assert.Equal(t, body.SandboxStartedAt, params.StartedAt.Time)
	assert.True(t, params.Secure)
	require.NotNil(t, params.AllowInternetAccess)
	assert.True(t, *params.AllowInternetAccess)
	assert.Equal(t, "node-a", params.OriginNodeID)
	assert.False(t, params.AutoPause)
	assert.Equal(t, int64(2), params.Vcpu)
	assert.Equal(t, int64(2048), params.RamMb)
	assert.Equal(t, int64(0), params.FreeDiskSizeMb)
	require.NotNil(t, params.TotalDiskSizeMb)
	assert.Equal(t, int64(1024), *params.TotalDiskSizeMb)
	assert.Equal(t, "6.1.0", params.KernelVersion)
	assert.Equal(t, "1.4.0", params.FirecrackerVersion)
	require.NotNil(t, params.EnvdVersion)
	assert.Equal(t, "v1.0.0", *params.EnvdVersion)
	assert.Equal(t, types.BuildStatusSnapshotting, params.Status)

	require.NotNil(t, params.Config)
	assert.Equal(t, types.PausedSandboxConfigVersion, params.Config.Version)
	require.NotNil(t, params.Config.Network)
	require.NotNil(t, params.Config.Network.Ingress)
	require.NotNil(t, params.Config.Network.Ingress.AllowPublicAccess)
	assert.True(t, *params.Config.Network.Ingress.AllowPublicAccess)
	require.NotNil(t, params.Config.AutoResume)
	assert.Equal(t, types.SandboxAutoResumeAny, params.Config.AutoResume.Policy)
	require.Len(t, params.Config.VolumeMounts, 1)
	assert.Equal(t, types.SandboxVolumeMountConfig{ID: "vol-1", Type: "nfs", Name: "data", Path: "/data"}, *params.Config.VolumeMounts[0])

	// source_machine_info lands in the cpu_* build columns
	require.NotNil(t, params.CpuArchitecture)
	assert.Equal(t, "x86_64", *params.CpuArchitecture)
	require.NotNil(t, params.CpuFamily)
	assert.Equal(t, "6", *params.CpuFamily)
	require.NotNil(t, params.CpuModel)
	assert.Equal(t, "85", *params.CpuModel)
	require.NotNil(t, params.CpuModelName)
	assert.Equal(t, "Test CPU", *params.CpuModelName)
	assert.Equal(t, []string{"avx2"}, params.CpuFlags)
}

func TestRuntimeMetadataToUpsertParamsDefaults(t *testing.T) {
	t.Parallel()

	body := apispec.RuntimeSnapshotMetadata{
		OperationId:        "op-1",
		TeamId:             uuid.New(),
		BaseTemplateId:     "base-env",
		SourcePodUid:       "pod-uid",
		SourceNodeName:     "node-a",
		SandboxStartedAt:   time.Now().UTC(),
		Vcpu:               1,
		RamMb:              512,
		TotalDiskSizeMb:    1024,
		KernelVersion:      "6.1.0",
		FirecrackerVersion: "1.4.0",
		EnvdVersion:        "v1.0.0",
	}

	params, err := runtimeMetadataToUpsertParams("sbx-1", body)
	require.NoError(t, err)

	assert.NotNil(t, params.Metadata)
	assert.Empty(t, params.Metadata)
	assert.False(t, params.AutoPause)
	assert.Nil(t, params.AllowInternetAccess)
	assert.Equal(t, &types.PausedSandboxConfig{Version: types.PausedSandboxConfigVersion}, params.Config)
	assert.Nil(t, params.CpuArchitecture)
	assert.Nil(t, params.CpuFamily)
	assert.Nil(t, params.CpuModel)
	assert.Nil(t, params.CpuModelName)
	assert.Nil(t, params.CpuFlags)
}

func TestRuntimeMetadataToUpsertParamsInvalidObjects(t *testing.T) {
	t.Parallel()

	body := newRuntimeSnapshotBody(uuid.New(), "base-env", "node-a")
	body.SourceMachineInfo = &map[string]interface{}{"cpu_flags": "not-a-list"}

	_, err := runtimeMetadataToUpsertParams("sbx-1", body)
	require.ErrorContains(t, err, "invalid source_machine_info")

	body = newRuntimeSnapshotBody(uuid.New(), "base-env", "node-a")
	body.AutoResume = &map[string]interface{}{"timeout": "not-a-number"}

	_, err = runtimeMetadataToUpsertParams("sbx-1", body)
	require.ErrorContains(t, err, "invalid auto_resume config")
}

func TestResumeMetadataFromLastSnapshot(t *testing.T) {
	t.Parallel()

	buildID := uuid.New()
	startedAt := time.Now().UTC().Truncate(time.Second)
	allowPublic := true
	row := queries.GetLastSnapshotRow{
		Aliases: []string{"base-alias"},
		Snapshot: queries.Snapshot{
			EnvID:            "snapshot-env",
			BaseEnvID:        "base-env",
			OriginNodeID:     "node-a",
			EnvSecure:        true,
			AutoPause:        true,
			SandboxStartedAt: pgtype.Timestamptz{Time: startedAt, Valid: true},
			Metadata:         types.JSONBStringMap{"k": "v"},
			Config: &types.PausedSandboxConfig{
				Version:    types.PausedSandboxConfigVersion,
				Network:    &types.SandboxNetworkConfig{Ingress: &types.SandboxNetworkIngressConfig{AllowPublicAccess: &allowPublic}},
				AutoResume: &types.SandboxAutoResumeConfig{Policy: types.SandboxAutoResumeOff},
			},
		},
		EnvBuild: queries.EnvBuild{
			ID:                 buildID,
			Vcpu:               2,
			RamMb:              2048,
			TotalDiskSizeMb:    new(int64(1024)),
			KernelVersion:      "6.1.0",
			FirecrackerVersion: "1.4.0",
			EnvdVersion:        new("v1.0.0"),
			CpuArchitecture:    new("aarch64"),
		},
	}

	result := resumeMetadataFromLastSnapshot(row)

	assert.Equal(t, "snapshot-env", result.TemplateId)
	assert.Equal(t, buildID, result.BuildId)
	require.NotNil(t, result.BaseTemplateId)
	assert.Equal(t, "base-env", *result.BaseTemplateId)
	require.NotNil(t, result.Aliases)
	assert.Equal(t, []string{"base-alias"}, *result.Aliases)
	assert.Equal(t, "node-a", result.OriginNode)
	assert.Equal(t, int64(2), result.Vcpu)
	assert.Equal(t, int64(2048), result.RamMb)
	require.NotNil(t, result.TotalDiskSizeMb)
	assert.Equal(t, int64(1024), *result.TotalDiskSizeMb)
	assert.True(t, result.EnvSecure)
	assert.True(t, result.AutoPause)
	assert.Equal(t, startedAt, result.SandboxStartedAt)
	require.NotNil(t, result.Metadata)
	assert.Equal(t, map[string]string{"k": "v"}, *result.Metadata)
	require.NotNil(t, result.Network)
	assert.Equal(t, map[string]interface{}{"allowPublicAccess": true}, (*result.Network)["ingress"])
	require.NotNil(t, result.AutoResume)
	assert.Equal(t, "off", (*result.AutoResume)["policy"])
	assert.Nil(t, result.VolumeMounts)
	require.NotNil(t, result.MachineInfo)
	assert.Equal(t, map[string]interface{}{"cpu_architecture": "aarch64"}, *result.MachineInfo)

	// No aliases, no metadata, no config and no CPU info leave the optional fields unset
	empty := resumeMetadataFromLastSnapshot(queries.GetLastSnapshotRow{})
	assert.Nil(t, empty.Aliases)
	assert.Nil(t, empty.Metadata)
	assert.Nil(t, empty.Network)
	assert.Nil(t, empty.AutoResume)
	assert.Nil(t, empty.MachineInfo)
}

// TestInternalHandlersRejectInvalidIDs covers the 400 answers given before any query runs.
func TestInternalHandlersRejectInvalidIDs(t *testing.T) {
	t.Parallel()

	store := &APIStore{}
	teamID := uuid.New()

	c, w := doJSONRequest(t, http.MethodPost, "/internal/builds/not-a-uuid/status", apispec.BuildStatusRequest{OperationId: "op-1", Status: apispec.Success})
	store.PostInternalBuildsBuildIDStatus(c, "not-a-uuid")
	assert.Equal(t, http.StatusBadRequest, w.Code)

	c, w = doJSONRequest(t, http.MethodGet, "/internal/snapshots/bad%20id/resolve", nil)
	store.GetInternalSnapshotsSnapshotIDResolve(c, "bad id!", apispec.GetInternalSnapshotsSnapshotIDResolveParams{TeamId: teamID})
	assert.Equal(t, http.StatusBadRequest, w.Code)

	c, w = doJSONRequest(t, http.MethodDelete, "/internal/snapshot-templates/bad%20id", nil)
	store.DeleteInternalSnapshotTemplatesSnapshotID(c, "bad id!", apispec.DeleteInternalSnapshotTemplatesSnapshotIDParams{TeamId: teamID})
	assert.Equal(t, http.StatusBadRequest, w.Code)

	c, w = doJSONRequest(t, http.MethodDelete, "/internal/templates/bad%20id", nil)
	store.DeleteInternalTemplatesTemplateID(c, "bad id!", apispec.DeleteInternalTemplatesTemplateIDParams{TeamId: teamID})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestInternalSnapshotWireFormat pins the JSON field names the callers use
// (cri-multiplex e2b-verify 22 and tests/webhook-stub in KASandbox).
func TestInternalSnapshotWireFormat(t *testing.T) {
	t.Parallel()

	keys := func(v any) map[string]any {
		raw, err := json.Marshal(v)
		require.NoError(t, err)

		out := map[string]any{}
		require.NoError(t, json.Unmarshal(raw, &out))

		return out
	}

	assert.Subset(t, keys(apispec.PauseSnapshotResponse{}), map[string]any{"template_id": "", "build_id": uuid.Nil.String()})
	assert.Subset(t, keys(apispec.SnapshotTemplateResponse{}), map[string]any{"snapshot_id": "", "template_id": "", "build_id": uuid.Nil.String()})
	assert.Equal(t, map[string]any{"paused": false}, keys(apispec.SnapshotStateResponse{}))
	assert.Equal(t, map[string]any{"paused": true, "template_id": "t", "build_id": uuid.Nil.String()},
		keys(apispec.SnapshotStateResponse{Paused: true, TemplateId: new("t"), BuildId: new(uuid.Nil)}))

	resume := keys(apispec.ResumeMetadata{
		BaseTemplateId:  new("base"),
		TotalDiskSizeMb: new(int64(1)),
		EnvdVersion:     new("v"),
		Metadata:        &map[string]string{},
	})
	for _, key := range []string{
		"template_id", "build_id", "base_template_id", "origin_node", "vcpu", "ram_mb", "total_disk_size_mb",
		"kernel_version", "firecracker_version", "envd_version", "env_secure", "metadata", "auto_pause", "sandbox_started_at",
	} {
		assert.Contains(t, resume, key)
	}

	// Request bodies as e2b-verify 22 sends them
	var meta apispec.RuntimeSnapshotMetadata
	require.NoError(t, json.Unmarshal([]byte(`{
		"operation_id": "op", "team_id": "11111111-1111-1111-1111-111111111111", "base_template_id": "base",
		"source_pod_uid": "uid", "source_node_name": "node", "sandbox_started_at": "2026-11-03T10:00:00Z",
		"vcpu": 2, "ram_mb": 512, "total_disk_size_mb": 1024, "kernel_version": "k", "firecracker_version": "f",
		"envd_version": "e", "secure": false, "allow_internet_access": true, "metadata": {"source": "manual-e2e"}
	}`), &meta))
	assert.Equal(t, "op", meta.OperationId)
	assert.Equal(t, "node", meta.SourceNodeName)
	assert.Equal(t, int64(1024), meta.TotalDiskSizeMb)
	require.NotNil(t, meta.AllowInternetAccess)
	assert.True(t, *meta.AllowInternetAccess)
	require.NotNil(t, meta.Metadata)
	assert.Equal(t, "manual-e2e", (*meta.Metadata)["source"])

	var status apispec.BuildStatusRequest
	require.NoError(t, json.Unmarshal([]byte(`{"operation_id":"op","status":"failed","error":"boom"}`), &status))
	assert.Equal(t, apispec.Failed, status.Status)
	require.NotNil(t, status.Error)
	assert.Equal(t, "boom", *status.Error)
}
