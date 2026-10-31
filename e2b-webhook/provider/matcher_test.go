package provider

import (
	"context"
	"testing"
)

type fakeProvider struct{ handler string }

func (f *fakeProvider) Name() string    { return f.handler }
func (f *fakeProvider) Handler() string { return f.handler }
func (f *fakeProvider) Resolve(context.Context, Request) (*Result, error) {
	return &Result{Annotations: map[string]string{"test/handler": f.handler}}, nil
}

func TestMatcherSelectsByRuntimeClass(t *testing.T) {
	e2b := &fakeProvider{handler: "e2b"}
	cube := &fakeProvider{handler: "cube"}
	m, err := NewMatcher(e2b, cube)
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}

	if sel, ok := m.Select(Request{RuntimeClass: "cube"}); !ok || sel.Provider != cube || sel.Source != "runtimeClass" {
		t.Fatalf("runtimeClass selection = (%v, %v)", sel, ok)
	}
	// 注解不参与选路，只有注解时放行。
	if sel, ok := m.Select(Request{Annotations: map[string]string{"test/handler": "cube"}}); ok {
		t.Fatalf("annotation-only selection = %v, want pass-through", sel)
	}
}

func TestMatcherRuntimeClassWinsOverAnnotations(t *testing.T) {
	e2b := &fakeProvider{handler: "e2b"}
	cube := &fakeProvider{handler: "cube"}
	m, _ := NewMatcher(e2b, cube)
	sel, ok := m.Select(Request{RuntimeClass: "e2b", Annotations: map[string]string{"test/handler": "cube"}})
	if !ok || sel.Provider != e2b || sel.Source != "runtimeClass" {
		t.Fatalf("selection = (%v, %v), want e2b by runtimeClass", sel, ok)
	}
}

func TestMatcherPassesThroughUnknownSignals(t *testing.T) {
	m, _ := NewMatcher(&fakeProvider{handler: "e2b"})
	for _, req := range []Request{
		{RuntimeClass: "runc"},
		{},
		{Annotations: map[string]string{"test/handler": "unknown"}},
		{Annotations: map[string]string{"test/handler": "e2b"}},
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
