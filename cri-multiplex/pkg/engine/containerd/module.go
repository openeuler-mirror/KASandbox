package containerd

import "github.com/cri-multiplex/pkg/engine"

// Descriptor 是 containerd 后端的静态标识；没有镜像前缀，未命中前缀的镜像走默认后端。
var Descriptor = engine.BackendDescriptor{Type: engine.EngineTypeContainer, Handler: "container"}

// Module 只填 Descriptor 与 Engine：containerd 不参与清理、对账与状态恢复。
func (e *Engine) Module() engine.BackendModule {
	return engine.BackendModule{Descriptor: Descriptor, Engine: e}
}

var _ engine.RuntimeEngine = (*Engine)(nil)
