//go:build linux

package network

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

type notExistError interface {
	IsNotExist() bool
}

type multiUnwrapError interface {
	Unwrap() []error
}

func ignoreExpectedAbsent(err error, isExpected func(error) bool) bool {
	if err == nil {
		return true
	}

	var joined multiUnwrapError
	if errors.As(err, &joined) {
		for _, child := range joined.Unwrap() {
			if !ignoreExpectedAbsent(child, isExpected) {
				return false
			}
		}

		return true
	}

	return isExpected(err)
}

func isIPTablesNotExist(err error) bool {
	var notExist notExistError

	return errors.As(err, &notExist) && notExist.IsNotExist()
}

func isRouteNotExist(err error) bool {
	return errors.Is(err, unix.ESRCH) || errors.Is(err, unix.ENOENT)
}

func isLinkNotExist(err error) bool {
	var linkNotFound netlink.LinkNotFoundError

	return errors.As(err, &linkNotFound) || errors.Is(err, unix.ENODEV) || errors.Is(err, unix.ENOENT)
}

func isNamespaceNotExist(err error) bool {
	return os.IsNotExist(err) || errors.Is(err, unix.ENOENT)
}

func appendUnlessExpectedAbsentf(errs *[]error, err error, isExpected func(error) bool, format string) {
	if ignoreExpectedAbsent(err, isExpected) {
		return
	}

	*errs = append(*errs, fmt.Errorf(format, err))
}

func (s *Slot) CreateNetwork(ctx context.Context) (retErr error) {
	if s.ExternalNetNS {
		return s.CreateExternalNetNSNetwork(ctx)
	}

	// Prevent thread changes so we can safely manipulate with namespaces
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Save the original (host) namespace and restore it upon function exit
	hostNS, err := netns.Get()
	if err != nil {
		return fmt.Errorf("cannot get current (host) namespace: %w", err)
	}

	cleanupNeeded := false
	defer func() {
		restoreErr := netns.Set(hostNS)
		if restoreErr != nil {
			logger.L().Error(ctx, "error resetting network namespace back to the host namespace", zap.Error(restoreErr))
		}

		if retErr != nil && cleanupNeeded {
			if restoreErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("error resetting network namespace back to the host namespace before cleanup: %w", restoreErr))
			} else if cleanupErr := s.RemoveNetwork(); cleanupErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("error cleaning up partially created network: %w", cleanupErr))
			}
		}

		err = hostNS.Close()
		if err != nil {
			logger.L().Error(ctx, "error closing host network namespace", zap.Error(err))
		}
	}()

	// An existing namespace for this index is a stale reclaim anchor from a
	// failed teardown whose iptables rules, routes and veth may still exist. Run
	// a full (idempotent) RemoveNetwork to reclaim them; deleting only the
	// namespace would orphan those rules. On failure the anchor is kept and we
	// abort so the slot is retried later instead of leaking.
	available, err := isNamespaceAvailable(NetNamespacesDir, s.NamespaceID())
	if err != nil {
		return fmt.Errorf("cannot check for stale namespace: %w", err)
	}
	if !available {
		if err = s.RemoveNetwork(); err != nil {
			return fmt.Errorf("cannot reclaim stale network slot: %w", err)
		}
	}

	// Create NS for the sandbox
	ns, err := netns.NewNamed(s.NamespaceID())
	if err != nil {
		return fmt.Errorf("cannot create new namespace: %w", err)
	}
	cleanupNeeded = true

	defer ns.Close()

	// Create the Veth and Vpeer
	vethAttrs := netlink.NewLinkAttrs()
	vethAttrs.Name = s.VethName()
	veth := &netlink.Veth{
		LinkAttrs: vethAttrs,
		PeerName:  s.VpeerName(),
	}

	err = netlink.LinkAdd(veth)
	if err != nil {
		return fmt.Errorf("error creating veth device: %w", err)
	}

	vpeer, err := netlink.LinkByName(s.VpeerName())
	if err != nil {
		return fmt.Errorf("error finding vpeer: %w", err)
	}

	err = netlink.LinkSetUp(vpeer)
	if err != nil {
		return fmt.Errorf("error setting vpeer device up: %w", err)
	}

	err = netlink.AddrAdd(vpeer, &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   s.VpeerIP(),
			Mask: s.VrtMask(),
		},
	})
	if err != nil {
		return fmt.Errorf("error adding vpeer device address: %w", err)
	}

	// Move Veth device to the host NS
	err = netlink.LinkSetNsFd(veth, int(hostNS))
	if err != nil {
		return fmt.Errorf("error moving veth device to the host namespace: %w", err)
	}

	err = netns.Set(hostNS)
	if err != nil {
		return fmt.Errorf("error setting network namespace: %w", err)
	}

	vethInHost, err := netlink.LinkByName(s.VethName())
	if err != nil {
		return fmt.Errorf("error finding veth: %w", err)
	}

	err = netlink.LinkSetUp(vethInHost)
	if err != nil {
		return fmt.Errorf("error setting veth device up: %w", err)
	}

	err = netlink.AddrAdd(vethInHost, &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   s.VethIP(),
			Mask: s.VrtMask(),
		},
	})
	if err != nil {
		return fmt.Errorf("error adding veth device address: %w", err)
	}

	err = netns.Set(ns)
	if err != nil {
		return fmt.Errorf("error setting network namespace to %s: %w", ns.String(), err)
	}

	// Create Tap device for FC in NS
	tapAttrs := netlink.NewLinkAttrs()
	tapAttrs.Name = s.TapName()
	tapAttrs.Namespace = ns
	tap := &netlink.Tuntap{
		Mode:      netlink.TUNTAP_MODE_TAP,
		LinkAttrs: tapAttrs,
	}

	err = netlink.LinkAdd(tap)
	if err != nil {
		return fmt.Errorf("error creating tap device: %w", err)
	}

	// Keep resumed guests' cached gateway ARP entries valid. Tap devices are
	// isolated in per-sandbox network namespaces, so sharing this MAC is safe.
	err = netlink.LinkSetHardwareAddr(tap, tapHostHardwareAddr)
	if err != nil {
		return fmt.Errorf("error setting tap device hardware address: %w", err)
	}

	err = netlink.LinkSetUp(tap)
	if err != nil {
		return fmt.Errorf("error setting tap device up: %w", err)
	}

	err = netlink.AddrAdd(tap, &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   s.TapIP(),
			Mask: s.TapCIDR(),
		},
	})
	if err != nil {
		return fmt.Errorf("error setting address of the tap device: %w", err)
	}

	// The second TAP (cvd-mtap) serves the standalone config_server of Android
	// guests. It lives in the same namespace and uses its own /30. A missing
	// cvd-mtap must not fail sandbox creation for guests that never run
	// config_server, so log and continue on setup errors.
	if err := s.createExtraTap(); err != nil {
		logger.L().Warn(ctx, "cvd-mtap setup failed; continuing without the Android config_server tap", zap.Error(err))
	}

	s.tuneTapNeighbor(ctx)

	// Set NS lo device up
	lo, err := netlink.LinkByName(loopbackInterface)
	if err != nil {
		return fmt.Errorf("error finding lo: %w", err)
	}

	err = netlink.LinkSetUp(lo)
	if err != nil {
		return fmt.Errorf("error setting lo device up: %w", err)
	}

	// Add NS default route
	err = netlink.RouteAdd(&netlink.Route{
		Scope: netlink.SCOPE_UNIVERSE,
		Gw:    s.VethIP(),
	})
	if err != nil {
		return fmt.Errorf("error adding default NS route: %w", err)
	}

	tables, err := iptables.New()
	if err != nil {
		return fmt.Errorf("error initializing iptables: %w", err)
	}

	// Add NAT routing rules to NS
	err = tables.Append("nat", "POSTROUTING", "-o", s.VpeerName(), "-s", s.NamespaceIP(), "-j", "SNAT", "--to", s.HostIPString())
	if err != nil {
		return fmt.Errorf("error creating postrouting rule to vpeer: %w", err)
	}

	err = tables.Append("nat", "PREROUTING", "-i", s.VpeerName(), "-d", s.HostIPString(), "-j", "DNAT", "--to", s.NamespaceIP())
	if err != nil {
		return fmt.Errorf("error creating postrouting rule from vpeer: %w", err)
	}

	// Marker for downstream L3 firewalls. 0 disables; see SANDBOX_EGRESS_DSCP.
	// Slots are pooled: this seeds the untenanted class, Pool.Get re-stamps
	// for builds, recycle restores it.
	dscp := s.config.untenantedDSCP()
	if dscp > 0 {
		err = tables.Append("mangle", "POSTROUTING", s.dscpMangleRuleArgs(dscp)...)
		if err != nil {
			return fmt.Errorf("error creating DSCP mangle rule on vpeer: %w", err)
		}
	}
	s.egressDSCP.Store(uint32(dscp))

	err = s.InitializeFirewall()
	if err != nil {
		return fmt.Errorf("error initializing slot firewall: %w", err)
	}

	// Go back to original namespace
	err = netns.Set(hostNS)
	if err != nil {
		return fmt.Errorf("error setting network namespace to %s: %w", hostNS.String(), err)
	}

	// Add routing from host to FC namespace
	err = netlink.RouteAdd(&netlink.Route{
		Gw:  s.VpeerIP(),
		Dst: s.HostNet(),
	})
	if err != nil {
		return fmt.Errorf("error adding route from host to FC: %w", err)
	}

	// Add host forwarding rules
	err = tables.Append("filter", "FORWARD", "-i", s.VethName(), "-o", defaultGateway, "-j", "ACCEPT")
	if err != nil {
		return fmt.Errorf("error creating forwarding rule to default gateway: %w", err)
	}

	err = tables.Append("filter", "FORWARD", "-i", defaultGateway, "-o", s.VethName(), "-j", "ACCEPT")
	if err != nil {
		return fmt.Errorf("error creating forwarding rule from default gateway: %w", err)
	}

	// Add host postrouting rules
	err = tables.Append("nat", "POSTROUTING", "-s", s.HostCIDR(), "-o", defaultGateway, "-j", "MASQUERADE")
	if err != nil {
		return fmt.Errorf("error creating postrouting rule: %w", err)
	}

	// Redirect traffic destined for hyperloop proxy
	err = tables.Append(
		"nat", "PREROUTING", "-i", s.VethName(),
		"-p", "tcp", "-d", s.config.OrchestratorInSandboxIPAddress, "--dport", "80",
		"-j", "REDIRECT", "--to-port", s.hyperloopPort,
	)
	if err != nil {
		return fmt.Errorf("error creating HTTP redirect rule to sandbox hyperloop proxy server: %w", err)
	}

	// Redirect traffic destined for portmapper
	err = tables.Append("nat", "PREROUTING",
		"--in-interface", s.VethName(), "--protocol", "tcp",
		"--destination", s.config.OrchestratorInSandboxIPAddress, "--dport", "111",
		"--jump", "REDIRECT", "--to-port", fmt.Sprintf("%d", s.config.PortmapperPort),
	)
	if err != nil {
		return fmt.Errorf("error creating NFS redirect rule to sandbox portmapper server: %w", err)
	}

	// Redirect traffic destined for NFS proxy
	err = tables.Append("nat", "PREROUTING",
		"--in-interface", s.VethName(), "--protocol", "tcp",
		"--destination", s.config.OrchestratorInSandboxIPAddress, "--dport", "2049",
		"--jump", "REDIRECT", "--to-port", fmt.Sprintf("%d", s.config.NFSProxyPort),
	)
	if err != nil {
		return fmt.Errorf("error creating NFS redirect rule to sandbox NFS proxy server: %w", err)
	}

	// Create rules needed by egress proxy
	err = s.egressProxy.OnSlotCreate(s, tables)
	if err != nil {
		return err
	}

	return nil
}

func (s *Slot) RemoveNetwork() error {
	if s.ExternalNetNS {
		return s.RemoveExternalNetNSNetwork()
	}

	return s.removeNetwork(true)
}

// removeNetwork with deleteHostRules false leaves the slot's host iptables
// rules alone: Pool.Close has already removed them for the whole pool with
// DeleteSlotsHostRules.
func (s *Slot) removeNetwork(deleteHostRules bool) error {
	var errs []error

	err := s.CloseFirewall()
	if err != nil {
		errs = append(errs, fmt.Errorf("error closing firewall: %w", err))
	}

	var tables *iptables.IPTables
	if deleteHostRules {
		tables, err = iptables.New()
		if err != nil {
			errs = append(errs, fmt.Errorf("error initializing iptables: %w", err))
			tables = nil
		}
	}

	if tables != nil {
		// Delete host forwarding rules
		err = tables.Delete("filter", "FORWARD", "-i", s.VethName(), "-o", defaultGateway, "-j", "ACCEPT")
		appendUnlessExpectedAbsentf(&errs, err, isIPTablesNotExist, "error deleting host forwarding rule to default gateway: %w")

		err = tables.Delete("filter", "FORWARD", "-i", defaultGateway, "-o", s.VethName(), "-j", "ACCEPT")
		appendUnlessExpectedAbsentf(&errs, err, isIPTablesNotExist, "error deleting host forwarding rule from default gateway: %w")

		// Delete host postrouting rules
		err = tables.Delete("nat", "POSTROUTING", "-s", s.HostCIDR(), "-o", defaultGateway, "-j", "MASQUERADE")
		appendUnlessExpectedAbsentf(&errs, err, isIPTablesNotExist, "error deleting host postrouting rule: %w")

		// Delete hyperloop proxy redirect rule
		err = tables.Delete(
			"nat", "PREROUTING", "-i", s.VethName(),
			"-p", "tcp", "-d", s.config.OrchestratorInSandboxIPAddress, "--dport", "80",
			"-j", "REDIRECT", "--to-port", s.hyperloopPort,
		)
		appendUnlessExpectedAbsentf(&errs, err, isIPTablesNotExist, "error deleting sandbox hyperloop proxy redirect rule: %w")

		// Delete changes made by egress proxy
		err = s.egressProxy.OnSlotDelete(s, tables)
		appendUnlessExpectedAbsentf(&errs, err, isIPTablesNotExist, "%w")
	}

	// Delete routing from host to FC namespace
	err = netlink.RouteDel(&netlink.Route{
		Gw:  s.VpeerIP(),
		Dst: s.HostNet(),
	})
	appendUnlessExpectedAbsentf(&errs, err, isRouteNotExist, "error deleting route from host to FC: %w")

	// Delete veth device
	// We explicitly delete the veth device from the host namespace because even though deleting
	// is deleting the device there may be a race condition when creating a new veth device with
	// the same name immediately after deleting the namespace.
	veth, err := netlink.LinkByName(s.VethName())
	if err != nil {
		appendUnlessExpectedAbsentf(&errs, err, isLinkNotExist, "error finding veth: %w")
	} else {
		err = netlink.LinkDel(veth)
		appendUnlessExpectedAbsentf(&errs, err, isLinkNotExist, "error deleting veth device: %w")
	}

	if tables != nil {
		// Delete NFS proxy redirect rule
		err = tables.Delete("nat", "PREROUTING",
			"--in-interface", s.VethName(), "--protocol", "tcp",
			"--destination", s.config.OrchestratorInSandboxIPAddress, "--dport", "2049",
			"--jump", "REDIRECT", "--to-port", strconv.Itoa(int(s.config.NFSProxyPort)),
		)
		appendUnlessExpectedAbsentf(&errs, err, isIPTablesNotExist, "error deleting sandbox NFS proxy redirect rule: %w")

		// Delete portmapper redirect rule
		err = tables.Delete("nat", "PREROUTING",
			"--in-interface", s.VethName(), "--protocol", "tcp",
			"--destination", s.config.OrchestratorInSandboxIPAddress, "--dport", "111",
			"--jump", "REDIRECT", "--to-port", strconv.Itoa(int(s.config.PortmapperPort)),
		)
		appendUnlessExpectedAbsentf(&errs, err, isIPTablesNotExist, "error deleting sandbox portmapper redirect rule: %w")
	}

	// Delete the named namespace only after every host-side (root namespace)
	// teardown above has succeeded. The /run/netns entry is the anchor that
	// startup reclaim uses to rediscover a leaked slot, and the host-side
	// iptables/route/veth state removed above is keyed by the slot index. If any
	// of that teardown failed, deleting the namespace now would orphan the
	// remaining state with no way to rediscover and retry it. Preserving the
	// anchor lets the next teardown attempt (including startup reclaim) finish
	// the job. CreateNetwork removes a stale anchor before reusing the slot.
	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	err = deleteNamedNamespace(s.NamespaceID())
	appendUnlessExpectedAbsentf(&errs, err, isNamespaceNotExist, "error deleting namespace: %w")

	return errors.Join(errs...)
}

// CreateExternalNetNSNetwork wires an external netns slot inside the netns
// prepared by the CRI shim: the tap device for Firecracker, NAT between the
// pod IP and the guest, forwarding between the pod interface and the tap, and
// the slot firewall. The pod netns already has its interface, address and
// default route from CNI.
func (s *Slot) CreateExternalNetNSNetwork(ctx context.Context) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hostNS, err := netns.Get()
	if err != nil {
		return fmt.Errorf("cannot get current (host) namespace: %w", err)
	}
	defer func() {
		if setErr := netns.Set(hostNS); setErr != nil {
			logger.L().Error(ctx, "error resetting network namespace back to the host namespace", zap.Error(setErr))
		}
		if closeErr := hostNS.Close(); closeErr != nil {
			logger.L().Error(ctx, "error closing host network namespace", zap.Error(closeErr))
		}
	}()

	targetNS, err := netns.GetFromPath(s.NetNSPath)
	if err != nil {
		return fmt.Errorf("cannot open external network namespace %q: %w", s.NetNSPath, err)
	}
	defer targetNS.Close()

	if err = netns.Set(targetNS); err != nil {
		return fmt.Errorf("error setting external network namespace %q: %w", s.NetNSPath, err)
	}

	if lo, err := netlink.LinkByName(loopbackInterface); err == nil {
		if err = netlink.LinkSetUp(lo); err != nil {
			return fmt.Errorf("error setting lo device up: %w", err)
		}
	} else {
		return fmt.Errorf("error finding lo: %w", err)
	}

	tapAttrs := netlink.NewLinkAttrs()
	tapAttrs.Name = s.TapName()
	tap := &netlink.Tuntap{
		Mode:      netlink.TUNTAP_MODE_TAP,
		LinkAttrs: tapAttrs,
	}

	if err = netlink.LinkAdd(tap); err != nil {
		return fmt.Errorf("error creating external netns tap device: %w", err)
	}

	tapLink, err := netlink.LinkByName(s.TapName())
	if err != nil {
		return fmt.Errorf("error finding external netns tap device: %w", err)
	}

	if err = netlink.AddrAdd(tapLink, &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   s.TapIP(),
			Mask: s.TapCIDR(),
		},
	}); err != nil {
		return fmt.Errorf("error setting address of external netns tap device: %w", err)
	}

	if err = netlink.LinkSetUp(tapLink); err != nil {
		return fmt.Errorf("error setting external netns tap device up: %w", err)
	}

	if err = s.createExtraTapExternal(); err != nil {
		logger.L().Warn(ctx, "external netns cvd-mtap setup failed; continuing without the Android config_server tap", zap.Error(err))
	}

	if err = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644); err != nil {
		return fmt.Errorf("error enabling ip_forward in external netns: %w", err)
	}

	tables, err := iptablesHandle()
	if err != nil {
		return fmt.Errorf("error initializing iptables in external netns: %w", err)
	}

	// Install all per-slot iptables rules in a single iptables-restore
	// transaction. Sequential `iptables` execs serialize on the global
	// xtables.lock across all concurrent sandbox creations on the host,
	// which dominates slot setup time under bulk load.
	if err = s.applyExternalNetNSRules(tables); err != nil {
		return err
	}

	if err = s.InitializeFirewall(); err != nil {
		return fmt.Errorf("error initializing external netns slot firewall: %w", err)
	}

	return nil
}

// externalNetNSRulesScript is the iptables-restore input for the 4 per-slot
// rules of the external netns mode.
func (s *Slot) externalNetNSRulesScript() string {
	var b strings.Builder
	b.WriteString("*nat\n")
	fmt.Fprintf(&b, "-A PREROUTING -d %s -j DNAT --to-destination %s\n", s.HostIPString(), s.NamespaceIP())
	fmt.Fprintf(&b, "-A POSTROUTING -s %s -j SNAT --to-source %s\n", s.NamespaceIP(), s.HostIPString())
	b.WriteString("COMMIT\n*filter\n")
	fmt.Fprintf(&b, "-A FORWARD -i %s -o %s -j ACCEPT\n", s.VpeerName(), s.TapName())
	fmt.Fprintf(&b, "-A FORWARD -i %s -o %s -j ACCEPT\n", s.TapName(), s.VpeerName())
	b.WriteString("COMMIT\n")

	return b.String()
}

// applyExternalNetNSRules installs the 4 per-slot iptables rules of the
// external netns mode with one `iptables-restore --noflush` call instead of 4
// separate `iptables` execs. The rules are private to the sandbox netns, so
// the per-slot rules are private to the sandbox netns, but iptables-restore
// still takes the global xtables lock, so wait on contention instead of
// failing with status 4 under concurrent creations. Falls back to sequential
// appends when iptables-restore is not available.
func (s *Slot) applyExternalNetNSRules(tables *iptables.IPTables) error {
	cmd := exec.Command("iptables-restore", "--noflush", "-w")
	cmd.Stdin = strings.NewReader(s.externalNetNSRulesScript())
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}

	if errors.Is(err, exec.ErrNotFound) {
		return s.appendExternalNetNSRules(tables)
	}

	return fmt.Errorf("error applying external netns rules via iptables-restore: %w (output: %s)", err, strings.TrimSpace(string(out)))
}

// appendExternalNetNSRules is the sequential fallback for
// applyExternalNetNSRules when iptables-restore is missing.
func (s *Slot) appendExternalNetNSRules(tables *iptables.IPTables) error {
	if err := tables.Append("nat", "PREROUTING", "-d", s.HostIPString(), "-j", "DNAT", "--to-destination", s.NamespaceIP()); err != nil {
		return fmt.Errorf("error creating external netns dnat rule: %w", err)
	}
	if err := tables.Append("nat", "POSTROUTING", "-s", s.NamespaceIP(), "-j", "SNAT", "--to-source", s.HostIPString()); err != nil {
		return fmt.Errorf("error creating external netns snat rule: %w", err)
	}
	if err := tables.Append("filter", "FORWARD", "-i", s.VpeerName(), "-o", s.TapName(), "-j", "ACCEPT"); err != nil {
		return fmt.Errorf("error creating external netns forward rule to tap: %w", err)
	}
	if err := tables.Append("filter", "FORWARD", "-i", s.TapName(), "-o", s.VpeerName(), "-j", "ACCEPT"); err != nil {
		return fmt.Errorf("error creating external netns forward rule from tap: %w", err)
	}

	return nil
}

// RemoveExternalNetNSNetwork undoes CreateExternalNetNSNetwork. The netns
// itself belongs to the CRI shim and is left in place.
func (s *Slot) RemoveExternalNetNSNetwork() error {
	var errs []error

	err := s.CloseFirewall()
	if err != nil {
		errs = append(errs, fmt.Errorf("error closing external netns firewall: %w", err))
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hostNS, err := netns.Get()
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("cannot get current (host) namespace: %w", err))...)
	}
	defer func() {
		_ = netns.Set(hostNS)
		_ = hostNS.Close()
	}()

	targetNS, err := netns.GetFromPath(s.NetNSPath)
	if err != nil {
		errs = append(errs, fmt.Errorf("cannot open external network namespace %q: %w", s.NetNSPath, err))

		return errors.Join(errs...)
	}
	defer targetNS.Close()

	if err = netns.Set(targetNS); err != nil {
		errs = append(errs, fmt.Errorf("error setting external network namespace %q: %w", s.NetNSPath, err))

		return errors.Join(errs...)
	}

	tables, err := iptablesHandle()
	if err != nil {
		errs = append(errs, fmt.Errorf("error initializing iptables in external netns: %w", err))
	} else {
		if err = tables.Delete("nat", "PREROUTING", "-d", s.HostIPString(), "-j", "DNAT", "--to-destination", s.NamespaceIP()); err != nil {
			errs = append(errs, fmt.Errorf("error deleting external netns dnat rule: %w", err))
		}
		if err = tables.Delete("nat", "POSTROUTING", "-s", s.NamespaceIP(), "-j", "SNAT", "--to-source", s.HostIPString()); err != nil {
			errs = append(errs, fmt.Errorf("error deleting external netns snat rule: %w", err))
		}
		if err = tables.Delete("filter", "FORWARD", "-i", s.VpeerName(), "-o", s.TapName(), "-j", "ACCEPT"); err != nil {
			errs = append(errs, fmt.Errorf("error deleting external netns forward rule to tap: %w", err))
		}
		if err = tables.Delete("filter", "FORWARD", "-i", s.TapName(), "-o", s.VpeerName(), "-j", "ACCEPT"); err != nil {
			errs = append(errs, fmt.Errorf("error deleting external netns forward rule from tap: %w", err))
		}
	}

	if tap, err := netlink.LinkByName(s.TapName()); err == nil {
		if err = netlink.LinkDel(tap); err != nil {
			errs = append(errs, fmt.Errorf("error deleting external netns tap device: %w", err))
		}
	} else {
		errs = append(errs, fmt.Errorf("error finding external netns tap device: %w", err))
	}

	return errors.Join(errs...)
}

// createExtraTap creates the cvd-mtap device in the slot's network namespace.
func (s *Slot) createExtraTap() error {
	attrs := netlink.NewLinkAttrs()
	attrs.Name = s.ExtraTapName()
	tap := &netlink.Tuntap{
		Mode:      netlink.TUNTAP_MODE_TAP,
		LinkAttrs: attrs,
	}
	if err := netlink.LinkAdd(tap); err != nil {
		return fmt.Errorf("error creating cvd-mtap device: %w", err)
	}
	link, err := netlink.LinkByName(s.ExtraTapName())
	if err != nil {
		return fmt.Errorf("error finding cvd-mtap device: %w", err)
	}
	if err := netlink.AddrAdd(link, &netlink.Addr{
		IPNet: &net.IPNet{IP: s.ExtraTapIP(), Mask: s.ExtraTapCIDR()},
	}); err != nil {
		return fmt.Errorf("error setting address of cvd-mtap device: %w", err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("error setting cvd-mtap device up: %w", err)
	}

	return nil
}

// createExtraTapExternal creates the cvd-mtap device in the external (CNI) netns.
func (s *Slot) createExtraTapExternal() error {
	attrs := netlink.NewLinkAttrs()
	attrs.Name = s.ExtraTapName()
	tap := &netlink.Tuntap{
		Mode:      netlink.TUNTAP_MODE_TAP,
		LinkAttrs: attrs,
	}
	if err := netlink.LinkAdd(tap); err != nil {
		return fmt.Errorf("error creating external netns cvd-mtap device: %w", err)
	}
	link, err := netlink.LinkByName(s.ExtraTapName())
	if err != nil {
		return fmt.Errorf("error finding external netns cvd-mtap device: %w", err)
	}
	if err := netlink.AddrAdd(link, &netlink.Addr{
		IPNet: &net.IPNet{IP: s.ExtraTapIP(), Mask: s.ExtraTapCIDR()},
	}); err != nil {
		return fmt.Errorf("error setting address of external netns cvd-mtap device: %w", err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("error setting external netns cvd-mtap device up: %w", err)
	}

	return nil
}
