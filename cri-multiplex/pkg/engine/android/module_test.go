package android

import (
	"reflect"
	"testing"

	"github.com/cri-multiplex/pkg/engine"
)

func TestAndroidModuleExposesProductionDescriptorAndCapabilities(t *testing.T) {
	e := &Engine{}
	mod := e.Module()

	want := engine.BackendDescriptor{
		Type:          engine.EngineTypeAndroid,
		Handler:       "android",
		ImagePrefixes: []string{"android.dev/"},
	}
	if !reflect.DeepEqual(mod.Descriptor, want) {
		t.Fatalf("Descriptor = %+v, want %+v", mod.Descriptor, want)
	}
	if mod.Engine != engine.RuntimeEngine(e) {
		t.Fatal("Module.Engine is not the engine itself")
	}
	if mod.Cleaner == nil || mod.Reconciler == nil || mod.Restorer == nil {
		t.Fatalf("capabilities missing: cleaner=%v reconciler=%v restorer=%v", mod.Cleaner, mod.Reconciler, mod.Restorer)
	}
}
