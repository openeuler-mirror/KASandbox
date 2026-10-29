package network

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseConfigIsolateNetNSDirDefault(t *testing.T) {
	// Not parallel: mutates process env.
	cfg, err := ParseConfig()
	require.NoError(t, err)
	// 默认 false：裸机与 k8s Pod 内都不隔离 /run/netns。
	assert.Equal(t, "false", cfg.IsolateNetNSDir)
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	assert.False(t, cfg.shouldIsolateNetNSDir())
}

func TestShouldIsolateNetNSDir(t *testing.T) {
	// Not parallel: mutates process env (KUBERNETES_SERVICE_HOST).
	tests := []struct {
		name  string
		mode  string
		inPod bool
		want  bool
	}{
		{name: "auto on bare metal isolates", mode: "auto", inPod: false, want: true},
		{name: "auto in k8s pod skips", mode: "auto", inPod: true, want: false},
		{name: "explicit true isolates even in pod", mode: "true", inPod: true, want: true},
		{name: "explicit false skips even on bare metal", mode: "false", inPod: false, want: false},
		{name: "unknown value treated as auto", mode: "", inPod: true, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.inPod {
				t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
			} else {
				t.Setenv("KUBERNETES_SERVICE_HOST", "")
			}

			cfg := Config{IsolateNetNSDir: tt.mode}
			assert.Equal(t, tt.want, cfg.shouldIsolateNetNSDir())
		})
	}
}
