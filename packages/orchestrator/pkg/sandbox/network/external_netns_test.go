//go:build linux

package network

import (
	"encoding/binary"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/google/nftables"
	"github.com/mdlayher/netlink"
	"github.com/ngrok/firewall_toolkit/pkg/set"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
)

// None of these tests enter a network namespace or touch the kernel: slots are
// only constructed, the BYOP guard returns before the netns is opened, and the
// firewall rules are captured through an nftables test dial.

func TestNewExternalNetNSSlot(t *testing.T) {
	t.Parallel()

	slot, err := NewExternalNetNSSlot("/var/run/netns/cni-1234", &orchestrator.SandboxRuntimeNetworkConfig{
		Mode:      orchestrator.SandboxRuntimeNetworkConfig_CNI_EXTERNAL_NETNS,
		NetnsPath: "/var/run/netns/cni-1234",
		IfName:    "net1",
		PodIp:     "10.233.64.5",
	}, Config{HyperloopProxyPort: 5010})
	require.NoError(t, err)

	assert.True(t, slot.ExternalNetNS)
	assert.Equal(t, -1, slot.Idx)
	assert.Equal(t, "/var/run/netns/cni-1234", slot.Key)
	assert.Equal(t, "10.233.64.5", slot.HostIPString())
	assert.Equal(t, "10.233.64.5/32", slot.HostCIDR())
	assert.Equal(t, "cni-1234", slot.NamespaceID())
	assert.Equal(t, "/var/run/netns/cni-1234", slot.NamespacePath())
	assert.Equal(t, "net1", slot.VpeerName())
	assert.Equal(t, tapInterfaceName, slot.TapName())
	assert.Equal(t, tapIp, slot.TapIPString())
	assert.Equal(t, "5010", slot.hyperloopPort)
	assert.Nil(t, slot.Firewall)
}

func TestNewExternalNetNSSlot_DefaultsIfNameToEth0(t *testing.T) {
	t.Parallel()

	slot, err := NewExternalNetNSSlot("key", &orchestrator.SandboxRuntimeNetworkConfig{
		NetnsPath: "/var/run/netns/cni-1234",
		PodIp:     "10.233.64.5",
	}, Config{})
	require.NoError(t, err)

	assert.Equal(t, "eth0", slot.VpeerName())
}

func TestNewExternalNetNSSlot_RejectsInvalidConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  *orchestrator.SandboxRuntimeNetworkConfig
	}{
		{name: "nil config", cfg: nil},
		{name: "missing netns path", cfg: &orchestrator.SandboxRuntimeNetworkConfig{PodIp: "10.233.64.5"}},
		{name: "missing pod ip", cfg: &orchestrator.SandboxRuntimeNetworkConfig{NetnsPath: "/var/run/netns/cni-1234"}},
		{name: "invalid pod ip", cfg: &orchestrator.SandboxRuntimeNetworkConfig{NetnsPath: "/var/run/netns/cni-1234", PodIp: "10.233.64"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewExternalNetNSSlot("key", tt.cfg, Config{})
			require.Error(t, err)
		})
	}
}

// A pooled slot keeps its ns-<idx> name and path.
func TestNamespacePath_PooledSlot(t *testing.T) {
	t.Parallel()

	slot, err := NewSlot("key", 7, Config{}, NewNoopEgressProxy())
	require.NoError(t, err)

	assert.False(t, slot.ExternalNetNS)
	assert.Equal(t, "ns-7", slot.NamespaceID())
	assert.Equal(t, filepath.Join(NetNamespacesDir, "ns-7"), slot.NamespacePath())
	assert.Equal(t, "eth0", slot.VpeerName())
}

func TestExternalNetNSRulesScript(t *testing.T) {
	t.Parallel()

	slot, err := NewExternalNetNSSlot("key", &orchestrator.SandboxRuntimeNetworkConfig{
		NetnsPath: "/var/run/netns/cni-1234",
		IfName:    "eth0",
		PodIp:     "10.233.64.5",
	}, Config{})
	require.NoError(t, err)

	assert.Equal(t, "*nat\n"+
		"-A PREROUTING -d 10.233.64.5 -j DNAT --to-destination 169.254.0.21\n"+
		"-A POSTROUTING -s 169.254.0.21 -j SNAT --to-source 10.233.64.5\n"+
		"COMMIT\n"+
		"*filter\n"+
		"-A FORWARD -i eth0 -o tap0 -j ACCEPT\n"+
		"-A FORWARD -i tap0 -o eth0 -j ACCEPT\n"+
		"COMMIT\n", slot.externalNetNSRulesScript())
}

// The pod netns has no TCP redirect to the egress proxy, so BYOP is refused
// before the netns is entered or the slot is marked as customized.
func TestExternalNetNSSlot_RejectsEgressProxy(t *testing.T) {
	t.Parallel()

	slot, err := NewExternalNetNSSlot("key", &orchestrator.SandboxRuntimeNetworkConfig{
		NetnsPath: "/nonexistent/netns/cni-1234",
		PodIp:     "10.233.64.5",
	}, Config{})
	require.NoError(t, err)
	slot.Firewall = &Firewall{}

	egress := &orchestrator.SandboxNetworkEgressConfig{EgressProxyAddress: "proxy.example.com:1080"}

	err = slot.ConfigureInternet(t.Context(), &orchestrator.SandboxNetworkConfig{Egress: egress})
	require.ErrorIs(t, err, errExternalNetNSEgressProxy)
	assert.False(t, slot.firewallCustomRules.Load())

	err = slot.UpdateInternet(t.Context(), egress)
	require.ErrorIs(t, err, errExternalNetNSEgressProxy)
	assert.False(t, slot.firewallCustomRules.Load())
}

// installRules must keep rules 4/5 non-TCP only for pooled slots (TCP goes to
// the egress proxy) and match all protocols for external netns slots, where the
// nftables rules are the only enforcement point. Rule 3 follows BYOP as before.
func TestInstallRules_UserRulesProtocols(t *testing.T) { //nolint:paralleltest // nftables.Conn.AddSet bumps a package-global set ID without locking
	tests := []struct {
		name                  string
		userRulesAllProtocols bool
		byop                  bool
		wantNonTCPRules       int
	}{
		{name: "pooled slot", userRulesAllProtocols: false, byop: false, wantNonTCPRules: 2},
		{name: "pooled slot with BYOP", userRulesAllProtocols: false, byop: true, wantNonTCPRules: 3},
		{name: "external netns slot", userRulesAllProtocols: true, byop: false, wantNonTCPRules: 0},
		{name: "external netns slot with BYOP", userRulesAllProtocols: true, byop: true, wantNonTCPRules: 1},
	}

	for _, tt := range tests { //nolint:paralleltest // see above
		t.Run(tt.name, func(t *testing.T) {
			var rules [][]byte
			conn, err := nftables.New(nftables.WithTestDial(func(req []netlink.Message) ([]netlink.Message, error) {
				for _, msg := range req {
					if msg.Header.Type == netlink.HeaderType(unix.NFNL_SUBSYS_NFTABLES<<8|unix.NFT_MSG_NEWRULE) {
						rules = append(rules, msg.Data)
					}
				}

				return req, nil
			}))
			require.NoError(t, err)

			fw := newTestFirewall(t, conn, tt.userRulesAllProtocols)
			fw.installRules(tt.byop)
			require.NoError(t, conn.Flush())

			require.Len(t, rules, 5)
			nonTCP := 0
			for _, rule := range rules {
				matches, err := ruleMatchesL4Proto(rule)
				require.NoError(t, err)
				if matches {
					nonTCP++
				}
			}
			assert.Equal(t, tt.wantNonTCPRules, nonTCP)
		})
	}
}

func newTestFirewall(t *testing.T, conn *nftables.Conn, userRulesAllProtocols bool) *Firewall {
	t.Helper()

	table := conn.AddTable(&nftables.Table{Name: tableName, Family: nftables.TableFamilyINet})
	policy := nftables.ChainPolicyAccept
	chain := conn.AddChain(&nftables.Chain{
		Name:     "PREROUTE_FILTER",
		Table:    table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookPrerouting,
		Priority: nftables.ChainPriorityRef(-150),
		Policy:   &policy,
	})

	newSet := func(name string) set.Set {
		s, err := set.New(conn, table, name, nftables.TypeIPAddr)
		require.NoError(t, err)

		return s
	}

	return &Firewall{
		conn:                  conn,
		table:                 table,
		filterChain:           chain,
		predefinedDenySet:     newSet("filtered_always_denylist"),
		predefinedAllowSet:    newSet("filtered_always_allowlist"),
		userDenySet:           newSet("filtered_denylist"),
		userAllowSet:          newSet("filtered_allowlist"),
		tapInterface:          tapInterfaceName,
		userRulesAllProtocols: userRulesAllProtocols,
	}
}

// ruleMatchesL4Proto reports whether a NEWRULE message carries a `meta l4proto`
// expression, which is how addNonTCPSetFilterRule restricts a rule to non-TCP.
func ruleMatchesL4Proto(data []byte) (bool, error) {
	if len(data) < 4 {
		return false, fmt.Errorf("short rule message: %d bytes", len(data))
	}

	ad, err := netlink.NewAttributeDecoder(data[4:]) // skip nfgenmsg
	if err != nil {
		return false, err
	}
	ad.ByteOrder = binary.BigEndian

	found := false
	for ad.Next() {
		if ad.Type() != unix.NFTA_RULE_EXPRESSIONS {
			continue
		}

		ad.Nested(func(list *netlink.AttributeDecoder) error {
			for list.Next() {
				list.Nested(func(e *netlink.AttributeDecoder) error {
					var name string
					var exprData []byte
					for e.Next() {
						switch e.Type() {
						case unix.NFTA_EXPR_NAME:
							name = e.String()
						case unix.NFTA_EXPR_DATA:
							exprData = e.Bytes()
						}
					}
					if name != "meta" {
						return nil
					}

					meta, err := netlink.NewAttributeDecoder(exprData)
					if err != nil {
						return err
					}
					meta.ByteOrder = binary.BigEndian
					for meta.Next() {
						if meta.Type() == unix.NFTA_META_KEY && meta.Uint32() == unix.NFT_META_L4PROTO {
							found = true
						}
					}

					return meta.Err()
				})
			}

			return nil
		})
	}

	return found, ad.Err()
}
