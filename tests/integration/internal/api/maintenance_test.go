package api

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

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
