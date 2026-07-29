package engine

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func TestAndroidGuestNetworkForRecordAllocatesStablePerInstanceSubnet(t *testing.T) {
	tests := []struct {
		name     string
		base     int
		gateway  string
		guestIP  string
		subnet   string
		tapName  string
		wantCode codes.Code
	}{
		{name: "first instance", base: 1, gateway: "192.168.240.1", guestIP: "192.168.240.2", subnet: "192.168.240.0/30", tapName: "cvd-etap-01"},
		{name: "second instance", base: 2, gateway: "192.168.240.5", guestIP: "192.168.240.6", subnet: "192.168.240.4/30", tapName: "cvd-etap-02"},
		{name: "next octet", base: 65, gateway: "192.168.241.1", guestIP: "192.168.241.2", subnet: "192.168.241.0/30", tapName: "cvd-etap-65"},
		{name: "zero", base: 0, wantCode: codes.InvalidArgument},
		{name: "overflow", base: 1025, wantCode: codes.InvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := androidGuestNetworkForRecord(&AndroidSandboxRecord{BaseInstanceNum: tt.base}, []string{"8.8.8.8"})
			if tt.wantCode != codes.OK {
				if status.Code(err) != tt.wantCode {
					t.Fatalf("code = %v, want %v", status.Code(err), tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("androidGuestNetworkForRecord: %v", err)
			}
			if got.Gateway != tt.gateway || got.GuestIP != tt.guestIP || got.Subnet != tt.subnet || got.TapName != tt.tapName || got.Prefix != "30" {
				t.Fatalf("network = %+v, want gateway=%s guest=%s subnet=%s tap=%s", got, tt.gateway, tt.guestIP, tt.subnet, tt.tapName)
			}
		})
	}
}

func TestAndroidGuestServicePortsParsesDedupesAndSorts(t *testing.T) {
	rec := &AndroidSandboxRecord{Annotations: map[string]string{
		annAndroidGuestPorts: "18080, 80;443 invalid 0 65536 443",
	}}
	got := androidGuestServicePorts(rec)
	want := []int{80, 443, 18080}
	if len(got) != len(want) {
		t.Fatalf("ports len = %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ports = %v, want %v", got, want)
		}
	}
}

func TestAndroidEthernetIPConfigEncodesStaticDNSConfig(t *testing.T) {
	payload, err := androidEthernetIPConfig(androidGuestNetwork{
		Gateway: "192.168.240.1",
		GuestIP: "192.168.240.2",
		Prefix:  "30",
		DNS:     []string{"8.8.8.8", "1.1.1.1"},
	})
	if err != nil {
		t.Fatalf("androidEthernetIPConfig: %v", err)
	}
	wantHex := "" +
		"00000003000c697041737369676e6d656e740006535441544943" +
		"000b6c696e6b41646472657373000d3139322e3136382e3234302e32" +
		"0000001e0007676174657761790000000000000001000d3139322e3136382e3234302e31" +
		"0003646e730007382e382e382e380003646e730007312e312e312e31" +
		"000d70726f787953657474696e677300044e4f4e45000269640004657468310003656f73"
	want, err := hex.DecodeString(wantHex)
	if err != nil {
		t.Fatalf("decode want hex: %v", err)
	}
	if hex.EncodeToString(payload) != hex.EncodeToString(want) {
		t.Fatalf("payload hex = %s, want %s", hex.EncodeToString(payload), hex.EncodeToString(want))
	}
}

func TestAndroidEthernetIPConfigRejectsInvalidPrefix(t *testing.T) {
	if _, err := androidEthernetIPConfig(androidGuestNetwork{
		Gateway: "192.168.240.1",
		GuestIP: "192.168.240.2",
		Prefix:  "bad",
	}); err == nil {
		t.Fatal("expected invalid prefix error")
	}
}

func TestAndroidGuestDNSServersUsesAnnotationThenCNIThenDefault(t *testing.T) {
	e := NewAndroidEngine(AndroidConfig{Enabled: true})
	rec := &AndroidSandboxRecord{Annotations: map[string]string{annAndroidGuestDNS: "8.8.8.8,bad,1.1.1.1"}}
	if got := e.guestDNSServers(rec); len(got) != 2 || got[0] != "8.8.8.8" || got[1] != "1.1.1.1" {
		t.Fatalf("annotation DNS = %v", got)
	}
	rec = &AndroidSandboxRecord{Annotations: map[string]string{}, CNIRecord: &CNIRecord{DNS: []string{"10.233.0.10"}}}
	if got := e.guestDNSServers(rec); len(got) != 1 || got[0] != "10.233.0.10" {
		t.Fatalf("CNI DNS = %v", got)
	}
	if got := e.guestDNSServers(&AndroidSandboxRecord{Annotations: map[string]string{}}); len(got) != 2 || got[0] != "8.8.8.8" {
		t.Fatalf("default DNS = %v", got)
	}
}

func TestAndroidStatePersistsGuestNetworkFields(t *testing.T) {
	pod := &AndroidSandboxRecord{
		CRISandboxID:    "android-uid",
		PodUID:          "android-uid",
		Name:            "android",
		Namespace:       "default",
		BaseInstanceNum: 1,
		ADBPort:         6520,
		PodIP:           "10.233.71.64",
		NetNSName:       "android-uid",
		NetNSPath:       "/var/run/netns/android-uid",
		GuestIP:         "192.168.240.2",
		GuestGateway:    "192.168.240.1",
		GuestPrefix:     "30",
		TapName:         "cvd-etap-01",
		CNIRecord: &CNIRecord{
			SandboxID: "android-uid",
			NetNSName: "android-uid",
			NetNSPath: "/var/run/netns/android-uid",
			PodIP:     "10.233.71.64",
		},
		Annotations: map[string]string{annAndroidGuestPorts: "18080"},
		Labels:      map[string]string{"app": "android"},
	}
	state := androidPodStateFromRecords(pod, nil)
	restored := androidSandboxRecordFromState(state)
	if restored.GuestIP != pod.GuestIP || restored.GuestGateway != pod.GuestGateway || restored.TapName != pod.TapName {
		t.Fatalf("restored guest network mismatch: %+v", restored)
	}
	if restored.CNIRecord == nil || restored.CNIRecord.PodIP != pod.PodIP || restored.PodIP != pod.PodIP {
		t.Fatalf("restored CNI mismatch: %+v", restored)
	}
}

func TestAndroidStartMarksUnknownWhenGuestNetworkConfigureFails(t *testing.T) {
	e, _, _ := newEnabledMockAndroidEngine(t, nil, nil)
	e.ops.configureGuestNetwork = func(e *AndroidEngine, ctx context.Context, rec *AndroidSandboxRecord) error {
		return errors.New("guest network failed")
	}
	if _, err := e.RunPodSandbox(context.Background(), androidRunReq("android-uid")); err != nil {
		t.Fatalf("RunPodSandbox: %v", err)
	}
	if _, err := e.CreateContainer(context.Background(), &runtime.CreateContainerRequest{
		PodSandboxId: "android-uid",
		Config:       &runtime.ContainerConfig{Metadata: &runtime.ContainerMetadata{Name: "app"}, Image: &runtime.ImageSpec{Image: androidDefaultImage}},
	}); err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if _, err := e.StartContainer(context.Background(), &runtime.StartContainerRequest{ContainerId: "android-uid-c"}); err == nil {
		t.Fatal("expected StartContainer error")
	}
	if e.pods["android-uid"].State != androidSandboxUnknown {
		t.Fatalf("sandbox state = %s, want Unknown", e.pods["android-uid"].State)
	}
}
