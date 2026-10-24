package engine

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Deps 是后端构造时共享的依赖集合。
type Deps struct {
	Store    StateStore
	NodeName string
	NodeIP   string
}

// BackendDescriptor 是后端的静态标识：CRI handler、handler 别名与镜像前缀。
// Handler/别名在 Register 时规范化（小写 + trim）；ImagePrefixes 大小写敏感。
type BackendDescriptor struct {
	Type          EngineType
	Handler       string
	Aliases       []string
	ImagePrefixes []string
}

// BackendModule 是注册表的最小单元：一个后端 + 它可选实现的能力。
type BackendModule struct {
	Descriptor BackendDescriptor
	Engine     RuntimeEngine
	Cleaner    Cleaner
	Reconciler Reconciler
	Restorer   StateRestorer
	Health     HealthChecker
}

type imageRoute struct {
	prefix  string
	backend EngineType
}

// Registry 是唯一的后端装配点：所有路由解析都经过它，不允许按名字分支。
type Registry struct {
	mu          sync.RWMutex
	modules     map[EngineType]*BackendModule
	handlers    map[string]EngineType
	imageRoutes []imageRoute
	ordered     []*BackendModule
	snapshot    []*BackendModule
	defaultType EngineType
	frozen      bool
}

// NewRegistry 创建注册表。defaultType 为空表示"不要求默认后端"。
func NewRegistry(defaultType EngineType) *Registry {
	return &Registry{
		modules:     make(map[EngineType]*BackendModule),
		handlers:    make(map[string]EngineType),
		defaultType: defaultType,
	}
}

// Register 校验并登记一个后端模块；全部校验在写入前完成，失败不留半成品。
func (r *Registry) Register(m BackendModule) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.frozen {
		return fmt.Errorf("engine registry is frozen")
	}
	if m.Engine == nil {
		return fmt.Errorf("backend %q: nil engine", m.Descriptor.Type)
	}
	if m.Descriptor.Type == "" {
		return fmt.Errorf("backend handler %q: empty backend type", m.Descriptor.Handler)
	}
	if strings.TrimSpace(m.Descriptor.Handler) == "" {
		return fmt.Errorf("backend %q: empty handler", m.Descriptor.Type)
	}
	if m.Engine.Type() != m.Descriptor.Type {
		return fmt.Errorf("backend %q: engine type is %q", m.Descriptor.Type, m.Engine.Type())
	}
	if _, exists := r.modules[m.Descriptor.Type]; exists {
		return fmt.Errorf("backend %q: already registered", m.Descriptor.Type)
	}

	handler := normalizeHandler(m.Descriptor.Handler)
	if _, taken := r.handlers[handler]; taken {
		return fmt.Errorf("backend %q: handler %q already taken", m.Descriptor.Type, handler)
	}

	aliases := make([]string, 0, len(m.Descriptor.Aliases))
	seen := make(map[string]struct{}, len(m.Descriptor.Aliases))
	for _, raw := range m.Descriptor.Aliases {
		alias := normalizeHandler(raw)
		if alias == "" || alias == handler {
			continue
		}
		if _, dup := seen[alias]; dup {
			continue
		}
		if _, taken := r.handlers[alias]; taken {
			return fmt.Errorf("backend %q: alias %q already taken", m.Descriptor.Type, alias)
		}
		seen[alias] = struct{}{}
		aliases = append(aliases, alias)
	}

	prefixes := make([]string, 0, len(m.Descriptor.ImagePrefixes))
	prefixSeen := make(map[string]struct{}, len(m.Descriptor.ImagePrefixes))
	for _, prefix := range m.Descriptor.ImagePrefixes {
		if prefix == "" {
			return fmt.Errorf("backend %q: empty image prefix", m.Descriptor.Type)
		}
		if _, dup := prefixSeen[prefix]; dup {
			continue
		}
		for _, route := range r.imageRoutes {
			if route.prefix == prefix {
				return fmt.Errorf("backend %q: image prefix %q already taken by %q", m.Descriptor.Type, prefix, route.backend)
			}
		}
		prefixSeen[prefix] = struct{}{}
		prefixes = append(prefixes, prefix)
	}

	stored := &BackendModule{
		Descriptor: BackendDescriptor{
			Type:          m.Descriptor.Type,
			Handler:       handler,
			Aliases:       aliases,
			ImagePrefixes: prefixes,
		},
		Engine:     m.Engine,
		Cleaner:    m.Cleaner,
		Reconciler: m.Reconciler,
		Restorer:   m.Restorer,
		Health:     m.Health,
	}

	r.modules[stored.Descriptor.Type] = stored
	r.handlers[handler] = stored.Descriptor.Type
	for _, alias := range aliases {
		r.handlers[alias] = stored.Descriptor.Type
	}
	for _, prefix := range prefixes {
		r.imageRoutes = append(r.imageRoutes, imageRoute{prefix: prefix, backend: stored.Descriptor.Type})
	}
	sort.SliceStable(r.imageRoutes, func(i, j int) bool {
		return len(r.imageRoutes[i].prefix) > len(r.imageRoutes[j].prefix)
	})
	r.rebuildOrderedLocked()

	return nil
}

// Freeze 冻结注册表：之后 Register 返回错误，重复 Freeze 是 no-op。
// 默认后端在 defaultType 非空且未注册时报错。
func (r *Registry) Freeze() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.frozen {
		return nil
	}
	if r.defaultType != "" {
		if _, ok := r.modules[r.defaultType]; !ok {
			return fmt.Errorf("default backend %q is not registered", r.defaultType)
		}
	}
	r.rebuildOrderedLocked()
	r.snapshot = append([]*BackendModule(nil), r.ordered...)
	r.frozen = true

	return nil
}

// Get 按类型取模块。
func (r *Registry) Get(t EngineType) (*BackendModule, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	mod, ok := r.modules[t]

	return mod, ok
}

// Default 返回默认后端；未配置默认后端时返回 nil，调用方必须判空。
func (r *Registry) Default() *BackendModule {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.defaultType == "" {
		return nil
	}

	return r.modules[r.defaultType]
}

// ResolveHandler 按 CRI runtime handler（含别名）解析后端，大小写与首尾空白不敏感。
func (r *Registry) ResolveHandler(handler string) (*BackendModule, bool) {
	normalized := normalizeHandler(handler)
	if normalized == "" {
		return nil, false
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	typ, ok := r.handlers[normalized]
	if !ok {
		return nil, false
	}
	mod, ok := r.modules[typ]

	return mod, ok
}

// ResolveImage 按镜像前缀（最长优先）解析后端。
func (r *Registry) ResolveImage(image string) (*BackendModule, bool) {
	if image == "" {
		return nil, false
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, route := range r.imageRoutes {
		if strings.HasPrefix(image, route.prefix) {
			mod, ok := r.modules[route.backend]
			if ok {
				return mod, true
			}
		}
	}

	return nil, false
}

// Modules 返回按类型排序的模块列表；frozen 后返回缓存快照。
func (r *Registry) Modules() []*BackendModule {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.frozen {
		return r.snapshot
	}

	ordered := r.sortedLocked()

	return ordered
}

func (r *Registry) rebuildOrderedLocked() {
	r.ordered = r.sortedLocked()
}

func (r *Registry) sortedLocked() []*BackendModule {
	ordered := make([]*BackendModule, 0, len(r.modules))
	for _, mod := range r.modules {
		ordered = append(ordered, mod)
	}
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].Descriptor.Type < ordered[j].Descriptor.Type
	})

	return ordered
}

func normalizeHandler(handler string) string {
	return strings.ToLower(strings.TrimSpace(handler))
}
