package engine

import (
	"slices"
	"testing"
)

// stubEngine 只实现 Type()；其余 29 个方法由嵌入的 nil 接口提供。
// 仅用于注册表/清理调度单测，禁止在 Mux 或后端行为测试里复用。
type stubEngine struct {
	RuntimeEngine
	typ EngineType
}

func (s *stubEngine) Type() EngineType { return s.typ }

func mustRegister(t *testing.T, reg *Registry, m BackendModule) {
	t.Helper()
	if err := reg.Register(m); err != nil {
		t.Fatalf("Register(%s): %v", m.Descriptor.Type, err)
	}
}

func TestRegistryResolveHandlerNormalizesAndFollowsAliases(t *testing.T) {
	reg := NewRegistry("")
	mustRegister(t, reg, BackendModule{
		Descriptor: BackendDescriptor{Type: EngineTypeE2B, Handler: "e2b", Aliases: []string{"e2b-sandbox"}},
		Engine:     &stubEngine{typ: EngineTypeE2B},
	})
	for _, handler := range []string{"e2b", "E2B", " e2b ", "e2b-sandbox"} {
		mod, ok := reg.ResolveHandler(handler)
		if !ok || mod.Descriptor.Type != EngineTypeE2B {
			t.Fatalf("ResolveHandler(%q) = (%v, %v), want e2b", handler, mod, ok)
		}
	}
	if _, ok := reg.ResolveHandler(""); ok {
		t.Fatal("empty handler must not resolve")
	}
	if _, ok := reg.ResolveHandler("runc"); ok {
		t.Fatal("unregistered handler must not resolve")
	}
}

func TestRegistryRejectsInvalidModules(t *testing.T) {
	reg := NewRegistry("")
	mustRegister(t, reg, BackendModule{
		Descriptor: BackendDescriptor{Type: EngineTypeE2B, Handler: "e2b", Aliases: []string{"sandbox"}, ImagePrefixes: []string{"e2b.dev/"}},
		Engine:     &stubEngine{typ: EngineTypeE2B},
	})
	cases := []struct {
		name string
		mod  BackendModule
	}{
		{"duplicate handler", BackendModule{Descriptor: BackendDescriptor{Type: EngineTypeAndroid, Handler: "e2b"}, Engine: &stubEngine{typ: EngineTypeAndroid}}},
		{"duplicate alias", BackendModule{Descriptor: BackendDescriptor{Type: EngineTypeAndroid, Handler: "android", Aliases: []string{"sandbox"}}, Engine: &stubEngine{typ: EngineTypeAndroid}}},
		{"duplicate image prefix", BackendModule{Descriptor: BackendDescriptor{Type: EngineTypeAndroid, Handler: "android", ImagePrefixes: []string{"e2b.dev/"}}, Engine: &stubEngine{typ: EngineTypeAndroid}}},
		{"type mismatch", BackendModule{Descriptor: BackendDescriptor{Type: EngineTypeAndroid, Handler: "android"}, Engine: &stubEngine{typ: EngineTypeE2B}}},
		{"nil engine", BackendModule{Descriptor: BackendDescriptor{Type: EngineTypeAndroid, Handler: "android"}}},
		{"empty handler", BackendModule{Descriptor: BackendDescriptor{Type: EngineTypeAndroid}, Engine: &stubEngine{typ: EngineTypeAndroid}}},
	}
	for _, tc := range cases {
		if err := reg.Register(tc.mod); err == nil {
			t.Fatalf("%s: Register accepted an invalid module", tc.name)
		}
	}
}

func TestRegistryResolveImageUsesLongestPrefix(t *testing.T) {
	reg := NewRegistry("")
	mustRegister(t, reg, BackendModule{Descriptor: BackendDescriptor{Type: EngineTypeE2B, Handler: "e2b", ImagePrefixes: []string{"e2b.dev/"}}, Engine: &stubEngine{typ: EngineTypeE2B}})
	mustRegister(t, reg, BackendModule{Descriptor: BackendDescriptor{Type: "cube", Handler: "cube", ImagePrefixes: []string{"e2b.dev/cube/"}}, Engine: &stubEngine{typ: "cube"}})

	if mod, ok := reg.ResolveImage("e2b.dev/cube/base:latest"); !ok || mod.Descriptor.Type != "cube" {
		t.Fatalf("longest prefix = (%v, %v), want cube", mod, ok)
	}
	if mod, ok := reg.ResolveImage("e2b.dev/base:latest"); !ok || mod.Descriptor.Type != EngineTypeE2B {
		t.Fatalf("shorter prefix = (%v, %v), want e2b", mod, ok)
	}
	if _, ok := reg.ResolveImage("docker.io/library/alpine"); ok {
		t.Fatal("unrouted image must not resolve")
	}
}

func TestRegistryFreezeRejectsRegistrationAndOrdersModules(t *testing.T) {
	reg := NewRegistry(EngineTypeContainer)
	mustRegister(t, reg, BackendModule{Descriptor: BackendDescriptor{Type: EngineTypeE2B, Handler: "e2b"}, Engine: &stubEngine{typ: EngineTypeE2B}})
	mustRegister(t, reg, BackendModule{Descriptor: BackendDescriptor{Type: EngineTypeContainer, Handler: "container"}, Engine: &stubEngine{typ: EngineTypeContainer}})
	mustRegister(t, reg, BackendModule{Descriptor: BackendDescriptor{Type: EngineTypeAndroid, Handler: "android"}, Engine: &stubEngine{typ: EngineTypeAndroid}})

	if err := reg.Freeze(); err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if err := reg.Register(BackendModule{Descriptor: BackendDescriptor{Type: "cube", Handler: "cube"}, Engine: &stubEngine{typ: "cube"}}); err == nil {
		t.Fatal("Register after Freeze succeeded")
	}
	var got []EngineType
	for _, mod := range reg.Modules() {
		got = append(got, mod.Descriptor.Type)
	}
	if want := []EngineType{EngineTypeAndroid, EngineTypeContainer, EngineTypeE2B}; !slices.Equal(got, want) {
		t.Fatalf("Modules order = %v, want %v", got, want)
	}
	if def := reg.Default(); def == nil || def.Descriptor.Type != EngineTypeContainer {
		t.Fatalf("Default() = %v, want container", def)
	}
	if err := reg.Freeze(); err != nil {
		t.Fatalf("second Freeze must be a no-op, got %v", err)
	}
}

func TestRegistryFreezeRequiresDefaultBackend(t *testing.T) {
	if err := NewRegistry(EngineTypeContainer).Freeze(); err == nil {
		t.Fatal("Freeze without the default backend succeeded")
	}
	if err := NewRegistry("").Freeze(); err != nil {
		t.Fatalf("Freeze without a default requirement: %v", err)
	}
}
