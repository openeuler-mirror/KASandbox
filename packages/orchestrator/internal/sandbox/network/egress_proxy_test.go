package network

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildEgressProxyRules(t *testing.T) {
	t.Parallel()

	got, err := buildEgressProxyRules("tap0", "192.0.2.1", []string{"10.233.64.0/18", "172.16.0.0/12"}, 15001, true)
	require.NoError(t, err)

	want := `*nat
:E2B_EGRESS_PROXY - [0:0]
-F E2B_EGRESS_PROXY
-A PREROUTING -i tap0 -j E2B_EGRESS_PROXY
-A E2B_EGRESS_PROXY -p tcp -d 192.0.2.1/32 -j RETURN
-A E2B_EGRESS_PROXY -p tcp -d 169.254.0.0/30 -j RETURN
-A E2B_EGRESS_PROXY -p tcp -d 10.233.64.0/18 -j RETURN
-A E2B_EGRESS_PROXY -p tcp -d 172.16.0.0/12 -j RETURN
-A E2B_EGRESS_PROXY -p tcp -j REDIRECT --to-ports 15001
COMMIT
`
	assert.Equal(t, want, got)
}

func TestBuildEgressProxyRulesNoExemptCIDRs(t *testing.T) {
	t.Parallel()

	got, err := buildEgressProxyRules("tap0", "192.0.2.1", nil, 15001, true)
	require.NoError(t, err)

	want := `*nat
:E2B_EGRESS_PROXY - [0:0]
-F E2B_EGRESS_PROXY
-A PREROUTING -i tap0 -j E2B_EGRESS_PROXY
-A E2B_EGRESS_PROXY -p tcp -d 192.0.2.1/32 -j RETURN
-A E2B_EGRESS_PROXY -p tcp -d 169.254.0.0/30 -j RETURN
-A E2B_EGRESS_PROXY -p tcp -j REDIRECT --to-ports 15001
COMMIT
`
	assert.Equal(t, want, got)
}

// Re-applies (install retries) must omit the append-only PREROUTING jump;
// the chain is flushed and rewritten in the same payload.
func TestBuildEgressProxyRulesReapplyOmitsJump(t *testing.T) {
	t.Parallel()

	got, err := buildEgressProxyRules("tap0", "192.0.2.1", nil, 15001, false)
	require.NoError(t, err)

	assert.NotContains(t, got, "-A PREROUTING")
	assert.Contains(t, got, ":E2B_EGRESS_PROXY - [0:0]\n-F E2B_EGRESS_PROXY\n")
	assert.Contains(t, got, "-A E2B_EGRESS_PROXY -p tcp -j REDIRECT --to-ports 15001\n")
}

func TestBuildEgressProxyRulesExemptionOrderAndNoSelfExemption(t *testing.T) {
	t.Parallel()

	got, err := buildEgressProxyRules("tap0", "192.0.2.1", []string{"10.0.0.0/8"}, 15001, true)
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	// *nat, chain declaration, chain flush, jump, COMMIT wrap the in-chain rules.
	require.Equal(t, "*nat", lines[0])
	require.Equal(t, ":E2B_EGRESS_PROXY - [0:0]", lines[1])
	require.Equal(t, "-F E2B_EGRESS_PROXY", lines[2])
	require.Equal(t, "-A PREROUTING -i tap0 -j E2B_EGRESS_PROXY", lines[3])
	require.Equal(t, "COMMIT", lines[len(lines)-1])

	chainRules := lines[4 : len(lines)-1]
	// Exemptions (orchestrator IP, link-local, EXEMPT_CIDRS) must precede the
	// catch-all REDIRECT, and the REDIRECT must be the only non-RETURN rule.
	for i, line := range chainRules[:len(chainRules)-1] {
		assert.Contains(t, line, "-j RETURN", "rule %d must be an exemption", i)
		assert.Contains(t, line, "-A E2B_EGRESS_PROXY", "rule %d must live in the dedicated chain", i)
	}
	assert.Equal(t, "-A E2B_EGRESS_PROXY -p tcp -j REDIRECT --to-ports 15001", chainRules[len(chainRules)-1])

	// The proxy lives in the same netns, so unlike the retired DNAT gateway
	// form there must be no "gateway self" exemption entry: exactly 2 + len
	// (EXEMPT_CIDRS) RETURN rules and nothing else.
	assert.Len(t, chainRules, 4)
	assert.NotContains(t, got, "DNAT")
}

func TestBuildEgressProxyRulesRejectsInvalidExemptCIDR(t *testing.T) {
	t.Parallel()

	_, err := buildEgressProxyRules("tap0", "192.0.2.1", []string{"not-a-cidr"}, 15001, true)
	require.Error(t, err)
}

func TestBuildEgressProxyRemoveRules(t *testing.T) {
	t.Parallel()

	got := buildEgressProxyRemoveRules("tap0", true, true)
	want := `*nat
-D PREROUTING -i tap0 -j E2B_EGRESS_PROXY
-F E2B_EGRESS_PROXY
-X E2B_EGRESS_PROXY
COMMIT
`
	assert.Equal(t, want, got)
}

// Idempotency: the removal payload must never reference absent objects —
// restore is atomic per table and one bad line fails the whole commit.
func TestBuildEgressProxyRemoveRulesPartialState(t *testing.T) {
	t.Parallel()

	got := buildEgressProxyRemoveRules("tap0", false, true)
	assert.NotContains(t, got, "-D PREROUTING")
	assert.Contains(t, got, "-F E2B_EGRESS_PROXY\n-X E2B_EGRESS_PROXY\n")

	got = buildEgressProxyRemoveRules("tap0", true, false)
	assert.Contains(t, got, "-D PREROUTING -i tap0 -j E2B_EGRESS_PROXY\n")
	assert.NotContains(t, got, "-F E2B_EGRESS_PROXY")
	assert.NotContains(t, got, "-X E2B_EGRESS_PROXY")

	got = buildEgressProxyRemoveRules("tap0", false, false)
	assert.Equal(t, "*nat\nCOMMIT\n", got)
}
