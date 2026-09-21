package network

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/caarlos0/env/v11"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

var (
	meter = otel.Meter("github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/network")

	newSlotsAvailableCounter = utils.Must(meter.Int64UpDownCounter("orchestrator.network.slots_pool.new",
		metric.WithDescription("Number of new network slots ready to be used."),
		metric.WithUnit("{slot}"),
	))
	reusableSlotsAvailableCounter = utils.Must(meter.Int64UpDownCounter("orchestrator.network.slots_pool.reused",
		metric.WithDescription("Number of reused network slots ready to be used."),
		metric.WithUnit("{slot}"),
	))
	acquiredSlots = utils.Must(meter.Int64Counter("orchestrator.network.slots_pool.acquired",
		metric.WithDescription("Number of network slots acquired."),
		metric.WithUnit("{slot}"),
	))
	returnedSlotCounter = utils.Must(meter.Int64Counter("orchestrator.network.slots_pool.returned",
		metric.WithDescription("Number of network slots returned."),
		metric.WithUnit("{slot}"),
	))
	releasedSlotCounter = utils.Must(meter.Int64Counter("orchestrator.network.slots_pool.released",
		metric.WithDescription("Number of network slots released."),
		metric.WithUnit("{slot}"),
	))
)

type Config struct {
	// Pool sizes for pre-warmed network slots.
	// Set NETWORK_POOL_NEW_SLOTS_SIZE=0 to disable pre-warming entirely — this is
	// recommended for CNI-only deployments where sandboxes bring their own
	// external netns and never acquire a slot from this pool; otherwise startup
	// eagerly creates `size` netns/veth/iptables rule sets that nothing uses.
	NewSlotsPoolSize    int `env:"NETWORK_POOL_NEW_SLOTS_SIZE"    envDefault:"300"`
	ReusedSlotsPoolSize int `env:"NETWORK_POOL_REUSED_SLOTS_SIZE" envDefault:"1000"`

	// Using reserver IPv4 in range that is used for experiments and documentation
	// https://en.wikipedia.org/wiki/Reserved_IP_addresses
	OrchestratorInSandboxIPAddress string `env:"SANDBOX_ORCHESTRATOR_IP" envDefault:"192.0.2.1"`

	// DeniedPodCIDRs are extra CIDRs added to every sandbox firewall's
	// predefinedDenySet (all protocols, hard block). In CNI/external-netns
	// deployments this should be set to the cluster Pod CIDR (e.g. from
	// kubePodsCIDR) so sandboxes cannot reach each other's PodIPs while
	// keeping host/internet access. Comma-separated, empty = disabled.
	DeniedPodCIDRs []string `env:"SANDBOX_DENIED_POD_CIDR"`

	// FirewallAllowedCIDRs are extra CIDRs added to every sandbox firewall's
	// predefinedAllowSet, which is evaluated before the deny sets. Use it to
	// exempt addresses inside DeniedPodCIDRs (e.g. the bridge gateway or
	// shared in-cluster services). Comma-separated, empty = none.
	FirewallAllowedCIDRs []string `env:"SANDBOX_FIREWALL_ALLOWED_CIDRS"`

	// Per-sandbox egress proxy (native network mode only). Empty
	// SANDBOX_EGRESS_PROXY_MODE = the feature is off and behavior is exactly
	// as before; "per-sandbox" = this node can run one mitmproxy+addon proxy
	// process per sandbox inside the sandbox netns. "shared" is reserved for
	// the shared-proxy form and currently fails startup validation.
	SandboxEgressProxyMode string `env:"SANDBOX_EGRESS_PROXY_MODE"`

	// SandboxProxyListenPort is the in-netns listen port of every per-sandbox
	// proxy. Netns are isolated from each other, so one fixed node-wide port
	// is fine and no host port allocation is needed.
	SandboxProxyListenPort uint16 `env:"SANDBOX_PROXY_LISTEN_PORT" envDefault:"15001"`

	// SandboxProxyBinary is the mitmproxy standalone binary (PyInstaller
	// single-file, pre-provisioned in the node image). Its version must stay
	// locked to the addon's verified version.
	SandboxProxyBinary string `env:"SANDBOX_PROXY_BINARY" envDefault:"/opt/mitmproxy/mitmdump"`

	// SandboxProxyAddon is the addon.py script path (pre-provisioned in the
	// node image, synced from the opensandbox-egress upstream).
	SandboxProxyAddon string `env:"SANDBOX_PROXY_ADDON" envDefault:"/opt/opensandbox-egress/addon.py"`

	// SandboxProxyConfDir is the mitmproxy confdir holding the proxy CA
	// (cert+key). The CA must be the same one built into template guest trust
	// stores; all per-sandbox proxies share it, which is why it is node-level.
	SandboxProxyConfDir string `env:"SANDBOX_PROXY_CONFDIR" envDefault:"/var/lib/cri-multiplex/egress-ca"`

	// SandboxProxyApply controls when proxy spawn + rule installation happen:
	// "ready" (default) runs them asynchronously off the create path (boot-time
	// traffic egresses directly); "immediate" runs them synchronously inside
	// sandbox creation and fails the create on any error.
	SandboxProxyApply string `env:"SANDBOX_PROXY_APPLY" envDefault:"ready"`

	// SandboxProxyExemptCIDRs are extra destination CIDRs (comma-separated)
	// whose TCP egress bypasses the proxy (netns PREROUTING RETURN before the
	// catch-all REDIRECT), aligning with the addon's internal-direct semantics.
	SandboxProxyExemptCIDRs []string `env:"SANDBOX_PROXY_EXEMPT_CIDRS"`

	// SandboxProxyRestart: "on-crash" (default) lets the hostservice
	// supervisor restart a crashed proxy with backoff; "never" keeps the
	// sandbox fail-close (REDIRECT target unlistened) after a crash.
	SandboxProxyRestart string `env:"SANDBOX_PROXY_RESTART" envDefault:"on-crash"`

	// SandboxProxyLogDir, when set, writes per-sandbox proxy stdout/stderr to
	// <dir>/<sandboxID>.log (removed with the sandbox). Empty = proxy output
	// is piped into the orchestrator log via zapio.
	SandboxProxyLogDir string `env:"SANDBOX_PROXY_LOG_DIR"`

	// SandboxProxyUpstream is the node-default upstream proxy URL
	// (http://[user:pass@]host:port, usually the unified egress proxy) that
	// per-sandbox proxies cascade external traffic to. Empty = direct egress
	// after local interception. Per-sandbox annotation
	// cri-multiplex.dev/egress-upstream overrides it.
	SandboxProxyUpstream string `env:"SANDBOX_PROXY_UPSTREAM"`

	// SandboxProxyExtraArgs are extra mitmdump CLI args (space-separated, no
	// quoting support) appended verbatim to every per-sandbox proxy spawn, for
	// deployment-specific tuning without code changes. Typical use:
	// "--set ssl_verify_upstream_trusted_ca=<pem>" when the upstream (or a test
	// mock site) presents certs signed by this deployment's own CA — mitmproxy's
	// default upstream verification trusts certifi only, and both upstream trust
	// options (ssl_verify_upstream_trusted_ca / _confdir) REPLACE certifi rather
	// than append, so point one of them at a bundle that also covers public CAs
	// when sandboxes must still reach the real internet.
	SandboxProxyExtraArgs string `env:"SANDBOX_PROXY_EXTRA_ARGS"`

	// SandboxProxyCAAuto (SANDBOX_PROXY_CA_AUTO) enables automatic proxy CA
	// generation ("true"/"false", default "false" = the pre-feature behavior is
	// byte-for-byte unchanged: a missing CA is a WARNING and sandboxes run
	// mitm_capable=false). With "true" in per-sandbox mode, ParseConfig
	// generates the CA into SandboxProxyConfDir when missing (idempotent,
	// fail-fast on any error) and the create/resume path injects the CA cert
	// into the guest trust store via envd before installing redirect rules.
	// Only meaningful in per-sandbox mode; an invalid value always fails
	// config parsing.
	SandboxProxyCAAuto string `env:"SANDBOX_PROXY_CA_AUTO" envDefault:"false"`

	// SandboxProxyPoolSize is the pre-warm size of the egress-proxy slot
	// sub-pool (nil = unset → defaultProxySlotsPoolSize; 0 = no proxy
	// pre-warming). The sub-pool only exists when SandboxEgressProxyMode is
	// "per-sandbox"; see ProxySlotsPoolSize. For all-proxy deployments set
	// NETWORK_POOL_NEW_SLOTS_SIZE=0 to disable the unused plain pool.
	SandboxProxyPoolSize *int `env:"SANDBOX_PROXY_POOL_SIZE"`

	// SandboxProxyReusedPoolSize is the capacity of the egress-proxy reused
	// slot pool (nil = unset → defaultProxySlotsPoolSize). Deliberately
	// decoupled from ReusedSlotsPoolSize: all-proxy deployments run with the
	// plain pools shrunk/disabled, and the proxy reuse pool must still have a
	// sane capacity. Only effective in per-sandbox mode; see
	// ProxyReusedSlotsPoolSize.
	SandboxProxyReusedPoolSize *int `env:"SANDBOX_PROXY_REUSED_POOL_SIZE"`

	HyperloopProxyPort uint16 `env:"SANDBOX_HYPERLOOP_PROXY_PORT" envDefault:"5010"`
	NFSProxyPort       uint16 `env:"SANDBOX_NFS_PROXY_PORT"       envDefault:"5011"`
	PortmapperPort     uint16 `env:"SANDBOX_PORTMAPPER_PORT"      envDefault:"5012"`

	UseLocalNamespaceStorage bool `env:"USE_LOCAL_NAMESPACE_STORAGE"`

	// TCP firewall ports - separate ports for different traffic types to avoid
	// protocol detection blocking on server-first protocols like SSH.
	// - HTTP port: for traffic destined to port 80 (HTTP Host header inspection)
	// - TLS port: for traffic destined to port 443 (TLS SNI inspection)
	// - Other port: for all other traffic (CIDR-only check, no protocol inspection)
	SandboxTCPFirewallHTTPPort  uint16 `env:"SANDBOX_TCP_FIREWALL_HTTP_PORT"  envDefault:"5016"`
	SandboxTCPFirewallTLSPort   uint16 `env:"SANDBOX_TCP_FIREWALL_TLS_PORT"   envDefault:"5017"`
	SandboxTCPFirewallOtherPort uint16 `env:"SANDBOX_TCP_FIREWALL_OTHER_PORT" envDefault:"5018"`
}

func ParseConfig() (Config, error) {
	cfg, err := env.ParseAs[Config]()
	if err != nil {
		return Config{}, err
	}

	if err := cfg.ValidateEgressProxy(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// defaultProxySlotsPoolSize is the egress-proxy sub-pool pre-warm size used
// when SANDBOX_PROXY_POOL_SIZE is unset. Deliberately decoupled from
// NewSlotsPoolSize: all-proxy deployments run with NETWORK_POOL_NEW_SLOTS_SIZE=0
// (plain pool unused), and the proxy pool must still have a sane size.
const defaultProxySlotsPoolSize = 200

// ProxySlotsPoolSize resolves the effective egress-proxy sub-pool pre-warm
// size: the sub-pool exists only in per-sandbox mode (otherwise 0 — no proxy
// slots, no prewarm goroutines, behavior identical to native), and an unset
// SANDBOX_PROXY_POOL_SIZE falls back to defaultProxySlotsPoolSize.
func (c Config) ProxySlotsPoolSize() int {
	if c.SandboxEgressProxyMode != EgressProxyModePerSandbox {
		return 0
	}
	if c.SandboxProxyPoolSize == nil {
		return defaultProxySlotsPoolSize
	}

	return *c.SandboxProxyPoolSize
}

// ProxyReusedSlotsPoolSize resolves the effective egress-proxy reused-slot
// capacity: 0 unless per-sandbox mode is on; unset falls back to
// defaultProxySlotsPoolSize (independent of ReusedSlotsPoolSize).
func (c Config) ProxyReusedSlotsPoolSize() int {
	if c.SandboxEgressProxyMode != EgressProxyModePerSandbox {
		return 0
	}
	if c.SandboxProxyReusedPoolSize == nil {
		return defaultProxySlotsPoolSize
	}

	return *c.SandboxProxyReusedPoolSize
}

// ProxyCAAutoEnabled reports whether SANDBOX_PROXY_CA_AUTO is enabled
// (validated to be exactly "true"/"false" by ValidateEgressProxy).
func (c Config) ProxyCAAutoEnabled() bool {
	return c.SandboxProxyCAAuto == "true"
}

type Pool struct {
	config Config

	done     chan struct{}
	doneOnce sync.Once

	newSlots     chan *Slot
	newSlotsSize int
	reusedSlots  chan *Slot

	// Egress-proxy slot sub-pool: pre-warmed slots built in proxy shape
	// (egressProxy set before CreateNetwork — no host tcpProxy redirect, vrt
	// SNAT, all-protocol firewall baseline). Empty and unused unless
	// SandboxEgressProxyMode is "per-sandbox"; the proxy reuse pool capacity
	// follows SANDBOX_PROXY_REUSED_POOL_SIZE (default 200).
	proxyNewSlots     chan *Slot
	proxyNewSlotsSize int
	proxyReusedSlots  chan *Slot

	slotStorage Storage
}

var ErrClosed = errors.New("cannot read from a closed pool")

func NewPool(newSlotsPoolSize, reusedSlotsPoolSize int, slotStorage Storage, config Config) *Pool {
	// 必须在创建任何 netns 之前隔离 /run/netns；失败时降级为原行为（仅记录日志）
	if err := isolateNetNSDir(); err != nil {
		logger.L().Error(context.Background(), "failed to isolate netns mount dir, named netns mounts may leak via shared propagation", zap.Error(err))
	}

	// One slot is always in flight being created, so the buffer holds size-1.
	// Clamp at 0 so a non-positive size disables pre-warming instead of panicking.
	newSlots := make(chan *Slot, max(newSlotsPoolSize-1, 0))
	reusedSlots := make(chan *Slot, max(reusedSlotsPoolSize, 0))

	proxyNewSlotsSize := config.ProxySlotsPoolSize()
	proxyNewSlots := make(chan *Slot, max(proxyNewSlotsSize-1, 0))
	proxyReusedSlots := make(chan *Slot, max(config.ProxyReusedSlotsPoolSize(), 0))

	pool := &Pool{
		config:       config,
		done:         make(chan struct{}),
		newSlots:     newSlots,
		newSlotsSize: max(newSlotsPoolSize, 0),
		reusedSlots:  reusedSlots,

		proxyNewSlots:     proxyNewSlots,
		proxyNewSlotsSize: proxyNewSlotsSize,
		proxyReusedSlots:  proxyReusedSlots,

		slotStorage: slotStorage,
	}

	return pool
}

// isolateNetNSDir 把 /run/netns 变成独立的 private 挂载点。
// systemd 系统上 /run 默认是 shared 传播，netns 的 bind mount 会传播出同路径的
// 叠加副本，而销毁路径只做单次 umount，残留会随时间累积并拖慢所有 netns/mount
// 操作。等价于 mount --bind /run/netns /run/netns && mount --make-rprivate /run/netns，
// 与 iproute2 对共享 /run 的处理方式一致。
// 注意幂等：已是挂载点时不再重复 bind——否则每次重启都会多叠一层，
// 并把上一层的 netns 挂载掩埋进挂载表（仍能访问旧路径但无法按名清理）。
func isolateNetNSDir() error {
	if err := os.MkdirAll(netnsRunDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", netnsRunDir, err)
	}

	mounted, err := isMountPoint(netnsRunDir)
	if err != nil {
		return fmt.Errorf("check %s mountpoint: %w", netnsRunDir, err)
	}

	if !mounted {
		if err := unix.Mount(netnsRunDir, netnsRunDir, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			return fmt.Errorf("bind mount %s: %w", netnsRunDir, err)
		}
	}

	if err := unix.Mount("", netnsRunDir, "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("make %s private: %w", netnsRunDir, err)
	}

	return nil
}

// isMountPoint 判断路径是否为挂载点，以 /proc/self/mountinfo 为准。
// 不能用 st_dev 与父目录比较——同一文件系统内的 bind mount 设备号相同。
func isMountPoint(path string) (bool, error) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false, fmt.Errorf("read mountinfo: %w", err)
	}

	// 每行第 5 个字段是挂载点
	for line := range strings.Lines(string(data)) {
		fields := strings.Fields(line)
		if len(fields) >= 5 && fields[4] == path {
			return true, nil
		}
	}

	return false, nil
}

func (p *Pool) Config() Config {
	return p.config
}

func (p *Pool) createNetworkSlot(ctx context.Context) (*Slot, error) {
	ips, err := p.slotStorage.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire network slot: %w", err)
	}

	err = ips.CreateNetwork(ctx)
	if err != nil {
		// CreateNetwork 中途失败可能已留下 netns bind mount、veth 或 iptables 残留，
		// 先尽力清理再释放槽位，避免残留随时间累积
		removeErr := ips.RemoveNetwork()
		releaseErr := p.slotStorage.Release(ips)
		err = errors.Join(err, removeErr, releaseErr)

		return nil, fmt.Errorf("failed to create network: %w", err)
	}

	return ips, nil
}

// createProxyNetworkSlot builds a slot in egress-proxy shape: the
// egressProxy marker is set BEFORE CreateNetwork, which is what shapes the
// slot — no host-side tcpProxy redirect, an extra vrt SNAT rule, and an
// all-protocol firewall baseline.
func (p *Pool) createProxyNetworkSlot(ctx context.Context) (*Slot, error) {
	ips, err := p.slotStorage.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire network slot: %w", err)
	}

	ips.egressProxy = true

	err = ips.CreateNetwork(ctx)
	if err != nil {
		releaseErr := p.slotStorage.Release(ips)
		err = errors.Join(err, releaseErr)

		return nil, fmt.Errorf("failed to create egress-proxy network: %w", err)
	}

	return ips, nil
}

func (p *Pool) Populate(ctx context.Context) {
	defer close(p.newSlots)

	if p.proxyNewSlotsSize > 0 {
		go p.populateProxySlots(ctx)
	}

	if p.newSlotsSize == 0 {
		logger.L().Info(ctx, "[network slot pool]: pre-warming disabled (NETWORK_POOL_NEW_SLOTS_SIZE=0)")

		return
	}

	for {
		select {
		case <-p.done:
			return
		case <-ctx.Done():
			return
		default:
			slot, err := p.createNetworkSlot(ctx)
			if err != nil {
				logger.L().Error(ctx, "[network slot pool]: failed to create network", zap.Error(err))

				continue
			}

			newSlotsAvailableCounter.Add(ctx, 1)
			p.newSlots <- slot
			p.logPoolStatus(ctx)
		}
	}
}

// populateProxySlots pre-warms the egress-proxy sub-pool, mirroring the
// newSlots refill loop. Each slot is built in proxy shape (egressProxy set
// before CreateNetwork), so acquisition is a pure dequeue with no rule
// changes on the create path.
func (p *Pool) populateProxySlots(ctx context.Context) {
	defer close(p.proxyNewSlots)

	for {
		select {
		case <-p.done:
			return
		case <-ctx.Done():
			return
		default:
			slot, err := p.createProxyNetworkSlot(ctx)
			if err != nil {
				logger.L().Error(ctx, "[network slot pool]: failed to create egress-proxy network", zap.Error(err))

				continue
			}

			newSlotsAvailableCounter.Add(ctx, 1)
			p.proxyNewSlots <- slot
			p.logPoolStatus(ctx)
		}
	}
}

func (p *Pool) logPoolStatus(ctx context.Context) {
	if p.proxyNewSlotsSize > 0 {
		logger.L().Info(ctx, "[Pool Status] newSlots: %d/%d, reusedSlots: %d/%d, proxyNewSlots: %d/%d, proxyReusedSlots: %d/%d\n",
			zap.Int("newSlots len", len(p.newSlots)),
			zap.Int("newSlots cap", cap(p.newSlots)),
			zap.Int("reusedSlots len", len(p.reusedSlots)),
			zap.Int("reusedSlots cap", cap(p.reusedSlots)),
			zap.Int("proxyNewSlots len", len(p.proxyNewSlots)),
			zap.Int("proxyNewSlots cap", cap(p.proxyNewSlots)),
			zap.Int("proxyReusedSlots len", len(p.proxyReusedSlots)),
			zap.Int("proxyReusedSlots cap", cap(p.proxyReusedSlots)))

		return
	}

	logger.L().Info(ctx, "[Pool Status] newSlots: %d/%d, reusedSlots: %d/%d\n",
		zap.Int("newSlots len", len(p.newSlots)),
		zap.Int("newSlots cap", cap(p.newSlots)),
		zap.Int("reusedSlots len", len(p.reusedSlots)),
		zap.Int("reusedSlots cap", cap(p.reusedSlots)))
}

func (p *Pool) Get(ctx context.Context, network *orchestrator.SandboxNetworkConfig) (*Slot, error) {
	var slot *Slot

	select {
	case <-p.done:
		return nil, ErrClosed
	case s := <-p.reusedSlots:
		reusableSlotsAvailableCounter.Add(ctx, -1)
		acquiredSlots.Add(ctx, 1, metric.WithAttributes(attribute.String("pool", "reused")))
		telemetry.ReportEvent(ctx, "reused network slot")

		slot = s
	default:
		select {
		case <-p.done:
			return nil, ErrClosed
		case <-ctx.Done():
			return nil, ctx.Err()
		case s := <-p.newSlots:
			newSlotsAvailableCounter.Add(ctx, -1)
			acquiredSlots.Add(ctx, 1, metric.WithAttributes(attribute.String("pool", "new")))
			telemetry.ReportEvent(ctx, "new network slot")

			slot = s
		}
	}

	err := slot.ConfigureInternet(ctx, network)
	if err != nil {
		// Return the slot to the pool if configuring internet fails
		go func() {
			if returnErr := p.Return(context.WithoutCancel(ctx), slot); returnErr != nil {
				logger.L().Error(ctx, "failed to return slot to the pool", zap.Error(returnErr), zap.Int("slot_index", slot.Idx))
			}
		}()

		return nil, fmt.Errorf("error setting slot internet access: %w", err)
	}

	return slot, nil
}

// GetEgressProxySlot acquires a slot from the egress-proxy sub-pool for a
// sandbox that runs its own in-netns egress proxy (egress-mode=per-sandbox).
// The sub-pool slots are pre-warmed in proxy shape (no host tcpProxy redirect,
// vrt SNAT, all-protocol firewall baseline — the shape difference from plain
// pooled slots is node-level homogeneous, so it is fixed at prewarm time), so
// acquisition is a pure dequeue: non-blocking drain of proxyReusedSlots first,
// otherwise block on proxyNewSlots (or done/ctx).
func (p *Pool) GetEgressProxySlot(ctx context.Context, network *orchestrator.SandboxNetworkConfig) (*Slot, error) {
	var slot *Slot

	select {
	case <-p.done:
		return nil, ErrClosed
	case s := <-p.proxyReusedSlots:
		reusableSlotsAvailableCounter.Add(ctx, -1)
		acquiredSlots.Add(ctx, 1, metric.WithAttributes(attribute.String("pool", "egress-proxy-reused")))
		telemetry.ReportEvent(ctx, "reused egress-proxy network slot")

		slot = s
	default:
		select {
		case <-p.done:
			return nil, ErrClosed
		case <-ctx.Done():
			return nil, ctx.Err()
		case s := <-p.proxyNewSlots:
			newSlotsAvailableCounter.Add(ctx, -1)
			acquiredSlots.Add(ctx, 1, metric.WithAttributes(attribute.String("pool", "egress-proxy-new")))
			telemetry.ReportEvent(ctx, "new egress-proxy network slot")

			slot = s
		}
	}

	if err := slot.ConfigureInternet(ctx, network); err != nil {
		// The slot's rule set is per-sandbox customized; it must not be
		// returned to the reusable pool.
		go func() {
			if cleanupErr := p.cleanup(context.WithoutCancel(ctx), slot); cleanupErr != nil {
				logger.L().Error(ctx, "failed to cleanup egress-proxy slot", zap.Error(cleanupErr), zap.Int("slot_index", slot.Idx))
			}
		}()

		return nil, fmt.Errorf("error setting slot internet access: %w", err)
	}

	return slot, nil
}

// Discard tears down a slot's network and releases it to storage without
// returning it to the reusable pool. Used for egress-proxy slots whose rule
// set differs from pooled slots (no tcpProxy redirect, extra vrt SNAT).
func (p *Pool) Discard(ctx context.Context, slot *Slot) error {
	return p.cleanup(ctx, slot)
}

func (p *Pool) Return(ctx context.Context, slot *Slot) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return ErrClosed
	default:
	}

	if slot.egressProxy {
		return p.returnEgressProxySlot(ctx, slot)
	}

	err := slot.ResetInternet(ctx)
	if err != nil {
		// Cleanup the slot if resetting internet fails
		if cerr := p.cleanup(ctx, slot); cerr != nil {
			return fmt.Errorf("reset internet: %w; cleanup: %w", err, cerr)
		}

		return fmt.Errorf("error resetting slot internet access: %w", err)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return ErrClosed
	case p.reusedSlots <- slot:
		returnedSlotCounter.Add(ctx, 1)
		reusableSlotsAvailableCounter.Add(ctx, 1)
	default:
		err := p.cleanup(ctx, slot)
		if err != nil {
			return fmt.Errorf("failed to return slot '%d': %w", slot.Idx, err)
		}
	}

	return nil
}

// returnEgressProxySlot strips the per-sandbox pieces of a proxy slot — the
// netns E2B_EGRESS_PROXY redirect chain and the firewall customizations — and
// returns the slot to the proxy reuse pool. ANY failure during cleanup falls
// back to Discard (cleanup): a slot whose per-sandbox state cannot be fully
// peeled must never re-enter the pool. The firewall baseline needs no
// re-shaping: userRulesAllProtocols was fixed at NewFirewall time and
// Firewall.Reset rebuilds it.
func (p *Pool) returnEgressProxySlot(ctx context.Context, slot *Slot) error {
	if err := removeSlotEgressProxyRules(ctx, slot); err != nil {
		logger.L().Error(ctx, "failed to remove egress proxy rules, discarding slot", zap.Error(err), zap.Int("slot_index", slot.Idx))
		if cerr := p.cleanup(ctx, slot); cerr != nil {
			return fmt.Errorf("remove egress proxy rules: %w; cleanup: %w", err, cerr)
		}

		return fmt.Errorf("error removing egress proxy rules, slot discarded: %w", err)
	}

	if err := slot.ResetInternet(ctx); err != nil {
		logger.L().Error(ctx, "failed to reset egress-proxy slot internet access, discarding slot", zap.Error(err), zap.Int("slot_index", slot.Idx))
		if cerr := p.cleanup(ctx, slot); cerr != nil {
			return fmt.Errorf("reset internet: %w; cleanup: %w", err, cerr)
		}

		return fmt.Errorf("error resetting slot internet access, slot discarded: %w", err)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return ErrClosed
	case p.proxyReusedSlots <- slot:
		returnedSlotCounter.Add(ctx, 1)
		reusableSlotsAvailableCounter.Add(ctx, 1)
	default:
		err := p.cleanup(ctx, slot)
		if err != nil {
			return fmt.Errorf("failed to return slot '%d': %w", slot.Idx, err)
		}
	}

	return nil
}

func (p *Pool) cleanup(ctx context.Context, slot *Slot) error {
	var errs []error

	err := slot.RemoveNetwork()
	if err != nil {
		errs = append(errs, fmt.Errorf("cannot remove network when releasing slot '%d': %w", slot.Idx, err))
	}

	err = p.slotStorage.Release(slot)
	if err != nil {
		errs = append(errs, fmt.Errorf("failed to release slot '%d': %w", slot.Idx, err))
	}

	releasedSlotCounter.Add(ctx, 1)

	return errors.Join(errs...)
}

func (p *Pool) Close(ctx context.Context) error {
	logger.L().Info(ctx, "Closing network pool")

	p.doneOnce.Do(func() {
		close(p.done)
	})

	var errs []error

	for slot := range p.newSlots {
		err := p.cleanup(ctx, slot)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to cleanup slot '%d': %w", slot.Idx, err))
		}
	}

	close(p.reusedSlots)

	for slot := range p.reusedSlots {
		err := p.cleanup(ctx, slot)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to cleanup slot '%d': %w", slot.Idx, err))
		}
	}

	if p.proxyNewSlotsSize > 0 {
		for slot := range p.proxyNewSlots {
			err := p.cleanup(ctx, slot)
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to cleanup slot '%d': %w", slot.Idx, err))
			}
		}

		close(p.proxyReusedSlots)

		for slot := range p.proxyReusedSlots {
			err := p.cleanup(ctx, slot)
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to cleanup slot '%d': %w", slot.Idx, err))
			}
		}
	}

	return errors.Join(errs...)
}
