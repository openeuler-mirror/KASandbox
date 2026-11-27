package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/gin-gonic/gin"
	middleware "github.com/oapi-codegen/gin-middleware"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/api/internal/utils"
	"github.com/e2b-dev/infra/packages/auth/pkg/auth"
)

// TestInternalSnapshotRoutesAuthAndValidation checks the /internal snapshot routes at
// the OpenAPI validator, the way the webhook stub and e2b-verify 22 call them: the
// X-Admin-Token header is the only credential, and malformed requests stop with 400.
func TestInternalSnapshotRoutesAuthAndValidation(t *testing.T) {
	t.Parallel()

	swagger, err := api.GetSpec()
	require.NoError(t, err)
	swagger.Servers = nil

	r := gin.New()
	r.Use(middleware.OapiRequestValidatorWithOptions(swagger, &middleware.Options{
		ErrorHandler: func(c *gin.Context, message string, fallbackStatusCode int) {
			statusCode := max(c.Writer.Status(), fallbackStatusCode)
			utils.ErrorHandler(c, message, statusCode)
		},
		MultiErrorHandler: utils.MultiErrorHandler,
		Options: openapi3filter.Options{
			AuthenticationFunc: auth.CreateAuthenticationFunc([]auth.Authenticator{auth.NewAdminApiKeyAuthenticator("admin-token")}, nil),
			MultiError:         true,
		},
		SilenceServersWarning: true,
	}))
	r.Any("/*any", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	const teamID = "11111111-1111-1111-1111-111111111111"
	validBody := `{"operation_id":"op","team_id":"` + teamID + `","base_template_id":"base","source_pod_uid":"uid",` +
		`"source_node_name":"node","sandbox_started_at":"2026-11-03T10:00:00Z","vcpu":2,"ram_mb":512,` +
		`"total_disk_size_mb":1024,"kernel_version":"k","firecracker_version":"f","envd_version":"e","secure":false}`

	for _, tc := range []struct {
		name   string
		method string
		path   string
		token  string
		body   string
		want   int
	}{
		{"no token", http.MethodGet, "/internal/sandboxes/sbx/snapshot-state", "", "", http.StatusUnauthorized},
		{"wrong token", http.MethodGet, "/internal/sandboxes/sbx/snapshot-state", "wrong-admin-token", "", http.StatusUnauthorized},
		{"valid token", http.MethodGet, "/internal/sandboxes/sbx/snapshot-state", "admin-token", "", http.StatusOK},
		{"pause snapshot", http.MethodPost, "/internal/sandboxes/sbx/pause-snapshot", "admin-token", validBody, http.StatusOK},
		{"pause snapshot without team_id", http.MethodPost, "/internal/sandboxes/sbx/pause-snapshot", "admin-token", strings.Replace(validBody, `"team_id":"`+teamID+`",`, "", 1), http.StatusBadRequest},
		{"checkpoint", http.MethodPost, "/internal/sandboxes/sbx/snapshot-templates", "admin-token", validBody, http.StatusOK},
		{"last snapshot", http.MethodGet, "/internal/sandboxes/sbx/last-snapshot", "admin-token", "", http.StatusOK},
		{"build status", http.MethodPost, "/internal/builds/b/status", "admin-token", `{"operation_id":"op","status":"success"}`, http.StatusOK},
		{"build status outside the enum", http.MethodPost, "/internal/builds/b/status", "admin-token", `{"operation_id":"op","status":"building"}`, http.StatusBadRequest},
		{"delete snapshot", http.MethodDelete, "/internal/sandboxes/sbx/snapshot", "admin-token", "", http.StatusOK},
		{"resolve", http.MethodGet, "/internal/snapshots/tmpl:default/resolve?team_id=" + teamID, "admin-token", "", http.StatusOK},
		{"resolve without team_id", http.MethodGet, "/internal/snapshots/tmpl:default/resolve", "admin-token", "", http.StatusBadRequest},
		{"delete checkpoint", http.MethodDelete, "/internal/snapshot-templates/tmpl:default?team_id=" + teamID, "admin-token", "", http.StatusOK},
		{"delete template", http.MethodDelete, "/internal/templates/tmpl?team_id=" + teamID, "admin-token", "", http.StatusOK},
		{"delete template without team_id", http.MethodDelete, "/internal/templates/tmpl", "admin-token", "", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, strings.NewReader(tc.body))
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			if tc.token != "" {
				req.Header.Set(auth.HeaderAdminToken, tc.token)
			}

			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			require.Equal(t, tc.want, rr.Code, rr.Body.String())
		})
	}
}
