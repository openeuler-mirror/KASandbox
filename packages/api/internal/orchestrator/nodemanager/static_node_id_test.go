package nodemanager

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/servicediscovery"
)

//nolint:paralleltest // ReplaceGlobals mutates process state
func TestWarnOnStaticNodeIDMismatch(t *testing.T) {
	tests := map[string]struct {
		backend  string
		listed   string
		reported string
		warns    bool
	}{
		"a static entry named differently": {backend: servicediscovery.BackendStatic, listed: "node1", reported: "host-a", warns: true},
		"a static entry named alike":       {backend: servicediscovery.BackendStatic, listed: "host-a", reported: "host-a"},
		"a static entry without a name":    {backend: servicediscovery.BackendStatic, reported: "host-a"},
		"another backend":                  {backend: servicediscovery.BackendKubernetes, listed: "node1", reported: "host-a"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			core, logs := observer.New(zapcore.WarnLevel)
			t.Cleanup(logger.ReplaceGlobals(t.Context(), logger.NewTracedLoggerFromCore(core)))

			discovered := NodePlaneInstance{
				WorkloadID:          "10.0.0.1:5008",
				NodeID:              tt.listed,
				OrchestratorAddress: "10.0.0.1:5008",
				IPAddress:           "10.0.0.1",
				Backend:             tt.backend,
			}
			warnOnStaticNodeIDMismatch(t.Context(), discovered, tt.reported)

			if !tt.warns {
				assert.Zero(t, logs.Len())

				return
			}

			entries := logs.All()
			require.Len(t, entries, 1)
			fields := entries[0].ContextMap()
			assert.Equal(t, "node1", fields["listed_node_id"])
			assert.Equal(t, "10.0.0.1:5008", fields["address"])
		})
	}
}
