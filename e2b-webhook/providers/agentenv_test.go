package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/e2b/e2b-webhook/provider"
)

type fakeAgentENVResolver struct {
	resolution  TemplateResolution
	err         error
	templateRef string
	buildRef    string
	calls       int
}

func (f *fakeAgentENVResolver) ResolveTemplate(_ context.Context, templateRef string, buildRef string) (TemplateResolution, error) {
	f.calls++
	f.templateRef = templateRef
	f.buildRef = buildRef
	return f.resolution, f.err
}

func TestAgentENVProviderResolveAddsCanonicalAnnotations(t *testing.T) {
	resolver := &fakeAgentENVResolver{
		resolution: TemplateResolution{CanonicalTemplateID: "tpl-canonical", BuildID: "build-ready"},
	}
	providerUnderTest := NewAgentENVProvider(resolver)

	result, err := providerUnderTest.Resolve(context.Background(), provider.Request{
		Kind: "Pod",
		Annotations: map[string]string{
			annAgentENVTemplate: "team/demo-alias",
		},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolver.templateRef != "team/demo-alias" {
		t.Fatalf("template ref = %q", resolver.templateRef)
	}
	if result.RuntimeClass != agentENVProviderName {
		t.Fatalf("runtimeClass = %q", result.RuntimeClass)
	}
	if got := result.Annotations[annAgentENVCanonicalTemplate]; got != "tpl-canonical" {
		t.Fatalf("canonical annotation = %q", got)
	}
	if got := result.Annotations[annAgentENVBuild]; got != "build-ready" {
		t.Fatalf("build annotation = %q", got)
	}
	if _, exists := result.Annotations["sandbox.cri/backend"]; exists {
		t.Fatal("Provider must not inject sandbox.cri/backend")
	}
}

func TestAgentENVProviderResolveKeepsPinnedBuild(t *testing.T) {
	resolver := &fakeAgentENVResolver{
		resolution: TemplateResolution{CanonicalTemplateID: "tpl-canonical", BuildID: "build-ready"},
	}
	providerUnderTest := NewAgentENVProvider(resolver)

	result, err := providerUnderTest.Resolve(context.Background(), provider.Request{
		Annotations: map[string]string{
			annAgentENVTemplate:          "tpl-canonical",
			annAgentENVBuild:             "build-pinned",
			annAgentENVCanonicalTemplate: "tpl-canonical",
		},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolver.buildRef != "build-pinned" {
		t.Fatalf("build ref = %q", resolver.buildRef)
	}
	if _, exists := result.Annotations[annAgentENVBuild]; exists {
		t.Fatal("Provider must not replace the user build annotation")
	}
	if _, exists := result.Annotations[annAgentENVCanonicalTemplate]; exists {
		t.Fatal("Provider must not duplicate an existing canonical annotation")
	}
}

func TestAgentENVProviderRejectsMissingTemplate(t *testing.T) {
	providerUnderTest := NewAgentENVProvider(&fakeAgentENVResolver{})
	_, err := providerUnderTest.Resolve(context.Background(), provider.Request{
		Kind: "BatchSandbox",
	})
	if err == nil || !strings.Contains(err.Error(), annAgentENVTemplate) {
		t.Fatalf("error = %v, want missing template error", err)
	}
}

func TestAgentENVProviderRejectsCanonicalMismatch(t *testing.T) {
	providerUnderTest := NewAgentENVProvider(&fakeAgentENVResolver{
		resolution: TemplateResolution{CanonicalTemplateID: "tpl-canonical", BuildID: "build-ready"},
	})
	_, err := providerUnderTest.Resolve(context.Background(), provider.Request{
		Annotations: map[string]string{
			annAgentENVTemplate:          "tpl-canonical",
			annAgentENVCanonicalTemplate: "tpl-other",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error = %v, want canonical mismatch", err)
	}
}

func TestAgentENVProviderPropagatesResolverError(t *testing.T) {
	want := errors.New("template API unavailable")
	providerUnderTest := NewAgentENVProvider(&fakeAgentENVResolver{err: want})
	_, err := providerUnderTest.Resolve(context.Background(), provider.Request{
		Annotations: map[string]string{annAgentENVTemplate: "demo"},
	})
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func TestAgentENVTemplateResolverResolvesAliasAndLatestBuild(t *testing.T) {
	const apiKey = "test-api-key"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != apiKey {
			t.Errorf("X-API-Key = %q", r.Header.Get("X-API-Key"))
		}
		switch r.URL.Path {
		case "/templates/aliases/demo-alias":
			_, _ = w.Write([]byte(`{"templateID":"tpl-canonical"}`))
		case "/templates/tpl-canonical":
			_, _ = w.Write([]byte(`{"templateID":"tpl-canonical","builds":[{"buildID":"build-old","status":"ready","createdAt":"2026-01-01T00:00:00Z"},{"buildID":"build-new","status":"ready","createdAt":"2026-02-01T00:00:00Z"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	resolver, err := NewAgentENVTemplateResolver(server.URL, apiKey, time.Second)
	if err != nil {
		t.Fatalf("NewAgentENVTemplateResolver: %v", err)
	}
	resolved, err := resolver.ResolveTemplate(context.Background(), "demo-alias", "")
	if err != nil {
		t.Fatalf("ResolveTemplate: %v", err)
	}
	if resolved.CanonicalTemplateID != "tpl-canonical" || resolved.BuildID != "build-new" {
		t.Fatalf("resolved = %+v", resolved)
	}
}

func TestAgentENVTemplateResolverFallsBackToDirectTemplateID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/templates/aliases/tpl-direct":
			http.NotFound(w, r)
		case "/templates/tpl-direct":
			_, _ = w.Write([]byte(`{"templateID":"tpl-direct","builds":[{"buildID":"build-ready","status":"ready","createdAt":"2026-01-01T00:00:00Z"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	resolver, err := NewAgentENVTemplateResolver(server.URL, "", time.Second)
	if err != nil {
		t.Fatalf("NewAgentENVTemplateResolver: %v", err)
	}
	resolved, err := resolver.ResolveTemplate(context.Background(), "tpl-direct", "")
	if err != nil {
		t.Fatalf("ResolveTemplate: %v", err)
	}
	if resolved.CanonicalTemplateID != "tpl-direct" || resolved.BuildID != "build-ready" {
		t.Fatalf("resolved = %+v", resolved)
	}
}

func TestAgentENVTemplateResolverPinsRequestedBuild(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/templates/aliases/demo":
			_, _ = w.Write([]byte(`{"templateID":"tpl-canonical"}`))
		case "/templates/tpl-canonical":
			_, _ = w.Write([]byte(`{"templateID":"tpl-canonical","builds":[{"buildID":"build-pinned","status":"ready","createdAt":"2026-01-01T00:00:00Z"},{"buildID":"build-new","status":"ready","createdAt":"2026-02-01T00:00:00Z"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	resolver, err := NewAgentENVTemplateResolver(server.URL, "", time.Second)
	if err != nil {
		t.Fatalf("NewAgentENVTemplateResolver: %v", err)
	}
	resolved, err := resolver.ResolveTemplate(context.Background(), "demo", "build-pinned")
	if err != nil {
		t.Fatalf("ResolveTemplate: %v", err)
	}
	if resolved.BuildID != "build-pinned" {
		t.Fatalf("build ID = %q", resolved.BuildID)
	}
}

func TestAgentENVTemplateResolverRejectsUnreadyLatestBuild(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/templates/aliases/demo":
			_, _ = w.Write([]byte(`{"templateID":"tpl-canonical"}`))
		case "/templates/tpl-canonical":
			_, _ = w.Write([]byte(`{"templateID":"tpl-canonical","builds":[{"buildID":"build-old","status":"ready","createdAt":"2026-01-01T00:00:00Z"},{"buildID":"build-new","status":"building","createdAt":"2026-02-01T00:00:00Z"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	resolver, err := NewAgentENVTemplateResolver(server.URL, "", time.Second)
	if err != nil {
		t.Fatalf("NewAgentENVTemplateResolver: %v", err)
	}
	_, err = resolver.ResolveTemplate(context.Background(), "demo", "")
	if err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("error = %v, want not ready", err)
	}
}
