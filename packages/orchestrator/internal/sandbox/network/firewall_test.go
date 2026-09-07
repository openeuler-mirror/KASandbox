package network

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeCIDRs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      []string
		want    []string
		wantErr bool
	}{
		{
			name: "empty input",
			in:   nil,
			want: []string{},
		},
		{
			name: "cidr passthrough",
			in:   []string{"10.233.64.0/18"},
			want: []string{"10.233.64.0/18"},
		},
		{
			name: "bare ip normalized to /32",
			in:   []string{"10.233.64.1"},
			want: []string{"10.233.64.1/32"},
		},
		{
			name: "empty entries skipped and whitespace trimmed",
			in:   []string{" 10.233.64.0/18 ", "", "  "},
			want: []string{"10.233.64.0/18"},
		},
		{
			name: "multiple cidrs",
			in:   []string{"10.233.64.0/18", "10.233.128.0/18"},
			want: []string{"10.233.64.0/18", "10.233.128.0/18"},
		},
		{
			name:    "invalid entry rejected",
			in:      []string{"not-a-cidr"},
			wantErr: true,
		},
		{
			name:    "invalid among valid rejected",
			in:      []string{"10.233.64.0/18", "999.1.2.3"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := normalizeCIDRs(tt.in)
			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestBuildAllowedRanges(t *testing.T) {
	t.Parallel()

	t.Run("orchestrator ip only", func(t *testing.T) {
		t.Parallel()

		got, err := buildAllowedRanges("192.0.2.1", nil)
		require.NoError(t, err)
		assert.Equal(t, []string{"192.0.2.1/32", "169.254.0.22/32", "192.168.97.1/32"}, got)
	})

	t.Run("orchestrator ip plus extras", func(t *testing.T) {
		t.Parallel()

		got, err := buildAllowedRanges("192.0.2.1", []string{"10.233.64.1", "10.233.70.0/24"})
		require.NoError(t, err)
		// Orchestrator IP stays first so it can never be shadowed by extras.
		assert.Equal(t, []string{"192.0.2.1/32", "169.254.0.22/32", "192.168.97.1/32", "10.233.64.1/32", "10.233.70.0/24"}, got)
	})

	t.Run("invalid extra rejected", func(t *testing.T) {
		t.Parallel()

		_, err := buildAllowedRanges("192.0.2.1", []string{"bogus"})
		require.Error(t, err)
	})
}

func TestNewFirewallRejectsInvalidCIDRs(t *testing.T) {
	t.Parallel()

	// Validation happens before any nftables interaction, so these fail fast
	// without creating kernel state.
	_, err := NewFirewall("tap0", "192.0.2.1", []string{"bad-cidr"}, nil, true)
	require.Error(t, err)

	_, err = NewFirewall("tap0", "192.0.2.1", nil, []string{"bad-cidr"}, true)
	require.Error(t, err)
}

func TestParseConfigFirewallCIDRs(t *testing.T) {
	// Not parallel: mutates process env.
	t.Setenv("SANDBOX_DENIED_POD_CIDR", "10.233.64.0/18,10.233.128.0/18")
	t.Setenv("SANDBOX_FIREWALL_ALLOWED_CIDRS", "10.233.64.1,10.233.70.5/32")

	cfg, err := ParseConfig()
	require.NoError(t, err)
	assert.Equal(t, []string{"10.233.64.0/18", "10.233.128.0/18"}, cfg.DeniedPodCIDRs)
	assert.Equal(t, []string{"10.233.64.1", "10.233.70.5/32"}, cfg.FirewallAllowedCIDRs)
	assert.Equal(t, "192.0.2.1", cfg.OrchestratorInSandboxIPAddress)
}

func TestParseConfigFirewallCIDRsDefaultEmpty(t *testing.T) {
	// Not parallel: mutates process env.
	t.Setenv("SANDBOX_DENIED_POD_CIDR", "")
	t.Setenv("SANDBOX_FIREWALL_ALLOWED_CIDRS", "")

	cfg, err := ParseConfig()
	require.NoError(t, err)
	assert.Empty(t, cfg.DeniedPodCIDRs)
	assert.Empty(t, cfg.FirewallAllowedCIDRs)
}
