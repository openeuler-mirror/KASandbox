package network

import (
	"bytes"
	"testing"

	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
)

func TestNewExternalNetNSSlotUsesCvdTapLayout(t *testing.T) {
	slot, err := NewExternalNetNSSlot(
		"android",
		&orchestrator.SandboxRuntimeNetworkConfig{
			Mode:      orchestrator.SandboxRuntimeNetworkConfig_CNI_EXTERNAL_NETNS,
			NetnsPath: "/var/run/netns/e2b-abc123",
			IfName:    "eth0",
			PodIp:     "10.233.71.159",
		},
		Config{},
	)
	if err != nil {
		t.Fatalf("NewExternalNetNSSlot: %v", err)
	}

	if got := slot.NamespaceID(); got != "e2b-abc123" {
		t.Fatalf("NamespaceID = %q, want %q", got, "e2b-abc123")
	}
	if got := slot.ExtraTapName(); got != "cvd-mtap" {
		t.Fatalf("ExtraTapName = %q, want %q", got, "cvd-mtap")
	}
	if got := slot.ExtraTapIPString(); got != "192.168.97.1" {
		t.Fatalf("ExtraTapIPString = %q, want %q", got, "192.168.97.1")
	}
	if got := slot.ExtraTapCIDR(); !bytes.Equal(got, []byte{255, 255, 255, 252}) {
		t.Fatalf("ExtraTapCIDR = %v, want %v", got, []byte{255, 255, 255, 252})
	}
}

// Per-sandbox egress proxy slots (native mode): no host tcpProxy redirect,
// all-protocols user firewall rules, and the vrt SNAT rule for the proxy's
// upstream connections. Off slots must behave exactly as before.
func TestEgressProxySlotNativeRuleCompanion(t *testing.T) {
	t.Parallel()

	proxied, err := NewSlot("proxied", 7, Config{})
	if err != nil {
		t.Fatalf("NewSlot: %v", err)
	}
	proxied.egressProxy = true

	if proxied.installTCPProxy() {
		t.Fatal("per-sandbox slot must skip the host tcpProxy redirect rules")
	}
	if !proxied.userRulesAllProtocols() {
		t.Fatal("per-sandbox slot firewall user rules must cover all protocols")
	}

	wantSNAT := []string{"-o", "eth0", "-s", vrtNetworkCIDR.String(), "-j", "SNAT", "--to", proxied.HostIPString()}
	gotSNAT := proxied.egressProxyVrtSNATSpec()
	if len(gotSNAT) != len(wantSNAT) {
		t.Fatalf("egressProxyVrtSNATSpec = %v, want %v", gotSNAT, wantSNAT)
	}
	for i := range wantSNAT {
		if gotSNAT[i] != wantSNAT[i] {
			t.Fatalf("egressProxyVrtSNATSpec = %v, want %v", gotSNAT, wantSNAT)
		}
	}

	// off slot: behavior unchanged (tcpProxy installed, TCP user rules stay
	// with the tcpProxy in native mode).
	plain, err := NewSlot("plain", 8, Config{})
	if err != nil {
		t.Fatalf("NewSlot: %v", err)
	}
	if !plain.installTCPProxy() {
		t.Fatal("off slot must keep installing the tcpProxy redirect rules")
	}
	if plain.userRulesAllProtocols() {
		t.Fatal("off native slot user rules must not cover TCP (handled by tcpProxy)")
	}

	// CNI slots keep their existing all-protocols behavior.
	external := &Slot{ExternalNetNS: true}
	if !external.installTCPProxy() {
		t.Fatal("external netns slot: tcpProxy decision untouched by the egress proxy marker")
	}
	if !external.userRulesAllProtocols() {
		t.Fatal("external netns slot user rules must cover all protocols")
	}
}
