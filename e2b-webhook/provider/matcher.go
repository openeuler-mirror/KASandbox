package provider

import (
	"fmt"
	"strings"
)

// Matcher 按 runtimeClass 选择 Provider
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

// Select 只按 runtimeClass 返回命中的 Provider；未命中时返回 false，
// 由调用方决定走旧版默认选路还是放行。
func (m *Matcher) Select(req Request) (*Selection, bool) {
	if m == nil {
		return nil, false
	}
	runtimeClass := strings.TrimSpace(req.RuntimeClass)
	if p, ok := m.byHandler[runtimeClass]; ok {
		return &Selection{Provider: p, Source: "runtimeClass"}, true
	}
	return nil, false
}
