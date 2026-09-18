package engine

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"log"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/coreos/go-iptables/iptables"
)

// 宿主端口池默认范围（避开 NodePort 段 30000-32767），可由
// SANDBOX_HOSTPORT_POOL_START / SANDBOX_HOSTPORT_POOL_END 覆盖（main.go 启动时解析校验）。
const (
	defaultHostPortPoolStart = 20000
	defaultHostPortPoolEnd   = 29999
)

// reservedHostPorts 为节点保留端口清单（cri-multiplex/orchestrator 自身监听端口），
// expose-ports 写法②③指定的宿主端口命中时拒绝，避免与管控面争抢。
var reservedHostPorts = map[int]struct{}{
	5008: {}, // orchestrator SandboxService gRPC
}

// exposePortKind 标识 expose-ports 条目的三种写法（设计文档 4.4.2.6）。
type exposePortKind int

const (
	exposePortAuto  exposePortKind = iota // ① P：池内自动分配宿主端口
	exposePortFixed                       // ② P:H：指定宿主端口，被占用则创建失败
	exposePortRange                       // ③ P:H1-H2：候选区间内取首个空闲，全占用则创建失败
)

// ExposePortSpec 为 expose-ports 单条目的解析结果。
type ExposePortSpec struct {
	GuestPort int            // 沙箱（guest）端口
	Kind      exposePortKind // 写法①②③
	HostPort  int            // 写法②：指定宿主端口
	HostStart int            // 写法③：区间起点（含）
	HostEnd   int            // 写法③：区间终点（含）
}

type HostPortManager struct {
	mu        sync.Mutex
	start     int
	end       int
	allocated map[string]int // key -> hostPort
	// bindProbe 探测宿主端口是否被本机其他进程占用（写法②③的双重判定之一）；
	// 为 nil 时跳过探测。测试可注入桩。
	bindProbe func(port int) error
}

type PortMapping struct {
	HostPort    int `json:"host_port"`
	SandboxPort int `json:"sandbox_port"`
}

type hostPortMappingOps struct {
	setup   func(nodeIP string, hostPort int, sandboxIP string, sandboxPort int) error
	cleanup func(nodeIP string, hostPort int, sandboxIP string, sandboxPort int) error
	// setupBatch 批量安装一个沙箱全部映射的 iptables 规则（单次 iptables-restore 事务）；
	// 为 nil 时创建路径回退为逐条 setup。
	setupBatch func(nodeIP string, mappings []PortMapping, sandboxIP string) error
}

func defaultHostPortMappingOps() hostPortMappingOps {
	return hostPortMappingOps{
		setup:      SetupHostPortMapping,
		cleanup:    CleanupHostPortMapping,
		setupBatch: SetupHostPortMappings,
	}
}

func NewHostPortManager(start, end int) *HostPortManager {
	return &HostPortManager{
		start:     start,
		end:       end,
		allocated: make(map[string]int),
		bindProbe: defaultBindProbe,
	}
}

// defaultBindProbe 通过临时监听探测宿主端口是否空闲：能绑上才视为空闲（探测即关）。
// 存在 TOCTOU 窗口，由规则安装幂等 + orphan reconciler 对账兜底。
func defaultBindProbe(port int) error {
	ln, err := net.Listen("tcp4", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	return ln.Close()
}

func (m *HostPortManager) Allocate(sandboxID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for port := m.start; port <= m.end; port++ {
		inUse := false
		for _, p := range m.allocated {
			if p == port {
				inUse = true
				break
			}
		}
		if !inUse {
			m.allocated[sandboxID] = port
			return port, nil
		}
	}
	return 0, fmt.Errorf("no available host port in range [%d, %d]", m.start, m.end)
}

func (m *HostPortManager) Release(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.allocated, key)
}

// parseExposePorts 解析 e2b.dev/expose-ports 注解，支持三种写法（设计文档 4.4.2.6）：
// ① P（池内自动分配）、② P:H（指定宿主端口）、③ P:H1-H2（候选区间首个空闲）。
// 行为变更：任一条目 malformed（格式错误、端口越界、区间倒挂、guest 端口重复、
// 写法②③命中特权/保留端口）即整体返回 error，由创建路径转为 InvalidArgument，
// 不再静默跳过。
func parseExposePorts(s string) ([]ExposePortSpec, error) {
	var specs []ExposePortSpec
	seen := make(map[int]struct{})
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return nil, fmt.Errorf("empty entry in %q", s)
		}
		spec, err := parseExposePortEntry(entry)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[spec.GuestPort]; dup {
			return nil, fmt.Errorf("duplicate guest port %d", spec.GuestPort)
		}
		seen[spec.GuestPort] = struct{}{}
		specs = append(specs, spec)
	}
	return specs, nil
}

func parseExposePortEntry(entry string) (ExposePortSpec, error) {
	var spec ExposePortSpec
	parts := strings.Split(entry, ":")
	if len(parts) > 2 {
		return spec, fmt.Errorf("entry %q: too many ':' segments", entry)
	}
	guest, err := parsePortNum(strings.TrimSpace(parts[0]))
	if err != nil {
		return spec, fmt.Errorf("entry %q: guest port: %v", entry, err)
	}
	spec.GuestPort = guest
	if len(parts) == 1 {
		spec.Kind = exposePortAuto
		return spec, nil
	}
	host := strings.TrimSpace(parts[1])
	if h1, h2, found := strings.Cut(host, "-"); found {
		// 写法③：候选区间
		start, err := parsePortNum(strings.TrimSpace(h1))
		if err != nil {
			return spec, fmt.Errorf("entry %q: host range start: %v", entry, err)
		}
		end, err := parsePortNum(strings.TrimSpace(h2))
		if err != nil {
			return spec, fmt.Errorf("entry %q: host range end: %v", entry, err)
		}
		if start > end {
			return spec, fmt.Errorf("entry %q: host range %d-%d is inverted", entry, start, end)
		}
		if start < 1024 {
			return spec, fmt.Errorf("entry %q: host range [%d, %d] overlaps privileged ports (<1024)", entry, start, end)
		}
		for p := range reservedHostPorts {
			if p >= start && p <= end {
				return spec, fmt.Errorf("entry %q: host range [%d, %d] covers reserved port %d", entry, start, end, p)
			}
		}
		spec.Kind = exposePortRange
		spec.HostStart = start
		spec.HostEnd = end
		return spec, nil
	}
	// 写法②：指定宿主端口
	h, err := parsePortNum(host)
	if err != nil {
		return spec, fmt.Errorf("entry %q: host port: %v", entry, err)
	}
	if h < 1024 {
		return spec, fmt.Errorf("entry %q: host port %d is privileged (<1024)", entry, h)
	}
	if _, ok := reservedHostPorts[h]; ok {
		return spec, fmt.Errorf("entry %q: host port %d is reserved", entry, h)
	}
	spec.Kind = exposePortFixed
	spec.HostPort = h
	return spec, nil
}

func parsePortNum(s string) (int, error) {
	p, err := strconv.Atoi(s)
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("invalid port %q (must be 1-65535)", s)
	}
	return p, nil
}

// AllocatePorts 按 spec 列表为多个 guest 端口分配宿主端口：
// 写法①在池范围 [start, end] 内线性扫描首个空闲（仅查内存 map）；
// 写法②③为 map 空闲 + bindProbe 双重判定（指定端口不受池范围约束）。
// 任一 spec 失败即回滚本批全部分配并返回 error（整批失败 → 沙箱创建失败）。
func (m *HostPortManager) AllocatePorts(sandboxID string, specs []ExposePortSpec) ([]PortMapping, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var mappings []PortMapping
	for _, spec := range specs {
		port, err := m.allocateOneLocked(spec)
		if err != nil {
			// 回滚本批
			for _, mapp := range mappings {
				delete(m.allocated, sandboxID+"-"+strconv.Itoa(mapp.SandboxPort))
			}
			return nil, fmt.Errorf("sandbox %s guest port %d: %w", sandboxID, spec.GuestPort, err)
		}
		m.allocated[sandboxID+"-"+strconv.Itoa(spec.GuestPort)] = port
		mappings = append(mappings, PortMapping{HostPort: port, SandboxPort: spec.GuestPort})
	}
	return mappings, nil
}

func (m *HostPortManager) allocateOneLocked(spec ExposePortSpec) (int, error) {
	switch spec.Kind {
	case exposePortFixed:
		if m.hostPortInUseLocked(spec.HostPort) {
			return 0, fmt.Errorf("host port %d already allocated", spec.HostPort)
		}
		if err := m.probeHostPort(spec.HostPort); err != nil {
			return 0, fmt.Errorf("host port %d is in use (bind probe): %w", spec.HostPort, err)
		}
		return spec.HostPort, nil
	case exposePortRange:
		for p := spec.HostStart; p <= spec.HostEnd; p++ {
			if m.hostPortInUseLocked(p) {
				continue
			}
			if err := m.probeHostPort(p); err != nil {
				continue
			}
			return p, nil
		}
		return 0, fmt.Errorf("no available host port in range [%d, %d]", spec.HostStart, spec.HostEnd)
	default: // exposePortAuto
		for p := m.start; p <= m.end; p++ {
			if !m.hostPortInUseLocked(p) {
				return p, nil
			}
		}
		return 0, fmt.Errorf("no available host port in pool [%d, %d]", m.start, m.end)
	}
}

func (m *HostPortManager) hostPortInUseLocked(port int) bool {
	for _, used := range m.allocated {
		if used == port {
			return true
		}
	}
	return false
}

func (m *HostPortManager) probeHostPort(port int) error {
	if m.bindProbe == nil {
		return nil
	}
	return m.bindProbe(port)
}

// ReleasePorts 释放所有端口
func (m *HostPortManager) ReleasePorts(sandboxID string, ports []int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, port := range ports {
		delete(m.allocated, sandboxID+"-"+strconv.Itoa(port))
	}
}

func (m *HostPortManager) RestorePorts(sandboxID string, mappings []PortMapping) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mapping := range mappings {
		if mapping.HostPort <= 0 || mapping.SandboxPort <= 0 {
			continue
		}
		m.allocated[sandboxID+"-"+strconv.Itoa(mapping.SandboxPort)] = mapping.HostPort
	}
}

func SetupHostPortMapping(nodeIP string, hostPort int, sandboxIP string, sandboxPort int) error {
	tables, err := iptables.New()
	if err != nil {
		return fmt.Errorf("init iptables: %w", err)
	}

	comment := hostPortRuleComment(nodeIP, hostPort, sandboxIP, sandboxPort)
	// PREROUTING
	appendRuleIfMissing := func(table, chain string, rulespec ...string) error {
		exists, err := tables.Exists(table, chain, rulespec...)
		if err != nil {
			return err
		}
		if exists {
			return nil
		}
		return tables.Append(table, chain, rulespec...)
	}

	if err := appendRuleIfMissing("nat", "PREROUTING",
		"-p", "tcp",
		"-d", nodeIP,
		"--dport", fmt.Sprintf("%d", hostPort),
		"-m", "comment", "--comment", comment,
		"-j", "DNAT",
		"--to-destination", fmt.Sprintf("%s:%d", sandboxIP, sandboxPort),
	); err != nil {
		return fmt.Errorf("append nat PREROUTING rule: %w", err)
	}

	// OUTPUT（宿主机本地访问）
	if err := appendRuleIfMissing("nat", "OUTPUT",
		"-p", "tcp",
		"-d", nodeIP,
		"--dport", fmt.Sprintf("%d", hostPort),
		"-m", "comment", "--comment", comment,
		"-j", "DNAT",
		"--to-destination", fmt.Sprintf("%s:%d", sandboxIP, sandboxPort),
	); err != nil {
		return fmt.Errorf("append nat OUTPUT rule: %w", err)
	}

	// POSTROUTING
	if err := appendRuleIfMissing("nat", "POSTROUTING",
		"-p", "tcp",
		"-d", sandboxIP,
		"--dport", fmt.Sprintf("%d", sandboxPort),
		"-m", "comment", "--comment", comment,
		"-j", "MASQUERADE",
	); err != nil {
		return fmt.Errorf("append nat POSTROUTING rule: %w", err)
	}

	// FORWARD 放行
	if err := appendRuleIfMissing("filter", "FORWARD",
		"-p", "tcp",
		"-d", sandboxIP,
		"--dport", fmt.Sprintf("%d", sandboxPort),
		"-m", "comment", "--comment", comment,
		"-j", "ACCEPT",
	); err != nil {
		return fmt.Errorf("append filter FORWARD dst rule: %w", err)
	}
	if err := appendRuleIfMissing("filter", "FORWARD",
		"-p", "tcp",
		"-s", sandboxIP,
		"--sport", fmt.Sprintf("%d", sandboxPort),
		"-m", "comment", "--comment", comment,
		"-j", "ACCEPT",
	); err != nil {
		return fmt.Errorf("append filter FORWARD src rule: %w", err)
	}

	return nil
}

func CleanupHostPortMapping(nodeIP string, hostPort int, sandboxIP string, sandboxPort int) error {
	tables, err := iptables.New()
	if err != nil {
		return fmt.Errorf("init iptables: %w", err)
	}

	comment := hostPortRuleComment(nodeIP, hostPort, sandboxIP, sandboxPort)
	var errs []error
	deleteBoth := func(table, chain string, legacy []string) {
		tagged := append([]string(nil), legacy...)
		insertAt := len(tagged)
		for i, arg := range tagged {
			if arg == "-j" {
				insertAt = i
				break
			}
		}
		tagged = append(tagged[:insertAt], append([]string{"-m", "comment", "--comment", comment}, tagged[insertAt:]...)...)
		if err := tables.Delete(table, chain, tagged...); err != nil && !isCleanupNotFound(err) {
			errs = append(errs, err)
		}
		if err := tables.Delete(table, chain, legacy...); err != nil && !isCleanupNotFound(err) {
			errs = append(errs, err)
		}
	}

	deleteBoth("nat", "PREROUTING", []string{
		"-p", "tcp", "-d", nodeIP, "--dport", fmt.Sprintf("%d", hostPort),
		"-j", "DNAT", "--to-destination", fmt.Sprintf("%s:%d", sandboxIP, sandboxPort),
	})
	deleteBoth("nat", "OUTPUT", []string{
		"-p", "tcp", "-d", nodeIP, "--dport", fmt.Sprintf("%d", hostPort),
		"-j", "DNAT", "--to-destination", fmt.Sprintf("%s:%d", sandboxIP, sandboxPort),
	})
	deleteBoth("nat", "POSTROUTING", []string{
		"-p", "tcp", "-d", sandboxIP, "--dport", fmt.Sprintf("%d", sandboxPort),
		"-j", "MASQUERADE",
	})
	deleteBoth("filter", "FORWARD", []string{
		"-p", "tcp", "-d", sandboxIP, "--dport", fmt.Sprintf("%d", sandboxPort),
		"-j", "ACCEPT",
	})
	deleteBoth("filter", "FORWARD", []string{
		"-p", "tcp", "-s", sandboxIP, "--sport", fmt.Sprintf("%d", sandboxPort),
		"-j", "ACCEPT",
	})

	return errors.Join(errs...)
}

// SetupHostPortMappings 批量安装一个沙箱全部映射的 iptables 规则（5×N 条）：
// 先 iptables-save 提取现有 comment 集合去重（幂等），再用一次
// iptables-restore --noflush 事务整体提交；iptables-restore 不可用时
// 回退为逐条 SetupHostPortMapping。失败仅由调用方记 WARNING，不阻断创建。
func SetupHostPortMappings(nodeIP string, mappings []PortMapping, sandboxIP string) error {
	if len(mappings) == 0 {
		return nil
	}
	if _, err := exec.LookPath("iptables-restore"); err != nil {
		return setupHostPortMappingsLegacy(nodeIP, mappings, sandboxIP)
	}
	out, err := exec.Command("iptables-save").CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables-save: %w: %s", err, strings.TrimSpace(string(out)))
	}
	script := buildHostPortRestoreScript(nodeIP, mappings, sandboxIP, existingHostPortComments(out))
	if script == "" {
		return nil // 全部规则已存在（幂等重入）
	}
	cmd := exec.Command("iptables-restore", "--noflush")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("iptables-restore: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// setupHostPortMappingsLegacy 为无 iptables-restore 环境下的逐条回退路径。
func setupHostPortMappingsLegacy(nodeIP string, mappings []PortMapping, sandboxIP string) error {
	var errs []error
	for _, m := range mappings {
		if err := SetupHostPortMapping(nodeIP, m.HostPort, sandboxIP, m.SandboxPort); err != nil {
			log.Printf("[HostPort] WARNING: setup mapping %d->%d failed: %v", m.HostPort, m.SandboxPort, err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// existingHostPortComments 从 iptables-save 输出中提取 hostport 规则的 comment 集合。
// comment 不含空格（hostPortRuleComment 已替换 ":" 与空格），截取到首个空白/引号为止。
func existingHostPortComments(saveOutput []byte) map[string]struct{} {
	comments := make(map[string]struct{})
	scanner := bufio.NewScanner(bytes.NewReader(saveOutput))
	for scanner.Scan() {
		line := scanner.Text()
		idx := strings.Index(line, "cri-multiplex:hostport:")
		if idx < 0 {
			continue
		}
		comment := line[idx:]
		if end := strings.IndexAny(comment, " \t\""); end >= 0 {
			comment = comment[:end]
		}
		comments[comment] = struct{}{}
	}
	return comments
}

// buildHostPortRestoreScript 生成 iptables-restore --noflush 的输入脚本；
// comment 已在 existing 中的映射整体跳过（幂等去重）。全部已存在时返回空串。
func buildHostPortRestoreScript(nodeIP string, mappings []PortMapping, sandboxIP string, existing map[string]struct{}) string {
	var natRules, filterRules []string
	for _, m := range mappings {
		comment := hostPortRuleComment(nodeIP, m.HostPort, sandboxIP, m.SandboxPort)
		if _, ok := existing[comment]; ok {
			continue
		}
		hostPort := strconv.Itoa(m.HostPort)
		sandboxPort := strconv.Itoa(m.SandboxPort)
		natRules = append(natRules,
			fmt.Sprintf("-A PREROUTING -p tcp -d %s --dport %s -m comment --comment %s -j DNAT --to-destination %s:%s",
				nodeIP, hostPort, comment, sandboxIP, sandboxPort),
			fmt.Sprintf("-A OUTPUT -p tcp -d %s --dport %s -m comment --comment %s -j DNAT --to-destination %s:%s",
				nodeIP, hostPort, comment, sandboxIP, sandboxPort),
			fmt.Sprintf("-A POSTROUTING -p tcp -d %s --dport %s -m comment --comment %s -j MASQUERADE",
				sandboxIP, sandboxPort, comment),
		)
		filterRules = append(filterRules,
			fmt.Sprintf("-A FORWARD -p tcp -d %s --dport %s -m comment --comment %s -j ACCEPT",
				sandboxIP, sandboxPort, comment),
			fmt.Sprintf("-A FORWARD -p tcp -s %s --sport %s -m comment --comment %s -j ACCEPT",
				sandboxIP, sandboxPort, comment),
		)
	}
	if len(natRules) == 0 && len(filterRules) == 0 {
		return ""
	}
	var b strings.Builder
	if len(natRules) > 0 {
		b.WriteString("*nat\n")
		for _, r := range natRules {
			b.WriteString(r + "\n")
		}
		b.WriteString("COMMIT\n")
	}
	if len(filterRules) > 0 {
		b.WriteString("*filter\n")
		for _, r := range filterRules {
			b.WriteString(r + "\n")
		}
		b.WriteString("COMMIT\n")
	}
	return b.String()
}

func hostPortRuleComment(nodeIP string, hostPort int, sandboxIP string, sandboxPort int) string {
	replacer := strings.NewReplacer(":", "_", " ", "_")
	return fmt.Sprintf("cri-multiplex:hostport:%s:%d:%s:%d",
		replacer.Replace(nodeIP), hostPort, replacer.Replace(sandboxIP), sandboxPort)
}
