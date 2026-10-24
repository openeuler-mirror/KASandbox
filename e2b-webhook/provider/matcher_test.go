package provider

import (
	"context"
	"testing"
)

type fakeProvider struct{ handler string }

func (f *fakeProvider) Name() string    { return f.handler }
func (f *fakeProvider) Handler() string { return f.handler }
func (f *fakeProvider) Resolve(context.Context, Request) (*Result, error) {
	return &Result{Annotations: map[string]string{AnnBackend: f.handler}}, nil
}

func TestMatcherSelectsByRuntimeClassThenAnnotation(t *testing.T) {
	e2b := &fakeProvider{handler: "e2b"}
	cube := &fakeProvider{handler: "cube"}
	m, err := NewMatcher(e2b, cube)
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}

	if sel, ok := m.Select(Request{RuntimeClass: "cube"}); !ok || sel.Provider != cube || sel.Source != "runtimeClass" {
		t.Fatalf("runtimeClass selection = (%v, %v)", sel, ok)
	}
	if sel, ok := m.Select(Request{Annotations: map[string]string{AnnBackend: "cube"}}); !ok || sel.Provider != cube || sel.Source != "annotation" {
		t.Fatalf("annotation selection = (%v, %v)", sel, ok)
	}
}

func TestMatcherRuntimeClassWinsConflict(t *testing.T) {
	e2b := &fakeProvider{handler: "e2b"}
	cube := &fakeProvider{handler: "cube"}
	m, _ := NewMatcher(e2b, cube)
	sel, ok := m.Select(Request{RuntimeClass: "e2b", Annotations: map[string]string{AnnBackend: "cube"}})
	if !ok || sel.Provider != e2b || sel.Source != "runtimeClass" {
		t.Fatalf("conflict selection = (%v, %v), want e2b", sel, ok)
	}
}

func TestMatcherPassesThroughUnknownSignals(t *testing.T) {
	m, _ := NewMatcher(&fakeProvider{handler: "e2b"})
	for _, req := range []Request{
		{RuntimeClass: "runc"},
		{},
		{Annotations: map[string]string{AnnBackend: "unknown"}},
	} {
		if sel, ok := m.Select(req); ok {
			t.Fatalf("Select(%+v) = %v, want pass-through", req, sel)
		}
	}
}

func TestNewMatcherRejectsDuplicateHandler(t *testing.T) {
	if _, err := NewMatcher(&fakeProvider{handler: "e2b"}, &fakeProvider{handler: "e2b"}); err == nil {
		t.Fatal("duplicate handler accepted")
	}
}

func TestUnifiedSkipsEmptyValues(t *testing.T) {
	got := Unified("cube", "", "tmpl")
	want := map[string]string{AnnBackend: "cube", AnnTemplate: "tmpl"}
	if len(got) != len(want) || got[AnnBackend] != "cube" || got[AnnTemplate] != "tmpl" {
		t.Fatalf("Unified = %v, want %v", got, want)
	}
	if _, ok := got[AnnSandboxID]; ok {
		t.Fatal("empty sandbox id must be skipped, not emitted empty")
	}
}

func TestMergeKeepsExistingKeys(t *testing.T) {
	dst := map[string]string{"user": "keep"}
	Merge(dst, map[string]string{"user": "override", "new": "v"})
	if dst["user"] != "keep" || dst["new"] != "v" {
		t.Fatalf("Merge = %v", dst)
	}
}
