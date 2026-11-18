package providers

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestAgentENVTemplateResolverLiveAPI 需要显式启用，避免普通单元测试依赖外部服务。
func TestAgentENVTemplateResolverLiveAPI(t *testing.T) {
	if os.Getenv("AGENTENV_TEMPLATE_API_E2E") != "1" {
		t.Skip("set AGENTENV_TEMPLATE_API_E2E=1 to run live template API test")
	}
	env := func(key string) string {
		t.Helper()
		value := strings.TrimSpace(os.Getenv(key))
		if value == "" {
			t.Fatalf("%s is required", key)
		}
		return value
	}

	resolver, err := NewAgentENVTemplateResolver(env("AGENTENV_TEMPLATE_API_URL"), env("AGENTENV_TEMPLATE_API_KEY"), 2*time.Second)
	if err != nil {
		t.Fatalf("NewAgentENVTemplateResolver: %v", err)
	}

	namespacedAlias := env("AGENTENV_TEMPLATE_READY_ALIAS")
	bareAlias := namespacedAlias
	if index := strings.LastIndex(bareAlias, "/"); index >= 0 {
		bareAlias = bareAlias[index+1:]
	}
	canonicalID := env("AGENTENV_TEMPLATE_READY_CANONICAL_ID")
	readyBuildID := env("AGENTENV_TEMPLATE_READY_BUILD_ID")

	for _, reference := range []string{namespacedAlias, bareAlias} {
		resolved, err := resolver.ResolveTemplate(context.Background(), reference, "")
		if err != nil {
			t.Fatalf("resolve alias %q: %v", reference, err)
		}
		if resolved.CanonicalTemplateID != canonicalID || resolved.BuildID != readyBuildID {
			t.Fatalf("alias %q resolved to %+v", reference, resolved)
		}
	}

	directID := env("AGENTENV_TEMPLATE_DIRECT_ID")
	directBuildID := env("AGENTENV_TEMPLATE_DIRECT_BUILD_ID")
	resolved, err := resolver.ResolveTemplate(context.Background(), directID, "")
	if err != nil {
		t.Fatalf("resolve direct template ID: %v", err)
	}
	if resolved.CanonicalTemplateID != directID || resolved.BuildID != directBuildID {
		t.Fatalf("direct template resolved to %+v", resolved)
	}

	unreadyAlias := env("AGENTENV_TEMPLATE_UNREADY_ALIAS")
	pinnedBuildID := env("AGENTENV_TEMPLATE_PINNED_BUILD_ID")
	resolved, err = resolver.ResolveTemplate(context.Background(), unreadyAlias, pinnedBuildID)
	if err != nil {
		t.Fatalf("resolve pinned ready build: %v", err)
	}
	if resolved.BuildID != pinnedBuildID {
		t.Fatalf("pinned build resolved to %+v", resolved)
	}

	if _, err := resolver.ResolveTemplate(context.Background(), unreadyAlias, ""); err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("unready latest build error = %v", err)
	}

	badResolver, err := NewAgentENVTemplateResolver(env("AGENTENV_TEMPLATE_API_URL"), "invalid-api-key", 2*time.Second)
	if err != nil {
		t.Fatalf("NewAgentENVTemplateResolver with bad key: %v", err)
	}
	if _, err := badResolver.ResolveTemplate(context.Background(), namespacedAlias, ""); err == nil {
		t.Fatal("invalid API key unexpectedly succeeded")
	}
}
