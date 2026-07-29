package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	androidGuestIface      = "eth1"
	androidGuestRouteTable = "local_network"
	androidGuestNetBaseA   = 192
	androidGuestNetBaseB   = 168
	androidGuestNetBaseC   = 240
	androidGuestNetMaxInst = 1024
	androidGuestNetID      = 100

	androidEthernetIPConfigPath = "/data/misc/apexdata/com.android.tethering/misc/ethernet/ipconfig.txt"
	androidEthernetIPConfigTmp  = "/data/local/tmp/cri-multiplex-eth-ipconfig.txt"
)

type androidGuestNetwork struct {
	Gateway string
	GuestIP string
	Prefix  string
	Subnet  string
	TapName string
	DNS     []string
}

func (e *AndroidEngine) configureGuestNetwork(ctx context.Context, rec *AndroidSandboxRecord) error {
	if rec == nil || !e.cfg.CNI.Enabled {
		return nil
	}
	if rec.NetNSName == "" || rec.PodIP == "" {
		return status.Errorf(codes.Internal, "android CNI guest network requires netns and pod IP")
	}
	network, err := androidGuestNetworkForRecord(rec, e.guestDNSServers(rec))
	if err != nil {
		return err
	}

	if err := e.configureGuestNetworkHostSide(ctx, rec, network); err != nil {
		return err
	}
	if err := e.configureGuestNetworkGuestSide(ctx, rec, network); err != nil {
		return err
	}

	rec.GuestIP = network.GuestIP
	rec.GuestGateway = network.Gateway
	rec.GuestPrefix = network.Prefix
	rec.TapName = network.TapName
	log.Printf("[AndroidEngine] configured guest CNI network: sandbox=%s podIP=%s tap=%s guest=%s gateway=%s",
		rec.CRISandboxID, rec.PodIP, rec.TapName, rec.GuestIP, rec.GuestGateway)
	return nil
}

func (e *AndroidEngine) cleanupGuestNetwork(ctx context.Context, rec *AndroidSandboxRecord) {
	if rec == nil || !e.cfg.CNI.Enabled || rec.NetNSName == "" {
		return
	}
	network, err := androidGuestNetworkForRecord(rec, e.guestDNSServers(rec))
	if err != nil {
		log.Printf("[AndroidEngine] cleanup guest CNI network skipped sandbox=%s: %v", rec.CRISandboxID, err)
		return
	}
	for _, port := range androidGuestServicePorts(rec) {
		_ = e.runInAndroidNetNS(ctx, rec, "iptables", "-t", "nat", "-D", "PREROUTING", "-p", "tcp", "-d", rec.PodIP, "--dport", strconv.Itoa(port), "-j", "DNAT", "--to-destination", net.JoinHostPort(network.GuestIP, strconv.Itoa(port)))
		_ = e.runInAndroidNetNS(ctx, rec, "iptables", "-D", "FORWARD", "-p", "tcp", "-d", network.GuestIP, "--dport", strconv.Itoa(port), "-j", "ACCEPT")
	}
	_ = e.runInAndroidNetNS(ctx, rec, "iptables", "-t", "nat", "-D", "POSTROUTING", "-s", network.Subnet, "-o", e.cfg.CNI.IfName, "-j", "MASQUERADE")
	_ = e.runInAndroidNetNS(ctx, rec, "iptables", "-D", "FORWARD", "-i", network.TapName, "-o", e.cfg.CNI.IfName, "-j", "ACCEPT")
	_ = e.runInAndroidNetNS(ctx, rec, "iptables", "-D", "FORWARD", "-i", e.cfg.CNI.IfName, "-o", network.TapName, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT")
}

func (e *AndroidEngine) configureGuestNetworkHostSide(ctx context.Context, rec *AndroidSandboxRecord, network androidGuestNetwork) error {
	if err := e.runInAndroidNetNS(ctx, rec, "ip", "link", "set", network.TapName, "up"); err != nil {
		return status.Errorf(codes.Internal, "set android tap up: %v", err)
	}
	if err := e.runInAndroidNetNS(ctx, rec, "ip", "addr", "replace", network.Gateway+"/"+network.Prefix, "dev", network.TapName); err != nil {
		return status.Errorf(codes.Internal, "configure android tap address: %v", err)
	}
	if err := e.runInAndroidNetNS(ctx, rec, "sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
		return status.Errorf(codes.Internal, "enable android netns ip_forward: %v", err)
	}

	if err := e.runInAndroidNetNSShell(ctx, rec,
		fmt.Sprintf("iptables -t nat -C POSTROUTING -s %s -o %s -j MASQUERADE 2>/dev/null || iptables -t nat -A POSTROUTING -s %s -o %s -j MASQUERADE",
			shellQuote(network.Subnet), shellQuote(e.cfg.CNI.IfName), shellQuote(network.Subnet), shellQuote(e.cfg.CNI.IfName))); err != nil {
		return status.Errorf(codes.Internal, "configure android guest SNAT: %v", err)
	}
	if err := e.runInAndroidNetNSShell(ctx, rec,
		fmt.Sprintf("iptables -C FORWARD -i %s -o %s -j ACCEPT 2>/dev/null || iptables -A FORWARD -i %s -o %s -j ACCEPT",
			shellQuote(network.TapName), shellQuote(e.cfg.CNI.IfName), shellQuote(network.TapName), shellQuote(e.cfg.CNI.IfName))); err != nil {
		return status.Errorf(codes.Internal, "allow android guest egress forwarding: %v", err)
	}
	if err := e.runInAndroidNetNSShell(ctx, rec,
		fmt.Sprintf("iptables -C FORWARD -i %s -o %s -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || iptables -A FORWARD -i %s -o %s -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT",
			shellQuote(e.cfg.CNI.IfName), shellQuote(network.TapName), shellQuote(e.cfg.CNI.IfName), shellQuote(network.TapName))); err != nil {
		return status.Errorf(codes.Internal, "allow android guest return forwarding: %v", err)
	}

	for _, port := range androidGuestServicePorts(rec) {
		portText := strconv.Itoa(port)
		toDest := net.JoinHostPort(network.GuestIP, portText)
		if err := e.runInAndroidNetNSShell(ctx, rec,
			fmt.Sprintf("iptables -t nat -C PREROUTING -p tcp -d %s --dport %s -j DNAT --to-destination %s 2>/dev/null || iptables -t nat -A PREROUTING -p tcp -d %s --dport %s -j DNAT --to-destination %s",
				shellQuote(rec.PodIP), shellQuote(portText), shellQuote(toDest), shellQuote(rec.PodIP), shellQuote(portText), shellQuote(toDest))); err != nil {
			return status.Errorf(codes.Internal, "configure android guest port DNAT %d: %v", port, err)
		}
		if err := e.runInAndroidNetNSShell(ctx, rec,
			fmt.Sprintf("iptables -C FORWARD -p tcp -d %s --dport %s -j ACCEPT 2>/dev/null || iptables -A FORWARD -p tcp -d %s --dport %s -j ACCEPT",
				shellQuote(network.GuestIP), shellQuote(portText), shellQuote(network.GuestIP), shellQuote(portText))); err != nil {
			return status.Errorf(codes.Internal, "allow android guest port forwarding %d: %v", port, err)
		}
	}
	return nil
}

func (e *AndroidEngine) configureGuestNetworkGuestSide(ctx context.Context, rec *AndroidSandboxRecord, network androidGuestNetwork) error {
	adbPath := filepath.Join(rec.ArtifactsDir, "bin", "adb")
	serial := net.JoinHostPort(rec.accessIP(), strconv.Itoa(rec.ADBPort))
	for _, args := range [][]string{
		{"connect", serial},
		{"-s", serial, "wait-for-device"},
	} {
		if err := runAndroidCommand(ctx, 30*time.Second, adbPath, args...); err != nil {
			return status.Errorf(codes.Internal, "prepare adb %s: %v", serial, err)
		}
	}

	if err := e.configureGuestKernelNetwork(ctx, adbPath, serial, network); err != nil {
		return err
	}
	if len(network.DNS) > 0 {
		if err := e.installGuestEthernetConfig(ctx, adbPath, serial, network); err != nil {
			return err
		}
		if err := e.restartGuestFramework(ctx, adbPath, serial); err != nil {
			return err
		}
		if err := e.configureGuestKernelNetwork(ctx, adbPath, serial, network); err != nil {
			return err
		}
	}
	return nil
}

func (e *AndroidEngine) configureGuestKernelNetwork(ctx context.Context, adbPath, serial string, network androidGuestNetwork) error {
	commands := []string{
		fmt.Sprintf("ip link set %s up", shellQuote(androidGuestIface)),
		fmt.Sprintf("ip addr replace %s/%s dev %s", shellQuote(network.GuestIP), shellQuote(network.Prefix), shellQuote(androidGuestIface)),
		fmt.Sprintf("ndc network create %d 2>/dev/null || true", androidGuestNetID),
		fmt.Sprintf("ndc network interface add %d %s 2>/dev/null || true", androidGuestNetID, shellQuote(androidGuestIface)),
		fmt.Sprintf("ndc network default set %d 2>/dev/null || true", androidGuestNetID),
		fmt.Sprintf("ip route replace %s dev %s src %s table %s", shellQuote(network.Subnet), shellQuote(androidGuestIface), shellQuote(network.GuestIP), shellQuote(androidGuestRouteTable)),
		fmt.Sprintf("ip route replace default via %s dev %s table %s onlink", shellQuote(network.Gateway), shellQuote(androidGuestIface), shellQuote(androidGuestRouteTable)),
		fmt.Sprintf("while ip rule del pref 100 2>/dev/null; do :; done; ip rule add from %s/32 lookup %s pref 100", shellQuote(network.GuestIP), shellQuote(androidGuestRouteTable)),
		fmt.Sprintf("while ip rule del pref 101 2>/dev/null; do :; done; ip rule add to %s lookup %s pref 101", shellQuote(network.Subnet), shellQuote(androidGuestRouteTable)),
	}
	if len(network.DNS) > 0 {
		dnsArgs := strings.Join(shellQuoteList(network.DNS), " ")
		commands = append(commands,
			fmt.Sprintf("setprop net.dns1 %s 2>/dev/null || true", shellQuote(network.DNS[0])),
			fmt.Sprintf("setprop net.%s.dns1 %s 2>/dev/null || true", shellQuote(androidGuestIface), shellQuote(network.DNS[0])),
			fmt.Sprintf("ndc resolver setnetdns %d '' %s 2>/dev/null || true", androidGuestNetID, dnsArgs),
			fmt.Sprintf("ndc dnsresolver setnetdns %d '' %s 2>/dev/null || true", androidGuestNetID, dnsArgs),
		)
	}

	for _, command := range commands {
		args := []string{"-s", serial, "shell", "su 0 sh -c " + shellQuote(command)}
		if err := runAndroidCommand(ctx, 30*time.Second, adbPath, args...); err != nil {
			return status.Errorf(codes.Internal, "configure android guest command %q: %v", command, err)
		}
	}
	return nil
}

func (e *AndroidEngine) installGuestEthernetConfig(ctx context.Context, adbPath, serial string, network androidGuestNetwork) error {
	payload, err := androidEthernetIPConfig(network)
	if err != nil {
		return status.Errorf(codes.Internal, "build android ethernet config: %v", err)
	}

	tmp, err := os.CreateTemp("", "cri-multiplex-android-eth-*.bin")
	if err != nil {
		return status.Errorf(codes.Internal, "create android ethernet config temp file: %v", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return status.Errorf(codes.Internal, "write android ethernet config temp file: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return status.Errorf(codes.Internal, "close android ethernet config temp file: %v", err)
	}

	if err := runAndroidCommand(ctx, 30*time.Second, adbPath, "-s", serial, "push", tmpPath, androidEthernetIPConfigTmp); err != nil {
		return status.Errorf(codes.Internal, "push android ethernet config: %v", err)
	}
	install := fmt.Sprintf("mkdir -p %s && cp %s %s && chown system:system %s && chmod 600 %s",
		shellQuote(filepath.Dir(androidEthernetIPConfigPath)),
		shellQuote(androidEthernetIPConfigTmp),
		shellQuote(androidEthernetIPConfigPath),
		shellQuote(androidEthernetIPConfigPath),
		shellQuote(androidEthernetIPConfigPath))
	if err := runAndroidCommand(ctx, 30*time.Second, adbPath, "-s", serial, "shell", "su 0 sh -c "+shellQuote(install)); err != nil {
		return status.Errorf(codes.Internal, "install android ethernet config: %v", err)
	}
	return nil
}

func (e *AndroidEngine) restartGuestFramework(ctx context.Context, adbPath, serial string) error {
	if err := runAndroidCommand(ctx, 60*time.Second, adbPath, "-s", serial, "shell", "su 0 sh -c "+shellQuote("stop; sleep 2; start")); err != nil {
		return status.Errorf(codes.Internal, "restart android framework for ethernet config: %v", err)
	}
	if err := runAndroidCommand(ctx, 90*time.Second, adbPath, "connect", serial); err != nil {
		return status.Errorf(codes.Internal, "reconnect adb after framework restart: %v", err)
	}
	if err := runAndroidCommand(ctx, 90*time.Second, adbPath, "-s", serial, "wait-for-device"); err != nil {
		return status.Errorf(codes.Internal, "wait adb after framework restart: %v", err)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(12 * time.Second):
	}
	return nil
}

func androidEthernetIPConfig(network androidGuestNetwork) ([]byte, error) {
	if network.GuestIP == "" || network.Prefix == "" || network.Gateway == "" {
		return nil, fmt.Errorf("guest IP, prefix and gateway are required")
	}
	buf := bytes.NewBuffer(nil)
	writeInt32 := func(v int32) {
		_ = binary.Write(buf, binary.BigEndian, v)
	}
	writeUTF := func(s string) {
		b := []byte(s)
		_ = binary.Write(buf, binary.BigEndian, uint16(len(b)))
		_, _ = buf.Write(b)
	}

	prefix, err := strconv.Atoi(network.Prefix)
	if err != nil {
		return nil, fmt.Errorf("invalid guest prefix %q: %w", network.Prefix, err)
	}

	writeInt32(3)
	writeUTF("ipAssignment")
	writeUTF("STATIC")
	writeUTF("linkAddress")
	writeUTF(network.GuestIP)
	writeInt32(int32(prefix))
	writeUTF("gateway")
	writeInt32(0)
	writeInt32(1)
	writeUTF(network.Gateway)
	for _, dns := range network.DNS {
		writeUTF("dns")
		writeUTF(dns)
	}
	writeUTF("proxySettings")
	writeUTF("NONE")
	writeUTF("id")
	writeUTF(androidGuestIface)
	writeUTF("eos")
	return buf.Bytes(), nil
}

func androidGuestNetworkForRecord(rec *AndroidSandboxRecord, dns []string) (androidGuestNetwork, error) {
	base := rec.BaseInstanceNum
	if base <= 0 || base > androidGuestNetMaxInst {
		return androidGuestNetwork{}, status.Errorf(codes.InvalidArgument, "android base instance num %d is outside guest CNI range 1-%d", base, androidGuestNetMaxInst)
	}
	offset := (base - 1) * 4
	third := androidGuestNetBaseC + offset/256
	fourth := offset % 256
	if third > 255 || fourth > 252 {
		return androidGuestNetwork{}, status.Errorf(codes.InvalidArgument, "android base instance num %d has no guest CNI subnet", base)
	}
	return androidGuestNetwork{
		Gateway: fmt.Sprintf("%d.%d.%d.%d", androidGuestNetBaseA, androidGuestNetBaseB, third, fourth+1),
		GuestIP: fmt.Sprintf("%d.%d.%d.%d", androidGuestNetBaseA, androidGuestNetBaseB, third, fourth+2),
		Prefix:  "30",
		Subnet:  fmt.Sprintf("%d.%d.%d.%d/30", androidGuestNetBaseA, androidGuestNetBaseB, third, fourth),
		TapName: fmt.Sprintf("cvd-etap-%02d", base),
		DNS:     dns,
	}, nil
}

func androidGuestServicePorts(rec *AndroidSandboxRecord) []int {
	if rec == nil {
		return nil
	}
	raw := strings.TrimSpace(rec.Annotations[annAndroidGuestPorts])
	if raw == "" {
		return nil
	}
	seen := map[int]struct{}{}
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' '
	}) {
		port, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || port <= 0 || port > 65535 {
			continue
		}
		seen[port] = struct{}{}
	}
	ports := make([]int, 0, len(seen))
	for port := range seen {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	return ports
}

func (e *AndroidEngine) guestDNSServers(rec *AndroidSandboxRecord) []string {
	raw := ""
	if rec != nil {
		raw = strings.TrimSpace(rec.Annotations[annAndroidGuestDNS])
	}
	if raw == "" && rec != nil && rec.CNIRecord != nil {
		raw = strings.Join(rec.CNIRecord.DNS, ",")
	}
	if raw == "" {
		raw = "8.8.8.8,1.1.1.1"
	}
	out := make([]string, 0, 2)
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' '
	}) {
		ip := strings.TrimSpace(part)
		if net.ParseIP(ip) != nil {
			out = append(out, ip)
		}
	}
	return out
}

func (e *AndroidEngine) runInAndroidNetNS(ctx context.Context, rec *AndroidSandboxRecord, args ...string) error {
	cmdArgs := append([]string{"netns", "exec", rec.NetNSName}, args...)
	return runAndroidCommand(ctx, 30*time.Second, "ip", cmdArgs...)
}

func (e *AndroidEngine) runInAndroidNetNSShell(ctx context.Context, rec *AndroidSandboxRecord, command string) error {
	return e.runInAndroidNetNS(ctx, rec, "/bin/bash", "-lc", command)
}

func runAndroidCommand(ctx context.Context, timeout time.Duration, name string, args ...string) error {
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, name, args...)
	out, err := cmd.CombinedOutput()
	if cmdCtx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("%s timed out after %s", shellQuoteArgs(append([]string{name}, args...)), timeout)
	}
	if err != nil {
		return fmt.Errorf("%s failed: %w output=%s", shellQuoteArgs(append([]string{name}, args...)), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func shellQuoteList(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, shellQuote(value))
	}
	return out
}

func cniNetNSName(rec *CNIRecord) string {
	if rec == nil {
		return ""
	}
	return rec.NetNSName
}

func cniNetNSPath(rec *CNIRecord) string {
	if rec == nil {
		return ""
	}
	return rec.NetNSPath
}
