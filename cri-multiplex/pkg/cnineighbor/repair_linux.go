// Package cnineighbor repairs stale host ARP entries before a new CNI sandbox
// starts using an IP address previously assigned to another sandbox.
package cnineighbor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

var isLinux419 = sync.OnceValue(func() bool {
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		return false
	}
	return is419Release(unix.ByteSliceToString(uts.Release[:]))
})

func is419Release(release string) bool {
	return release == "4.19" || strings.HasPrefix(release, "4.19.")
}

// Repair removes an incorrect dynamic host neighbor for a newly assigned Pod
// IP. The bool reports whether an entry was removed. Call after CNI ADD and
// before starting traffic to the sandbox. Other kernels preserve their behavior.
func Repair(ctx context.Context, netNSPath, ifName, podIP string) (bool, error) {
	if !isLinux419() {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	ip := net.ParseIP(podIP).To4()
	if ip == nil || !ip.IsGlobalUnicast() {
		return false, fmt.Errorf("invalid CNI Pod IPv4 address %q", podIP)
	}
	host, err := netlink.NewHandle(unix.NETLINK_ROUTE)
	if err != nil {
		return false, fmt.Errorf("open host netlink handle: %w", err)
	}
	defer host.Close()
	if err := setTimeout(ctx, host); err != nil {
		return false, err
	}

	// Only the directly connected bridge path has been validated. In
	// particular, never delete a gateway neighbor for a routed CNI network.
	routes, err := host.RouteGet(ip)
	if err != nil {
		return false, fmt.Errorf("look up route to Pod %s: %w", podIP, err)
	}
	if len(routes) != 1 || routes[0].Type != unix.RTN_UNICAST ||
		len(routes[0].Gw) != 0 || routes[0].LinkIndex == 0 {
		return false, nil
	}
	link, err := host.LinkByIndex(routes[0].LinkIndex)
	if err != nil {
		return false, fmt.Errorf("look up Pod route interface: %w", err)
	}
	if link.Type() != "bridge" {
		return false, nil
	}

	ns, err := netns.GetFromPath(netNSPath)
	if err != nil {
		return false, fmt.Errorf("open CNI namespace: %w", err)
	}
	defer ns.Close()
	pod, err := netlink.NewHandleAt(ns, unix.NETLINK_ROUTE)
	if err != nil {
		return false, fmt.Errorf("open Pod netlink handle: %w", err)
	}
	defer pod.Close()
	if err := setTimeout(ctx, pod); err != nil {
		return false, err
	}
	podLink, err := pod.LinkByName(ifName)
	if err != nil {
		return false, fmt.Errorf("look up CNI interface %s: %w", ifName, err)
	}
	addrs, err := pod.AddrList(podLink, unix.AF_INET)
	if err != nil {
		return false, fmt.Errorf("read CNI interface addresses: %w", err)
	}
	owned := false
	for _, addr := range addrs {
		if addr.IP.Equal(ip) {
			owned = true
			break
		}
	}
	if !owned {
		return false, fmt.Errorf("Pod IP %s is not assigned to CNI interface %s", podIP, ifName)
	}
	actualMAC := podLink.Attrs().HardwareAddr
	if len(actualMAC) != 6 || actualMAC[0]&1 != 0 || bytes.Equal(actualMAC, make([]byte, 6)) {
		return false, fmt.Errorf("invalid MAC on CNI interface %s", ifName)
	}

	neighbors, err := host.NeighList(link.Attrs().Index, unix.AF_INET)
	if err != nil {
		return false, fmt.Errorf("read host Pod neighbors: %w", err)
	}
	for _, neighbor := range neighbors {
		if !neighbor.IP.Equal(ip) || len(neighbor.HardwareAddr) == 0 ||
			bytes.Equal(neighbor.HardwareAddr, actualMAC) {
			continue
		}
		const dynamic = netlink.NUD_REACHABLE | netlink.NUD_STALE | netlink.NUD_DELAY | netlink.NUD_PROBE
		if neighbor.State&dynamic == 0 || neighbor.State&(netlink.NUD_PERMANENT|netlink.NUD_NOARP) != 0 ||
			neighbor.Flags != 0 || neighbor.FlagsExt != 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		// CNI owns this new IP assignment. Delete exactly its dynamic mapping,
		// so the first connection resolves the new MAC instead of probing the
		// previous owner's MAC for ~8 seconds. Concurrent expiry is harmless.
		err := host.NeighDel(&netlink.Neigh{
			LinkIndex: link.Attrs().Index, Family: unix.AF_INET, IP: ip,
		})
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ESRCH) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("remove stale Pod neighbor %s on %s: %w", podIP, link.Attrs().Name, err)
		}
		return true, nil
	}
	return false, nil
}

func setTimeout(ctx context.Context, h *netlink.Handle) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timeout := time.Second
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining < time.Microsecond {
			return context.DeadlineExceeded
		}
		if remaining < timeout {
			timeout = remaining
		}
	}
	return h.SetSocketTimeout(timeout)
}
