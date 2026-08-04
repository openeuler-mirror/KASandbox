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
