package admin

import (
	"fmt"
	"strings"

	"github.com/cri-multiplex/pkg/engine"
)

// defaultBackend 是 backend 为空时的目标后端，与 v1 行为一致。
const defaultBackend = string(engine.EngineTypeE2B)

// Registry 是 admin 侧的后端索引：把请求里的 backend 名字解析成 BackendModule。
// 它只读取 Descriptor，不关心 Engine 是否已装配（非 nil 校验属于 engine.Registry）。
type Registry struct {
	modules map[string]*engine.BackendModule
}

// NewRegistry 按 Type、Handler 与 Aliases 建立小写索引；重复键（不同 Module）报错。
func NewRegistry(modules []*engine.BackendModule) (*Registry, error) {
	r := &Registry{modules: make(map[string]*engine.BackendModule, len(modules)*3)}
	for _, mod := range modules {
		if mod == nil {
			return nil, fmt.Errorf("admin registry: nil module")
		}
		desc := mod.Descriptor
		names := make([]string, 0, 2+len(desc.Aliases))
		names = append(names, string(desc.Type), desc.Handler)
		names = append(names, desc.Aliases...)
		for _, name := range names {
			key := strings.ToLower(strings.TrimSpace(name))
			if key == "" {
				continue
			}
			if existing, ok := r.modules[key]; ok && existing != mod {
				return nil, fmt.Errorf("admin registry: name %q is used by both %q and %q", key, existing.Descriptor.Type, desc.Type)
			}
			r.modules[key] = mod
		}
	}
	return r, nil
}

// Resolve 返回 backend 对应的 Module；backend 为空默认 e2b，未注册返回 false。
func (r *Registry) Resolve(backend string) (*engine.BackendModule, bool) {
	if r == nil {
		return nil, false
	}
	key := strings.ToLower(strings.TrimSpace(backend))
	if key == "" {
		key = defaultBackend
	}
	mod, ok := r.modules[key]
	return mod, ok
}
