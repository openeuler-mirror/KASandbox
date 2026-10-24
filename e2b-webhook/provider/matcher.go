package provider

import (
	"fmt"
	"strings"

	"k8s.io/klog/v2"
)

// Matcher 按 P1 runtimeClass、P2 sandbox.cri/backend 的顺序选择 Provider。
type Matcher struct {
	providers []Provider
	byHandler map[string]Provider
}

// NewMatcher 构造选择器；Handler 重复即报错。
func NewMatcher(providers ...Provider) (*Matcher, error) {
	m := &Matcher{
		providers: make([]Provider, 0, len(providers)),
		byHandler: make(map[string]Provider, len(providers)),
	}
	for _, p := range providers {
		if p == nil {
			continue
		}
		handler := p.Handler()
		if handler == "" {
			return nil, fmt.Errorf("provider %q has an empty handler", p.Name())
		}
		if _, exists := m.byHandler[handler]; exists {
			return nil, fmt.Errorf("duplicate provider handler %q", handler)
		}
		m.byHandler[handler] = p
		m.providers = append(m.providers, p)
	}
	return m, nil
}

// Resolve 按 Handler 查 Provider（用于旧版默认选路：android env / 默认 e2b）。
func (m *Matcher) Resolve(handler string) (Provider, bool) {
	if m == nil {
		return nil, false
	}
	p, ok := m.byHandler[strings.TrimSpace(handler)]
	return p, ok
}

// Select 返回命中的 Provider 与来源；都没有命中时返回 false（放行且不注入）。
func (m *Matcher) Select(req Request) (*Selection, bool) {
	if m == nil {
		return nil, false
	}
	runtimeClass := strings.TrimSpace(req.RuntimeClass)
	annotation := ""
	if req.Annotations != nil {
		annotation = strings.TrimSpace(req.Annotations[AnnBackend])
	}

	byRuntimeClass, runtimeClassHit := m.byHandler[runtimeClass]
	byAnnotation, annotationHit := m.byHandler[annotation]
	if runtimeClassHit && annotationHit && byRuntimeClass != byAnnotation {
		klog.Warningf("webhook backend conflict: runtimeClass=%s annotation=%s (runtimeClass wins)", runtimeClass, annotation)
	}
	if runtimeClassHit {
		return &Selection{Provider: byRuntimeClass, Source: "runtimeClass"}, true
	}
	if annotationHit {
		return &Selection{Provider: byAnnotation, Source: "annotation"}, true
	}
	return nil, false
}
