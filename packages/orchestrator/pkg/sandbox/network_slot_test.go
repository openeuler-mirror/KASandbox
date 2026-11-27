//go:build linux

package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/network"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
)

var errFakePoolGet = errors.New("fake pool get")

// recordingPool fails every Get so tests can tell whether the pool was used
// without creating any network state.
type recordingPool struct {
	gets int
}

var _ network.PoolInterface = (*recordingPool)(nil)

func (p *recordingPool) Get(context.Context, *orchestrator.SandboxNetworkConfig, network.EgressClass) (*network.Slot, error) {
	p.gets++

	return nil, errFakePoolGet
}

func (p *recordingPool) ReturnAsync(context.Context, *network.Slot, network.ReleaseNotify, time.Duration) error {
	return nil
}

func (p *recordingPool) Close(context.Context) error {
	return nil
}

// A CRI sandbox (CNI_EXTERNAL_NETNS) is networked in its pod netns and must
// never take a pooled slot. The pod IP is invalid here, so the external path
// fails before it opens any network namespace.
func TestGetNetworkSlot_ExternalNetNSBypassesPool(t *testing.T) {
	t.Parallel()

	pool := &recordingPool{}
	runtimeNetwork := &orchestrator.SandboxRuntimeNetworkConfig{
		Mode:      orchestrator.SandboxRuntimeNetworkConfig_CNI_EXTERNAL_NETNS,
		NetnsPath: "/var/run/netns/cni-test",
		PodIp:     "not-an-ip",
	}

	_, err := getNetworkSlot(t.Context(), pool, network.Config{}, NewCleanup(), nil, runtimeNetwork,
		func(context.Context, string) {}, network.EgressClassSandbox).Wait(t.Context())

	require.ErrorContains(t, err, "failed to create external netns slot")
	require.Zero(t, pool.gets, "an external netns sandbox must not take a pooled slot")
}

func TestGetNetworkSlot_OrchestratorManagedUsesPool(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		runtimeNetwork *orchestrator.SandboxRuntimeNetworkConfig
	}{
		{name: "no runtime network", runtimeNetwork: nil},
		{name: "orchestrator managed", runtimeNetwork: &orchestrator.SandboxRuntimeNetworkConfig{
			Mode:      orchestrator.SandboxRuntimeNetworkConfig_ORCHESTRATOR_MANAGED,
			NetnsPath: "/var/run/netns/cni-test",
			PodIp:     "10.233.64.5",
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pool := &recordingPool{}

			_, err := getNetworkSlot(t.Context(), pool, network.Config{}, NewCleanup(), nil, tt.runtimeNetwork,
				func(context.Context, string) {}, network.EgressClassSandbox).Wait(t.Context())

			require.ErrorIs(t, err, errFakePoolGet)
			require.Equal(t, 1, pool.gets)
		})
	}
}
