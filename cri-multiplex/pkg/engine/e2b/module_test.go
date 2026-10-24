package e2b

import (
	"reflect"
	"testing"

	"github.com/cri-multiplex/pkg/engine"
)

func TestModuleExposesDescriptorAndCapabilities(t *testing.T) {
	e := &Engine{}
	mod := e.Module()
	if !reflect.DeepEqual(mod.Descriptor, Descriptor) {
		t.Fatalf("Descriptor = %+v, want %+v", mod.Descriptor, Descriptor)
	}
	if mod.Descriptor.Type != engine.EngineTypeE2B || mod.Descriptor.Handler != "e2b" {
		t.Fatalf("descriptor = %+v", mod.Descriptor)
	}
	if mod.Engine != engine.RuntimeEngine(e) {
		t.Fatal("Module.Engine is not the engine itself")
	}
	if mod.Cleaner == nil || mod.Reconciler == nil || mod.Restorer == nil {
		t.Fatalf("capabilities missing: cleaner=%v reconciler=%v restorer=%v", mod.Cleaner, mod.Reconciler, mod.Restorer)
	}
}
