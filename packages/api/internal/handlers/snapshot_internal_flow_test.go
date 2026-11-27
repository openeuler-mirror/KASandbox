package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/gin-gonic/gin"
	middleware "github.com/oapi-codegen/gin-middleware"
	"github.com/stretchr/testify/require"

	apispec "github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/api/internal/utils"
	"github.com/e2b-dev/infra/packages/auth/pkg/auth"
	"github.com/e2b-dev/infra/packages/db/pkg/testutils"
)

const internalTestAdminToken = "test-admin-token"

// internalSnapshotClient serves the generated routes of store behind the OpenAPI validator
// and the admin token check, as main.go wires them, and calls them over HTTP.
type internalSnapshotClient struct {
	t       *testing.T
	router  *gin.Engine
	teamID  string
	baseEnv string
}

func newInternalSnapshotClient(t *testing.T) *internalSnapshotClient {
	t.Helper()

	store, testDB := newInternalTestStore(t)
	teamUUID := testutils.CreateTestTeam(t, testDB)

	swagger, err := apispec.GetSpec()
	require.NoError(t, err)
	swagger.Servers = nil

	r := gin.New()
	r.Use(middleware.OapiRequestValidatorWithOptions(swagger, &middleware.Options{
		ErrorHandler: func(c *gin.Context, message string, fallbackStatusCode int) {
			utils.ErrorHandler(c, message, max(c.Writer.Status(), fallbackStatusCode))
		},
		MultiErrorHandler: utils.MultiErrorHandler,
		Options: openapi3filter.Options{
			AuthenticationFunc: auth.CreateAuthenticationFunc([]auth.Authenticator{auth.NewAdminApiKeyAuthenticator(internalTestAdminToken)}, nil),
			MultiError:         true,
		},
		SilenceServersWarning: true,
	}))
	apispec.RegisterHandlersWithOptions(r, store, apispec.GinServerOptions{
		ErrorHandler: func(c *gin.Context, err error, statusCode int) {
			utils.ErrorHandler(c, err.Error(), statusCode)
		},
	})

	return &internalSnapshotClient{
		t:       t,
		router:  r,
		teamID:  teamUUID.String(),
		baseEnv: testutils.CreateTestTemplate(t, testDB, teamUUID),
	}
}

func (cl *internalSnapshotClient) call(method, path string, body any, token string) (int, map[string]any) {
	cl.t.Helper()

	reader := bytes.NewReader(nil)
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(cl.t, err)
		reader = bytes.NewReader(raw)
	}

	req := httptest.NewRequestWithContext(cl.t.Context(), method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set(auth.HeaderAdminToken, token)
	}

	rr := httptest.NewRecorder()
	cl.router.ServeHTTP(rr, req)

	out := map[string]any{}
	if rr.Body.Len() > 0 {
		require.NoError(cl.t, json.Unmarshal(rr.Body.Bytes(), &out), rr.Body.String())
	}

	return rr.Code, out
}

func (cl *internalSnapshotClient) do(method, path string, body any) (int, map[string]any) {
	cl.t.Helper()

	return cl.call(method, path, body, internalTestAdminToken)
}

// snapshotBody is the RuntimeSnapshotMetadata body the webhook stub sends.
func (cl *internalSnapshotClient) snapshotBody(operationID, sourceNode string, vcpu int64) map[string]any {
	return map[string]any{
		"operation_id":          operationID,
		"team_id":               cl.teamID,
		"base_template_id":      cl.baseEnv,
		"source_pod_uid":        "pod-uid-" + operationID,
		"source_node_name":      sourceNode,
		"sandbox_started_at":    time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339),
		"vcpu":                  vcpu,
		"ram_mb":                int64(512),
		"total_disk_size_mb":    int64(1024),
		"kernel_version":        "vmlinux-6.1.158",
		"firecracker_version":   "v1.10.1",
		"envd_version":          "v0.3.0",
		"secure":                false,
		"metadata":              map[string]string{"stub": operationID},
		"allow_internet_access": true,
		"auto_pause":            false,
	}
}

func (cl *internalSnapshotClient) setStatus(buildID, operationID, status string) int {
	cl.t.Helper()

	code, _ := cl.do(http.MethodPost, "/internal/builds/"+buildID+"/status", map[string]any{"operation_id": operationID, "status": status})

	return code
}

// TestInternalSnapshotWebhookStubFlow replays the call sequence of KASandbox's
// tests/webhook-stub (scenarios 1-9) over HTTP against a real database.
func TestInternalSnapshotWebhookStubFlow(t *testing.T) {
	t.Parallel()

	cl := newInternalSnapshotClient(t)
	call, do, snapshotBody, setStatus := cl.call, cl.do, cl.snapshotBody, cl.setStatus
	teamID, baseEnv := cl.teamID, cl.baseEnv

	const pauseSandboxID = "stub-sandbox-0001"
	const checkpointSandboxID = "stub-sandbox-0002"
	statePath := "/internal/sandboxes/" + pauseSandboxID + "/snapshot-state"

	// 1. auth
	code, _ := call(http.MethodGet, statePath, nil, "")
	require.Equal(t, http.StatusUnauthorized, code)
	code, _ = call(http.MethodGet, statePath, nil, "wrong-admin-token")
	require.Equal(t, http.StatusUnauthorized, code)
	code, _ = do(http.MethodGet, statePath, nil)
	require.Equal(t, http.StatusOK, code)

	// 2. validation
	invalid := snapshotBody("op-validation-0001", "stub-node-a", 2)
	delete(invalid, "team_id")
	code, _ = do(http.MethodPost, "/internal/sandboxes/"+pauseSandboxID+"/pause-snapshot", invalid)
	require.Equal(t, http.StatusBadRequest, code)

	// 3. pause flow
	code, pause := do(http.MethodPost, "/internal/sandboxes/"+pauseSandboxID+"/pause-snapshot", snapshotBody("op-pause-0001", "stub-node-a", 2))
	require.Equal(t, http.StatusOK, code)
	pauseTemplateID, pauseBuildID1 := pause["template_id"].(string), pause["build_id"].(string)
	require.NotEmpty(t, pauseTemplateID)
	require.NotEmpty(t, pauseBuildID1)
	require.Equal(t, http.StatusOK, setStatus(pauseBuildID1, "op-pause-0001", "success"))

	code, state := do(http.MethodGet, statePath, nil)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, map[string]any{"paused": true, "template_id": pauseTemplateID, "build_id": pauseBuildID1}, state)

	code, last := do(http.MethodGet, "/internal/sandboxes/"+pauseSandboxID+"/last-snapshot", nil)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, pauseTemplateID, last["template_id"])
	require.Equal(t, pauseBuildID1, last["build_id"])
	require.Equal(t, baseEnv, last["base_template_id"])
	require.Equal(t, "stub-node-a", last["origin_node"])
	require.InDelta(t, 2, last["vcpu"], 0)
	require.InDelta(t, 512, last["ram_mb"], 0)
	require.InDelta(t, 1024, last["total_disk_size_mb"], 0)
	require.Equal(t, "vmlinux-6.1.158", last["kernel_version"])
	require.Equal(t, "v1.10.1", last["firecracker_version"])
	require.Equal(t, "v0.3.0", last["envd_version"])
	require.Equal(t, false, last["env_secure"])
	require.Equal(t, map[string]any{"stub": "op-pause-0001"}, last["metadata"])

	// 4. pause again under the same sandbox ID: new build, same template
	code, revive := do(http.MethodPost, "/internal/sandboxes/"+pauseSandboxID+"/pause-snapshot", snapshotBody("op-pause-0002", "stub-node-b", 4))
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, pauseTemplateID, revive["template_id"])
	pauseBuildID2 := revive["build_id"].(string)
	require.NotEqual(t, pauseBuildID1, pauseBuildID2)
	require.Equal(t, http.StatusOK, setStatus(pauseBuildID2, "op-pause-0002", "success"))

	// 5. build status: repeat -> 200, conflicting terminal value -> 409
	require.Equal(t, http.StatusOK, setStatus(pauseBuildID2, "op-pause-0002", "success"))
	require.Equal(t, http.StatusConflict, setStatus(pauseBuildID2, "op-pause-0002", "failed"))

	// 6. checkpoint flow
	code, cp := do(http.MethodPost, "/internal/sandboxes/"+checkpointSandboxID+"/snapshot-templates", snapshotBody("op-checkpoint-0001", "stub-node-a", 2))
	require.Equal(t, http.StatusOK, code)
	cpTemplateID, cpBuildID := cp["template_id"].(string), cp["build_id"].(string)
	cpSnapshotID := cp["snapshot_id"].(string)
	require.Equal(t, cpTemplateID+":default", cpSnapshotID)
	require.Equal(t, http.StatusOK, setStatus(cpBuildID, "op-checkpoint-0001", "success"))

	resolvePath := "/internal/snapshots/" + cpSnapshotID + "/resolve?team_id=" + teamID
	code, resolved := do(http.MethodGet, resolvePath, nil)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, cpTemplateID, resolved["template_id"])
	require.Equal(t, cpBuildID, resolved["build_id"])

	// 7. delete the checkpoint template: 204, repeat 204, resolve 404
	cpPath := "/internal/snapshot-templates/" + cpSnapshotID + "?team_id=" + teamID
	code, _ = do(http.MethodDelete, cpPath, nil)
	require.Equal(t, http.StatusNoContent, code)
	code, _ = do(http.MethodDelete, cpPath, nil)
	require.Equal(t, http.StatusNoContent, code)
	code, _ = do(http.MethodGet, resolvePath, nil)
	require.Equal(t, http.StatusNotFound, code)

	// 8. delete the paused snapshot: 204, repeat 404, paused=false, last-snapshot 404
	snapshotPath := "/internal/sandboxes/" + pauseSandboxID + "/snapshot"
	code, _ = do(http.MethodDelete, snapshotPath, nil)
	require.Equal(t, http.StatusNoContent, code)
	code, _ = do(http.MethodDelete, snapshotPath, nil)
	require.Equal(t, http.StatusNotFound, code)
	code, state = do(http.MethodGet, statePath, nil)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, map[string]any{"paused": false}, state)
	code, _ = do(http.MethodGet, "/internal/sandboxes/"+pauseSandboxID+"/last-snapshot", nil)
	require.Equal(t, http.StatusNotFound, code)

	// 9. error paths
	code, _ = do(http.MethodGet, "/internal/snapshots/nonexistent-snapshot-zz/resolve?team_id="+teamID, nil)
	require.Equal(t, http.StatusNotFound, code)
	code, _ = do(http.MethodGet, "/internal/snapshots/"+baseEnv+":default/resolve?team_id=22222222-2222-2222-2222-222222222222", nil)
	require.Equal(t, http.StatusNotFound, code)
	// The checkpoint sandbox's snapshot row still references the base env -> 409
	code, _ = do(http.MethodDelete, "/internal/templates/"+baseEnv+"?team_id="+teamID, nil)
	require.Equal(t, http.StatusConflict, code)
}

// TestInternalSnapshotVerify22Flow replays the API calls of cri-multiplex
// e2b-verify/22_pause_checkpoint_resume.sh: the checkpoint is taken on the sandbox
// that was paused and resumed, and the deletes run in the script's order.
func TestInternalSnapshotVerify22Flow(t *testing.T) {
	t.Parallel()

	cl := newInternalSnapshotClient(t)
	const sandboxID = "zzzzzzzzzzzzzzzzzzzz"

	// 0.2 token check
	code, _ := cl.call(http.MethodGet, "/internal/sandboxes/"+sandboxID+"/snapshot-state", nil, "")
	require.Equal(t, http.StatusUnauthorized, code)

	// 2.1 pause-snapshot, 2.4 snapshot-state, 2.6 build status
	code, pause := cl.do(http.MethodPost, "/internal/sandboxes/"+sandboxID+"/pause-snapshot", cl.snapshotBody("manual-op-1", "node-1", 2))
	require.Equal(t, http.StatusOK, code)
	pauseTemplateID, pauseBuildID := pause["template_id"].(string), pause["build_id"].(string)

	code, state := cl.do(http.MethodGet, "/internal/sandboxes/"+sandboxID+"/snapshot-state", nil)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, true, state["paused"])
	require.Equal(t, pauseBuildID, state["build_id"])

	require.Equal(t, http.StatusOK, cl.setStatus(pauseBuildID, "manual-op-1", "success"))
	require.Equal(t, http.StatusOK, cl.setStatus(pauseBuildID, "manual-op-1", "success"))
	require.Equal(t, http.StatusConflict, cl.setStatus(pauseBuildID, "manual-op-1", "failed"))

	// 3.1 last-snapshot
	code, last := cl.do(http.MethodGet, "/internal/sandboxes/"+sandboxID+"/last-snapshot", nil)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, pauseTemplateID, last["template_id"])
	require.Equal(t, pauseBuildID, last["build_id"])
	require.Equal(t, "node-1", last["origin_node"])

	// 4.1 snapshot-templates on the resumed sandbox, 4.3 build status, 4.4 resolve
	code, cp := cl.do(http.MethodPost, "/internal/sandboxes/"+sandboxID+"/snapshot-templates", cl.snapshotBody("manual-ckpt-1", "node-1", 2))
	require.Equal(t, http.StatusOK, code)
	cpTemplateID, cpBuildID, cpSnapshotID := cp["template_id"].(string), cp["build_id"].(string), cp["snapshot_id"].(string)
	require.Equal(t, cpTemplateID+":default", cpSnapshotID)
	require.Equal(t, http.StatusOK, cl.setStatus(cpBuildID, "manual-ckpt-1", "success"))

	resolvePath := "/internal/snapshots/" + cpSnapshotID + "/resolve?team_id=" + cl.teamID
	code, resolved := cl.do(http.MethodGet, resolvePath, nil)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, cpTemplateID, resolved["template_id"])
	require.Equal(t, cpBuildID, resolved["build_id"])

	// 6.2 delete checkpoint twice, 6.3 resolve 404
	cpPath := "/internal/snapshot-templates/" + cpSnapshotID + "?team_id=" + cl.teamID
	code, _ = cl.do(http.MethodDelete, cpPath, nil)
	require.Equal(t, http.StatusNoContent, code)
	code, _ = cl.do(http.MethodDelete, cpPath, nil)
	require.Equal(t, http.StatusNoContent, code)
	code, _ = cl.do(http.MethodGet, resolvePath, nil)
	require.Equal(t, http.StatusNotFound, code)

	// 6.4 base template still referenced by the paused snapshot -> 409
	code, _ = cl.do(http.MethodDelete, "/internal/templates/"+cl.baseEnv+"?team_id="+cl.teamID, nil)
	require.Equal(t, http.StatusConflict, code)

	// 6.5 delete the paused snapshot: 204, repeat 404; 6.6 last-snapshot 404; 6.7 paused=false
	code, _ = cl.do(http.MethodDelete, "/internal/sandboxes/"+sandboxID+"/snapshot", nil)
	require.Equal(t, http.StatusNoContent, code)
	code, _ = cl.do(http.MethodDelete, "/internal/sandboxes/"+sandboxID+"/snapshot", nil)
	require.Equal(t, http.StatusNotFound, code)
	code, _ = cl.do(http.MethodGet, "/internal/sandboxes/"+sandboxID+"/last-snapshot", nil)
	require.Equal(t, http.StatusNotFound, code)
	code, state = cl.do(http.MethodGet, "/internal/sandboxes/"+sandboxID+"/snapshot-state", nil)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, map[string]any{"paused": false}, state)

	// With no snapshot left the base template can be deleted
	code, _ = cl.do(http.MethodDelete, "/internal/templates/"+cl.baseEnv+"?team_id="+cl.teamID, nil)
	require.Equal(t, http.StatusNoContent, code)
}
