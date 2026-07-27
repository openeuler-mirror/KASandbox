package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/e2b-dev/infra/packages/clickhouse/pkg/hoststats"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/cfg"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/build"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/cgroup"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/fc"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/hostservice"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/nbd"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/network"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/rootfs"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/stratovirt"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/template"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/uffd"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/uffd/prefetch"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/vmm"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/metadata"
	featureflags "github.com/e2b-dev/infra/packages/shared/pkg/feature-flags"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	sbxlogger "github.com/e2b-dev/infra/packages/shared/pkg/logger/sandbox"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

var (
	meter                        = otel.GetMeterProvider().Meter("orchestrator.internal.sandbox")
	envdInitCalls                = utils.Must(telemetry.GetCounter(meter, telemetry.EnvdInitCalls))
	waitForEnvdDurationHistogram = utils.Must(telemetry.GetHistogram(meter, telemetry.WaitForEnvdDurationHistogramName))
)

var SandboxHttpTransport = otelhttp.NewTransport(
	&http.Transport{
		DisableKeepAlives: true,
		ForceAttemptHTTP2: false,
	},
)

// Http client that should be used for requests to sandboxes.
var sandboxHttpClient = http.Client{
	Timeout:   10 * time.Second,
	Transport: SandboxHttpTransport,
}

type Config struct {
	// TODO: Remove when the rootfs path is constant.
	// Only used for v1 rootfs paths format.
	BaseTemplateID string

	Vcpu  int64
	RamMB int64

	// TotalDiskSizeMB optional, now used only for metrics.
	TotalDiskSizeMB int64
	HugePages       bool

	Network        *orchestrator.SandboxNetworkConfig
	RuntimeNetwork *orchestrator.SandboxRuntimeNetworkConfig

	Envd EnvdMetadata

	VMMConfig vmm.VMMConfig

	// OSType is the guest OS family. Used by CreateSandbox to decide whether
	// to start host-side services (adb vsock proxy when OSType == OSTypeAndroid).
	// ResumeSandbox ignores this field and reads OSType from template metadata.
	// OSType metadata.OSType

	VolumeMounts []VolumeMountConfig
}

type VolumeMountConfig struct {
	ID   string
	Name string
	Path string
	Type string
}

type EnvdMetadata struct {
	Vars           map[string]string
	DefaultUser    *string
	DefaultWorkdir *string
	AccessToken    *string
	Version        string
}

type RuntimeMetadata struct {
	TemplateID  string
	SandboxID   string
	ExecutionID string

	// TeamID optional, used only for logging
	TeamID string
}

type Resources struct {
	Slot   *network.Slot
	rootfs []rootfs.Provider
	memory uffd.MemoryBackend
}

type internalConfig struct {
	EnvdInitRequestTimeout time.Duration
}

type Metadata struct {
	internalConfig internalConfig
	Config         Config
	Runtime        RuntimeMetadata

	startedAtMu sync.RWMutex // protects startedAt
	startedAt   time.Time

	endAtMu sync.RWMutex // protects endAt
	endAt   time.Time
}

// GetEndAt returns the sandbox end time in a thread-safe manner.
func (m *Metadata) GetEndAt() time.Time {
	m.endAtMu.RLock()
	defer m.endAtMu.RUnlock()

	return m.endAt
}

// SetEndAt sets the sandbox end time in a thread-safe manner.
func (m *Metadata) SetEndAt(t time.Time) {
	m.endAtMu.Lock()
	defer m.endAtMu.Unlock()

	m.endAt = t
}

type Sandbox struct {
	*Resources
	*Metadata

	// LifecycleID is a unique identifier for each VMM process.
	// It is used internally by the orchestrator for map eviction guards
	// and proxy connection pooling. Unlike ExecutionID (which is stable
	// across checkpoints and shared with the API), LifecycleID changes
	// every time a new VMM is started.
	LifecycleID string

	config  cfg.BuilderConfig
	files   *storage.SandboxFiles
	cleanup *Cleanup

	process      vmm.Process
	cgroupHandle *cgroup.CgroupHandle
	hostSvcMgr   *hostservice.Manager
	// hostSvcPorts is zero-value for non-Android sandboxes. Propagated to
	// the API via the gRPC SandboxCreateResponse.
	hostSvcPorts hostservice.Ports

	Template template.Template

	Checks *Checks

	hostStatsCollector *HostStatsCollector

	// Deprecated: to be removed in the future
	// It was used to store the config to allow API restarts
	APIStoredConfig *orchestrator.SandboxConfig

	exit *utils.ErrorOnce

	stop utils.Lazy[error]
}

func (s *Sandbox) LoggerMetadata() sbxlogger.SandboxMetadata {
	return sbxlogger.SandboxMetadata{
		SandboxID:  s.Runtime.SandboxID,
		TemplateID: s.Runtime.TemplateID,
		TeamID:     s.Runtime.TeamID,
	}
}

// HostServicePorts returns the host-side ports for this sandbox's Android
// emulator services. Zero-value for non-Android sandboxes.
func (s *Sandbox) HostServicePorts() hostservice.Ports {
	return s.hostSvcPorts
}

type networkSlotRes struct {
	slot *network.Slot
	err  error
}

// GetStartedAt returns the sandbox start time in a thread-safe manner.
func (m *Metadata) GetStartedAt() time.Time {
	m.startedAtMu.RLock()
	defer m.startedAtMu.RUnlock()

	return m.startedAt
}

// SetStartedAt sets the sandbox start time in a thread-safe manner.
func (m *Metadata) SetStartedAt(t time.Time) {
	m.startedAtMu.Lock()
	defer m.startedAtMu.Unlock()

	m.startedAt = t
}

type Factory struct {
	config            cfg.BuilderConfig
	networkPool       *network.Pool
	devicePool        *nbd.DevicePool
	featureFlags      *featureflags.Client
	hostStatsDelivery hoststats.Delivery
	cgroupManager     cgroup.Manager
	cidPool           *hostservice.CIDPool
}

func NewFactory(
	config cfg.BuilderConfig,
	networkPool *network.Pool,
	devicePool *nbd.DevicePool,
	featureFlags *featureflags.Client,
	hostStatsDelivery hoststats.Delivery,
	cgroupManager cgroup.Manager,
) *Factory {
	return &Factory{
		config:            config,
		networkPool:       networkPool,
		devicePool:        devicePool,
		featureFlags:      featureFlags,
		hostStatsDelivery: hostStatsDelivery,
		cgroupManager:     cgroupManager,
		cidPool:           hostservice.NewCIDPool(1000),
	}
}

func newVMMFactory(backend vmm.BackendType) (vmm.Factory, error) {
	switch backend {
	case vmm.BackendStratoVirt:
		return stratovirt.NewDefaultFactory(), nil
	case vmm.BackendFirecracker:
		return fc.NewDefaultFactory(), nil
	default:
		return nil, fmt.Errorf("unsupported VMM type %q", backend)
	}
}

// CreateSandbox creates the sandbox.
// IMPORTANT: You must Close() the sandbox after you are done with it.
func (f *Factory) CreateSandbox(
	ctx context.Context,
	config Config,
	runtime RuntimeMetadata,
	template template.Template,
	sandboxTimeout time.Duration,
	directDiskPaths map[string]string,
	processOptions vmm.ProcessOptions,
	apiConfigToStore *orchestrator.SandboxConfig,
) (s *Sandbox, e error) {
	ctx, span := tracer.Start(ctx, "create sandbox")
	defer span.End()
	defer handleSpanError(span, &e)

	execCtx, execSpan := startExecutionSpan(ctx)

	exit := utils.NewErrorOnce()

	cleanup := NewCleanup()
	defer func() {
		if e != nil {
			cleanupErr := cleanup.Run(ctx)
			e = errors.Join(e, cleanupErr)
			handleSpanError(execSpan, &e)
			execSpan.End()
		}
	}()

	ipsPromise := getNetworkSlot(ctx, f.networkPool, cleanup, config.Network, config.RuntimeNetwork)

	sandboxFiles := template.Files().NewSandboxFiles(runtime.SandboxID)
	cleanup.Add(ctx, cleanupFiles(f.config, sandboxFiles))

	disks, err := template.Disks(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get template disks: %w", err)
	}

	rootfsProviders := make([]rootfs.Provider, 0, len(disks))
	for _, disk := range disks {
		var provider rootfs.Provider
		if directPath := directDiskPaths[disk.Name]; directPath != "" {
			provider, err = rootfs.NewDirectProvider(ctx, disk.Device, directPath)
		} else {
			provider, err = rootfs.NewNBDProvider(ctx, disk.Device, sandboxFiles.SandboxCacheDiskPath(f.config.StorageConfig, disk.Name), f.devicePool, f.featureFlags)
		}
		if err != nil {
			return nil, fmt.Errorf("failed to create %s disk provider: %w", disk.Name, err)
		}
		rootfsProviders = append(rootfsProviders, provider)
		cleanup.Add(ctx, provider.Close)
		go func(name string, p rootfs.Provider) {
			if runErr := p.Start(execCtx); runErr != nil {
				logger.L().Error(ctx, "disk overlay error", zap.String("disk", name), zap.Error(runErr))
			}
		}(disk.Name, provider)
	}

	memfile, err := template.Memfile(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get memfile: %w", err)
	}

	memfileSize, err := memfile.Size(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get memfile size: %w", err)
	}

	// / ==== END of resources initialization ====
	ips, err := ipsPromise.Wait(ctx)
	if err != nil {
		return nil, err
	}

	vmmFactory, err := newVMMFactory(config.VMMConfig.Backend())
	if err != nil {
		return nil, err
	}

	vmmHandle, err := vmmFactory.NewProcess(
		ctx,
		execCtx,
		f.config,
		ips,
		sandboxFiles,
		config.VMMConfig,
		rootfsProviders,
		vmm.ConstantRootfsPaths,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to init VMM: %w", err)
	}

	telemetry.ReportEvent(ctx, "created vmm client")

	// Allocate CID and build Android host services BEFORE the VMM boots so
	// the guest sees the right vsock CID. All four services share the
	// per-sandbox CID.
	var hostSvcMgr *hostservice.Manager
	var hostSvcPorts hostservice.Ports
	if metadata.OSType(config.VMMConfig.OsType) == metadata.OSTypeAndroid {
		allocatedCID, cidErr := f.cidPool.Allocate(ctx)
		if cidErr != nil {
			return nil, fmt.Errorf("allocate vsock CID: %w", cidErr)
		}
		cleanup.Add(ctx, func(context.Context) error {
			f.cidPool.Release(allocatedCID)
			return nil
		})

		if svProc, ok := vmmHandle.(*stratovirt.Process); ok {
			svProc.SetVsockConfig(allocatedCID)
		}

		var services []hostservice.Service
		services, hostSvcPorts, err = buildAndroidHostServices(ctx, f.config, allocatedCID, runtime.SandboxID, sandboxFiles.SandboxHostDir())
		if err != nil {
			return nil, fmt.Errorf("build android host services: %w", err)
		}
		hostSvcMgr = hostservice.NewManager(services)

		logger.L().Info(ctx, "android host services configured",
			zap.Int64("cid", allocatedCID),
			zap.Int("adb_port", hostSvcPorts.AdbPort),
			zap.Int("modem_simulator_port", hostSvcPorts.ModemSimulatorPort),
			zap.Int("webrtc_http_port", hostSvcPorts.WebrtcHttpPort),
			zap.Int("webrtc_streaming_port", hostSvcPorts.WebrtcStreamingPort),
			zap.String("sandbox_id", runtime.SandboxID),
		)
	}
	if hostSvcMgr != nil {
		if err := hostSvcMgr.StartAll(ctx); err != nil {
			return nil, fmt.Errorf("failed to start android host services: %w", err)
		}
		cleanup.AddPriority(ctx, func(ctx context.Context) error {
			return hostSvcMgr.StopAll(ctx)
		})
	}

	err = vmmHandle.Create(
		ctx,
		sbxlogger.SandboxMetadata{
			SandboxID:  runtime.SandboxID,
			TemplateID: runtime.TemplateID,
			TeamID:     runtime.TeamID,
		},
		config.Vcpu,
		config.RamMB,
		config.HugePages,
		processOptions,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create VMM: %w", err)
	}
	telemetry.ReportEvent(ctx, "created vmm process")

	if hostSvcMgr != nil && hostSvcPorts.AdbPort > 0 {
		proxyAddr := fmt.Sprintf("127.0.0.1:%d", hostSvcPorts.AdbPort)
		if err := hostservice.PollVsockProxyReady(ctx, proxyAddr, 30*time.Second); err != nil {
			return nil, fmt.Errorf("vsock proxy not ready: %w", err)
		}
	}

	resources := &Resources{
		Slot:   ips,
		rootfs: rootfsProviders,
		memory: uffd.NewNoopMemory(memfileSize, memfile.BlockSize()),
	}

	metadata := &Metadata{
		internalConfig: internalConfig{
			EnvdInitRequestTimeout: f.GetEnvdInitRequestTimeout(ctx),
		},

		Config:  config,
		Runtime: runtime,

		startedAt: time.Now(),
		endAt:     time.Now().Add(sandboxTimeout),
	}

	sbx := &Sandbox{
		LifecycleID: uuid.NewString(),

		Resources: resources,
		Metadata:  metadata,

		Template: template,
		config:   f.config,
		files:    sandboxFiles,
		process:  vmmHandle,

		hostSvcMgr:   hostSvcMgr,
		hostSvcPorts: hostSvcPorts,

		cleanup: cleanup,

		APIStoredConfig: apiConfigToStore,

		exit: exit,
	}

	sbx.Checks = NewChecks(sbx, false)

	// Stop the sandbox first if it is still running, otherwise do nothing
	cleanup.AddPriority(ctx, sbx.Stop)

	go func() {
		defer execSpan.End()

		ctx, span := tracer.Start(execCtx, "sandbox-exit-wait")
		defer span.End()

		// If the process exists, stop the sandbox properly
		vmmErr := vmmHandle.Exit().Wait()
		err := sbx.Stop(ctx)

		exit.SetError(errors.Join(err, vmmErr))
	}()

	return sbx, nil
}

// Usage: defer handleSpanError(span, &err)
func handleSpanError(span trace.Span, err *error) {
	defer span.End()
	if err != nil && *err != nil {
		span.RecordError(*err)
		span.SetStatus(codes.Error, (*err).Error())
	}
}

// ResumeSandbox resumes the sandbox from already saved template or snapshot.
// IMPORTANT: You must Close() the sandbox after you are done with it.
func (f *Factory) ResumeSandbox(
	ctx context.Context,
	t template.Template,
	config Config,
	runtime RuntimeMetadata,
	startedAt time.Time,
	endAt time.Time,
	apiConfigToStore *orchestrator.SandboxConfig,
) (s *Sandbox, e error) {
	ctx, span := tracer.Start(ctx, "resume sandbox")
	defer span.End()
	defer handleSpanError(span, &e)

	startTime := time.Now()
	traceID := span.SpanContext().TraceID().String()
	zap.L().Sugar().Infof("[ResumeSandbox] enter, traceID=%s", traceID)
	defer func() {
		zap.L().Sugar().Infof("[ResumeSandbox] total cost: %d ms, traceID=%s", time.Since(startTime).Milliseconds(), traceID)
	}()

	execCtx, execSpan := startExecutionSpan(ctx)

	exit := utils.NewErrorOnce()

	cleanup := NewCleanup()
	defer func() {
		if e != nil {
			cleanupErr := cleanup.Run(ctx)
			e = errors.Join(e, cleanupErr)
			handleSpanError(execSpan, &e)
			execSpan.End()
		}
	}()

	sandboxFiles := t.Files().NewSandboxFiles(runtime.SandboxID)
	cleanup.Add(ctx, cleanupFiles(f.config, sandboxFiles))

	telemetry.ReportEvent(ctx, "created sandbox files")

	// Uffd initialization
	fcUffdPath := sandboxFiles.SandboxUffdSocketPath()
	uffdPromise := utils.NewPromise(func() (*uffd.Uffd, error) {
		memfile, err := t.Memfile(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to get memfile: %w", err)
		}

		telemetry.ReportEvent(ctx, "got template memfile")

		return uffd.New(memfile, fcUffdPath), nil
	})

	// Prefetching
	go func() {
		memfile, err := t.Memfile(ctx)
		if err != nil {
			return
		}

		meta, err := t.Metadata()
		if err != nil {
			return
		}

		telemetry.ReportEvent(ctx, "got metadata")

		// Start background prefetcher as early as possible if prefetch mapping exists
		// Fetching from source starts immediately; copying waits for uffd to be ready
		if meta.Prefetch != nil && meta.Prefetch.Memory != nil {
			fcUffd, err := uffdPromise.Wait(ctx)
			if err != nil {
				return
			}

			telemetry.ReportEvent(ctx, "starting prefetcher")
			l := logger.L().With(logger.WithSandboxID(runtime.SandboxID), logger.WithTemplateID(runtime.TemplateID), logger.WithTeamID(runtime.TeamID))

			go func() {
				p := prefetch.New(
					l,
					memfile,
					fcUffd,
					meta.Prefetch.Memory,
					f.featureFlags,
				)
				err := p.Start(execCtx)
				if err != nil {
					l.Error(ctx, "failed to start prefetcher", zap.Error(err))
				}
			}()
		}
	}()

	disks, err := t.Disks(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get template disks: %w", err)
	}

	rootfsProviders := make([]rootfs.Provider, 0, len(disks))
	for _, disk := range disks {
		provider, providerErr := rootfs.NewNBDProvider(
			ctx,
			disk.Device,
			sandboxFiles.SandboxCacheDiskPath(f.config.StorageConfig, disk.Name),
			f.devicePool,
			f.featureFlags,
		)
		if providerErr != nil {
			return nil, fmt.Errorf("failed to create %s disk overlay: %w", disk.Name, providerErr)
		}
		rootfsProviders = append(rootfsProviders, provider)
		cleanup.Add(ctx, provider.Close)
		go func(name string, p rootfs.Provider) {
			if runErr := p.Start(execCtx); runErr != nil {
				logger.L().Error(ctx, "disk overlay error", zap.String("disk", name), zap.Error(runErr))
			}
		}(disk.Name, provider)
	}
	telemetry.ReportEvent(ctx, "created disk overlays")

	// Slot initialization
	ipsPromise := getNetworkSlot(ctx, f.networkPool, cleanup, config.Network, config.RuntimeNetwork)

	// Memory initialization
	memoryPromise := utils.NewPromise(func() (struct{}, error) {
		fcUffd, err := uffdPromise.Wait(ctx)
		if err != nil {
			return struct{}{}, err
		}

		err = serveMemory(
			execCtx,
			cleanup,
			fcUffd,
			runtime.SandboxID,
		)
		if err != nil {
			return struct{}{}, fmt.Errorf("failed to serve memory: %w", err)
		}

		telemetry.ReportEvent(ctx, "started serving memory")

		return struct{}{}, nil
	})

	t2 := time.Now()
	// Wait for all resources to be initialized
	ips, err := ipsPromise.Wait(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get network slot: %w", err)
	}
	zap.L().Sugar().Infof("[ResumeSandbox] wait network slot cost: %.3f ms, traceID=%s", time.Since(t2).Seconds()*1000, traceID)

	telemetry.ReportEvent(ctx, "got network slot")

	tMemory := time.Now()
	_, err = memoryPromise.Wait(ctx)
	if err != nil {
		return nil, err
	}
	zap.L().Sugar().Infof("[ResumeSandbox] wait memory init cost: %.3f ms, traceID=%s", time.Since(tMemory).Seconds()*1000, traceID)
	// ==== END of resources initialization ====

	tRootfs := time.Now()
	rootfs, err := t.Rootfs()
	if err != nil {
		return nil, fmt.Errorf("failed to get rootfs overlay: %w", err)
	}
	zap.L().Sugar().Infof("[ResumeSandbox] t.Rootfs cost: %.3f ms, traceID=%s", time.Since(tRootfs).Seconds()*1000, traceID)

	t3 := time.Now()
	meta, err := t.Metadata()
	if err != nil {
		return nil, fmt.Errorf("failed to get metadata: %w", err)
	}
	zap.L().Sugar().Infof("[ResumeSandbox] get template metadata cost: %.3f ms, traceID=%s", time.Since(t3).Seconds()*1000, traceID)

	tCgroup := time.Now()
	// Create cgroup for sandbox resource accounting
	cgroupHandle, cgroupFD := createCgroup(ctx, f.cgroupManager, runtime.SandboxID, cleanup)
	zap.L().Sugar().Infof("[ResumeSandbox] create cgroup cost: %.3f ms, traceID=%s", time.Since(tCgroup).Seconds()*1000, traceID)

	t4 := time.Now()
	metadataVMM := vmm.BackendType(meta.Template.VMMType).OrDefault()
	metadataOS, err := vmm.ParseOsType(meta.Template.OsType)
	if err != nil {
		return nil, fmt.Errorf("invalid template OS/VMM metadata: %w", err)
	}
	if err := vmm.ValidateBackendForOS(metadataOS, metadataVMM); err != nil {
		return nil, fmt.Errorf("invalid template OS/VMM metadata: %w", err)
	}
	config.VMMConfig.Type = metadataVMM
	config.VMMConfig.OsType = metadataOS
	vmmFactory, vmmErr := newVMMFactory(config.VMMConfig.Backend())
	if vmmErr != nil {
		return nil, vmmErr
	}

	vmmHandle, vmmErr := vmmFactory.NewProcess(
		ctx,
		execCtx,
		f.config,
		ips,
		sandboxFiles,
		config.VMMConfig,
		rootfsProviders,
		vmm.RootfsPaths{
			TemplateVersion: meta.Version,
			TemplateID:      config.BaseTemplateID,
			BuildID:         rootfs.Header().Metadata.BaseBuildId.String(),
		},
	)

	if vmmErr != nil {
		return nil, fmt.Errorf("failed to create VMM: %w", vmmErr)
	}

	zap.L().Sugar().Infof("[ResumeSandbox] vmmFactory.NewProcess cost: %.3f ms, traceID=%s", time.Since(t4).Seconds()*1000, traceID)
	var hostSvcMgr *hostservice.Manager
	var hostSvcPorts hostservice.Ports
	if metadata.OSType(meta.Template.OsType) == metadata.OSTypeAndroid {
		allocatedCID, err := f.cidPool.Allocate(ctx)
		if err != nil {
			return nil, fmt.Errorf("allocate vsock CID: %w", err)
		}
		cleanup.Add(ctx, func(context.Context) error {
			f.cidPool.Release(allocatedCID)
			return nil
		})

		if svProc, ok := vmmHandle.(*stratovirt.Process); ok {
			svProc.SetVsockConfig(allocatedCID)
		}

		var services []hostservice.Service
		services, hostSvcPorts, err = buildAndroidHostServices(ctx, f.config, allocatedCID, runtime.SandboxID, sandboxFiles.SandboxHostDir())
		if err != nil {
			return nil, fmt.Errorf("build android host services: %w", err)
		}
		hostSvcMgr = hostservice.NewManager(services)

		logger.L().Info(ctx, "android host services configured",
			zap.Int64("cid", allocatedCID),
			zap.Int("adb_port", hostSvcPorts.AdbPort),
			zap.Int("modem_simulator_port", hostSvcPorts.ModemSimulatorPort),
			zap.Int("webrtc_http_port", hostSvcPorts.WebrtcHttpPort),
			zap.Int("webrtc_streaming_port", hostSvcPorts.WebrtcStreamingPort),
			zap.String("sandbox_id", runtime.SandboxID),
		)
	}
	if hostSvcMgr != nil {
		if err := hostSvcMgr.StartAll(ctx); err != nil {
			return nil, fmt.Errorf("failed to start android host services: %w", err)
		}
		cleanup.AddPriority(ctx, func(ctx context.Context) error {
			return hostSvcMgr.StopAll(ctx)
		})
	}

	// ==================== 6. 恢复 VM ====================
	phaseStart := time.Now()
	telemetry.ReportEvent(ctx, "created VMM process")

	// todo: check if kernel, firecracker, and envd versions exist
	tSnapfile := time.Now()
	snapfile, err := t.Snapfile()
	if err != nil {
		return nil, fmt.Errorf("failed to get snapfile: %w", err)
	}
	zap.L().Sugar().Infof("[ResumeSandbox] t.Snapfile cost: %.3f ms, snapfile=%s, traceID=%s",
		time.Since(tSnapfile).Seconds()*1000, snapfile.Path(), traceID)

	telemetry.ReportEvent(ctx, "got snapfile")

	tUffd := time.Now()
	vmmUffd, err := uffdPromise.Wait(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get uffd: %w", err)
	}
	zap.L().Sugar().Infof("[ResumeSandbox] wait uffd promise cost: %.3f ms, traceID=%s", time.Since(tUffd).Seconds()*1000, traceID)

	uffdStartCtx, cancelUffdStartCtx := context.WithCancelCause(ctx)
	defer cancelUffdStartCtx(fmt.Errorf("uffd finished starting"))
	go func() {
		uffdWaitErr := vmmUffd.Exit().Wait()

		cancelUffdStartCtx(fmt.Errorf("uffd process exited: %w", errors.Join(uffdWaitErr, context.Cause(uffdStartCtx))))
	}()
	sandboxMetadata := sbxlogger.SandboxMetadata{
		SandboxID:  runtime.SandboxID,
		TemplateID: runtime.TemplateID,
		TeamID:     runtime.TeamID,
	}

	resumeCgroupFD := cgroupFD
	if metadata.OSType(meta.Template.OsType) == metadata.OSTypeAndroid {
		// CLONE_INTO_CGROUP requires a cgroup v2 directory FD. The deployment
		// runs cgroup v1, so attach accounting after launch rather than making
		// fork/exec fail before StratoVirt starts.
		resumeCgroupFD = cgroup.NoCgroupFD
	}

	vmmStartErr := vmmHandle.Resume(
		uffdStartCtx,
		sandboxMetadata,
		fcUffdPath,
		snapfile,
		vmmUffd.Ready(),
		config.Envd.AccessToken,
		config.RamMB,
		config.Vcpu,
		config.HugePages,
		resumeCgroupFD,
		traceID,
	)

	// Release the cgroup directory FD — the kernel already used it during clone
	if cgroupHandle != nil {
		if releaseErr := cgroupHandle.ReleaseCgroupFD(); releaseErr != nil {
			logger.L().Warn(ctx, "failed to release cgroup directory FD",
				logger.WithSandboxID(runtime.SandboxID),
				zap.Error(releaseErr))
		}
	}

	if vmmStartErr != nil {
		return nil, fmt.Errorf("failed to start VMM: %w", vmmStartErr)
	}
	if hostSvcMgr != nil && hostSvcPorts.AdbPort > 0 {
		proxyAddr := fmt.Sprintf("127.0.0.1:%d", hostSvcPorts.AdbPort)
		if err := hostservice.PollVsockProxyReady(ctx, proxyAddr, 30*time.Second); err != nil {
			return nil, fmt.Errorf("vsock proxy not ready (guest adbd unreachable): %w", err)
		}
		logger.L().Info(ctx, "android host services ready",
			zap.String("sandbox_id", runtime.SandboxID),
			zap.String("proxy_addr", proxyAddr),
		)
	}

	zap.L().Sugar().Infof("[ResumeSandbox] resume VM cost: %d ms, traceID=%s", time.Since(phaseStart).Milliseconds(), traceID)
	telemetry.ReportEvent(ctx, "initialized VMM")

	resources := &Resources{
		Slot:   ips,
		rootfs: rootfsProviders,
		memory: vmmUffd,
	}

	metadata := &Metadata{
		internalConfig: internalConfig{
			EnvdInitRequestTimeout: f.GetEnvdInitRequestTimeout(ctx),
		},

		Config:  config,
		Runtime: runtime,

		startedAt: startedAt,
		endAt:     endAt,
	}

	sbx := &Sandbox{
		LifecycleID: uuid.NewString(),

		Resources:    resources,
		Metadata:     metadata,
		cgroupHandle: cgroupHandle,

		Template: t,
		config:   f.config,
		files:    sandboxFiles,
		process:  vmmHandle,

		hostSvcMgr:   hostSvcMgr,
		hostSvcPorts: hostSvcPorts,

		cleanup: cleanup,

		APIStoredConfig: apiConfigToStore,

		exit: exit,
	}

	useClickhouseMetrics := f.featureFlags.BoolFlag(ctx, featureflags.MetricsWriteFlag)

	// Part of the sandbox as we need to stop Checks before pausing the sandbox
	// This is to prevent race condition of reporting unhealthy sandbox
	sbx.Checks = NewChecks(sbx, useClickhouseMetrics)

	cleanup.AddPriority(ctx, func(ctx context.Context) error {
		// Stop the sandbox first if it is still running, otherwise do nothing
		return sbx.Stop(ctx)
	})

	telemetry.ReportEvent(execCtx, "waiting for envd")

	err = sbx.WaitForEnvd(
		ctx,
		f.config.EnvdTimeout,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to wait for sandbox start: %w", err)
	}

	telemetry.ReportEvent(execCtx, "envd initialized")

	if f.featureFlags.BoolFlag(execCtx, featureflags.HostStatsEnabled) {
		samplingInterval := time.Duration(f.featureFlags.IntFlag(execCtx, featureflags.HostStatsSamplingInterval)) * time.Millisecond
		initializeHostStatsCollector(execCtx, sbx, vmmHandle, meta.Template.BuildID, runtime, config, f.hostStatsDelivery, samplingInterval)
	}

	go sbx.Checks.Start(execCtx)

	go func() {
		defer execSpan.End()

		ctx, span := tracer.Start(execCtx, "sandbox-exit-wait")
		defer span.End()

		// Wait for either uffd or VMM process to exit
		select {
		case <-vmmUffd.Exit().Done():
		case <-vmmHandle.Exit().Done():
		}

		err := sbx.Stop(ctx)

		uffdWaitErr := vmmUffd.Exit().Wait()
		vmmErr := vmmHandle.Exit().Wait()
		exit.SetError(errors.Join(err, vmmErr, uffdWaitErr))
	}()

	return sbx, nil
}

func startExecutionSpan(ctx context.Context) (context.Context, trace.Span) {
	parentSpan := trace.SpanFromContext(ctx)

	ctx = context.WithoutCancel(ctx)
	ctx, span := tracer.Start(ctx, "execute sandbox", //nolint:spancheck // this is still just a helper method
		trace.WithNewRoot(),
	)

	parentSpan.AddLink(trace.LinkFromContext(ctx))

	return ctx, span //nolint:spancheck // this is still just a helper method
}

func (s *Sandbox) Wait(ctx context.Context) error {
	return s.exit.WaitWithContext(ctx)
}

func (s *Sandbox) Close(ctx context.Context) error {
	err := s.cleanup.Run(ctx)
	if err != nil {
		return fmt.Errorf("failed to cleanup sandbox: %w", err)
	}

	return nil
}

// Stop kills the sandbox. It is safe to call multiple times; only the first
// call will actually perform the stop operation.
func (s *Sandbox) Stop(ctx context.Context) error {
	return s.stop.GetOrInit(func() error {
		return s.doStop(ctx)
	})
}

// doStop performs the actual stop operation.
func (s *Sandbox) doStop(ctx context.Context) error {
	ctx, span := tracer.Start(ctx, "sandbox-close")
	defer span.End()

	var errs []error

	// Stop host stats collector and collect final sample
	if s.hostStatsCollector != nil {
		s.hostStatsCollector.Stop(ctx)
	}

	// Stop the health checks before stopping the sandbox
	s.Checks.Stop()

	vmmStopErr := s.process.Stop(ctx)
	if vmmStopErr != nil {
		errs = append(errs, fmt.Errorf("failed to stop VMM: %w", vmmStopErr))
	}

	// The process exited, we can continue with the rest of the cleanup.
	// We could use select with ctx.Done() to wait for cancellation, but if the process is not exited the whole cleanup will be in a bad state and will result in unexpected behavior.
	<-s.process.Exit().Done()

	if s.hostSvcMgr != nil {
		if hostSvcErr := s.hostSvcMgr.StopAll(ctx); hostSvcErr != nil {
			errs = append(errs, fmt.Errorf("failed to stop host services: %w", hostSvcErr))
		}
	}

	// Remove cgroup after process has exited
	if s.cgroupHandle != nil {
		if cgroupErr := s.cgroupHandle.Remove(ctx); cgroupErr != nil {
			logger.L().Warn(ctx, "failed to remove cgroup during cleanup",
				logger.WithSandboxID(s.Runtime.SandboxID),
				zap.Error(cgroupErr))
		}
	}

	uffdStopErr := s.Resources.memory.Stop()
	if uffdStopErr != nil {
		errs = append(errs, fmt.Errorf("failed to stop uffd: %w", uffdStopErr))
	}

	return errors.Join(errs...)
}

func (s *Sandbox) Shutdown(ctx context.Context) error {
	ctx, span := tracer.Start(ctx, "shutdown sandbox")
	defer span.End()

	// Stop the health check before pausing the VM
	s.Checks.Stop()

	// Suspend host-side services (e.g. adb vsock proxy) before pausing the
	// VMM so their connection state is consistent with the paused guest.
	if s.hostSvcMgr != nil {
		if err := s.hostSvcMgr.SuspendAll(); err != nil {
			return fmt.Errorf("failed to suspend host services: %w", err)
		}
	}

	if err := s.process.Pause(ctx); err != nil {
		return fmt.Errorf("failed to pause VM: %w", err)
	}

	// This is required because the FC API doesn't support passing /dev/null
	tf, err := storage.TemplateFiles{
		BuildID: uuid.New().String(),
	}.CacheFiles(s.config.StorageConfig)
	if err != nil {
		return fmt.Errorf("failed to create template files: %w", err)
	}
	defer tf.Close()

	// The snapfile is required only because the FC API doesn't support passing /dev/null
	snapfile := template.NewLocalFileLink(tf.CacheSnapfilePath())
	defer snapfile.Close()

	err = s.process.CreateSnapshot(ctx, snapfile.Path())
	if err != nil {
		return fmt.Errorf("error creating snapshot: %w", err)
	}

	// This should properly flush rootfs to the underlying device.
	err = s.Close(ctx)
	if err != nil {
		return fmt.Errorf("error stopping sandbox: %w", err)
	}

	return nil
}

// Pause creates a snapshot of the sandbox.
//
// Currently the memory snapshotting works like this:
//  1. We pause FC VM
//  2. We call FC snapshot endpoint without specifying memfile path. With our custom FC,
//     this only creates the snapfile and drains and flushes the disk.
//  3. We call custom FC endpoint that returns memory addresses of the sandbox memory, that we will process after.
//  4. In case of NoopMemory (the sandbox was not a resume) we also call the custom FC endpoint,
//     that returns info about resident memory pages and about empty memory pages.
//  5. Base on the info from the custom FC endpoint or from Uffd we copy the pages directly from the FC process to a local cache.
//  6. We then can either close the sandbox or resume it.
func (s *Sandbox) Pause(
	ctx context.Context,
	m metadata.Template,
) (st *Snapshot, e error) {
	ctx, span := tracer.Start(ctx, "sandbox-snapshot")
	defer span.End()

	cleanup := NewCleanup()
	defer func() {
		// Cleanup the snapshot if an error occurs
		if e != nil {
			err := cleanup.Run(ctx)
			e = errors.Join(e, err)
		}
	}()

	snapshotTemplateFiles, err := storage.TemplateFiles{BuildID: m.Template.BuildID}.CacheFiles(s.config.StorageConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to get template files: %w", err)
	}
	cleanup.AddNoContext(ctx, snapshotTemplateFiles.Close)

	buildID, err := uuid.Parse(snapshotTemplateFiles.BuildID)
	if err != nil {
		return nil, fmt.Errorf("failed to parse build id: %w", err)
	}

	// Stop the health check before pausing the VM
	s.Checks.Stop()

	// Android host processes hold live vsock connections. Freezing those
	// processes would persist host socket state that cannot be recreated by a
	// new vhost-vsock backend on snapshot restore, leaving the restored guest's
	// vsock device unusable. Template snapshots are terminal for this Sandbox,
	// so fully stop the per-sandbox processes before pausing Android. Shared
	// config/modem listeners remain host-wide and are not owned by this manager.
	// Other guest types retain the existing suspend behavior.
	if s.hostSvcMgr != nil {
		if s.Config.VMMConfig.OsType.OrDefault() == vmm.OsAndroid {
			if err := s.hostSvcMgr.StopAll(ctx); err != nil {
				return nil, fmt.Errorf("failed to stop Android host services before snapshot: %w", err)
			}
		} else if err := s.hostSvcMgr.SuspendAll(); err != nil {
			return nil, fmt.Errorf("failed to suspend host services: %w", err)
		}
	}

	if err := s.process.Pause(ctx); err != nil {
		return nil, fmt.Errorf("failed to pause VM: %w", err)
	}

	// Snapfile is not closed as it's returned and cached for later use (like resume)
	snapfile := template.NewLocalFileLink(snapshotTemplateFiles.CacheSnapfilePath())
	cleanup.AddNoContext(ctx, snapfile.Close)

	err = s.process.CreateSnapshot(ctx, snapfile.Path())
	if err != nil {
		return nil, fmt.Errorf("error creating snapshot: %w", err)
	}

	// Gather data for postprocessing
	originalMemfile, err := s.Template.Memfile(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get original memfile: %w", err)
	}

	originalDisks, err := s.Template.Disks(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get original disks: %w", err)
	}

	memfileDiffMetadata, err := s.Resources.memory.DiffMetadata(ctx, s.process)
	if err != nil {
		return nil, fmt.Errorf("failed to get memfile metadata: %w", err)
	}

	// Start POSTPROCESSING
	memfileDiff, memfileDiffHeader, err := pauseProcessMemory(
		ctx,
		buildID,
		originalMemfile.Header(),
		memfileDiffMetadata,
		s.config.DefaultCacheDir,
		s.process,
	)
	if err != nil {
		return nil, fmt.Errorf("error while post processing: %w", err)
	}
	cleanup.AddNoContext(ctx, memfileDiff.Close)

	rootfsDiffs := make(map[build.DiffType]build.Diff, len(originalDisks))
	rootfsDiffHeaders := make(map[build.DiffType]*header.Header, len(originalDisks))
	var resultMu sync.Mutex
	barrier := newDiskCloseBarrier(len(originalDisks), func() error {
		return s.Close(ctx)
	})
	closeHook := func(closeCtx context.Context) error {
		barrier.Arrive()
		select {
		case <-barrier.Done():
			return barrier.Err()
		case <-closeCtx.Done():
			return closeCtx.Err()
		}
	}
	group, groupCtx := errgroup.WithContext(ctx)
	for i, disk := range originalDisks {
		i, disk := i, disk
		group.Go(func() error {
			rootfsDiff, rootfsDiffHeader, err := pauseProcessRootfs(groupCtx, buildID, disk.DiffType, disk.Device.Header(), &RootfsDiffCreator{rootfs: s.rootfs[i], closeHook: closeHook}, s.config.DefaultCacheDir)
			if err != nil {
				barrier.Abort()
				return fmt.Errorf("post processing %s disk: %w", disk.Name, err)
			}
			resultMu.Lock()
			rootfsDiffs[disk.DiffType] = rootfsDiff
			rootfsDiffHeaders[disk.DiffType] = rootfsDiffHeader
			resultMu.Unlock()
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		barrier.Abort()
		resultMu.Lock()
		for _, rootfsDiff := range rootfsDiffs {
			err = errors.Join(err, rootfsDiff.Close())
		}
		resultMu.Unlock()
		err = errors.Join(err, barrier.Err())
		return nil, fmt.Errorf("error while post processing disks: %w", err)
	}
	for _, rootfsDiff := range rootfsDiffs {
		cleanup.AddNoContext(ctx, rootfsDiff.Close)
	}

	metadataFileLink := template.NewLocalFileLink(snapshotTemplateFiles.CacheMetadataPath())
	cleanup.AddNoContext(ctx, metadataFileLink.Close)

	err = m.ToFile(metadataFileLink.Path())
	if err != nil {
		return nil, err
	}

	return &Snapshot{
		Snapfile:          snapfile,
		Metafile:          metadataFileLink,
		MemfileDiff:       memfileDiff,
		MemfileDiffHeader: memfileDiffHeader,
		RootfsDiffs:       rootfsDiffs,
		RootfsDiffHeaders: rootfsDiffHeaders,

		cleanup: cleanup,
	}, nil
}

// MemoryPrefetchData returns the ordered page fault data for prefetch mapping.
func (s *Sandbox) MemoryPrefetchData(ctx context.Context) (block.PrefetchData, error) {
	prefetchData, err := s.Resources.memory.PrefetchData(ctx)
	if err != nil {
		return block.PrefetchData{}, fmt.Errorf("failed to get prefetch data: %w", err)
	}

	return prefetchData, nil
}

type diskCloseBarrier struct {
	total int

	mu      sync.Mutex
	arrived int
	err     error

	once    sync.Once
	done    chan struct{}
	closeFn func() error
}

func newDiskCloseBarrier(total int, closeFn func() error) *diskCloseBarrier {
	return &diskCloseBarrier{total: total, done: make(chan struct{}), closeFn: closeFn}
}

func (b *diskCloseBarrier) Arrive() {
	b.mu.Lock()
	b.arrived++
	ready := b.arrived == b.total
	b.mu.Unlock()

	if ready {
		b.close()
	}
}

func (b *diskCloseBarrier) Abort() {
	b.close()
}

func (b *diskCloseBarrier) close() {
	b.once.Do(func() {
		err := b.closeFn()
		b.mu.Lock()
		b.err = err
		b.mu.Unlock()
		close(b.done)
	})
}

func (b *diskCloseBarrier) Done() <-chan struct{} {
	return b.done
}

func (b *diskCloseBarrier) Err() error {
	<-b.done
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.err
}

func pauseProcessMemory(
	ctx context.Context,
	buildID uuid.UUID,
	originalHeader *header.Header,
	diffMetadata *header.DiffMetadata,
	cacheDir string,
	process vmm.Process,
) (d build.Diff, h *header.Header, e error) {
	ctx, span := tracer.Start(ctx, "process-memory")
	defer span.End()

	header, err := diffMetadata.ToDiffHeader(ctx, originalHeader, buildID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create memfile header: %w", err)
	}

	memfileDiffPath := build.GenerateDiffCachePath(cacheDir, buildID.String(), build.Memfile)

	cache, err := process.ExportMemory(
		ctx,
		diffMetadata.Dirty,
		memfileDiffPath,
		diffMetadata.BlockSize,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to export memory: %w", err)
	}

	diff, err := build.NewLocalDiffFromCache(
		build.GetDiffStoreKey(buildID.String(), build.Memfile),
		cache,
	)
	if err != nil {
		// Close the cache even if the diff creation fails.
		return nil, nil, fmt.Errorf("failed to create local diff from cache: %w", errors.Join(err, cache.Close()))
	}

	return diff, header, nil
}

func pauseProcessRootfs(
	ctx context.Context,
	buildId uuid.UUID,
	diffType build.DiffType,
	originalHeader *header.Header,
	diffCreator DiffCreator,
	cacheDir string,
) (d build.Diff, h *header.Header, e error) {
	ctx, span := tracer.Start(ctx, "process-rootfs")
	defer span.End()

	rootfsDiffFile, err := build.NewLocalDiffFile(cacheDir, buildId.String(), diffType)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create rootfs diff: %w", err)
	}

	rootfsDiffMetadata, err := diffCreator.process(ctx, rootfsDiffFile)
	if err != nil {
		err = errors.Join(err, rootfsDiffFile.Close())

		return nil, nil, fmt.Errorf("error creating diff: %w", err)
	}
	telemetry.ReportEvent(ctx, "exported rootfs")

	rootfsDiff, err := rootfsDiffFile.CloseToDiff(int64(originalHeader.Metadata.BlockSize))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to convert rootfs diff file to local diff: %w", err)
	}
	telemetry.ReportEvent(ctx, "converted rootfs diff file to local diff")

	rootfsHeader, err := rootfsDiffMetadata.ToDiffHeader(ctx, originalHeader, buildId)
	if err != nil {
		err = errors.Join(err, rootfsDiff.Close())

		return nil, nil, fmt.Errorf("failed to create rootfs header: %w", err)
	}

	return rootfsDiff, rootfsHeader, nil
}

// createCgroup creates a cgroup for sandbox resource accounting if cgroup
// accounting is enabled (cgroupManager is non-nil). It registers cleanup with
// the provided Cleanup so the cgroup is removed on error paths.
//
// Returns the CgroupHandle and the cgroup directory FD to pass to the
// VMM process. If cgroup accounting is disabled, returns (nil, cgroup.NoCgroupFD).
func createCgroup(ctx context.Context, cgroupManager cgroup.Manager, sandboxID string, cleanup *Cleanup) (*cgroup.CgroupHandle, int) {
	ctx, span := tracer.Start(ctx, "sandbox-create-cgroup", trace.WithAttributes(
		telemetry.WithSandboxID(sandboxID),
	))
	defer span.End()

	if cgroupManager == nil {
		return nil, cgroup.NoCgroupFD
	}

	handle, err := cgroupManager.Create(ctx, sandboxID)
	if err != nil {
		logger.L().Warn(ctx, "failed to create cgroup, continuing without cgroup accounting",
			logger.WithSandboxID(sandboxID),
			zap.Error(err))

		telemetry.ReportEvent(ctx, "cgroup creation failed, continuing without accounting")

		return nil, cgroup.NoCgroupFD
	}

	cleanup.Add(ctx, func(ctx context.Context) error {
		return handle.Remove(ctx)
	})

	return handle, handle.GetFD()
}

func getNetworkSlot(
	ctx context.Context,
	networkPool *network.Pool,
	cleanup *Cleanup,
	networkConfig *orchestrator.SandboxNetworkConfig,
	runtimeNetwork *orchestrator.SandboxRuntimeNetworkConfig,
) *utils.Promise[*network.Slot] {
	return utils.NewPromise(func() (*network.Slot, error) {
		ctx, span := tracer.Start(ctx, "get network-slot")
		defer span.End()

		if runtimeNetwork.GetMode() == orchestrator.SandboxRuntimeNetworkConfig_CNI_EXTERNAL_NETNS {
			slot, err := network.NewExternalNetNSSlot(runtimeNetwork.GetNetnsPath(), runtimeNetwork, networkPool.Config())
			if err != nil {
				return nil, fmt.Errorf("failed to create external netns slot: %w", err)
			}
			if err = slot.CreateNetwork(ctx); err != nil {
				return nil, fmt.Errorf("failed to create external netns network: %w", err)
			}
			cleanup.Add(ctx, func(ctx context.Context) error {
				return slot.RemoveNetwork()
			})
			return slot, nil
		}

		slot, err := networkPool.Get(ctx, networkConfig)
		if err != nil {
			return nil, fmt.Errorf("failed to get network slot: %w", err)
		}

		cleanup.Add(ctx, func(ctx context.Context) error {
			ctx, span := tracer.Start(ctx, "clean network-slot")
			defer span.End()

			// We can run this cleanup asynchronously, as it is not important for the sandbox lifecycle
			go func(ctx context.Context) {
				returnErr := networkPool.Return(ctx, slot)
				if returnErr != nil {
					logger.L().Error(ctx, "failed to return network slot", zap.Error(returnErr))
				}
			}(context.WithoutCancel(ctx))

			return nil
		})

		return slot, nil
	})
}

func serveMemory(
	ctx context.Context,
	cleanup *Cleanup,
	fcUffd *uffd.Uffd,
	sandboxID string,
) error {
	ctx, span := tracer.Start(ctx, "serve-memory")
	defer span.End()

	telemetry.ReportEvent(ctx, "created uffd")

	if err := fcUffd.Start(ctx, sandboxID); err != nil {
		return fmt.Errorf("failed to start uffd: %w", err)
	}

	telemetry.ReportEvent(ctx, "started uffd")

	cleanup.Add(ctx, func(ctx context.Context) error {
		_, span := tracer.Start(ctx, "uffd-stop")
		defer span.End()

		if err := fcUffd.Stop(); err != nil {
			return fmt.Errorf("failed to stop uffd: %w", err)
		}

		return nil
	})

	return nil
}

func (s *Sandbox) WaitForExit(ctx context.Context) error {
	ctx, span := tracer.Start(ctx, "sandbox-wait-for-exit")
	defer span.End()

	timeout := time.Until(s.GetEndAt())

	select {
	case <-time.After(timeout):
		return fmt.Errorf("waiting for exit took too long")
	case <-ctx.Done():
		return nil
	case <-s.exit.Done():
		err := s.exit.Error()
		if err == nil {
			return nil
		}

		return fmt.Errorf("vmm process exited prematurely: %w", err)
	}
}

func (s *Sandbox) WaitForEnvd(
	ctx context.Context,
	timeout time.Duration,
) (e error) {
	start := time.Now()
	ctx, span := tracer.Start(ctx, "sandbox-wait-for-start")
	defer span.End()

	defer func() {
		if e != nil {
			return
		}
		duration := time.Since(start).Milliseconds()
		waitForEnvdDurationHistogram.Record(ctx, duration, metric.WithAttributes(
			telemetry.WithEnvdVersion(s.Config.Envd.Version),
			attribute.Int64("timeout_ms", s.internalConfig.EnvdInitRequestTimeout.Milliseconds()),
		))
		// Update the sandbox as started now
		s.SetStartedAt(time.Now())
	}()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	go func() {
		select {
		// Ensure the syncing takes at most timeout seconds.
		case <-time.After(timeout):
			cancel(fmt.Errorf("syncing took too long"))
		case <-ctx.Done():
			return
		case <-s.process.Exit().Done():
			err := s.process.Exit().Error()

			cancel(fmt.Errorf("vmm process exited prematurely: %w", err))
		}
	}()

	if err := s.initEnvd(ctx); err != nil {
		return fmt.Errorf("failed to init new envd: %w", err)
	}

	telemetry.ReportEvent(ctx, fmt.Sprintf("[sandbox %s]: initialized new envd", s.Metadata.Runtime.SandboxID))

	return nil
}

func (f *Factory) GetEnvdInitRequestTimeout(ctx context.Context) time.Duration {
	envdInitRequestTimeoutMs := f.featureFlags.IntFlag(ctx, featureflags.EnvdInitTimeoutMilliseconds)

	return time.Duration(envdInitRequestTimeoutMs) * time.Millisecond
}
