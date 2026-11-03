package providers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// transformAPIServer 启动一个模拟 /sandboxes/transform 的 httptest.Server。
// cfg 为返回的沙箱配置；status 为 HTTP 状态码；captured 记录最后一次请求体（可选）。
func transformAPIServer(t *testing.T, cfg sandboxConfig, status int, captured *sandboxTransformRequest) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sandboxes/transform" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if captured != nil {
			_ = json.NewDecoder(r.Body).Decode(captured)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(sandboxTransformResponse{Sandbox: cfg})
	}))
}

func sampleConfig(templateID string) sandboxConfig {
	return sandboxConfig{
		TemplateID: templateID,
		BuildID:    "build-1",
		TeamID:     "team-1",
		Vcpu:       2,
		RAMMB:      512,
	}
}
