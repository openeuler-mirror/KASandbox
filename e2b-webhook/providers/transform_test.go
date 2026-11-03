package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// =============================================================================
// sandboxConfig.toE2BAnnotations
// =============================================================================

func TestSandboxConfig_ToE2BAnnotations(t *testing.T) {
	internetTrue := true
	token := "secret-token"
	cfg := sandboxConfig{
		SandboxID:           "sbx-1",
		BaseTemplateID:      "base-1",
		TemplateID:          "tpl-xyz",
		BuildID:             "build-1",
		TeamID:              "team-1",
		Vcpu:                4,
		RAMMB:               2048,
		TotalDiskSizeMB:     10240,
		MaxSandboxLength:    600,
		HugePages:           true,
		AutoPause:           true,
		Snapshot:            true,
		AllowInternetAccess: &internetTrue,
		EnvdVersion:         "0.1.0",
		KernelVersion:       "5.10",
		FirecrackerVersion:  "1.0",
		ExecutionID:         "exec-1",
		EnvdAccessToken:     &token,
		Network: &sandboxNetworkConfig{
			Egress: &sandboxNetworkEgressConfig{
				AllowedCidrs: []string{"10.0.0.0/8"},
			},
		},
	}

	annos := cfg.toE2BAnnotations()

	cases := map[string]string{
		"e2b.dev/sandbox-id":          "sbx-1",
		"e2b.dev/base_template_id":    "base-1",
		"e2b.dev/template-id":         "tpl-xyz",
		"e2b.dev/build-id":            "build-1",
		"e2b.dev/team-id":             "team-1",
		"e2b.dev/vcpu":                "4",
		"e2b.dev/ram-mb":              "2048",
		"e2b.dev/total-disk-size-mb":  "10240",
		"e2b.dev/max-sandbox-length":  "600",
		"e2b.dev/huge-pages":          "true",
		"e2b.dev/auto-pause":          "true",
		"e2b.dev/snapshot":            "true",
		"e2b.dev/envd-version":        "0.1.0",
		"e2b.dev/kernel-version":      "5.10",
		"e2b.dev/firecracker-version": "1.0",
		"e2b.dev/execution-id":        "exec-1",
		"e2b.dev/allow-internet":      "true",
		"e2b.dev/envd-access-token":   "secret-token",
		"e2b.dev/network":             `{"egress":{"allowedCidrs":["10.0.0.0/8"]}}`,
		"e2b.dev/env-vars":            "{}",
		"e2b.dev/volume-mounts":       "[]",
		"e2b.dev/auto-resume":         `{"policy":"off"}`,
	}
	for k, want := range cases {
		if got := annos[k]; got != want {
			t.Errorf("annotation %s = %q, want %q", k, got, want)
		}
	}
}

func TestSandboxConfig_ToE2BAnnotations_NilOptionals(t *testing.T) {
	cfg := sandboxConfig{
		TemplateID: "tpl-xyz",
		Vcpu:       1,
		RAMMB:      512,
	}
	annos := cfg.toE2BAnnotations()

	if annos["e2b.dev/allow-internet"] != "true" {
		t.Errorf("nil allow_internet_access should default to true, got %q", annos["e2b.dev/allow-internet"])
	}
	if annos["e2b.dev/envd-access-token"] != "" {
		t.Errorf("nil envd_access_token should default to empty, got %q", annos["e2b.dev/envd-access-token"])
	}
	if annos["e2b.dev/network"] != `{"egress":{},"ingress":{}}` {
		t.Errorf("empty network should default to empty object, got %q", annos["e2b.dev/network"])
	}
	if _, ok := annos["e2b.dev/sandbox-id"]; ok {
		t.Errorf("empty SandboxID should not produce e2b.dev/sandbox-id annotation")
	}
}

// =============================================================================
// SandboxTransformer.Transform
// =============================================================================

func TestTransformCallsAPIWithTemplateName(t *testing.T) {
	var captured sandboxTransformRequest
	server := transformAPIServer(t, sampleConfig("tpl-1"), http.StatusOK, &captured)
	defer server.Close()

	tr := NewSandboxTransformer(server.URL, "key-1", 2*time.Second)
	cfg, err := tr.Transform(context.Background(), "tpl-1")
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	if cfg == nil || cfg.TemplateID != "tpl-1" || cfg.BuildID != "build-1" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if captured.TemplateName != "tpl-1" {
		t.Fatalf("captured templateName = %q", captured.TemplateName)
	}
}

func TestTransformSendsAPIKey(t *testing.T) {
	var gotKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sandboxTransformResponse{Sandbox: sampleConfig("tpl-1")})
	}))
	defer server.Close()

	if _, err := NewSandboxTransformer(server.URL, "secret", time.Second).Transform(context.Background(), "tpl-1"); err != nil {
		t.Fatalf("Transform: %v", err)
	}
	if gotKey != "secret" {
		t.Fatalf("X-API-Key = %q", gotKey)
	}
}

func TestTransformEmptyTemplateSkipsAPI(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
	}))
	defer server.Close()

	cfg, err := NewSandboxTransformer(server.URL, "", time.Second).Transform(context.Background(), "")
	if err != nil || cfg != nil {
		t.Fatalf("Transform = (%+v, %v), want (nil, nil)", cfg, err)
	}
	if calls != 0 {
		t.Fatalf("API called %d times for empty template", calls)
	}
}

func TestTransformAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	if _, err := NewSandboxTransformer(server.URL, "", time.Second).Transform(context.Background(), "tpl-1"); err == nil {
		t.Fatal("expected error on API failure")
	}
}

func TestTransformDoesNotCache(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sandboxTransformResponse{Sandbox: sampleConfig("tpl-1")})
	}))
	defer server.Close()

	tr := NewSandboxTransformer(server.URL, "", time.Second)
	for i := 0; i < 2; i++ {
		if _, err := tr.Transform(context.Background(), "tpl-1"); err != nil {
			t.Fatalf("Transform: %v", err)
		}
	}
	if calls != 2 {
		t.Fatalf("API calls = %d, want 2 (no cache)", calls)
	}
}
