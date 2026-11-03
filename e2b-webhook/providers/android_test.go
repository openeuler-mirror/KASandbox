package providers

import (
	"context"
	"testing"

	"github.com/e2b/e2b-webhook/provider"
)

func TestAndroidProviderReturnsRuntimeClassOnly(t *testing.T) {
	result, err := NewAndroidProvider().Resolve(context.Background(), provider.Request{Template: "ignored"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if result.RuntimeClass != "android" {
		t.Fatalf("RuntimeClass = %q", result.RuntimeClass)
	}
	if len(result.Annotations) != 0 {
		t.Fatalf("annotations = %v, want none", result.Annotations)
	}
}

func TestAndroidProviderIdentity(t *testing.T) {
	p := NewAndroidProvider()
	if p.Name() != "android" || p.Handler() != "android" {
		t.Fatalf("Name/Handler = %q/%q", p.Name(), p.Handler())
	}
}
