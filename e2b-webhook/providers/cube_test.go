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
	for key := range result.Annotations {
		if len(key) >= 12 && key[:12] == "sandbox.cri/" {
			t.Fatalf("unexpected sandbox.cri annotation %q in %v", key, result.Annotations)
		}
	}
}

func TestCubeProviderResolvesTemplate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/templates/resolve" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"template_id":"tpl-ready","vcpu":4,"ram_mb":8192}`))
	}))
	defer srv.Close()

	result, err := NewCubeProvider(srv.URL).Resolve(context.Background(), provider.Request{Template: "ready"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if result.Annotations["cube.dev/template-id"] != "tpl-ready" || result.Annotations["cube.dev/vcpu"] != "4" || result.Annotations["cube.dev/ram-mb"] != "8192" {
		t.Fatalf("annotations = %v", result.Annotations)
	}
	if _, ok := result.Annotations["sandbox.cri/backend"]; ok {
		t.Fatalf("should not dual-write sandbox.cri: %v", result.Annotations)
	}
}
