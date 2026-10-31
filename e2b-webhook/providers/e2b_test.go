package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/e2b/e2b-webhook/provider"
)

// newTestTransformer 返回一个指向固定 JSON 响应的 transform 客户端。
func newTestTransformer(t *testing.T, body string) *SandboxTransformer {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return NewSandboxTransformer(server.URL, "", time.Second)
}

func TestE2BProviderProducesE2BAnnotations(t *testing.T) {
	transformer := newTestTransformer(t, `{"sandbox":{"templateId":"tmpl","buildId":"build","teamId":"team","vcpu":2,"ramMb":1024,"totalDiskSizeMb":4096,"envdVersion":"0.2.0","sandboxId":"sb1"}}`)
	p := NewE2BProvider(transformer)

	result, err := p.Resolve(context.Background(), provider.Request{Template: "tmpl", Name: "pod-1"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if result.RuntimeClass != "e2b" {
		t.Fatalf("RuntimeClass = %q", result.RuntimeClass)
	}
	for key, want := range map[string]string{
		"e2b.dev/template-id":  "tmpl",
		"e2b.dev/build-id":     "build",
		"e2b.dev/team-id":      "team",
		"e2b.dev/vcpu":         "2",
		"e2b.dev/ram-mb":       "1024",
		"e2b.dev/envd-version": "0.2.0",
		"e2b.dev/sandbox-id":   "sb1",
	} {
		if result.Annotations[key] != want {
			t.Fatalf("%s = %q, want %q (annotations=%v)", key, result.Annotations[key], want, result.Annotations)
		}
	}
	for key := range result.Annotations {
		if strings.HasPrefix(key, "sandbox.cri/") {
			t.Fatalf("unexpected unified annotation %q (annotations=%v)", key, result.Annotations)
		}
	}
}

func TestE2BProviderWithoutTemplateSkipsResolution(t *testing.T) {
	p := NewE2BProvider(newTestTransformer(t, `{}`))
	result, err := p.Resolve(context.Background(), provider.Request{Name: "pod-1"})
	if err != nil || result != nil {
		t.Fatalf("Resolve = (%v, %v), want (nil, nil)", result, err)
	}
}

func TestE2BProviderPropagatesAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	p := NewE2BProvider(NewSandboxTransformer(server.URL, "", time.Second))
	if _, err := p.Resolve(context.Background(), provider.Request{Template: "tmpl"}); err == nil {
		t.Fatal("expected error when transform API fails")
	}
}

func TestE2BProviderIdentity(t *testing.T) {
	p := NewE2BProvider(newTestTransformer(t, `{}`))
	if p.Name() != "e2b" || p.Handler() != "e2b" {
		t.Fatalf("Name/Handler = %q/%q", p.Name(), p.Handler())
	}
}
