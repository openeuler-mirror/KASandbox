package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// 检查生成代码内嵌的完整契约，避免只更新 YAML 而遗漏生成结果。
func TestMaintenanceContract(t *testing.T) {
	spec, err := GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	if err := spec.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/internal/sandboxes/{sandboxID}/pause-snapshot",
		"/internal/sandboxes/{sandboxID}/snapshot-templates",
		"/internal/builds/{buildID}/status",
	} {
		op := spec.Paths.Find(path).Post
		if op == nil {
			t.Fatal(path)
		}
		response := op.Responses.Status(503)
		if response == nil || response.Value == nil {
			t.Fatalf("缺少503: %s", path)
		}
		header := response.Value.Headers["Retry-After"]
		if header == nil || !header.Value.Required {
			t.Fatalf("缺少Retry-After: %s", path)
		}
		if len(header.Value.Schema.Value.Enum) != 1 || header.Value.Schema.Value.Enum[0] != "5" {
			t.Fatal(path)
		}
	}
}

// 三个调用入口应能读取新增的结构化错误，并保留重试响应头。
func TestMaintenanceClientResponses(t *testing.T) {
	parsers := map[string]func(*http.Response) (*Maintenance503, *http.Response, error){
		"pause": func(raw *http.Response) (*Maintenance503, *http.Response, error) {
			r, err := ParsePostInternalSandboxesSandboxIDPauseSnapshotResponse(raw)
			if err != nil {
				return nil, nil, err
			}
			return r.JSON503, r.HTTPResponse, nil
		},
		"checkpoint": func(raw *http.Response) (*Maintenance503, *http.Response, error) {
			r, err := ParsePostInternalSandboxesSandboxIDSnapshotTemplatesResponse(raw)
			if err != nil {
				return nil, nil, err
			}
			return r.JSON503, r.HTTPResponse, nil
		},
		"build-status": func(raw *http.Response) (*Maintenance503, *http.Response, error) {
			r, err := ParsePostInternalBuildsBuildIDStatusResponse(raw)
			if err != nil {
				return nil, nil, err
			}
			return r.JSON503, r.HTTPResponse, nil
		},
	}
	for name, parse := range parsers {
		t.Run(name, func(t *testing.T) {
			body, response, err := parse(&http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Header:     http.Header{"Content-Type": {"application/json"}, "Retry-After": {"5"}},
				Body:       io.NopCloser(strings.NewReader(`{"code":503,"message":"runtime is in maintenance mode"}`)),
			})
			if err != nil {
				t.Fatal(err)
			}
			if body == nil || body.Code != 503 || body.Message != "runtime is in maintenance mode" {
				t.Fatalf("错误体未解析: %+v", body)
			}
			if response.Header.Get("Retry-After") != "5" {
				t.Fatal("重试响应头丢失")
			}
		})
	}
}
