package server

import (
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	sandbox_network "github.com/e2b-dev/infra/packages/shared/pkg/sandbox-network"
)

// withAllowInternetAccess denies all egress when the create request turns
// internet access off, as the KASandbox orchestrator does. The 2026.30 api
// folds allow_internet_access into the network config itself, but
// cri-multiplex calls the orchestrator directly and may only set the flag
// (e2b.dev/allow-internet). network must be a copy the caller owns.
func withAllowInternetAccess(network *orchestrator.SandboxNetworkConfig, allow *bool) *orchestrator.SandboxNetworkConfig {
	if allow == nil || *allow {
		return network
	}

	if network == nil {
		network = &orchestrator.SandboxNetworkConfig{}
	}
	if network.GetEgress() == nil {
		network.Egress = &orchestrator.SandboxNetworkEgressConfig{}
	}
	network.Egress.DeniedCidrs = []string{sandbox_network.AllInternetTrafficCIDR}

	return network
}
