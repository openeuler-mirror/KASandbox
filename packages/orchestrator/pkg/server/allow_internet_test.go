package server

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	sandbox_network "github.com/e2b-dev/infra/packages/shared/pkg/sandbox-network"
)

func TestWithAllowInternetAccess(t *testing.T) {
	t.Parallel()

	allow, deny := true, false
	withRules := func() *orchestrator.SandboxNetworkConfig {
		return &orchestrator.SandboxNetworkConfig{Egress: &orchestrator.SandboxNetworkEgressConfig{
			AllowedCidrs: []string{"10.0.0.0/8"},
			DeniedCidrs:  []string{"192.0.2.0/24"},
		}}
	}

	t.Run("unset keeps the network config", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, withAllowInternetAccess(nil, nil))
		assert.Equal(t, withRules(), withAllowInternetAccess(withRules(), nil))
	})

	t.Run("true keeps the network config", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, withAllowInternetAccess(nil, &allow))
		assert.Equal(t, withRules(), withAllowInternetAccess(withRules(), &allow))
	})

	t.Run("false denies all egress", func(t *testing.T) {
		t.Parallel()
		got := withAllowInternetAccess(nil, &deny)
		assert.Equal(t, []string{sandbox_network.AllInternetTrafficCIDR}, got.GetEgress().GetDeniedCidrs())

		got = withAllowInternetAccess(withRules(), &deny)
		assert.Equal(t, []string{sandbox_network.AllInternetTrafficCIDR}, got.GetEgress().GetDeniedCidrs())
		assert.Equal(t, []string{"10.0.0.0/8"}, got.GetEgress().GetAllowedCidrs(), "allowed CIDRs stay as exceptions")
	})
}
