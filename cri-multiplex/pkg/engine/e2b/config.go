package e2b

import (
	"fmt"

	"github.com/cri-multiplex/pkg/engine"
	"github.com/cri-multiplex/pkg/engine/util"
)

// Config 是 e2b 后端的构造参数；共享依赖（Store/NodeName/NodeIP）来自 engine.Deps。
type Config struct {
	engine.Deps

	OrchestratorAddr      string
	OrchestratorProxyAddr string
	// HideLabel 形如 "key=value"；非空时，带该 label 的 E2B sandbox 从
	// ListPodSandbox/ListContainers 响应中隐藏（kubelet PLEG/runtimeCache 不可见，
	// 从而不触发孤儿 sandbox 清杀）；为空则保持原有行为。
	HideLabel string
	CNI       util.CNIConfig
}

// New 构造 e2b 引擎：只做校验与初始化，不注册、不写状态。
func New(cfg Config) (*Engine, error) {
	if cfg.OrchestratorAddr == "" {
		return nil, fmt.Errorf("e2b: orchestrator address is required")
	}
	if cfg.NodeIP == "" {
		return nil, fmt.Errorf("e2b: node IP is required")
	}

	return newGRPCE2BEngine(cfg.OrchestratorAddr, cfg.OrchestratorProxyAddr, cfg.NodeIP, cfg.NodeName, cfg.CNI, cfg.Store, cfg.HideLabel), nil
}
