package agentenv

import "github.com/cri-multiplex/pkg/engine"

func newWithClients(config Config, store engine.StateStore, lifecycle lifecycleAPI, admin adminAPI) *Engine {
	e := &Engine{
		config:     config,
		store:      store,
		pods:       make(map[string]*podRecord),
		containers: make(map[string]*containerRecord),
		lifecycle:  lifecycle,
		admin:      admin,
	}
	e.dialOnce.Do(func() {})
	return e
}
