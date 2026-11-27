package handlers

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/api/internal/cfg"
	"github.com/e2b-dev/infra/packages/shared/pkg/servicediscovery"
)

func TestParseStaticNodes_FollowsTheKASandboxRules(t *testing.T) {
	t.Parallel()

	got, ignored := parseStaticNodes(" node1=10.0.0.1 , node2=10.0.0.2:5009,,node3,=10.0.0.9,node4=10.0.0.4:x,node5=10.0.0.1,node2=10.0.0.6", 5008)

	want := []servicediscovery.Instance{
		{WorkloadID: "10.0.0.1:5008", NodeID: "node1", IPAddress: "10.0.0.1", Port: 5008, Backend: servicediscovery.BackendStatic},
		{WorkloadID: "10.0.0.2:5009", NodeID: "node2", IPAddress: "10.0.0.2", Port: 5009, Backend: servicediscovery.BackendStatic},
		{WorkloadID: "node3:5008", NodeID: "node3", IPAddress: "node3", Port: 5008, Backend: servicediscovery.BackendStatic},
		{WorkloadID: "10.0.0.4:5008", NodeID: "node4", IPAddress: "10.0.0.4", Port: 5008, Backend: servicediscovery.BackendStatic},
	}
	assert.Equal(t, want, got)
	assert.Equal(t, []string{"=10.0.0.9", "node5=10.0.0.1", "node2=10.0.0.6"}, ignored)

	got, ignored = parseStaticNodes(" , ,", 5008)
	assert.Empty(t, got)
	assert.Empty(t, ignored)
}

// A node name listed again is dropped even when its address is new, and an
// address listed again is dropped even under a new name: the first entry wins
// either way, so builds find exactly one builder per node.
func TestParseStaticNodes_FirstEntryWins(t *testing.T) {
	t.Parallel()

	got, ignored := parseStaticNodes("node1=10.0.0.1,node1=10.0.0.2,node2=10.0.0.1:5008,node2=10.0.0.3", 5008)

	want := []servicediscovery.Instance{
		{WorkloadID: "10.0.0.1:5008", NodeID: "node1", IPAddress: "10.0.0.1", Port: 5008, Backend: servicediscovery.BackendStatic},
		{WorkloadID: "10.0.0.3:5008", NodeID: "node2", IPAddress: "10.0.0.3", Port: 5008, Backend: servicediscovery.BackendStatic},
	}
	assert.Equal(t, want, got)
	assert.Equal(t, []string{"node1=10.0.0.2", "node2=10.0.0.1:5008"}, ignored)
}

func TestServiceDiscoveryProvider_SystemdWithStaticNodesIsStatic(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		config   cfg.Config
		localEnv bool
		want     string
	}{
		"systemd with nodes": {
			config: cfg.Config{OrchestratorType: "systemd", OrchestratorStaticNodes: "node1=10.0.0.1"},
			want:   cfg.ServiceDiscoveryProviderStatic,
		},
		"systemd with nodes in a local environment": {
			config: cfg.Config{OrchestratorType: "systemd", OrchestratorStaticNodes: "node1=10.0.0.1"}, localEnv: true,
			want: cfg.ServiceDiscoveryProviderStatic,
		},
		"systemd without nodes keeps the default": {
			config: cfg.Config{OrchestratorType: "systemd", OrchestratorStaticNodes: " , "},
			want:   cfg.ServiceDiscoveryProviderNomad,
		},
		"nodes without systemd keep the default": {
			config: cfg.Config{OrchestratorType: "k8s", OrchestratorStaticNodes: "node1=10.0.0.1"},
			want:   cfg.ServiceDiscoveryProviderNomad,
		},
		"a named provider wins": {
			config: cfg.Config{ServiceDiscoveryProvider: cfg.ServiceDiscoveryProviderKubernetes, OrchestratorType: "systemd", OrchestratorStaticNodes: "node1=10.0.0.1"},
			want:   cfg.ServiceDiscoveryProviderKubernetes,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := serviceDiscoveryProvider(tt.config, tt.localEnv)
			assert.Equal(t, tt.want, got)
			// Static nodes are found by the node plane, not by the local registry.
			assert.NotEqual(t, cfg.ServiceDiscoveryProviderLocal, got)
		})
	}
}

func TestNewServiceDiscovery_StaticListsEveryNodeOnBothPlanes(t *testing.T) {
	t.Parallel()

	config := cfg.Config{OrchestratorType: "systemd", OrchestratorStaticNodes: "node1=10.0.0.1,node2=10.0.0.2:5009"}

	sd, err := newServiceDiscovery(t.Context(), config, nil, serviceDiscoveryProvider(config, false))
	require.NoError(t, err)

	for name, plane := range map[string]servicediscovery.Discoverer{
		"nodes":            sd.nodes,
		"templateBuilders": sd.templateBuilders,
	} {
		instances, err := plane.ListInstances(t.Context())
		require.NoErrorf(t, err, "the %s plane", name)
		require.Lenf(t, instances, 2, "the %s plane", name)
		assert.Equal(t, "node1", instances[0].NodeID)
		assert.Equal(t, "10.0.0.2:5009", instances[1].Address())
	}
}

func TestNewServiceDiscovery_StaticWithoutNodesFails(t *testing.T) {
	t.Parallel()

	_, err := newServiceDiscovery(t.Context(), cfg.Config{}, nil, cfg.ServiceDiscoveryProviderStatic)
	require.Error(t, err)
}
