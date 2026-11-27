package cube

import "github.com/cri-multiplex/pkg/engine"

// Descriptor 是 cube 后端的静态标识。
// 模板不是镜像引用，所以不登记 ImagePrefixes；kubelet 通过 runtime handler "cube" 进来。
var Descriptor = engine.BackendDescriptor{Type: engine.EngineTypeCube, Handler: "cube"}

// Module 把引擎和它实现的可选能力交给注册表。
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
	_ engine.RuntimeEngine         = (*Engine)(nil)
	_ engine.Cleaner               = (*Engine)(nil)
	_ engine.Reconciler            = (*Engine)(nil)
	_ engine.StateRestorer         = (*Engine)(nil)
	_ engine.AdminPauser           = (*Engine)(nil)
	_ engine.AdminCheckpointer     = (*Engine)(nil)
	_ engine.AdminRuntimeDescriber = (*Engine)(nil)
)
