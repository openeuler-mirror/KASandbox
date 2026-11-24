package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/e2b/e2b-webhook/provider"
)

func TestCubeProviderUsesExistingAnnotations(t *testing.T) {
	result, err := NewCubeProvider("").Resolve(context.Background(), provider.Request{
		Template: "base",
		Annotations: map[string]string{
			"cube.dev/template-id": "tpl-base",
			"cube.dev/vcpu":        "2",
			"cube.dev/ram-mb":      "2048",
			"cube.dev/disk-mb":     "1G",
		},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if result.RuntimeClass != "cube" || result.Annotations["cube.dev/template-id"] != "tpl-base" {
		t.Fatalf("result = %+v", result)
	}
	if result.Annotations["cube.dev/vcpu"] != "2" || result.Annotations["cube.dev/ram-mb"] != "2048" {
		t.Fatalf("annotations = %v", result.Annotations)
	}
	if result.Annotations["cube.dev/disk-mb"] != "1G" {
		t.Fatalf("disk = %v", result.Annotations)
	}
	for key := range result.Annotations {
		if len(key) >= 12 && key[:12] == "sandbox.cri/" {
			t.Fatalf("unexpected sandbox.cri annotation %q in %v", key, result.Annotations)
		}
	}
}

func TestCubeProviderResolvesTemplateAndDisk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/templates/resolve":
			_, _ = w.Write([]byte(`{"template_id":"tpl-ready","vcpu":4,"ram_mb":8192}`))
		case r.Method == http.MethodGet && r.URL.Path == "/templates/tpl-ready":
			_, _ = w.Write([]byte(`{
				"templateID":"tpl-ready",
				"createRequest":{
					"annotations":{"cube.master.rootfs.writable_layer_size":"1536Mi"},
					"volumes":[{"name":"cube_rootfs_rw","volume_source":{"empty_dir":{"size_limit":"1536Mi"}}}]
				}
			}`))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	result, err := NewCubeProvider(srv.URL).Resolve(context.Background(), provider.Request{Template: "ready"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if result.Annotations["cube.dev/template-id"] != "tpl-ready" || result.Annotations["cube.dev/vcpu"] != "4" || result.Annotations["cube.dev/ram-mb"] != "8192" {
		t.Fatalf("annotations = %v", result.Annotations)
	}
	if result.Annotations["cube.dev/disk-mb"] != "1536Mi" {
		t.Fatalf("disk-mb = %v", result.Annotations)
	}
	if _, ok := result.Annotations["sandbox.cri/backend"]; ok {
		t.Fatalf("should not dual-write sandbox.cri: %v", result.Annotations)
	}
}

func TestCubeProviderFillsDiskWhenSpecPresent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/templates/tpl-ready" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"template_id":"tpl-ready",
			"createRequest":{
				"annotations":{"cube.master.rootfs.writable_layer_size":"2G"}
			}
		}`))
	}))
	defer srv.Close()

	result, err := NewCubeProvider(srv.URL).Resolve(context.Background(), provider.Request{
		Annotations: map[string]string{
			"cube.dev/template-id": "tpl-ready",
			"cube.dev/vcpu":        "2",
			"cube.dev/ram-mb":      "2048",
		},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if result.Annotations["cube.dev/disk-mb"] != "2G" {
		t.Fatalf("annotations = %v", result.Annotations)
	}
}

func TestCubeProviderUsesResolveDiskWhenPresent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/templates/resolve" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"template_id":"tpl-ready","vcpu":2,"ram_mb":2048,"disk_mb":"1024Mi"}`))
	}))
	defer srv.Close()

	result, err := NewCubeProvider(srv.URL).Resolve(context.Background(), provider.Request{Template: "ready"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if result.Annotations["cube.dev/disk-mb"] != "1024Mi" {
		t.Fatalf("annotations = %v", result.Annotations)
	}
}

func TestCubeProviderTeamID(t *testing.T) {
	const configured = "22222222-2222-2222-2222-222222222222"
	const fromRequest = "11111111-1111-1111-1111-111111111111"
	spec := map[string]string{
		"cube.dev/template-id": "tpl-base",
		"cube.dev/vcpu":        "2",
		"cube.dev/ram-mb":      "2048",
		"cube.dev/disk-mb":     "1G",
	}
	resolve := func(extra map[string]string) map[string]string {
		t.Helper()
		anns := map[string]string{}
		for k, v := range spec {
			anns[k] = v
		}
		for k, v := range extra {
			anns[k] = v
		}
		result, err := NewCubeProvider("").Resolve(context.Background(), provider.Request{
			Template:    "base",
			Annotations: anns,
		})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		return result.Annotations
	}

	t.Run("nil uuid when unset", func(t *testing.T) {
		t.Setenv("CUBE_TEAM_ID", "")
		if got := resolve(nil)[annE2BTeamID]; got != nilTeamID {
			t.Fatalf("team-id = %q", got)
		}
	})
	t.Run("configured uuid", func(t *testing.T) {
		t.Setenv("CUBE_TEAM_ID", configured)
		if got := resolve(map[string]string{annE2BTeamID: "team-1"})[annE2BTeamID]; got != configured {
			t.Fatalf("team-id = %q", got)
		}
	})
	t.Run("request uuid wins", func(t *testing.T) {
		t.Setenv("CUBE_TEAM_ID", configured)
		if got := resolve(map[string]string{annE2BTeamID: fromRequest})[annE2BTeamID]; got != fromRequest {
			t.Fatalf("team-id = %q", got)
		}
	})
	t.Run("invalid configured uuid falls back", func(t *testing.T) {
		t.Setenv("CUBE_TEAM_ID", "default")
		if got := resolve(nil)[annE2BTeamID]; got != nilTeamID {
			t.Fatalf("team-id = %q", got)
		}
	})
}

func TestDiskFromTemplatePayload(t *testing.T) {
	disk, err := diskFromTemplatePayload([]byte(`{
		"createRequest":{
			"volumes":[{"volumeSource":{"emptyDir":{"sizeLimit":"940Mi"}}}]
		}
	}`))
	if err != nil || disk != "940Mi" {
		t.Fatalf("disk=%q err=%v", disk, err)
	}
}
