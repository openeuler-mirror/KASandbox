package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apispec "github.com/e2b-dev/infra/packages/api/internal/api"
	templatecache "github.com/e2b-dev/infra/packages/api/internal/cache/templates"
	"github.com/e2b-dev/infra/packages/db/pkg/testutils"
	"github.com/e2b-dev/infra/packages/db/pkg/types"
	"github.com/e2b-dev/infra/packages/shared/pkg/id"
	redis_utils "github.com/e2b-dev/infra/packages/shared/pkg/redis"
)

func newInternalTestStore(t *testing.T) (*APIStore, *testutils.Database) {
	t.Helper()

	testDB := testutils.SetupDatabase(t)
	redis := redis_utils.SetupInstance(t)

	store := &APIStore{
		sqlcDB:        testDB.SqlcClient,
		authDB:        testDB.AuthDb,
		templateCache: templatecache.NewTemplateCache(testDB.SqlcClient, redis),
	}

	return store, testDB
}

func newRuntimeSnapshotBody(teamID uuid.UUID, baseTemplateID, nodeName string) apispec.RuntimeSnapshotMetadata {
	allowInternet := true
	autoPause := false

	return apispec.RuntimeSnapshotMetadata{
		OperationId:         "op-" + uuid.NewString(),
		TeamId:              teamID,
		BaseTemplateId:      baseTemplateID,
		SourcePodUid:        uuid.NewString(),
		SourceNodeName:      nodeName,
		SandboxStartedAt:    time.Now().UTC().Truncate(time.Second),
		Vcpu:                2,
		RamMb:               2048,
		TotalDiskSizeMb:     1024,
		KernelVersion:       "6.1.0",
		FirecrackerVersion:  "1.4.0",
		EnvdVersion:         "v1.0.0",
		Secure:              true,
		AllowInternetAccess: &allowInternet,
		AutoPause:           &autoPause,
		Network:             &map[string]interface{}{"ingress": map[string]interface{}{"allowPublicAccess": true}},
		SourceMachineInfo: &map[string]interface{}{
			"cpu_architecture": "x86_64",
			"cpu_family":       "6",
			"cpu_model":        "85",
			"cpu_model_name":   "Test CPU",
			"cpu_flags":        []string{"avx2"},
		},
	}
}

func doJSONRequest(t *testing.T, method, url string, body any) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()

	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)

		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, url, reader)
	c.Request.Header.Set("Content-Type", "application/json")

	return c, w
}

func TestInternalPauseSnapshotWriteAndIdempotent(t *testing.T) {
	t.Parallel()

	store, testDB := newInternalTestStore(t)
	ctx := t.Context()

	teamID := testutils.CreateTestTeam(t, testDB)
	baseTemplateID := testutils.CreateTestTemplate(t, testDB, teamID)
	sandboxID := "sbx-" + uuid.NewString()

	body := newRuntimeSnapshotBody(teamID, baseTemplateID, "node-a")

	// First write
	c, w := doJSONRequest(t, http.MethodPost, "/internal/sandboxes/"+sandboxID+"/pause-snapshot", body)
	store.PostInternalSandboxesSandboxIDPauseSnapshot(c, sandboxID)

	res, err := apispec.ParsePostInternalSandboxesSandboxIDPauseSnapshotResponse(w.Result())
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.StatusCode())
	require.NotNil(t, res.JSON200)
	require.NotEmpty(t, res.JSON200.TemplateId)
	require.NotEqual(t, uuid.Nil, res.JSON200.BuildId)

	templateID := res.JSON200.TemplateId
	buildID := res.JSON200.BuildId

	// Repeat write with a different operation_id is idempotent: same template, new build
	body.OperationId = "op-" + uuid.NewString()
	c, w = doJSONRequest(t, http.MethodPost, "/internal/sandboxes/"+sandboxID+"/pause-snapshot", body)
	store.PostInternalSandboxesSandboxIDPauseSnapshot(c, sandboxID)

	res, err = apispec.ParsePostInternalSandboxesSandboxIDPauseSnapshotResponse(w.Result())
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.StatusCode())
	require.NotNil(t, res.JSON200)
	assert.Equal(t, templateID, res.JSON200.TemplateId)
	assert.NotEqual(t, buildID, res.JSON200.BuildId)

	// Metadata landed in the snapshots row
	row, err := testDB.SqlcClient.GetSnapshotBySandboxID(ctx, sandboxID)
	require.NoError(t, err)
	assert.Equal(t, templateID, row.EnvID)
	assert.Equal(t, teamID, row.TeamID)
	require.NotNil(t, row.BuildID)
	assert.Equal(t, res.JSON200.BuildId, *row.BuildID)

	// The new build starts in snapshotting state
	assert.Equal(t, string(types.BuildStatusSnapshotting), testutils.GetBuildStatus(t, ctx, testDB, res.JSON200.BuildId))
}

func TestInternalLastSnapshot(t *testing.T) {
	t.Parallel()

	store, testDB := newInternalTestStore(t)
	ctx := t.Context()

	teamID := testutils.CreateTestTeam(t, testDB)
	baseTemplateID := testutils.CreateTestTemplate(t, testDB, teamID)
	sandboxID := "sbx-" + uuid.NewString()

	// 404 before any snapshot exists
	c, w := doJSONRequest(t, http.MethodGet, "/internal/sandboxes/"+sandboxID+"/last-snapshot", nil)
	store.GetInternalSandboxesSandboxIDLastSnapshot(c, sandboxID)
	require.Equal(t, http.StatusNotFound, w.Code)

	// Write pause metadata, then mark the build as success so it becomes ready
	body := newRuntimeSnapshotBody(teamID, baseTemplateID, "node-a")
	c, w = doJSONRequest(t, http.MethodPost, "/internal/sandboxes/"+sandboxID+"/pause-snapshot", body)
	store.PostInternalSandboxesSandboxIDPauseSnapshot(c, sandboxID)
	require.Equal(t, http.StatusOK, w.Code)

	pauseRes, err := apispec.ParsePostInternalSandboxesSandboxIDPauseSnapshotResponse(w.Result())
	require.NoError(t, err)

	// Still 404 while the build is in snapshotting state
	c, w = doJSONRequest(t, http.MethodGet, "/internal/sandboxes/"+sandboxID+"/last-snapshot", nil)
	store.GetInternalSandboxesSandboxIDLastSnapshot(c, sandboxID)
	require.Equal(t, http.StatusNotFound, w.Code)

	statusBody := apispec.BuildStatusRequest{OperationId: "op-1", Status: apispec.Success}
	c, w = doJSONRequest(t, http.MethodPost, "/internal/builds/"+pauseRes.JSON200.BuildId.String()+"/status", statusBody)
	store.PostInternalBuildsBuildIDStatus(c, pauseRes.JSON200.BuildId.String())
	require.Equal(t, http.StatusOK, w.Code)

	// Now the resume metadata is available
	c, w = doJSONRequest(t, http.MethodGet, "/internal/sandboxes/"+sandboxID+"/last-snapshot", nil)
	store.GetInternalSandboxesSandboxIDLastSnapshot(c, sandboxID)

	res, err := apispec.ParseGetInternalSandboxesSandboxIDLastSnapshotResponse(w.Result())
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.StatusCode())
	require.NotNil(t, res.JSON200)

	last := res.JSON200
	assert.Equal(t, pauseRes.JSON200.TemplateId, last.TemplateId)
	assert.Equal(t, pauseRes.JSON200.BuildId, last.BuildId)
	require.NotNil(t, last.BaseTemplateId)
	assert.Equal(t, baseTemplateID, *last.BaseTemplateId)
	assert.Equal(t, "node-a", last.OriginNode)
	assert.Equal(t, int64(2), last.Vcpu)
	assert.Equal(t, int64(2048), last.RamMb)
	require.NotNil(t, last.TotalDiskSizeMb)
	assert.Equal(t, int64(1024), *last.TotalDiskSizeMb)
	assert.Equal(t, "6.1.0", last.KernelVersion)
	assert.Equal(t, "1.4.0", last.FirecrackerVersion)
	require.NotNil(t, last.EnvdVersion)
	assert.Equal(t, "v1.0.0", *last.EnvdVersion)
	assert.True(t, last.EnvSecure)
	assert.False(t, last.AutoPause)

	// network config round-tripped through snapshots.config
	require.NotNil(t, last.Network)
	ingress, ok := (*last.Network)["ingress"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, true, ingress["allowPublicAccess"])

	// machine info assembled from cpu_* build columns
	require.NotNil(t, last.MachineInfo)
	assert.Equal(t, "x86_64", (*last.MachineInfo)["cpu_architecture"])

	_ = ctx
}

func TestInternalBuildStatusConflict(t *testing.T) {
	t.Parallel()

	store, testDB := newInternalTestStore(t)
	ctx := t.Context()

	teamID := testutils.CreateTestTeam(t, testDB)
	baseTemplateID := testutils.CreateTestTemplate(t, testDB, teamID)
	sandboxID := "sbx-" + uuid.NewString()

	// 404 for unknown build
	unknownBuild := uuid.NewString()
	c, w := doJSONRequest(t, http.MethodPost, "/internal/builds/"+unknownBuild+"/status", apispec.BuildStatusRequest{OperationId: "op-1", Status: apispec.Success})
	store.PostInternalBuildsBuildIDStatus(c, unknownBuild)
	require.Equal(t, http.StatusNotFound, w.Code)

	// 400 for invalid build ID
	c, w = doJSONRequest(t, http.MethodPost, "/internal/builds/not-a-uuid/status", apispec.BuildStatusRequest{OperationId: "op-1", Status: apispec.Success})
	store.PostInternalBuildsBuildIDStatus(c, "not-a-uuid")
	require.Equal(t, http.StatusBadRequest, w.Code)

	// Create a snapshotting build
	body := newRuntimeSnapshotBody(teamID, baseTemplateID, "node-a")
	c, w = doJSONRequest(t, http.MethodPost, "/internal/sandboxes/"+sandboxID+"/pause-snapshot", body)
	store.PostInternalSandboxesSandboxIDPauseSnapshot(c, sandboxID)
	require.Equal(t, http.StatusOK, w.Code)

	pauseRes, err := apispec.ParsePostInternalSandboxesSandboxIDPauseSnapshotResponse(w.Result())
	require.NoError(t, err)
	buildID := pauseRes.JSON200.BuildId.String()

	// Transition to success
	c, w = doJSONRequest(t, http.MethodPost, "/internal/builds/"+buildID+"/status", apispec.BuildStatusRequest{OperationId: "op-2", Status: apispec.Success})
	store.PostInternalBuildsBuildIDStatus(c, buildID)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, string(types.BuildStatusSuccess), testutils.GetBuildStatus(t, ctx, testDB, pauseRes.JSON200.BuildId))

	// Repeating the same terminal value is idempotent
	c, w = doJSONRequest(t, http.MethodPost, "/internal/builds/"+buildID+"/status", apispec.BuildStatusRequest{OperationId: "op-3", Status: apispec.Success})
	store.PostInternalBuildsBuildIDStatus(c, buildID)
	require.Equal(t, http.StatusOK, w.Code)

	// A conflicting terminal value returns 409
	errMsg := "snapshot upload failed"
	c, w = doJSONRequest(t, http.MethodPost, "/internal/builds/"+buildID+"/status", apispec.BuildStatusRequest{OperationId: "op-4", Status: apispec.Failed, Error: &errMsg})
	store.PostInternalBuildsBuildIDStatus(c, buildID)
	require.Equal(t, http.StatusConflict, w.Code)
	assert.Equal(t, string(types.BuildStatusSuccess), testutils.GetBuildStatus(t, ctx, testDB, pauseRes.JSON200.BuildId))
}

func TestInternalDeleteSnapshot(t *testing.T) {
	t.Parallel()

	store, testDB := newInternalTestStore(t)
	ctx := t.Context()

	teamID := testutils.CreateTestTeam(t, testDB)
	baseTemplateID := testutils.CreateTestTemplate(t, testDB, teamID)
	sandboxID := "sbx-" + uuid.NewString()

	// 404 when there is no paused snapshot
	c, w := doJSONRequest(t, http.MethodDelete, "/internal/sandboxes/"+sandboxID+"/snapshot", nil)
	store.DeleteInternalSandboxesSandboxIDSnapshot(c, sandboxID)
	require.Equal(t, http.StatusNotFound, w.Code)

	// Create a snapshot
	body := newRuntimeSnapshotBody(teamID, baseTemplateID, "node-a")
	c, w = doJSONRequest(t, http.MethodPost, "/internal/sandboxes/"+sandboxID+"/pause-snapshot", body)
	store.PostInternalSandboxesSandboxIDPauseSnapshot(c, sandboxID)
	require.Equal(t, http.StatusOK, w.Code)

	pauseRes, err := apispec.ParsePostInternalSandboxesSandboxIDPauseSnapshotResponse(w.Result())
	require.NoError(t, err)

	// Snapshot state reports paused
	c, w = doJSONRequest(t, http.MethodGet, "/internal/sandboxes/"+sandboxID+"/snapshot-state", nil)
	store.GetInternalSandboxesSandboxIDSnapshotState(c, sandboxID)
	stateRes, err := apispec.ParseGetInternalSandboxesSandboxIDSnapshotStateResponse(w.Result())
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, stateRes.StatusCode())
	require.NotNil(t, stateRes.JSON200)
	assert.True(t, stateRes.JSON200.Paused)
	require.NotNil(t, stateRes.JSON200.TemplateId)
	assert.Equal(t, pauseRes.JSON200.TemplateId, *stateRes.JSON200.TemplateId)

	// Delete: 204 and both rows are soft-deleted
	c, w = doJSONRequest(t, http.MethodDelete, "/internal/sandboxes/"+sandboxID+"/snapshot", nil)
	store.DeleteInternalSandboxesSandboxIDSnapshot(c, sandboxID)
	c.Writer.WriteHeaderNow()
	require.Equal(t, http.StatusNoContent, w.Code)

	env, err := testDB.SqlcClient.GetTemplateIncludingSoftDeleted(ctx, pauseRes.JSON200.TemplateId)
	require.NoError(t, err)
	require.NotNil(t, env.DeletedAt)

	// Snapshot state reports not paused anymore
	c, w = doJSONRequest(t, http.MethodGet, "/internal/sandboxes/"+sandboxID+"/snapshot-state", nil)
	store.GetInternalSandboxesSandboxIDSnapshotState(c, sandboxID)
	stateRes, err = apispec.ParseGetInternalSandboxesSandboxIDSnapshotStateResponse(w.Result())
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, stateRes.StatusCode())
	require.NotNil(t, stateRes.JSON200)
	assert.False(t, stateRes.JSON200.Paused)

	// Repeat delete is 404 (no live snapshot row)
	c, w = doJSONRequest(t, http.MethodDelete, "/internal/sandboxes/"+sandboxID+"/snapshot", nil)
	store.DeleteInternalSandboxesSandboxIDSnapshot(c, sandboxID)
	require.Equal(t, http.StatusNotFound, w.Code)
}

// createInternalSnapshotTemplate creates a checkpoint snapshot template via the internal
// endpoints and returns the created IDs.
func createInternalSnapshotTemplate(t *testing.T, store *APIStore, teamID uuid.UUID, baseTemplateID, sandboxID string) apispec.SnapshotTemplateResponse {
	t.Helper()

	body := newRuntimeSnapshotBody(teamID, baseTemplateID, "node-a")
	c, w := doJSONRequest(t, http.MethodPost, "/internal/sandboxes/"+sandboxID+"/snapshot-templates", body)
	store.PostInternalSandboxesSandboxIDSnapshotTemplates(c, sandboxID)

	res, err := apispec.ParsePostInternalSandboxesSandboxIDSnapshotTemplatesResponse(w.Result())
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.StatusCode())
	require.NotNil(t, res.JSON200)

	assert.Equal(t, id.WithTag(res.JSON200.TemplateId, id.DefaultTag), res.JSON200.SnapshotId)

	return *res.JSON200
}

func TestInternalResolveSnapshot(t *testing.T) {
	t.Parallel()

	store, testDB := newInternalTestStore(t)

	teamID := testutils.CreateTestTeam(t, testDB)
	baseTemplateID := testutils.CreateTestTemplate(t, testDB, teamID)
	otherTeamID := testutils.CreateTestTeam(t, testDB)
	sandboxID := "sbx-" + uuid.NewString()

	// 404 for unknown snapshot reference
	unknownSnapshot := id.WithTag("env-unknown-"+uuid.NewString(), id.DefaultTag)
	c, w := doJSONRequest(t, http.MethodGet, fmt.Sprintf("/internal/snapshots/%s/resolve?team_id=%s", unknownSnapshot, teamID), nil)
	store.GetInternalSnapshotsSnapshotIDResolve(c, unknownSnapshot, apispec.GetInternalSnapshotsSnapshotIDResolveParams{TeamId: teamID})
	require.Equal(t, http.StatusNotFound, w.Code)

	created := createInternalSnapshotTemplate(t, store, teamID, baseTemplateID, sandboxID)

	// 404 while the build is not ready yet
	c, w = doJSONRequest(t, http.MethodGet, fmt.Sprintf("/internal/snapshots/%s/resolve?team_id=%s", created.SnapshotId, teamID), nil)
	store.GetInternalSnapshotsSnapshotIDResolve(c, created.SnapshotId, apispec.GetInternalSnapshotsSnapshotIDResolveParams{TeamId: teamID})
	require.Equal(t, http.StatusNotFound, w.Code)

	// Mark the build as success
	buildID := created.BuildId.String()
	c, w = doJSONRequest(t, http.MethodPost, "/internal/builds/"+buildID+"/status", apispec.BuildStatusRequest{OperationId: "op-1", Status: apispec.Success})
	store.PostInternalBuildsBuildIDStatus(c, buildID)
	require.Equal(t, http.StatusOK, w.Code)

	// 422 for a foreign team
	c, w = doJSONRequest(t, http.MethodGet, fmt.Sprintf("/internal/snapshots/%s/resolve?team_id=%s", created.SnapshotId, otherTeamID), nil)
	store.GetInternalSnapshotsSnapshotIDResolve(c, created.SnapshotId, apispec.GetInternalSnapshotsSnapshotIDResolveParams{TeamId: otherTeamID})
	require.Equal(t, http.StatusUnprocessableEntity, w.Code)

	// 200 for the owning team
	c, w = doJSONRequest(t, http.MethodGet, fmt.Sprintf("/internal/snapshots/%s/resolve?team_id=%s", created.SnapshotId, teamID), nil)
	store.GetInternalSnapshotsSnapshotIDResolve(c, created.SnapshotId, apispec.GetInternalSnapshotsSnapshotIDResolveParams{TeamId: teamID})

	res, err := apispec.ParseGetInternalSnapshotsSnapshotIDResolveResponse(w.Result())
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.StatusCode())
	require.NotNil(t, res.JSON200)

	resolved := res.JSON200
	assert.Equal(t, created.TemplateId, resolved.TemplateId)
	assert.Equal(t, created.BuildId, resolved.BuildId)
	assert.Equal(t, "node-a", resolved.OriginNode)
	assert.Equal(t, int64(2), resolved.Vcpu)
	// Checkpoint templates carry no base env info
	assert.Nil(t, resolved.BaseTemplateId)
	assert.Nil(t, resolved.Aliases)
}

func TestInternalDeleteSnapshotTemplate(t *testing.T) {
	t.Parallel()

	store, testDB := newInternalTestStore(t)

	teamID := testutils.CreateTestTeam(t, testDB)
	baseTemplateID := testutils.CreateTestTemplate(t, testDB, teamID)
	otherTeamID := testutils.CreateTestTeam(t, testDB)
	sandboxID := "sbx-" + uuid.NewString()

	created := createInternalSnapshotTemplate(t, store, teamID, baseTemplateID, sandboxID)

	deleteSnapshotTemplate := func(snapshotRef string, team uuid.UUID) int {
		c, w := doJSONRequest(t, http.MethodDelete, fmt.Sprintf("/internal/snapshot-templates/%s?team_id=%s", snapshotRef, team), nil)
		store.DeleteInternalSnapshotTemplatesSnapshotID(c, snapshotRef, apispec.DeleteInternalSnapshotTemplatesSnapshotIDParams{TeamId: team})
		c.Writer.WriteHeaderNow()

		return w.Code
	}

	// 422 for a foreign team
	require.Equal(t, http.StatusUnprocessableEntity, deleteSnapshotTemplate(created.SnapshotId, otherTeamID))

	// 409 while the checkpoint build is still snapshotting
	require.Equal(t, http.StatusConflict, deleteSnapshotTemplate(created.SnapshotId, teamID))

	// Mark the build as success
	buildID := created.BuildId.String()
	c, w := doJSONRequest(t, http.MethodPost, "/internal/builds/"+buildID+"/status", apispec.BuildStatusRequest{OperationId: "op-1", Status: apispec.Success})
	store.PostInternalBuildsBuildIDStatus(c, buildID)
	require.Equal(t, http.StatusOK, w.Code)

	// 409 while a paused sandbox is mounted on the snapshot template
	pausedSandboxID := "sbx-" + uuid.NewString()
	pauseBody := newRuntimeSnapshotBody(teamID, created.TemplateId, "node-b")
	c, w = doJSONRequest(t, http.MethodPost, "/internal/sandboxes/"+pausedSandboxID+"/pause-snapshot", pauseBody)
	store.PostInternalSandboxesSandboxIDPauseSnapshot(c, pausedSandboxID)
	require.Equal(t, http.StatusOK, w.Code)

	require.Equal(t, http.StatusConflict, deleteSnapshotTemplate(created.SnapshotId, teamID))

	// Unmount the paused sandbox
	c, w = doJSONRequest(t, http.MethodDelete, "/internal/sandboxes/"+pausedSandboxID+"/snapshot", nil)
	store.DeleteInternalSandboxesSandboxIDSnapshot(c, pausedSandboxID)
	c.Writer.WriteHeaderNow()
	require.Equal(t, http.StatusNoContent, w.Code)

	// 204 now
	require.Equal(t, http.StatusNoContent, deleteSnapshotTemplate(created.SnapshotId, teamID))

	// Env row is soft-deleted
	env, err := testDB.SqlcClient.GetTemplateIncludingSoftDeleted(t.Context(), created.TemplateId)
	require.NoError(t, err)
	require.NotNil(t, env.DeletedAt)

	// Repeat delete stays idempotent: 204
	require.Equal(t, http.StatusNoContent, deleteSnapshotTemplate(created.SnapshotId, teamID))
}

func TestInternalDeleteTemplate(t *testing.T) {
	t.Parallel()

	store, testDB := newInternalTestStore(t)

	teamID := testutils.CreateTestTeam(t, testDB)
	otherTeamID := testutils.CreateTestTeam(t, testDB)
	templateID := testutils.CreateTestTemplate(t, testDB, teamID)

	deleteTemplate := func(templateRef string, team uuid.UUID) int {
		c, w := doJSONRequest(t, http.MethodDelete, fmt.Sprintf("/internal/templates/%s?team_id=%s", templateRef, team), nil)
		store.DeleteInternalTemplatesTemplateID(c, templateRef, apispec.DeleteInternalTemplatesTemplateIDParams{TeamId: team})
		c.Writer.WriteHeaderNow()

		return w.Code
	}

	// 422 for a foreign team
	require.Equal(t, http.StatusUnprocessableEntity, deleteTemplate(templateID, otherTeamID))

	// 204 for the owning team
	require.Equal(t, http.StatusNoContent, deleteTemplate(templateID, teamID))

	env, err := testDB.SqlcClient.GetTemplateIncludingSoftDeleted(t.Context(), templateID)
	require.NoError(t, err)
	require.NotNil(t, env.DeletedAt)

	// Repeat delete stays idempotent: 204
	require.Equal(t, http.StatusNoContent, deleteTemplate(templateID, teamID))

	// 404 for an unknown template
	require.Equal(t, http.StatusNotFound, deleteTemplate("env-unknown-"+uuid.NewString(), teamID))
}
