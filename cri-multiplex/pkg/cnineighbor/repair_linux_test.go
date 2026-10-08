package cnineighbor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

func TestKernelGate(t *testing.T) {
	for _, release := range []string{"4.19", "4.19.90-2003.4.0.0036.oe1.aarch64"} {
		if !is419Release(release) {
			t.Fatalf("4.19 release rejected: %s", release)
		}
	}
	for _, release := range []string{"", "4.190.1", "4.18.0", "5.4.0", "6.6.0", "6.18.35.2-microsoft-standard-WSL2"} {
		if is419Release(release) {
			t.Fatalf("other release accepted: %s", release)
		}
	}
	old := isLinux419
	isLinux419 = func() bool { return false }
	t.Cleanup(func() { isLinux419 = old })
	changed, err := Repair(context.Background(), "/does-not-exist", "", "invalid")
	if changed || err != nil {
		t.Fatalf("disabled gate must bypass all network operations: %v, %v", changed, err)
	}
}

func enableRepair(t *testing.T) {
	t.Helper()
	old := isLinux419
	isLinux419 = func() bool { return true }
	t.Cleanup(func() { isLinux419 = old })
}

func inNamespace(ns netns.NsHandle, fn func() error) error {
	runtime.LockOSThread()
	original, err := netns.Get()
	if err != nil {
		runtime.UnlockOSThread()
		return err
	}
	defer original.Close()
	defer func() {
		if err := netns.Set(original); err != nil {
			panic(fmt.Sprintf("restore test namespace: %v", err))
		}
		runtime.UnlockOSThread()
	}()
	if err := netns.Set(ns); err != nil {
		return err
	}
	return fn()
}

func newNamespace(t *testing.T) netns.NsHandle {
	t.Helper()
	runtime.LockOSThread()
	original, err := netns.Get()
	if err != nil {
		runtime.UnlockOSThread()
		t.Fatal(err)
	}
	ns, createErr := netns.New()
	if err := netns.Set(original); err != nil {
		panic(fmt.Sprintf("restore test namespace: %v", err))
	}
	original.Close()
	runtime.UnlockOSThread()
	if createErr != nil {
		t.Fatalf("create isolated test namespace: %v", createErr)
	}
	t.Cleanup(func() { ns.Close() })
	return ns
}

type fixture struct {
	hostNS, podNS netns.NsHandle
	host, pod     *netlink.Handle
	bridge        netlink.Link
	podLink       netlink.Link
	podIP         net.IP
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if os.Getenv("CNI_NEIGHBOR_NETNS_TEST") != "1" {
		t.Skip("set CNI_NEIGHBOR_NETNS_TEST=1 to run isolated Linux network tests")
	}
	f := &fixture{hostNS: newNamespace(t), podNS: newNamespace(t), podIP: net.ParseIP("192.0.2.2")}
	var err error
	f.host, err = netlink.NewHandleAt(f.hostNS, unix.NETLINK_ROUTE)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.host.Close)
	f.pod, err = netlink.NewHandleAt(f.podNS, unix.NETLINK_ROUTE)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.pod.Close)
	br := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "test-bridge"}}
	if err := f.host.LinkAdd(br); err != nil {
		t.Fatal(err)
	}
	f.bridge, err = f.host.LinkByName(br.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.host.LinkSetUp(f.bridge); err != nil {
		t.Fatal(err)
	}
	addr, _ := netlink.ParseAddr("192.0.2.1/24")
	if err := f.host.AddrAdd(f.bridge, addr); err != nil {
		t.Fatal(err)
	}
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: "test-veth", MasterIndex: f.bridge.Attrs().Index},
		PeerName:  "eth0", PeerNamespace: netlink.NsFd(f.podNS),
	}
	if err := f.host.LinkAdd(veth); err != nil {
		t.Fatal(err)
	}
	hostLink, err := f.host.LinkByName(veth.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.host.LinkSetUp(hostLink); err != nil {
		t.Fatal(err)
	}
	f.podLink, err = f.pod.LinkByName("eth0")
	if err != nil {
		t.Fatal(err)
	}
	addr, _ = netlink.ParseAddr("192.0.2.2/24")
	if err := f.pod.AddrAdd(f.podLink, addr); err != nil {
		t.Fatal(err)
	}
	if err := f.pod.LinkSetUp(f.podLink); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) repair(ip string) (changed bool, err error) {
	err = inNamespace(f.hostNS, func() error {
		var repairErr error
		changed, repairErr = Repair(context.Background(), fmt.Sprintf("/proc/self/fd/%d", f.podNS), "eth0", ip)
		return repairErr
	})
	return
}

func (f *fixture) neighbors(t *testing.T) []netlink.Neigh {
	t.Helper()
	n, err := f.host.NeighList(f.bridge.Attrs().Index, unix.AF_INET)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRepairRestoresConnectivity(t *testing.T) {
	enableRepair(t)
	f := newFixture(t)
	var listener net.Listener
	if err := inNamespace(f.podNS, func() error {
		var err error
		listener, err = net.Listen("tcp4", "192.0.2.2:0")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	wrongMAC, _ := net.ParseMAC("02:00:00:00:00:99")
	entry := &netlink.Neigh{LinkIndex: f.bridge.Attrs().Index, Family: unix.AF_INET,
		IP: f.podIP, HardwareAddr: wrongMAC, State: netlink.NUD_STALE}
	if err := f.host.NeighSet(entry); err != nil {
		t.Fatal(err)
	}
	dial := func(timeout time.Duration) error {
		return inNamespace(f.hostNS, func() error {
			conn, err := net.DialTimeout("tcp4", listener.Addr().String(), timeout)
			if conn != nil {
				conn.Close()
			}
			return err
		})
	}
	if err := dial(120 * time.Millisecond); err == nil {
		t.Fatal("control unexpectedly connected through the obsolete MAC")
	}
	changed, err := f.repair(f.podIP.String())
	if err != nil || !changed {
		t.Fatalf("obsolete MAC was not removed: changed=%v err=%v", changed, err)
	}
	if err := dial(time.Second); err != nil {
		t.Fatalf("first connection after repair failed: %v", err)
	}
	for _, n := range f.neighbors(t) {
		if n.IP.Equal(f.podIP) && n.HardwareAddr.String() == f.podLink.Attrs().HardwareAddr.String() {
			return
		}
	}
	t.Fatal("successful connection did not learn the current Pod MAC")
}

func TestRepairPreservesOtherEntries(t *testing.T) {
	enableRepair(t)
	f := newFixture(t)
	wrongMAC, _ := net.ParseMAC("02:00:00:00:00:99")
	otherIP := net.ParseIP("192.0.2.3")
	for _, ip := range []net.IP{f.podIP, otherIP} {
		if err := f.host.NeighSet(&netlink.Neigh{LinkIndex: f.bridge.Attrs().Index, Family: unix.AF_INET,
			IP: ip, HardwareAddr: wrongMAC, State: netlink.NUD_STALE}); err != nil {
			t.Fatal(err)
		}
	}
	changed, err := f.repair(f.podIP.String())
	if err != nil || !changed {
		t.Fatalf("repair: %v %v", changed, err)
	}
	entries := f.neighbors(t)
	if len(entries) != 1 || !entries[0].IP.Equal(otherIP) || entries[0].HardwareAddr.String() != wrongMAC.String() {
		t.Fatalf("unrelated neighbor changed: %+v", entries)
	}
}

func TestRepairLeavesCorrectMissingAndProtectedEntries(t *testing.T) {
	enableRepair(t)
	f := newFixture(t)
	if changed, err := f.repair(f.podIP.String()); err != nil || changed {
		t.Fatalf("missing entry: %v %v", changed, err)
	}
	wrongMAC, _ := net.ParseMAC("02:00:00:00:00:99")
	for _, tc := range []struct {
		name         string
		mac          net.HardwareAddr
		state, flags int
	}{
		{"correct", f.podLink.Attrs().HardwareAddr, netlink.NUD_STALE, 0},
		{"permanent", wrongMAC, netlink.NUD_PERMANENT, 0},
		{"externally managed", wrongMAC, netlink.NUD_REACHABLE, netlink.NTF_EXT_LEARNED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := &netlink.Neigh{LinkIndex: f.bridge.Attrs().Index, Family: unix.AF_INET,
				IP: f.podIP, HardwareAddr: tc.mac, State: tc.state, Flags: tc.flags}
			if err := f.host.NeighSet(entry); err != nil {
				t.Fatal(err)
			}
			if changed, err := f.repair(f.podIP.String()); err != nil || changed {
				t.Fatalf("protected/no-op repair: %v %v", changed, err)
			}
			entries := f.neighbors(t)
			if len(entries) != 1 || entries[0].HardwareAddr.String() != tc.mac.String() {
				t.Fatalf("entry changed: %+v", entries)
			}
			if err := f.host.NeighDel(entry); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRepairRejectsUnownedIP(t *testing.T) {
	enableRepair(t)
	f := newFixture(t)
	if changed, err := f.repair("192.0.2.99"); changed || err == nil {
		t.Fatalf("unassigned IP must not be repaired: %v %v", changed, err)
	}
}

func TestRepairHonorsCancellation(t *testing.T) {
	enableRepair(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if changed, err := Repair(ctx, "/does-not-exist", "eth0", "192.0.2.2"); changed || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled repair: %v %v", changed, err)
	}
}

func TestRepairSkipsRoutedAndNonBridgeNetworks(t *testing.T) {
	enableRepair(t)
	f := newFixture(t)
	_, dst, _ := net.ParseCIDR("198.51.100.0/24")
	if err := f.host.RouteAdd(&netlink.Route{LinkIndex: f.bridge.Attrs().Index, Dst: dst, Gw: f.podIP}); err != nil {
		t.Fatal(err)
	}
	// A missing namespace makes an accidental attempt to inspect the Pod fail.
	check := func(ip string) {
		t.Helper()
		if err := inNamespace(f.hostNS, func() error {
			changed, err := Repair(context.Background(), "/does-not-exist", "eth0", ip)
			if changed {
				return fmt.Errorf("unexpected repair of %s", ip)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	check("198.51.100.2")
	dummy := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "routed-cni"}}
	if err := f.host.LinkAdd(dummy); err != nil {
		t.Fatal(err)
	}
	if err := f.host.LinkSetUp(dummy); err != nil {
		t.Fatal(err)
	}
	addr, _ := netlink.ParseAddr("203.0.113.1/24")
	if err := f.host.AddrAdd(dummy, addr); err != nil {
		t.Fatal(err)
	}
	check("203.0.113.2")
}

func TestRepairConcurrentAddresses(t *testing.T) {
	enableRepair(t)
	f := newFixture(t)
	const count = 50
	wrongMAC, _ := net.ParseMAC("02:00:00:00:00:99")
	ips := make([]string, count)
	for i := range ips {
		ips[i] = fmt.Sprintf("192.0.2.%d", i+10)
		addr, _ := netlink.ParseAddr(ips[i] + "/24")
		if err := f.pod.AddrAdd(f.podLink, addr); err != nil {
			t.Fatal(err)
		}
		if err := f.host.NeighSet(&netlink.Neigh{LinkIndex: f.bridge.Attrs().Index, Family: unix.AF_INET,
			IP: net.ParseIP(ips[i]), HardwareAddr: wrongMAC, State: netlink.NUD_STALE}); err != nil {
			t.Fatal(err)
		}
	}
	results := make(chan error, count)
	start := time.Now()
	for _, ip := range ips {
		go func() {
			changed, err := f.repair(ip)
			if err == nil && !changed {
				err = fmt.Errorf("entry not removed: %s", ip)
			}
			results <- err
		}()
	}
	for range count {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	t.Logf("repaired %d addresses concurrently in %s (isolated namespace, not VM startup)", count, time.Since(start))
	for _, entry := range f.neighbors(t) {
		for _, ip := range ips {
			if entry.IP.Equal(net.ParseIP(ip)) {
				t.Errorf("target neighbor remains: %+v", entry)
			}
		}
	}
}
