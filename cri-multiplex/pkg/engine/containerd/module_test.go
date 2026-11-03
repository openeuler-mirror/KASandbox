package containerd

import (
	"reflect"
	"testing"

	"github.com/cri-multiplex/pkg/engine"
)

func TestContainerdModuleIsEngineOnly(t *testing.T) {
	e := &Engine{}
	mod := e.Module()

	want := engine.BackendDescriptor{
		Type:    engine.EngineTypeContainer,
		Handler: "container",
	}
	if !reflect.DeepEqual(mod.Descriptor, want) {
		t.Fatalf("Descriptor = %+v, want %+v", mod.Descriptor, want)
	}
	if mod.Engine != engine.RuntimeEngine(e) {
		t.Fatal("Module.Engine is not the engine itself")
	}
	if mod.Cleaner != nil || mod.Reconciler != nil || mod.Restorer != nil || mod.Health != nil {
		t.Fatalf("containerd must not advertise optional capabilities: %+v", mod)
	}
}
