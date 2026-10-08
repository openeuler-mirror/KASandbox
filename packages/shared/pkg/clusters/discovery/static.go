package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/e2b-dev/infra/packages/shared/pkg/consts"
	"github.com/e2b-dev/infra/packages/shared/pkg/env"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

// StaticDiscovery 静态清单 + gRPC 健康探测的混合服务发现。
// 适用于 orchestrator/template-manager 由 systemd 承载、无 Nomad/K8s 工作负载
// 可查询的部署形态：候选节点来自配置（环境变量或 JSON 文件），节点活性由
// 标准 grpc_health_v1 探测实时判定——orchestrator 进程已注册该健康服务，
// 被探测方无需任何改动。
type StaticDiscovery struct {
	entries atomic.Value // []staticEntry，当前生效的候选节点清单

	filePath  string    // JSON 清单路径（可选），mtime 变化时惰性重载
	fileMtime time.Time // 上次成功加载的文件 mtime
	probePort int       // 健康探测端口（默认 consts.OrchestratorAPIPort）
	prober    *healthProber
}

// staticEntry 配置解析出的单个候选节点
type staticEntry struct {
	NodeID string `json:"nodeID"`
	IP     string `json:"ip"`
	Port   int    `json:"port,omitempty"` // 可选，覆盖默认探测端口
}

// StaticOption 可选参数
type StaticOption func(*StaticDiscovery)

// WithStaticProbePort 覆盖健康探测端口
func WithStaticProbePort(port int) StaticOption {
	return func(sd *StaticDiscovery) { sd.probePort = port }
}

// WithStaticFile 设置 JSON 清单文件路径（环境变量优先时仅作兜底）
func WithStaticFile(path string) StaticOption {
	return func(sd *StaticDiscovery) { sd.filePath = path }
}

// WithStaticProber 注入自定义 prober（测试用；注意 healthCheck=true 时 nil 会被
// newHealthProberFromEnv() 覆盖，要关闭健康探测请传 healthCheck=false 而非 nil）
func WithStaticProber(p *healthProber) StaticOption {
	return func(sd *StaticDiscovery) { sd.prober = p }
}

// NewStaticDiscovery 以给定清单创建发现实例。
// healthCheck 为 true 时启动周期探测 goroutine，并同步完成首轮探测，
// 避免调用方启动后的首个周期内拿到空列表。
func NewStaticDiscovery(ctx context.Context, entries []staticEntry, healthCheck bool, opts ...StaticOption) *StaticDiscovery {
	sd := &StaticDiscovery{probePort: int(consts.OrchestratorAPIPort)}
	for _, opt := range opts {
		opt(sd)
	}
	sd.entries.Store(entries)

	if healthCheck && sd.prober == nil {
		sd.prober = newHealthProberFromEnv()
	}
	if sd.prober != nil {
		// 首轮同步探测：最多阻塞一个探测超时，保证启动后立即可用
		sd.prober.probeOnce(ctx, entries, sd.probePort)
		go sd.prober.run(ctx, &sd.entries, sd.probePort)
	}

	return sd
}

// NewStaticDiscoveryFromEnv 按约定环境变量创建发现实例：
//
//	E2B_STATIC_ALLOCATIONS       节点清单，如 "node1=10.0.0.1,node2=10.0.0.2:5009"
//	                             （省略 "=ip" 时以 NodeID 作为拨号地址）
//	E2B_STATIC_ALLOCATIONS_FILE  JSON 清单路径，[{"nodeID":"node1","ip":"10.0.0.1"}]，
//	                             仅在 E2B_STATIC_ALLOCATIONS 为空时生效；文件 mtime
//	                             变化后自动重载，扩缩容无需重启调用方
//	E2B_STATIC_DISCOVERY_HEALTHCHECK  "false" 关闭健康探测，退化为纯静态返回
func NewStaticDiscoveryFromEnv(ctx context.Context, opts ...StaticOption) *StaticDiscovery {
	entries := parseStaticEntries(env.GetEnv("E2B_STATIC_ALLOCATIONS", ""))

	// env 与 file 同时配置时 env 优先；仅当清单来自文件时才启用
	// mtime 热加载，避免热加载覆盖显式的 env 配置
	filePath := env.GetEnv("E2B_STATIC_ALLOCATIONS_FILE", "")
	if len(entries) == 0 && filePath != "" {
		if loaded, mtime, err := loadStaticEntriesFile(filePath); err != nil {
			logger.L().Warn(ctx, "Failed to load static allocations file", zap.String("path", filePath), zap.Error(err))
		} else {
			entries = loaded
			opts = append(opts, WithStaticFile(filePath), func(sd *StaticDiscovery) { sd.fileMtime = mtime })
		}
	}

	healthCheck := strings.ToLower(env.GetEnv("E2B_STATIC_DISCOVERY_HEALTHCHECK", "true")) != "false"

	return NewStaticDiscovery(ctx, entries, healthCheck, opts...)
}

// ListOrchestratorAndTemplateBuilderAllocations 返回当前存活的节点实例。
// 健康探测开启时仅返回探测为 Serving 的节点；关闭时返回全部配置节点。
func (sd *StaticDiscovery) ListOrchestratorAndTemplateBuilderAllocations(ctx context.Context) ([]Allocation, error) {
	sd.maybeReloadFile(ctx)

	entries, _ := sd.entries.Load().([]staticEntry)
	result := make([]Allocation, 0, len(entries))
	for _, e := range entries {
		if sd.prober != nil && !sd.prober.isServing(e.NodeID) {
			continue
		}
		result = append(result, Allocation{
			NodeID:       e.NodeID,
			AllocationID: "static-" + e.IP,
			AllocationIP: e.IP,
		})
	}

	return result, nil
}

// maybeReloadFile 清单文件 mtime 变化时惰性重载（stat 开销可忽略）
func (sd *StaticDiscovery) maybeReloadFile(ctx context.Context) {
	if sd.filePath == "" {
		return
	}
	info, err := os.Stat(sd.filePath)
	if err != nil {
		return
	}
	if !info.ModTime().After(sd.fileMtime) {
		return
	}
	loaded, mtime, err := loadStaticEntriesFile(sd.filePath)
	if err != nil {
		logger.L().Warn(ctx, "Failed to reload static allocations file", zap.String("path", sd.filePath), zap.Error(err))
		return
	}
	sd.fileMtime = mtime
	sd.entries.Store(loaded)
	logger.L().Info(ctx, "Reloaded static allocations file", zap.String("path", sd.filePath), zap.Int("count", len(loaded)))
}

// parseStaticEntries 解析 "node1=10.0.0.1,node2=10.0.0.2:5009,node3" 格式
func parseStaticEntries(raw string) []staticEntry {
	entries := make([]staticEntry, 0)
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		nodeID, addr, _ := strings.Cut(item, "=")
		nodeID = strings.TrimSpace(nodeID)
		if nodeID == "" {
			continue
		}
		addr = strings.TrimSpace(addr)
		if addr == "" {
			// 省略 "=ip" 时以 NodeID 作为拨号地址（DNS/hosts 可解析场景）
			addr = nodeID
		}
		ip, port := splitHostPort(addr)
		entries = append(entries, staticEntry{NodeID: nodeID, IP: ip, Port: port})
	}

	return entries
}

// loadStaticEntriesFile 读取 JSON 清单并校验
func loadStaticEntriesFile(path string) ([]staticEntry, time.Time, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("failed to read file: %w", err)
	}
	var entries []staticEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, time.Time{}, fmt.Errorf("failed to parse JSON: %w", err)
	}
	filtered := entries[:0]
	for _, e := range entries {
		if e.NodeID == "" {
			continue
		}
		filtered = append(filtered, e)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("failed to stat file: %w", err)
	}

	return filtered, info.ModTime(), nil
}

// splitHostPort 拆分 "ip:port"，无端口时返回 0
func splitHostPort(addr string) (string, int) {
	host, portStr, found := strings.Cut(addr, ":")
	if !found {
		return addr, 0
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil || port <= 0 {
		return host, 0
	}

	return host, port
}

// healthProber 周期性对候选节点执行 grpc_health_v1 探测并缓存活性结果。
// 状态机：探测成功立即标记 serving；连续 failThreshold 次失败才摘除，
// 防止单次网络抖动导致节点震荡。
type healthProber struct {
	interval      time.Duration
	timeout       time.Duration
	failThreshold int

	mu       sync.RWMutex
	serving  map[string]bool // nodeID -> 是否存活
	failures map[string]int  // nodeID -> 连续失败次数
}

func newHealthProber(interval, timeout time.Duration, failThreshold int) *healthProber {
	return &healthProber{
		interval:      interval,
		timeout:       timeout,
		failThreshold: failThreshold,
		serving:       make(map[string]bool),
		failures:      make(map[string]int),
	}
}

// newHealthProberFromEnv 按环境变量创建 prober：
//
//	E2B_STATIC_DISCOVERY_INTERVAL  探测周期秒数（默认 10）
//	E2B_STATIC_DISCOVERY_TIMEOUT   单次探测超时秒数（默认 3）
func newHealthProberFromEnv() *healthProber {
	intervalSec, err := env.GetEnvAsInt("E2B_STATIC_DISCOVERY_INTERVAL", 10)
	if err != nil || intervalSec <= 0 {
		intervalSec = 10
	}
	timeoutSec, err := env.GetEnvAsInt("E2B_STATIC_DISCOVERY_TIMEOUT", 3)
	if err != nil || timeoutSec <= 0 {
		timeoutSec = 3
	}

	return newHealthProber(time.Duration(intervalSec)*time.Second, time.Duration(timeoutSec)*time.Second, 2)
}

// isServing 查询节点当前活性（未探测过的节点视为不存活，首轮探测后才有确定状态）
func (p *healthProber) isServing(nodeID string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return p.serving[nodeID]
}

// run 周期探测循环，entries 引用支持热更新
func (p *healthProber) run(ctx context.Context, entries *atomic.Value, defaultPort int) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			list, _ := entries.Load().([]staticEntry)
			p.probeOnce(ctx, list, defaultPort)
		}
	}
}

// probeOnce 对所有候选节点并发执行一轮探测
func (p *healthProber) probeOnce(ctx context.Context, entries []staticEntry, defaultPort int) {
	var wg sync.WaitGroup
	for _, e := range entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			port := e.Port
			if port == 0 {
				port = defaultPort
			}
			p.update(ctx, e, p.check(ctx, e.IP, port))
		}()
	}
	wg.Wait()
}

// check 向目标地址发起一次健康检查，返回是否 Serving
func (p *healthProber) check(ctx context.Context, ip string, port int) bool {
	addr := fmt.Sprintf("%s:%d", ip, port)
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return false
	}
	defer conn.Close()

	checkCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	resp, err := grpc_health_v1.NewHealthClient(conn).Check(checkCtx, &grpc_health_v1.HealthCheckRequest{})

	return err == nil && resp.GetStatus() == grpc_health_v1.HealthCheckResponse_SERVING
}

// update 按状态机更新节点活性：成功立即 serving 并清零失败计数；
// 失败累计到 failThreshold 才摘除，防止单次抖动导致节点震荡
func (p *healthProber) update(ctx context.Context, e staticEntry, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if ok {
		if _, known := p.serving[e.NodeID]; known && !p.serving[e.NodeID] {
			logger.L().Info(ctx, "Static discovery node is back to serving",
				zap.String("node", e.NodeID), zap.String("ip", e.IP))
		}
		p.serving[e.NodeID] = true
		p.failures[e.NodeID] = 0

		return
	}

	p.failures[e.NodeID]++
	if p.serving[e.NodeID] && p.failures[e.NodeID] >= p.failThreshold {
		p.serving[e.NodeID] = false
		logger.L().Warn(ctx, "Static discovery node probe failed repeatedly, marking not serving",
			zap.String("node", e.NodeID), zap.String("ip", e.IP),
			zap.Int("failures", p.failures[e.NodeID]))
	}
}
