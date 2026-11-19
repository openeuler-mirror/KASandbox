package agentenv

import (
	"context"
	"encoding/json"

	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

const (
	agentenvRuntimeName    = "agentenv-cri-shim"
	agentenvRuntimeVersion = "0.1.0"
)

func (e *Engine) Version(context.Context, *runtime.VersionRequest) (*runtime.VersionResponse, error) {
	return &runtime.VersionResponse{
		Version:           agentenvRuntimeVersion,
		RuntimeName:       agentenvRuntimeName,
		RuntimeVersion:    agentenvRuntimeVersion,
		RuntimeApiVersion: "v1",
	}, nil
}

const (
	runtimeReady          = "RuntimeReady"
	networkReady          = "NetworkReady"
	reasonRuntimeDown     = "AgentENVRuntimeUnavailable"
	reasonNetworkNotReady = "AgentENVNetworkNotConfigured"
)

func (e *Engine) Status(ctx context.Context, req *runtime.StatusRequest) (*runtime.StatusResponse, error) {
	healthErr := e.Health(ctx)
	runtimeOK := healthErr == nil
	networkOK := runtimeOK
	networkMessage := ""
	networkReason := ""
	runtimeReason := ""
	runtimeMessage := ""
	if healthErr != nil {
		runtimeReason = reasonRuntimeDown
		runtimeMessage = healthErr.Error()
		networkOK = false
		networkReason = reasonRuntimeDown
		networkMessage = healthErr.Error()
	}
	response := &runtime.StatusResponse{Status: &runtime.RuntimeStatus{Conditions: []*runtime.RuntimeCondition{
		{Type: runtimeReady, Status: runtimeOK, Reason: runtimeReason, Message: runtimeMessage},
		{Type: networkReady, Status: networkOK, Reason: networkReason, Message: networkMessage},
	}}}
	if req != nil && req.GetVerbose() {
		info, err := json.Marshal(map[string]string{
			"node_name":      e.config.NodeName,
			"node_ip":        e.config.NodeIP,
			"network_mode":   runtimeManagedNetworkMode,
			"host_port_mode": "unsupported",
		})
		if err == nil {
			response.Info = map[string]string{"agentenv": string(info)}
		}
	}
	return response, nil
}

// UpdateRuntimeConfig 保留为无操作：网络由 AgentENV Runtime 管理。
func (e *Engine) UpdateRuntimeConfig(context.Context, *runtime.UpdateRuntimeConfigRequest) (*runtime.UpdateRuntimeConfigResponse, error) {
	return &runtime.UpdateRuntimeConfigResponse{}, nil
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
