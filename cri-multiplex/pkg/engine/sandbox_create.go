package engine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cri-multiplex/pkg/orchestrator"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// e2bCreateParams 承载 createE2BSandbox 的入参，抹平 CRI/admin 两条创建路径的差异。
type e2bCreateParams struct {
	sandboxID string                      // cri 侧 id（tracker/stateStore/CNI netns 键）
	cfg       *orchestrator.SandboxConfig // 已填好 template/build/team/alias/metadata
	metadata  map[string]string           // CRI=annotations；admin=cfg.Metadata（同 key 解析 expose-ports）
	labels    map[string]string           // CRI 才有，admin 置 nil
	podUID    string                      // CRI=Pod UID；admin=sandboxID
	name      string                      // CRI=Pod name；admin=alias 或 sandboxID
	namespace string                      // CRI=Pod namespace；admin="e2b-admin"
	startTime time.Time
	endTime   time.Time                 // 零值时回落到 startTime + MaxSandboxLength 小时
	cniConfig *runtime.PodSandboxConfig // CRI=req.Config；admin=合成最小 config
}

// createE2BSandbox 是沙箱创建的完整生命周期主体：expose-ports 解析（malformed 即
// InvalidArgument fail-fast）→ CNI（预热池优先 / pending 标记 / RuntimeNetwork 注入）
// → orchestrator Create（失败 CNI DEL 回滚）→ HostPort 分配（失败即创建失败并回滚
// orchestrator Delete + CNI DEL）与 iptables 批量安装 → tracker + stateStore 登记。
// 由 RunPodSandbox（CRI 面）与 AdminCreate（admin 面）共用；幂等重试检查与 inflight
// 计数保留在各适配层。全程持有 per-sandbox 操作锁，与同 ID 的清理路径
// （cleanupSandboxResources）互斥，防止清理方的 netns DeleteNamed 拆掉创建
// 刚建好的同名 netns（orchestrator setns 报 EINVAL/ENOENT）。
func (e *grpcE2BEngine) createE2BSandbox(ctx context.Context, p e2bCreateParams) (*orchestrator.SandboxCreateResponse, error) {
	sandboxID := p.sandboxID
	cfg := p.cfg

	mu, err := e.lockSandbox(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	defer mu.Unlock()

	// expose-ports 提前解析（设计文档 4.4.2.6）：malformed 直接 InvalidArgument
	// fail-fast，不进入 CNI/orchestrator，避免"声明被静默丢弃但业务以为已暴露"。
	var exposeSpecs []ExposePortSpec
	if portsStr, ok := p.metadata[annExposePorts]; ok && portsStr != "" {
		specs, err := parseExposePorts(portsStr)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid %s: %v", annExposePorts, err)
		}
		exposeSpecs = specs
	}

	// per-sandbox egress 代理注解前置校验（设计文档 §6.2）：与 malformed
	// expose-ports 同语义，CNI/orchestrator 调用之前 fail-fast，零回滚。
	if err := validateEgressConfig(p.metadata); err != nil {
		return nil, err
	}

	// 阶段耗时打点：CNI（池化/直连）→ orchestrator Create → HostPort，结束时输出
	// 单行 [PerfTrace] 日志，供并发压测脚本（28/29 号用例）采集统计。
	perfStart := time.Now()
	var cniStageMs, orchCreateMs, hostPortMs int64
	cniSource := "disabled"

	var cniRecord *CNIRecord
	if e.cniConfig.Enabled {
		if e.cniManager == nil {
			return nil, status.Errorf(codes.Unavailable, "e2b cni is enabled but cni manager is not initialized")
		}
		cniStageStart := time.Now()
		cniSource = "pool"
		var cniAddMs int64
		cniRecord = e.acquireCNIFromPool()
		if cniRecord != nil {
			// 池化 entry 在预热时已持有 pending 标记，沿用至本函数返回；
			// 其 netns/veth/IPAM 已就绪，cni_add_ms 记 0。
			defer e.unmarkPendingNetNS(cniRecord.NetNSName)
		} else {
			// pending 标记必须先于 Add：netns 文件在 Add 内部一开头就创建，
			// 而 orphan reconciler 扫描无宽限期，若在 CNI 插件执行期间扫描到
			// 未标记的 netns 会当孤儿删除，导致 orchestrator 打开 netns 报 ENOENT。
			cniSource = "direct"
			pendingNetNSName := e.cniManager.NetNSName(sandboxID)
			e.markPendingNetNS(pendingNetNSName)
			defer e.unmarkPendingNetNS(pendingNetNSName)
			cniStart := time.Now()
			var addErr error
			cniRecord, addErr = e.cniManager.Add(ctx, sandboxID, p.cniConfig)
			cniAddMs = time.Since(cniStart).Milliseconds()
			if addErr != nil {
				log.Printf("[GrpcE2BEngine] createE2BSandbox: CNI ADD failed for %s (cni_add_ms=%d): %v", sandboxID, cniAddMs, addErr)
				return nil, status.Errorf(codes.Unavailable, "cni add failed: %v", addErr)
			}
		}
		cfg.RuntimeNetwork = &orchestrator.SandboxRuntimeNetworkConfig{
			Mode:       orchestrator.SandboxRuntimeNetworkConfig_CNI_EXTERNAL_NETNS,
			NetnsPath:  cniRecord.NetNSPath,
			IfName:     cniRecord.IfName,
			PodIp:      cniRecord.PodIP,
			Gateway:    cniRecord.Gateway,
			DnsServers: cniRecord.DNS,
		}
		log.Printf("[GrpcE2BEngine] CNI ADD: sandbox=%s source=%s network=%s netns=%s podIP=%s cni_add_ms=%d",
			sandboxID, cniSource, cniRecord.Network, cniRecord.NetNSPath, cniRecord.PodIP, cniAddMs)
		cniStageMs = time.Since(cniStageStart).Milliseconds()
	}

	endTime := p.endTime
	if endTime.IsZero() {
		maxLen := cfg.MaxSandboxLength
		if maxLen <= 0 {
			maxLen = 1
		}
		endTime = p.startTime.Add(time.Duration(maxLen) * time.Hour)
	}
	e2bReq := &orchestrator.SandboxCreateRequest{
		Sandbox:   cfg,
		StartTime: timestamppb.New(p.startTime),
		EndTime:   timestamppb.New(endTime),
	}
	orchCreateStart := time.Now()
	resp, err := e.client.Create(ctx, e2bReq)
	orchCreateMs = time.Since(orchCreateStart).Milliseconds()
	if err != nil {
		log.Printf("[GrpcE2BEngine] createE2BSandbox: orchestrator.Create FAILED: %v", err)
		if cniRecord != nil {
			if delErr := e.cniManager.Del(context.Background(), cniRecord, p.cniConfig); delErr != nil {
				log.Printf("[GrpcE2BEngine] WARNING: CNI DEL rollback failed for %s: %v", sandboxID, delErr)
			}
		}
		return nil, mapE2BError(err)
	}

	hostIP := resp.GetHostIp()
	if hostIP == "" {
		log.Printf("[GrpcE2BEngine] WARNING: orchestrator did not return host_ip for %s", sandboxID)
	}

	// ===== 多端口分配 =====
	hostPortStart := time.Now()
	var allMappings []PortMapping
	if len(exposeSpecs) > 0 {
		if hostIP == "" || e.hostPortManager == nil {
			// 无沙箱 IP 无法安装 DNAT 规则，保持现状跳过（不满足分配前置条件）
			log.Printf("[GrpcE2BEngine] WARNING: expose-ports declared for %s but host_ip is unavailable, skip host port allocation", sandboxID)
		} else {
			// 分配失败即创建失败（行为变更，三种写法统一）：AllocatePorts 内部
			// 已回滚本批端口占用，此处按序回滚 orchestrator Delete → CNI DEL；
			// pending netns 标记由 defer 解除，tracker/stateStore 尚未登记无需回滚。
			mappings, err := e.hostPortManager.AllocatePorts(sandboxID, exposeSpecs)
			if err != nil {
				log.Printf("[GrpcE2BEngine] createE2BSandbox: allocate host ports failed for %s: %v", sandboxID, err)
				e.rollbackFailedCreate(ctx, sandboxID, cfg.SandboxId, cniRecord, p.cniConfig)
				return nil, status.Errorf(codes.ResourceExhausted, "allocate host ports for %s: %v", sandboxID, err)
			}
			// 批量安装 iptables 规则（单次 iptables-restore 事务）；
			// 单条/整批规则安装失败仅 WARNING 不阻断创建（与现状一致）。
			if setupErr := e.setupHostPortMappings(mappings, hostIP); setupErr != nil {
				log.Printf("[GrpcE2BEngine] WARNING: failed to setup host port mappings for %s: %v", sandboxID, setupErr)
			}
			for _, m := range mappings {
				log.Printf("[GrpcE2BEngine] HostPort mapping: %s:%d -> %s:%d", e.nodeIP, m.HostPort, hostIP, m.SandboxPort)
			}
			allMappings = mappings
		}
	}
	// ===== 多端口分配结束 =====
	hostPortMs = time.Since(hostPortStart).Milliseconds()

	// 提取默认端口映射（如果 49983 在声明中，会自然包含在 allMappings 里）
	defaultHostPort := 0
	for _, m := range allMappings {
		if m.SandboxPort == 49983 {
			defaultHostPort = m.HostPort
			break
		}
	}

	envdToken := cfg.GetEnvdAccessToken()

	pod := &podInfo{
		sandboxID:       sandboxID,
		e2bSandboxID:    cfg.SandboxId,
		podUID:          p.podUID,
		name:            p.name,
		namespace:       p.namespace,
		labels:          p.labels,
		annotations:     p.metadata,
		createdAt:       p.startTime,
		state:           stateRunning,
		templateID:      cfg.TemplateId,
		buildID:         cfg.BuildId,
		executionID:     cfg.ExecutionId,
		teamID:          cfg.TeamId,
		nodeName:        e.nodeName,
		envdAccessToken: envdToken,

		hostIP:       hostIP,
		hostPort:     defaultHostPort,
		podIP:        cniPodIP(cniRecord),
		cniEnabled:   cniRecord != nil,
		cniRecord:    cniRecord,
		portMappings: allMappings,
	}
	e.tracker.Add(sandboxID, pod)
	persistStart := time.Now()
	e.persistPodState(pod)
	persistMs := time.Since(persistStart).Milliseconds()

	log.Printf("[GrpcE2BEngine] sandbox created: cri_id=%s, e2b_id=%s (client_id=%s, host_ip=%s, default_port=%d, mappings=%v, envd_token_set=%v)",
		sandboxID, cfg.SandboxId, resp.ClientId, hostIP, defaultHostPort, allMappings, envdToken != "")

	var cniNetnsMs, cniLoUpMs, cniPluginMs, cniParseMs int64
	if cniRecord != nil && cniSource == "direct" {
		cniNetnsMs = cniRecord.NetnsMs
		cniLoUpMs = cniRecord.LoUpMs
		cniPluginMs = cniRecord.PluginMs
		cniParseMs = cniRecord.ParseMs
	}
	log.Printf("[PerfTrace] sandbox=%s cni_ms=%d cni_source=%s cni_netns_ms=%d cni_loup_ms=%d cni_plugin_ms=%d cni_parse_ms=%d orch_create_ms=%d hostport_ms=%d persist_ms=%d total_ms=%d",
		sandboxID, cniStageMs, cniSource, cniNetnsMs, cniLoUpMs, cniPluginMs, cniParseMs,
		orchCreateMs, hostPortMs, persistMs, time.Since(perfStart).Milliseconds())

	return resp, nil
}

// setupHostPortMappings 优先走批量安装（setupBatch，单次 iptables-restore 事务）；
// 未配置批量实现时回退为逐条 setup。
func (e *grpcE2BEngine) setupHostPortMappings(mappings []PortMapping, hostIP string) error {
	if e.hostPortOps.setupBatch != nil {
		return e.hostPortOps.setupBatch(e.nodeIP, mappings, hostIP)
	}
	var errs []error
	for _, m := range mappings {
		if err := e.hostPortOps.setup(e.nodeIP, m.HostPort, hostIP, m.SandboxPort); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// rollbackFailedCreate 回滚 orchestrator Create 成功之后才失败的创建（如 hostport
// 分配失败）：orchestrator Delete（幂等，销毁已建 VM）→ CNI DEL（沿用 Create 失败
// 路径的写法，含预热池 entry 回收）。失败仅记日志，由 remove/orphan reconcile 兜底。
func (e *grpcE2BEngine) rollbackFailedCreate(ctx context.Context, sandboxID, e2bSandboxID string, cniRecord *CNIRecord, podCfg *runtime.PodSandboxConfig) {
	if _, err := e.client.Delete(ctx, &orchestrator.SandboxDeleteRequest{SandboxId: e2bSandboxID}); err != nil {
		log.Printf("[GrpcE2BEngine] WARNING: rollback delete sandbox %s (e2b_id=%s) failed: %v (left for remove/reconcile)", sandboxID, e2bSandboxID, err)
	}
	if cniRecord != nil {
		if err := e.cniManager.Del(context.Background(), cniRecord, podCfg); err != nil {
			log.Printf("[GrpcE2BEngine] WARNING: rollback CNI DEL failed for %s: %v", sandboxID, err)
		}
	}
}

// validateEgressConfig 校验 per-sandbox egress 代理注解取值（设计文档 §3.2/§6.2）。
// 空值 = 未指定，合法；非法一律 InvalidArgument。egress-mode=per-sandbox 显式指定
// 时 sandbox-mis 必填——addon.py 中 SANDBOX_MIS 为空会静默禁用策略客户端
// （addon.py:1863-1868），不拦截会造成"业务以为有管控实际没有"；按 §8.2 推导命中
// per-sandbox 的场景由 orchestrator 侧校验，不在此层。
func validateEgressConfig(metadata map[string]string) error {
	mode := metadata[annEgressMode]
	switch mode {
	case "", "per-sandbox", "off":
	default:
		return status.Errorf(codes.InvalidArgument, "invalid %s=%q: expected per-sandbox or off", annEgressMode, mode)
	}
	// 当前仅支持 internal 发行版（UserId 取 SANDBOX_ID、策略 id_type=virtual），
	// 缺省亦由 orchestrator 侧按 internal 注入。
	if profile := metadata[annEgressProfile]; profile != "" && profile != "internal" {
		return status.Errorf(codes.InvalidArgument, "invalid %s=%q: only internal is supported", annEgressProfile, profile)
	}
	if mitm := metadata[annEgressMitm]; mitm != "" && mitm != "true" && mitm != "false" {
		return status.Errorf(codes.InvalidArgument, "invalid %s=%q: expected true or false", annEgressMitm, mitm)
	}
	if upstream := metadata[annEgressUpstream]; upstream != "" && upstream != "off" {
		if err := validateEgressUpstreamURL(upstream); err != nil {
			return status.Errorf(codes.InvalidArgument, "invalid %s=%q: %v", annEgressUpstream, upstream, err)
		}
	}
	if mode == "per-sandbox" && metadata[annSandboxMIS] == "" {
		return status.Errorf(codes.InvalidArgument, "%s is required when %s=per-sandbox", annSandboxMIS, annEgressMode)
	}
	return nil
}

// validateEgressUpstreamURL 校验上游代理 URL：scheme 限 http/https，host 必填，
// 端口（如指定）须为 1-65535；允许 URL 内嵌凭据（user:pass@）。
func validateEgressUpstreamURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("expected http or https scheme")
	}
	if u.Hostname() == "" {
		return fmt.Errorf("missing host")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("invalid port %q", p)
		}
	}
	return nil
}
