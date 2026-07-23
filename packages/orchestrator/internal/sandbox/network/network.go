package network

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"

	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

func (s *Slot) CreateNetwork(ctx context.Context) error {
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

	defer func() {
		err = netns.Set(hostNS)
		if err != nil {
			logger.L().Error(ctx, "error resetting network namespace back to the host namespace", zap.Error(err))
		}

		err = hostNS.Close()
		if err != nil {
			logger.L().Error(ctx, "error closing host network namespace", zap.Error(err))
		}
	}()

	// Create NS for the sandbox
	ns, err := netns.NewNamed(s.NamespaceID())
	if err != nil {
		return fmt.Errorf("cannot create new namespace: %w", err)
	}

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

	// Redirect TCP traffic to appropriate egress proxy ports based on destination port.
	// This preserves the original destination IP for SO_ORIGINAL_DST.
	err = s.tcpProxyConfig().append(tables)
	if err != nil {
		return err
	}

	return nil
}

func (s *Slot) RemoveNetwork() error {
	if s.ExternalNetNS {
		return s.RemoveExternalNetNSNetwork()
	}

	var errs []error

	err := s.CloseFirewall()
	if err != nil {
		errs = append(errs, fmt.Errorf("error closing firewall: %w", err))
	}

	tables, err := iptables.New()
	if err != nil {
		errs = append(errs, fmt.Errorf("error initializing iptables: %w", err))
	} else {
		// Delete host forwarding rules
		err = tables.Delete("filter", "FORWARD", "-i", s.VethName(), "-o", defaultGateway, "-j", "ACCEPT")
		if err != nil {
			errs = append(errs, fmt.Errorf("error deleting host forwarding rule to default gateway: %w", err))
		}

		err = tables.Delete("filter", "FORWARD", "-i", defaultGateway, "-o", s.VethName(), "-j", "ACCEPT")
		if err != nil {
			errs = append(errs, fmt.Errorf("error deleting host forwarding rule from default gateway: %w", err))
		}

		// Delete host postrouting rules
		err = tables.Delete("nat", "POSTROUTING", "-s", s.HostCIDR(), "-o", defaultGateway, "-j", "MASQUERADE")
		if err != nil {
			errs = append(errs, fmt.Errorf("error deleting host postrouting rule: %w", err))
		}

		// Delete hyperloop proxy redirect rule
		err = tables.Delete(
			"nat", "PREROUTING", "-i", s.VethName(),
			"-p", "tcp", "-d", s.config.OrchestratorInSandboxIPAddress, "--dport", "80",
			"-j", "REDIRECT", "--to-port", s.hyperloopPort,
		)
		if err != nil {
			errs = append(errs, fmt.Errorf("error deleting sandbox hyperloop proxy redirect rule: %w", err))
		}

		// Delete egress proxy redirect rules
		errs = append(errs, s.tcpProxyConfig().delete(tables)...)
	}

	// Delete routing from host to FC namespace
	err = netlink.RouteDel(&netlink.Route{
		Gw:  s.VpeerIP(),
		Dst: s.HostNet(),
	})
	if err != nil {
		errs = append(errs, fmt.Errorf("error deleting route from host to FC: %w", err))
	}

	// Delete veth device
	// We explicitly delete the veth device from the host namespace because even though deleting
	// is deleting the device there may be a race condition when creating a new veth device with
	// the same name immediately after deleting the namespace.
	veth, err := netlink.LinkByName(s.VethName())
	if err != nil {
		errs = append(errs, fmt.Errorf("error finding veth: %w", err))
	} else {
		err = netlink.LinkDel(veth)
		if err != nil {
			errs = append(errs, fmt.Errorf("error deleting veth device: %w", err))
		}
	}

	// Delete NFS proxy redirect rule
	err = tables.Delete("nat", "PREROUTING",
		"--in-interface", s.VethName(), "--protocol", "tcp",
		"--destination", s.config.OrchestratorInSandboxIPAddress, "--dport", "2049",
		"--jump", "REDIRECT", "--to-port", strconv.Itoa(int(s.config.NFSProxyPort)),
	)
	if err != nil {
		errs = append(errs, fmt.Errorf("error deleting sandbox NFS proxy redirect rule: %w", err))
	}

	// Delete portmapper redirect rule
	err = tables.Delete("nat", "PREROUTING",
		"--in-interface", s.VethName(), "--protocol", "tcp",
		"--destination", s.config.OrchestratorInSandboxIPAddress, "--dport", "111",
		"--jump", "REDIRECT", "--to-port", strconv.Itoa(int(s.config.PortmapperPort)),
	)
	if err != nil {
		errs = append(errs, fmt.Errorf("error deleting sandbox portmapper redirect rule: %w", err))
	}

	err = netns.DeleteNamed(s.NamespaceID())
	if err != nil {
		errs = append(errs, fmt.Errorf("error deleting namespace: %w", err))
	}

	return errors.Join(errs...)
}

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

	if err = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644); err != nil {
		return fmt.Errorf("error enabling ip_forward in external netns: %w", err)
	}

	tables, err := iptables.New()
	if err != nil {
		return fmt.Errorf("error initializing iptables in external netns: %w", err)
	}

	if err = tables.Append("nat", "PREROUTING", "-d", s.HostIPString(), "-j", "DNAT", "--to-destination", s.NamespaceIP()); err != nil {
		return fmt.Errorf("error creating external netns dnat rule: %w", err)
	}
	if err = tables.Append("nat", "POSTROUTING", "-s", s.NamespaceIP(), "-j", "SNAT", "--to-source", s.HostIPString()); err != nil {
		return fmt.Errorf("error creating external netns snat rule: %w", err)
	}
	if err = tables.Append("filter", "FORWARD", "-i", s.VpeerName(), "-o", s.TapName(), "-j", "ACCEPT"); err != nil {
		return fmt.Errorf("error creating external netns forward rule to tap: %w", err)
	}
	if err = tables.Append("filter", "FORWARD", "-i", s.TapName(), "-o", s.VpeerName(), "-j", "ACCEPT"); err != nil {
		return fmt.Errorf("error creating external netns forward rule from tap: %w", err)
	}

	if err = s.InitializeFirewall(); err != nil {
		return fmt.Errorf("error initializing external netns slot firewall: %w", err)
	}

	return nil
}

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

	tables, err := iptables.New()
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
