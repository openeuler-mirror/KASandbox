package agentenv

import "github.com/cri-multiplex/pkg/engine"

// VirtualImagePrefix 将 AgentENV Pod 的虚拟镜像路由到本 engine，避免 containerd 拉取。
const VirtualImagePrefix = "agentenv.dev/"

var Descriptor = engine.BackendDescriptor{
	Type:          engine.EngineTypeAgentENV,
	Handler:       "agentenv",
	Aliases:       []string{"aenv"},
	ImagePrefixes: []string{VirtualImagePrefix},
}

func (e *Engine) Module() engine.BackendModule {
	return engine.BackendModule{
		Descriptor: Descriptor,
		Engine:     e,
		Cleaner:    e,
		Reconciler: e,
		Restorer:   e,
		Health:     e,
	}
}

var (
	_ engine.RuntimeEngine         = (*Engine)(nil)
	_ engine.AdminLifecycle        = (*Engine)(nil)
	_ engine.AdminPauser           = (*Engine)(nil)
	_ engine.AdminCheckpointer     = (*Engine)(nil)
	_ engine.AdminRuntimeDescriber = (*Engine)(nil)
	_ engine.Cleaner               = (*Engine)(nil)
	_ engine.Reconciler            = (*Engine)(nil)
	_ engine.StateRestorer         = (*Engine)(nil)
	_ engine.HealthChecker         = (*Engine)(nil)
)
