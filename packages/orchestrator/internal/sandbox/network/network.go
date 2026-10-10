package network

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"

	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

// 本包直接使用 vishvananda/netlink 的包级函数，无需互斥锁：
// pkgHandle.sockets 为 nil 时，每个请求都会临时新建 socket（绑定调用方
// 当前 netns，用毕即关），序号由 atomic 分配（v1.3.1 nl_linux.go 的
// NewNetlinkRequest/ExecuteIter），因此包级调用并发安全。
// "每请求一个 socket"还保证在沙箱 netns 内执行的配置（lo/vpeer/ns 内路由）
// 落在正确命名空间；相反，netlink.NewHandle 的 socket 在创建时绑定 netns，
// 缓存后跨 ns 复用会写错命名空间，故本包不缓存 Handle。

// tapOpts configures a TAP device created via createTap.
//
// VnetHdr enables IFF_VNET_HDR on the TAP so the guest can use virtio-net
// offloads. If the kernel rejects IFF_VNET_HDR (e.g. very old kernels or
// tap already created without the flag), createTap retries without it so
// callers don't have to special-case the failure.
type tapOpts struct {
	Name    string
	Address *net.IPNet
	VnetHdr bool
}

// createTap creates and enables a TAP device in the current network namespace.
// An address is optional because secondary guest interfaces may be connected to
// a TAP without using the TAP itself as an L3 gateway.
func createTap(o tapOpts) error {
	if err := linkAddTuntap(o.Name, o.VnetHdr); err != nil {
		// Fall back to a plain TAP (no IFF_VNET_HDR) when the host kernel
		// rejects the flag. This keeps non-Android slots and older hosts
		// working without forcing a hard failure.
		if o.VnetHdr {
			if retryErr := linkAddTuntap(o.Name, false); retryErr != nil {
				return fmt.Errorf("error creating tap device %s (with vnet_hdr retry): %w (original: %v)", o.Name, retryErr, err)
			}
		} else {
			return fmt.Errorf("error creating tap device %s: %w", o.Name, err)
		}
	}

	tap, err := netlink.LinkByName(o.Name)
	if err != nil {
		return fmt.Errorf("error finding tap device %s: %w", o.Name, err)
	}

	if err := netlink.LinkSetUp(tap); err != nil {
		return fmt.Errorf("error setting tap device %s up: %w", o.Name, err)
	}

	if o.Address != nil {
		// broadcast "+" is computed by the kernel from IP/mask, so we
		// intentionally leave Broadcast unset here.
		if err := netlink.AddrAdd(tap, &netlink.Addr{IPNet: o.Address}); err != nil {
			return fmt.Errorf("error setting address of tap device %s: %w", o.Name, err)
		}
	}

	return nil
}

// linkAddTuntap adds a TAP device with the given name. When vnetHdr is true,
// IFF_VNET_HDR is requested so the guest can use virtio-net offloads.
func linkAddTuntap(name string, vnetHdr bool) error {
	tapAttrs := netlink.NewLinkAttrs()
	tapAttrs.Name = name
	tap := &netlink.Tuntap{
		Mode:      netlink.TUNTAP_MODE_TAP,
		LinkAttrs: tapAttrs,
	}
	if vnetHdr {
		tap.Flags = netlink.TUNTAP_VNET_HDR
	}

	// Keep temporary TAP descriptors out of children until LinkAdd closes them.
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()

	return netlink.LinkAdd(tap)
}

// netnsRunDir 与 vishvananda/netns 的 bindMountPath 保持一致
const netnsRunDir = "/run/netns"

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

	// Both TAP devices belong to the network slot and live for as long as its
	// namespace. Each NIC uses its own /30 so their host-side addresses and
	// routes don't conflict.
	if err := createTap(tapOpts{
		Name:    s.TapName(),
		Address: &net.IPNet{IP: s.TapIP(), Mask: s.TapCIDR()},
	}); err != nil {
		return err
	}
	if err := createTap(tapOpts{
		Name:    s.ExtraTapName(), // cvd-mtap
		Address: &net.IPNet{IP: s.ExtraTapIP(), Mask: s.ExtraTapCIDR()},
		VnetHdr: true, // IFF_VNET_HDR for virtio-net offloads
	}); err != nil {
		return err
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

	tables, err := iptables.New(iptables.Timeout(5))
	if err != nil {
		return fmt.Errorf("error initializing iptables: %w", err)
	}

	// Add NAT routing rules to NS
	err = tables.Append("nat", "POSTROUTING", "-o", s.VpeerName(), "-s", s.NamespaceIP(), "-j", "SNAT", "--to", s.HostIPString())
	if err != nil {
		return fmt.Errorf("error creating postrouting rule to vpeer: %w", err)
	}
	// Android normally routes guest traffic through cvd-mtap. Using
	// MASQUERADE here exposes the namespace vpeer (10.12.x.x) to host-side
	// proxies, but sandbox lookup is keyed by the per-slot HostIP (10.11.x.x).
	// Use the same stable source identity as the primary TAP so concurrent
	// Android sandboxes remain distinguishable to the TCP firewall.
	err = tables.Append("nat", "POSTROUTING", "-o", s.VpeerName(), "-s", cvdTapNetwork, "-j", "SNAT", "--to", s.HostIPString())
	if err != nil {
		return fmt.Errorf("error creating cvd-mtap postrouting masquerade rule: %w", err)
	}

	// Per-sandbox egress proxy slots: the in-netns proxy sources its upstream
	// connections from the vpeer (vrt network) address, which the SNAT rules
	// above and the host MASQUERADE don't cover — add the vrt SNAT so the
	// proxy's upstream traffic can leave the node.
	if s.egressProxy {
		err = tables.Append("nat", "POSTROUTING", s.egressProxyVrtSNATSpec()...)
		if err != nil {
			return fmt.Errorf("error creating egress proxy vrt postrouting rule: %w", err)
		}
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
	// Per-sandbox egress proxy slots skip this: their TCP egress is redirected
	// to the in-netns proxy, and the tcpProxy catch-all would hijack the
	// proxy's own upstream connections arriving via the host veth.
	if s.installTCPProxy() {
		err = s.tcpProxyConfig().append(tables)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *Slot) RemoveNetwork() error {
	return s.removeNetwork(true)
}

// RemoveNetworkSkipHostRules 与 RemoveNetwork 相同，但跳过宿主机 iptables 规则
// 删除——调用方（Pool.Close）已通过 DeleteSlotsHostRules 做池级批量删除。
// 注意 ExternalNetNS 槽位不受此开关影响：批量删除刻意跳过它们，其规则仍由
// RemoveExternalNetNSNetwork 逐条删除。
func (s *Slot) RemoveNetworkSkipHostRules() error {
	return s.removeNetwork(false)
}

func (s *Slot) removeNetwork(deleteHostRules bool) error {
	if s.ExternalNetNS {
		return s.RemoveExternalNetNSNetwork()
	}

	var errs []error

	err := s.CloseFirewall()
	if err != nil {
		errs = append(errs, fmt.Errorf("error closing firewall: %w", err))
	}

	// 宿主机 filter/nat 规则合并为每表一次 iptables-restore 批量删除，
	// 替代原先约 10 次独立的 `iptables -D`
	if deleteHostRules {
		if err := s.deleteHostSlotRules(); err != nil {
			errs = append(errs, fmt.Errorf("error deleting host iptables rules: %w", err))
		}
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

	err = deleteNamedNamespace(s.NamespaceID())
	if err != nil {
		errs = append(errs, fmt.Errorf("error deleting namespace: %w", err))
	}

	return errors.Join(errs...)
}

// iptBatchMu 串行化宿主机 iptables 的 save-过滤-restore 批量删除：该操作是
// 整表读-改-写，若并行执行，后提交的快照会把其他 worker 已删除的规则复活。
var iptBatchMu sync.Mutex

// 槽位在宿主机上有规则的表
var hostSlotRuleTables = []string{"filter", "nat"}

// deleteHostSlotRules 用每表一次 iptables-save + iptables-restore 删除槽位在
// 宿主机 filter/nat 表里的全部规则，替代约 10 次独立的 `iptables -D`。
// 每次 -D 都是一次持有全局 xtables.lock 的整表替换，是 Pool.Close 并行清理的
// 主要瓶颈；批量化后每槽位每表只剩 2 次进程调用、1 次锁内整表提交。
//
// 匹配方式：槽位的所有宿主规则都带有槽位唯一 token——veth 接口名或
// host /32 CIDR——按空白分隔的字段做精确匹配后整行剔除，其余行原样回灌，
// 因而不受 iptables-save 规范化输出（如 --to-port→--to-ports）的影响。
// 未安装成功的规则（如创建中途失败）不会出现在 dump 里，天然跳过。
//
// restore 是原子整表替换；与进程外写者（如 kube-proxy 写 KUBE-* 链）之间存在
// 极小的读-改-写窗口，语义与 kube-proxy 自身的全表 restore 相同，且对端会
// 在下一个同步周期自愈。
func (s *Slot) deleteHostSlotRules() error {
	tokenSet := make(map[string]struct{}, 2)
	tokenSet[s.VethName()] = struct{}{}
	tokenSet[s.HostCIDR()] = struct{}{}

	iptBatchMu.Lock()
	defer iptBatchMu.Unlock()

	var errs []error

	for _, table := range hostSlotRuleTables {
		if err := dropTokenRulesFromTable(table, tokenSet); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// DeleteSlotsHostRules 池级聚合删除：把全部槽位的宿主 filter/nat 规则合并为
// 每表一次 iptables-save + 过滤 + iptables-restore，替代每槽位各 2 次的
// 2N 次整表操作。Pool.Close 清理数百槽位时，xtables.lock 内的整表替换从
// ~600 次降到 2 次。失败时调用方应回退为各槽位自行删除，避免规则泄漏。
func DeleteSlotsHostRules(slots []*Slot) error {
	tokenSet := make(map[string]struct{}, len(slots)*2)

	for _, s := range slots {
		if s.ExternalNetNS {
			continue
		}

		tokenSet[s.VethName()] = struct{}{}
		tokenSet[s.HostCIDR()] = struct{}{}
	}

	if len(tokenSet) == 0 {
		return nil
	}

	iptBatchMu.Lock()
	defer iptBatchMu.Unlock()

	var errs []error

	for _, table := range hostSlotRuleTables {
		if err := dropTokenRulesFromTable(table, tokenSet); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// dropTokenRulesFromTable dump 指定表，剔除所有携带 token 的规则行后原子回灌。
// 表内没有匹配规则时不执行 restore。
func dropTokenRulesFromTable(table string, tokens map[string]struct{}) error {
	// -c 保留计数器：回灌时不至于把无关规则的计数清零
	dump, err := exec.Command("iptables-save", "-c", "-t", table).Output()
	if err != nil {
		return fmt.Errorf("error dumping %s table: %w", table, err)
	}

	lines := strings.Split(string(dump), "\n")
	kept := lines[:0]
	removed := 0

	for _, line := range lines {
		if isIPTablesSaveRuleLine(line) && ruleLineHasToken(line, tokens) {
			removed++

			continue
		}

		kept = append(kept, line)
	}

	if removed == 0 {
		return nil
	}

	cmd := exec.Command("iptables-restore", "-c")
	cmd.Stdin = strings.NewReader(strings.Join(kept, "\n"))

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("error restoring %s table: %w (output: %s)", table, err, strings.TrimSpace(string(out)))
	}

	return nil
}

// ruleLineHasToken 判断规则行是否有任一空白分隔字段命中 token 集合。
// 整字段比较避免 veth-60 误匹配 veth-600 这类前缀碰撞。
func ruleLineHasToken(line string, tokens map[string]struct{}) bool {
	for field := range strings.FieldsSeq(line) {
		if _, ok := tokens[field]; ok {
			return true
		}
	}

	return false
}

// isIPTablesSaveRuleLine 判断 iptables-save 输出中的规则行。带 -c 时行首是
// "[pkts:bytes] " 计数器前缀、-A 退居第二字段；不带 -c 时行首即 -A。曾经用
// HasPrefix(line, "-A ") 判定，在 -c 格式下永远为假，导致批量删除静默失效。
// 注意：该判定只用于过滤，保留行（含计数器前缀）原样回灌，不能改动行内容。
func isIPTablesSaveRuleLine(line string) bool {
	return strings.HasPrefix(line, "-A ") || strings.HasPrefix(line, "[")
}

// deleteNamedNamespace 删除命名 netns。/run 在 systemd 系统上是 shared 挂载，
// bind mount 可能被传播出同路径的多份叠加副本，而 netns.DeleteNamed 只做一次
// umount，叠挂时只摘顶层导致残留。DeleteNamed 失败时循环 umount 直到目标不再
// 是挂载点（EINVAL），再删除持久化文件。
func deleteNamedNamespace(name string) error {
	err := netns.DeleteNamed(name)
	if err == nil {
		return nil
	}

	path := filepath.Join(netnsRunDir, name)
	for range 8 {
		umountErr := unix.Unmount(path, unix.MNT_DETACH)
		if umountErr == nil {
			continue
		}
		// EINVAL：已不是挂载点（卸载干净）；ENOENT：残留已被并发清理
		if errors.Is(umountErr, unix.EINVAL) || errors.Is(umountErr, unix.ENOENT) {
			break
		}

		return errors.Join(err, fmt.Errorf("error unmounting duplicated namespace mount %s: %w", path, umountErr))
	}

	removeErr := os.Remove(path)
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return errors.Join(err, fmt.Errorf("error removing namespace file %s: %w", path, removeErr))
	}

	return nil
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

	// CNI 配置未带 routes 段时 netns 里只有直连路由，VM 出向（含 DNS）全部
	// 被路由查找丢弃；按 CNI 透传的网关补 default 路由。CNI 已装（IPAM
	// routes 段）时 RouteAdd 撞 EEXIST，视为成功。
	if s.gateway != nil {
		if err := netlink.RouteAdd(&netlink.Route{Scope: netlink.SCOPE_UNIVERSE, Gw: s.gateway}); err != nil && !errors.Is(err, syscall.EEXIST) {
			return fmt.Errorf("error adding external netns default route via %s: %w", s.gateway, err)
		}
	}

	if err := createTap(tapOpts{
		Name:    s.TapName(),
		Address: &net.IPNet{IP: s.TapIP(), Mask: s.TapCIDR()},
	}); err != nil {
		return fmt.Errorf("error creating external netns primary tap: %w", err)
	}
	if err := createTap(tapOpts{
		Name:    s.ExtraTapName(),
		Address: &net.IPNet{IP: s.ExtraTapIP(), Mask: s.ExtraTapCIDR()},
		VnetHdr: true,
	}); err != nil {
		return fmt.Errorf("error creating external netns Android mobile tap: %w", err)
	}

	if err = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644); err != nil {
		return fmt.Errorf("error enabling ip_forward in external netns: %w", err)
	}

	tables, err := iptables.New(iptables.Timeout(5))
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

// applyExternalNetNSRules installs the 7 per-slot iptables rules of the
// external netns mode with one `iptables-restore --noflush` call instead of 7
// separate `iptables` execs. The rules are private to the sandbox netns, so
// skipping the xtables.lock serialization is safe. Falls back to sequential
// appends when iptables-restore is not available.
func (s *Slot) applyExternalNetNSRules(tables *iptables.IPTables) error {
	var b strings.Builder
	b.WriteString("*nat\n")
	fmt.Fprintf(&b, "-A PREROUTING -d %s -j DNAT --to-destination %s\n", s.HostIPString(), s.NamespaceIP())
	fmt.Fprintf(&b, "-A POSTROUTING -s %s -j SNAT --to-source %s\n", s.NamespaceIP(), s.HostIPString())
	fmt.Fprintf(&b, "-A POSTROUTING -o %s -s %s -j SNAT --to-source %s\n", s.VpeerName(), cvdTapNetwork, s.HostIPString())
	b.WriteString("COMMIT\n*filter\n")
	fmt.Fprintf(&b, "-A FORWARD -i %s -o %s -j ACCEPT\n", s.VpeerName(), s.TapName())
	fmt.Fprintf(&b, "-A FORWARD -i %s -o %s -j ACCEPT\n", s.TapName(), s.VpeerName())
	fmt.Fprintf(&b, "-A FORWARD -i %s -o %s -j ACCEPT\n", s.VpeerName(), s.ExtraTapName())
	fmt.Fprintf(&b, "-A FORWARD -i %s -o %s -j ACCEPT\n", s.ExtraTapName(), s.VpeerName())
	b.WriteString("COMMIT\n")

	cmd := exec.Command("iptables-restore", "-w", "5", "--noflush")
	cmd.Stdin = strings.NewReader(b.String())
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
	if err := tables.Append("nat", "POSTROUTING", "-o", s.VpeerName(), "-s", cvdTapNetwork, "-j", "SNAT", "--to-source", s.HostIPString()); err != nil {
		return fmt.Errorf("error creating external netns cvd-mtap snat rule: %w", err)
	}
	if err := tables.Append("filter", "FORWARD", "-i", s.VpeerName(), "-o", s.TapName(), "-j", "ACCEPT"); err != nil {
		return fmt.Errorf("error creating external netns forward rule to tap: %w", err)
	}
	if err := tables.Append("filter", "FORWARD", "-i", s.TapName(), "-o", s.VpeerName(), "-j", "ACCEPT"); err != nil {
		return fmt.Errorf("error creating external netns forward rule from tap: %w", err)
	}
	if err := tables.Append("filter", "FORWARD", "-i", s.VpeerName(), "-o", s.ExtraTapName(), "-j", "ACCEPT"); err != nil {
		return fmt.Errorf("error creating external netns forward rule to cvd-mtap: %w", err)
	}
	if err := tables.Append("filter", "FORWARD", "-i", s.ExtraTapName(), "-o", s.VpeerName(), "-j", "ACCEPT"); err != nil {
		return fmt.Errorf("error creating external netns forward rule from cvd-mtap: %w", err)
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

	tables, err := iptables.New(iptables.Timeout(5))
	if err != nil {
		errs = append(errs, fmt.Errorf("error initializing iptables in external netns: %w", err))
	} else {
		if err = tables.Delete("nat", "PREROUTING", "-d", s.HostIPString(), "-j", "DNAT", "--to-destination", s.NamespaceIP()); err != nil {
			errs = append(errs, fmt.Errorf("error deleting external netns dnat rule: %w", err))
		}
		if err = tables.Delete("nat", "POSTROUTING", "-s", s.NamespaceIP(), "-j", "SNAT", "--to-source", s.HostIPString()); err != nil {
			errs = append(errs, fmt.Errorf("error deleting external netns snat rule: %w", err))
		}
		if err = tables.Delete("nat", "POSTROUTING", "-o", s.VpeerName(), "-s", cvdTapNetwork, "-j", "SNAT", "--to-source", s.HostIPString()); err != nil {
			errs = append(errs, fmt.Errorf("error deleting external netns cvd-mtap snat rule: %w", err))
		}
		if err = tables.Delete("filter", "FORWARD", "-i", s.VpeerName(), "-o", s.TapName(), "-j", "ACCEPT"); err != nil {
			errs = append(errs, fmt.Errorf("error deleting external netns forward rule to tap: %w", err))
		}
		if err = tables.Delete("filter", "FORWARD", "-i", s.TapName(), "-o", s.VpeerName(), "-j", "ACCEPT"); err != nil {
			errs = append(errs, fmt.Errorf("error deleting external netns forward rule from tap: %w", err))
		}
		if err = tables.Delete("filter", "FORWARD", "-i", s.VpeerName(), "-o", s.ExtraTapName(), "-j", "ACCEPT"); err != nil {
			errs = append(errs, fmt.Errorf("error deleting external netns forward rule to cvd-mtap: %w", err))
		}
		if err = tables.Delete("filter", "FORWARD", "-i", s.ExtraTapName(), "-o", s.VpeerName(), "-j", "ACCEPT"); err != nil {
			errs = append(errs, fmt.Errorf("error deleting external netns forward rule from cvd-mtap: %w", err))
		}
	}

	if tap, err := netlink.LinkByName(s.ExtraTapName()); err == nil {
		if err = netlink.LinkDel(tap); err != nil {
			errs = append(errs, fmt.Errorf("error deleting external netns Android mobile tap device: %w", err))
		}
	} else {
		errs = append(errs, fmt.Errorf("error finding external netns Android mobile tap device: %w", err))
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
