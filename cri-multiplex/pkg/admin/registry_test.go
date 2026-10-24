package admin

import (
	"testing"

	"github.com/cri-multiplex/pkg/engine"
)

func TestRegistryResolvesBackendByTypeHandlerAndAlias(t *testing.T) {
	mods := []*engine.BackendModule{
		{Descriptor: engine.BackendDescriptor{Type: engine.EngineTypeE2B, Handler: "e2b"}},
		{Descriptor: engine.BackendDescriptor{Type: "cube", Handler: "cube", Aliases: []string{"cubesandbox"}}},
	}
	reg, err := NewRegistry(mods)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	for input, want := range map[string]string{"": "e2b", "E2B": "e2b", "cube": "cube", "cubesandbox": "cube", "CUBESANDBOX": "cube"} {
		mod, ok := reg.Resolve(input)
		if !ok || string(mod.Descriptor.Type) != want {
			t.Fatalf("Resolve(%q) = (%v, %v), want %s", input, mod, ok, want)
		}
	}
	if _, ok := reg.Resolve("android"); ok {
		t.Fatal("unregistered backend resolved")
	}
}

func TestRegistryRejectsAmbiguousNames(t *testing.T) {
	_, err := NewRegistry([]*engine.BackendModule{
		{Descriptor: engine.BackendDescriptor{Type: "cube", Handler: "cube"}},
		{Descriptor: engine.BackendDescriptor{Type: "other", Handler: "cube"}},
	})
	if err == nil {
		t.Fatal("duplicate handler accepted")
	}
}
