package engine

import "time"

const annTemplateID = "e2b.dev/template-id"

type E2BBackendType string

const (
	BackendGRPC E2BBackendType = "grpc"
	BackendREST E2BBackendType = "rest"
)

type E2BConfig struct {
	Backend          E2BBackendType
	OrchestratorAddr string
	APIBaseURL       string
	APIKey           string
}

type E2BEngine interface {
	RuntimeEngine
}

func NewE2BEngine(cfg *E2BConfig) E2BEngine {
	switch cfg.Backend {
	case BackendREST:
		return newRestE2BEngine(cfg.APIBaseURL, cfg.APIKey)
	default:
		return newGRPCE2BEngine(cfg.OrchestratorAddr)
	}
}

type podInfo struct {
	sandboxID   string
	podUID      string
	name        string
	namespace   string
	labels      map[string]string
	annotations map[string]string
	createdAt   time.Time
	endedAt     *time.Time
}


