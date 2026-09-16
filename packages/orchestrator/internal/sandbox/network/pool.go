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
	return env.ParseAs[Config]()
}

type Pool struct {
	config Config

	done     chan struct{}
	doneOnce sync.Once

	newSlots     chan *Slot
	newSlotsSize int
	reusedSlots  chan *Slot

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

	pool := &Pool{
		config:       config,
		done:         make(chan struct{}),
		newSlots:     newSlots,
		newSlotsSize: max(newSlotsPoolSize, 0),
		reusedSlots:  reusedSlots,
		slotStorage:  slotStorage,
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

func (p *Pool) Populate(ctx context.Context) {
	defer close(p.newSlots)

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
			logger.L().Info(ctx, "[Pool Status] newSlots: %d/%d, reusedSlots: %d/%d\n",
				zap.Int("newSlots len", len(p.newSlots)),
				zap.Int("newSlots cap", cap(p.newSlots)),
				zap.Int("reusedSlots len", len(p.reusedSlots)),
				zap.Int("reusedSlots cap", cap(p.reusedSlots)))
		}
	}
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

func (p *Pool) Return(ctx context.Context, slot *Slot) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return ErrClosed
	default:
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

	return errors.Join(errs...)
}
