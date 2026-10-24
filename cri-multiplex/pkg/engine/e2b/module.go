package e2b

import "github.com/cri-multiplex/pkg/engine"

// Descriptor 是 e2b 后端的静态标识：e2b.dev/ 开头的镜像由 e2b 后端处理。
var Descriptor = engine.BackendDescriptor{Type: engine.EngineTypeE2B, Handler: "e2b", ImagePrefixes: []string{"e2b.dev/"}}

// Module 把引擎与它的可选能力打包给注册表。
func (e *Engine) Module() engine.BackendModule {
	return engine.BackendModule{
		Descriptor: Descriptor,
		Engine:     e,
		Cleaner:    e,
		Reconciler: e,
		Restorer:   e,
	}
}

var (
	_ engine.RuntimeEngine = (*Engine)(nil)
	_ engine.Cleaner       = (*Engine)(nil)
	_ engine.Reconciler    = (*Engine)(nil)
	_ engine.StateRestorer = (*Engine)(nil)
)
