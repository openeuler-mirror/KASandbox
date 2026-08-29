package engine

import (
	"context"
	"log"
	"strconv"
	"strings"
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

// createE2BSandbox 是沙箱创建的完整生命周期主体：CNI（预热池优先 / pending 标记 /
// RuntimeNetwork 注入）→ orchestrator Create（失败 CNI DEL 回滚）→ HostPort 分配
// 与 iptables → tracker + stateStore 登记。由 RunPodSandbox（CRI 面）与
// AdminCreate（admin 面）共用；幂等重试检查与 inflight 计数保留在各适配层。
func (e *grpcE2BEngine) createE2BSandbox(ctx context.Context, p e2bCreateParams) (*orchestrator.SandboxCreateResponse, error) {
	sandboxID := p.sandboxID
	cfg := p.cfg

	var cniRecord *CNIRecord
	if e.cniConfig.Enabled {
		if e.cniManager == nil {
			return nil, status.Errorf(codes.Unavailable, "e2b cni is enabled but cni manager is not initialized")
		}
		cniSource := "pool"
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
	resp, err := e.client.Create(ctx, e2bReq)
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
	var allMappings []PortMapping
	if hostIP != "" && e.hostPortManager != nil {
		// 1. 收集需要暴露的端口（仅来自 metadata）
		var ports []int
		if portsStr, ok := p.metadata[annExposePorts]; ok && portsStr != "" {
			for _, pt := range strings.Split(portsStr, ",") {
				if port, err := strconv.Atoi(strings.TrimSpace(pt)); err == nil && port > 0 && port < 65536 {
					ports = append(ports, port)
				}
			}
		}

		// 2. 分配所有端口
		if len(ports) > 0 {
			mappings, err := e.hostPortManager.AllocatePorts(sandboxID, ports)
			if err != nil {
				log.Printf("[GrpcE2BEngine] WARNING: failed to allocate host ports for %s: %v", sandboxID, err)
			} else {
				// 3. 为每个端口创建 iptables 规则
				for _, m := range mappings {
					if err := e.hostPortOps.setup(e.nodeIP, m.HostPort, hostIP, m.SandboxPort); err != nil {
						log.Printf("[GrpcE2BEngine] WARNING: failed to setup mapping %d->%d for %s: %v", m.HostPort, m.SandboxPort, sandboxID, err)
					} else {
						log.Printf("[GrpcE2BEngine] HostPort mapping: %s:%d -> %s:%d", e.nodeIP, m.HostPort, hostIP, m.SandboxPort)
					}
				}
				allMappings = mappings
			}
		}
	}
	// ===== 多端口分配结束 =====

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
	e.persistPodState(pod)

	log.Printf("[GrpcE2BEngine] sandbox created: cri_id=%s, e2b_id=%s (client_id=%s, host_ip=%s, default_port=%d, mappings=%v, envd_token_set=%v)",
		sandboxID, cfg.SandboxId, resp.ClientId, hostIP, defaultHostPort, allMappings, envdToken != "")

	return resp, nil
}
