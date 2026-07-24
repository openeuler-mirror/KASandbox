package engine

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestLoadDefaultNetworkConfPrefersConflist(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "20-conf.conf"), `{"cniVersion":"1.0.0","name":"conf-net","type":"bridge"}`)
	writeFile(t, filepath.Join(dir, "10-list.conflist"), `{"cniVersion":"1.0.0","name":"list-net","plugins":[{"type":"bridge"}]}`)

	conf, err := loadDefaultNetworkConf(dir)
	if err != nil {
		t.Fatalf("loadDefaultNetworkConf: %v", err)
	}
	if conf.Name != "list-net" {
		t.Fatalf("selected network = %q, want list-net", conf.Name)
	}
}

func TestLoadDefaultNetworkConfFallsBackToConf(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "20-b.conf"), `{"cniVersion":"1.0.0","name":"b-net","type":"bridge"}`)
	writeFile(t, filepath.Join(dir, "10-a.conf"), `{"cniVersion":"1.0.0","name":"a-net","type":"bridge"}`)

	conf, err := loadDefaultNetworkConf(dir)
	if err != nil {
		t.Fatalf("loadDefaultNetworkConf: %v", err)
	}
	if conf.Name != "a-net" {
		t.Fatalf("selected network = %q, want a-net", conf.Name)
	}
}

func TestLoadDefaultNetworkConfNoConfigs(t *testing.T) {
	if _, err := loadDefaultNetworkConf(t.TempDir()); err == nil {
		t.Fatal("expected no configs error")
	}
}

func TestCNIRuntimeConfArgs(t *testing.T) {
	m := &CNIManager{ifName: "eth9"}
	cfg := &runtime.PodSandboxConfig{Metadata: &runtime.PodSandboxMetadata{
		Name:      "pod-a",
		Namespace: "ns-a",
		Uid:       "uid-a",
	}}

	rt := m.runtimeConf("sandbox-a", "/var/run/netns/test", cfg)
	if rt.ContainerID != "sandbox-a" || rt.NetNS != "/var/run/netns/test" || rt.IfName != "eth9" {
		t.Fatalf("runtime conf basic fields mismatch: %+v", rt)
	}

	got := map[string]string{}
	for _, arg := range rt.Args {
		got[arg[0]] = arg[1]
	}
	want := map[string]string{
		"IgnoreUnknown":              "1",
		"K8S_POD_NAMESPACE":          "ns-a",
		"K8S_POD_NAME":               "pod-a",
		"K8S_POD_INFRA_CONTAINER_ID": "sandbox-a",
		"K8S_POD_UID":                "uid-a",
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("arg %s = %q, want %q; args=%v", k, got[k], v, rt.Args)
		}
	}
}

func TestFirstIPv4AndShortID(t *testing.T) {
	ipv6 := net.IPNet{IP: net.ParseIP("fd00::1"), Mask: net.CIDRMask(64, 128)}
	ipv4 := net.IPNet{IP: net.ParseIP("10.0.0.12"), Mask: net.CIDRMask(24, 32)}
	result := &current.Result{IPs: []*current.IPConfig{
		{Address: ipv6},
		{Address: ipv4, Gateway: net.ParseIP("10.0.0.1")},
	}}

	ip, gw := firstIPv4(result)
	if ip != "10.0.0.12" || gw != "10.0.0.1" {
		t.Fatalf("firstIPv4 = (%q, %q), want (10.0.0.12, 10.0.0.1)", ip, gw)
	}

	noIPv4 := &current.Result{IPs: []*current.IPConfig{{Address: ipv6}}}
	ip, gw = firstIPv4(noIPv4)
	if ip != "" || gw != "" {
		t.Fatalf("firstIPv4 with no IPv4 = (%q, %q), want empty", ip, gw)
	}

	if got := shortID("  abcdefghijklmnopqrstuvwxyz "); got != "abcdefghijkl" {
		t.Fatalf("shortID = %q, want abcdefghijkl", got)
	}
}

func TestNewCNIManagerDefaults(t *testing.T) {
	confDir := t.TempDir()
	binDir := t.TempDir()
	writeFile(t, filepath.Join(confDir, "10-test.conflist"), `{"cniVersion":"1.0.0","name":"test-net","plugins":[{"type":"bridge"}]}`)

	m, err := NewCNIManager(CNIConfig{ConfDir: confDir, BinDir: binDir})
	if err != nil {
		t.Fatalf("NewCNIManager: %v", err)
	}
	if m.ifName != "eth0" || m.netNSDir != "/var/run/netns" || m.prefix != "e2b-" {
		t.Fatalf("defaults mismatch: ifName=%q netNSDir=%q prefix=%q", m.ifName, m.netNSDir, m.prefix)
	}
}

var _ = cnitypes.Result(&current.Result{})
