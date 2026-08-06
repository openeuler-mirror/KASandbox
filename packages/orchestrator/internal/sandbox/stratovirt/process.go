package stratovirt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapio"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/cfg"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/network"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/rootfs"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/socket"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/template"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/vmm"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	sbxlogger "github.com/e2b-dev/infra/packages/shared/pkg/logger/sandbox"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

var tracer = otel.Tracer("github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/stratovirt")

const (
	windowsUEFIFirmwareFile = "QEMU_EFI-pflash.raw"
	androidBootloaderFile   = "bootloader.qemu"
)

var _ vmm.Process = (*Process)(nil)

var androidDiskNames = []string{storage.RootfsName, storage.PersistentName, storage.SDCardName}

type Process struct {
	Versions Config

	cmd       *exec.Cmd
	qmpSocket string
	qmpClient *qmpClient

	config          cfg.BuilderConfig
	slot            *network.Slot
	rootfsProviders []rootfs.Provider
	rootfsPaths     vmm.RootfsPaths
	rootfsPath      string
	kernelPath      string
	files           *storage.SandboxFiles

	vsockGuestCID int64

	exitOnce *utils.ErrorOnce
	execCtx  context.Context
}

func (p *Process) prepareDiskLinks() error {
	for i, provider := range p.rootfsProviders {
		name := storage.RootfsName
		if i < len(androidDiskNames) {
			name = androidDiskNames[i]
		}
		link := p.files.SandboxCacheDiskLinkPath(p.config.StorageConfig, name)
		if err := utils.SymlinkForce("/dev/null", link); err != nil {
			return fmt.Errorf("initialize %s disk link: %w", name, err)
		}
		path, err := provider.Path()
		if err != nil {
			return fmt.Errorf("get %s disk path: %w", name, err)
		}
		if err := utils.SymlinkForce(path, link); err != nil {
			return fmt.Errorf("symlink %s disk: %w", name, err)
		}
	}
	return nil
}

func (p *Process) SetVsockConfig(cid int64) {
	p.vsockGuestCID = cid
}

func NewProcess(
	ctx context.Context,
	execCtx context.Context,
	config cfg.BuilderConfig,
	slot *network.Slot,
	files *storage.SandboxFiles,
	versions Config,
	rootfsProviders []rootfs.Provider,
	rootfsPaths vmm.RootfsPaths,
) (*Process, error) {
	ctx, childSpan := tracer.Start(ctx, "initialize-stratovirt", trace.WithAttributes(
		attribute.Int("sandbox.slot.index", slot.Idx),
	))
	defer childSpan.End()

	_, err := os.Stat(versions.StratoVirtPath(config))
	if err != nil {
		return nil, fmt.Errorf("error stating stratovirt binary: %w", err)
	}

	if versions.OsType.OrDefault() == vmm.OsWindows {
		_, err := os.Stat(filepath.Join(config.FirmwarePackagesRoot, windowsUEFIFirmwareFile))
		if err != nil {
			return nil, fmt.Errorf("error stating UEFI firmware: %w", err)
		}
	} else if versions.OsType.OrDefault() == vmm.OsAndroid {
		androidBootloaderPath := filepath.Join(config.FirmwareDirForVersion(string(versions.AndroidVersion)), androidBootloaderFile)
		if _, err := os.Stat(androidBootloaderPath); err != nil {
			return nil, fmt.Errorf("error stating Android bootloader %q: %w", androidBootloaderPath, err)
		}
	} else if versions.OsType.OrDefault() == vmm.OsLinux {
		_, err := os.Stat(versions.HostKernelPath(config))
		if err != nil {
			return nil, fmt.Errorf("error stating kernel file: %w", err)
		}
	}

	return &Process{
		Versions:        versions,
		exitOnce:        utils.NewErrorOnce(),
		execCtx:         execCtx,
		config:          config,
		qmpSocket:       files.SandboxFirecrackerSocketPath(),
		qmpClient:       newQMPClient(files.SandboxFirecrackerSocketPath()),
		rootfsProviders: rootfsProviders,
		rootfsPaths:     rootfsPaths,
		files:           files,
		slot:            slot,
	}, nil
}

func (p *Process) Create(
	ctx context.Context,
	metadata sbxlogger.LoggerMetadata,
	vcpuCount int64,
	memoryMB int64,
	hugePages bool,
	options vmm.ProcessOptions,
) error {
	ctx, childSpan := tracer.Start(ctx, "create-stratovirt")
	defer childSpan.End()

	err := p.prepareDiskLinks()
	if err != nil {
		return err
	}
	telemetry.ReportEvent(ctx, "got rootfs path")
	telemetry.ReportEvent(ctx, "symlinked rootfs")

	ipv4 := fmt.Sprintf("%s::%s:%s:instance:%s:off:%s",
		p.slot.NamespaceIP(), p.slot.TapIPString(), p.slot.TapMaskString(),
		p.slot.VpeerName(), p.slot.TapName())

	args := defaultKernelArgs(options.InitScriptPath, ipv4, options)

	kernelArgsStr := args.String()

	builder := NewCommandBuilder(p.config)
	cmdResult := builder.Build(
		p.Versions,
		p.files,
		p.slot,
		kernelArgsStr,
		memoryMB,
		vcpuCount,
		hugePages,
		"",
		p.vsockGuestCID,
	)

	p.rootfsPath = cmdResult.RootfsPath
	p.kernelPath = cmdResult.KernelPath

	cmd := exec.CommandContext(p.execCtx, "unshare", "-m", "--", "bash", "-c", cmdResult.Value)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}

	err = p.startProcess(ctx, metadata, options.Stdout, options.Stderr, cmd)
	if err != nil {
		stopErr := p.Stop(ctx)
		return errors.Join(fmt.Errorf("error starting stratovirt process: %w", err), stopErr)
	}

	telemetry.ReportEvent(ctx, "started stratovirt")

	if err := p.setMmdsConfig(ctx); err != nil {
		_ = p.Stop(ctx)
		return fmt.Errorf("set mmds config: %w", err)
	}

	if err := p.setMmds(ctx, metadata.LoggerMetadata(), nil); err != nil {
		_ = p.Stop(ctx)
		return fmt.Errorf("set mmds: %w", err)
	}

	return nil
}

func (p *Process) Resume(
	ctx context.Context,
	metadata sbxlogger.SandboxMetadata,
	uffdSocketPath string,
	snapfile template.File,
	uffdReady chan struct{},
	accessToken *string,
	memoryMB int64,
	vcpuCount int64,
	hugePages bool,
	cgroupFD int,
	traceID string,
) error {
	ctx, span := tracer.Start(ctx, "resume-stratovirt")
	defer span.End()

	if err := p.prepareDiskLinks(); err != nil {
		return err
	}

	phaseStart := time.Now()
	restoreDir := p.files.SandboxRestoreDir(p.config.StorageConfig)
	if err := os.MkdirAll(restoreDir, 0o755); err != nil {
		return fmt.Errorf("create restore dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(restoreDir) }
	defer cleanup()

	if err := unpackSnapshot(snapfile.Path(), restoreDir); err != nil {
		return fmt.Errorf("unpack stratovirt snapshot: %w", err)
	}
	telemetry.ReportEvent(ctx, "unpacked stratovirt snapshot")
	zap.L().Sugar().Infof("[ResumeSandbox] unpack snapshot cost: %.3f ms, traceID=%s", time.Since(phaseStart).Seconds()*1000, traceID)

	phaseStart = time.Now()
	telemetry.ReportEvent(ctx, "symlinked rootfs")
	zap.L().Sugar().Infof("[ResumeSandbox] get rootfs path cost: %.3f ms, traceID=%s", time.Since(phaseStart).Seconds()*1000, traceID)

	phaseStart = time.Now()
	if err := socket.Wait(ctx, uffdSocketPath); err != nil {
		return fmt.Errorf("error waiting for uffd socket: %w", err)
	}
	telemetry.ReportEvent(ctx, "uffd socket ready")
	zap.L().Sugar().Infof("[ResumeSandbox] get uffd sock path cost: %.3f ms, traceID=%s", time.Since(phaseStart).Seconds()*1000, traceID)

	incomingURI := fmt.Sprintf("file:%s,memory=external,mapped=false,uffd_sock=%s", restoreDir, uffdSocketPath)

	cmdResult := NewCommandBuilder(p.config).Build(
		p.Versions,
		p.files,
		p.slot,
		"",
		memoryMB,
		vcpuCount,
		hugePages,
		incomingURI,
		p.vsockGuestCID,
	)

	cmd := exec.CommandContext(p.execCtx, "unshare", "-m", "--", "bash", "-c", cmdResult.Value)
	sysProcAttr := &syscall.SysProcAttr{Setsid: true}
	if cgroupFD >= 0 {
		sysProcAttr.UseCgroupFD = true
		sysProcAttr.CgroupFD = cgroupFD
	}
	cmd.SysProcAttr = sysProcAttr

	phaseStart = time.Now()
	if err := p.startProcess(ctx, metadata.LoggerMetadata(), nil, nil, cmd); err != nil {
		_ = p.Stop(ctx)
		return fmt.Errorf("error starting stratovirt: %w", err)
	}
	telemetry.ReportEvent(ctx, "configured stratovirt")
	zap.L().Sugar().Infof("[ResumeSandbox] start sv cost: %.3f ms, traceID=%s", time.Since(phaseStart).Seconds()*1000, traceID)

	select {
	case <-ctx.Done():
		_ = p.Stop(ctx)
		return fmt.Errorf("context canceled while waiting for uffd ready: %w", ctx.Err())
	case <-uffdReady:
	}
	telemetry.ReportEvent(ctx, "uffd ready")

	if err := p.setMmdsConfig(ctx); err != nil {
		_ = p.Stop(ctx)
		return fmt.Errorf("set mmds config: %w", err)
	}

	if err := p.setMmds(ctx, metadata.LoggerMetadata(), accessToken); err != nil {
		_ = p.Stop(ctx)
		return fmt.Errorf("set mmds: %w", err)
	}

	telemetry.ReportEvent(ctx, "resumed stratovirt")
	return nil
}

func (p *Process) startProcess(
	ctx context.Context,
	sbxMetadata sbxlogger.LoggerMetadata,
	stdoutExternal io.Writer,
	stderrExternal io.Writer,
	cmd *exec.Cmd,
) error {
	ctx, childSpan := tracer.Start(ctx, "start-stratovirt-process")
	defer childSpan.End()

	stdoutWriter := &zapio.Writer{Log: sbxlogger.I(sbxMetadata).Logger.Detach(ctx), Level: zap.InfoLevel}
	stdoutWriters := []io.Writer{stdoutWriter}
	if stdoutExternal != nil {
		stdoutWriters = append(stdoutWriters, stdoutExternal)
	}
	cmd.Stdout = io.MultiWriter(stdoutWriters...)

	stderrWriter := &zapio.Writer{Log: sbxlogger.I(sbxMetadata).Logger.Detach(ctx), Level: zap.ErrorLevel}
	stderrWriters := []io.Writer{stderrWriter}
	if stderrExternal != nil {
		stderrWriters = append(stderrWriters, stderrExternal)
	}
	cmd.Stderr = io.MultiWriter(stderrWriters...)

	p.cmd = cmd

	err := cmd.Start()
	if err != nil {
		return fmt.Errorf("error starting stratovirt process: %w", err)
	}

	startCtx, cancelStart := context.WithCancelCause(ctx)
	defer cancelStart(fmt.Errorf("stratovirt finished starting"))

	go func() {
		defer stderrWriter.Close()
		defer stdoutWriter.Close()

		waitErr := cmd.Wait()
		if waitErr != nil {
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() &&
					(status.Signal() == syscall.SIGKILL || status.Signal() == syscall.SIGTERM) {
					p.exitOnce.SetError(nil)
					return
				}
			}

			logger.L().Error(ctx, "error waiting for stratovirt process", zap.Error(waitErr))
			errMsg := fmt.Errorf("error waiting for stratovirt process: %w", waitErr)
			p.exitOnce.SetError(errMsg)
			cancelStart(errMsg)
			return
		}

		p.exitOnce.SetError(nil)
	}()

	err = socket.Wait(startCtx, p.qmpSocket)
	if err != nil {
		return fmt.Errorf("error waiting for stratovirt QMP socket: %w", err)
	}

	err = p.qmpClient.connect(ctx)
	if err != nil {
		return fmt.Errorf("error connecting to QMP: %w", err)
	}

	err = p.qmpClient.negotiateCapabilities(ctx)
	if err != nil {
		return fmt.Errorf("error negotiating QMP capabilities: %w", err)
	}

	telemetry.ReportEvent(ctx, "QMP connected and negotiated")

	return nil
}

func (p *Process) Stop(ctx context.Context) error {
	if p.cmd == nil || p.cmd.Process == nil {
		return fmt.Errorf("stratovirt process not started")
	}

	select {
	case <-p.exitOnce.Done():
		logger.L().Info(ctx, "stratovirt process already exited", logger.WithSandboxID(p.files.SandboxID))
		return nil
	default:
	}

	ctx = context.WithoutCancel(ctx)

	state, err := vmm.ProcessState(ctx, p.cmd.Process.Pid)
	if err != nil {
		logger.L().Warn(ctx, "failed to get stratovirt process state", zap.Error(err), logger.WithSandboxID(p.files.SandboxID))
	} else if state == "D" {
		logger.L().Info(ctx, "stratovirt process is in the D state before we call SIGTERM", logger.WithSandboxID(p.files.SandboxID))
	}

	err = p.qmpClient.quitVM(ctx)
	if err != nil {
		logger.L().Warn(ctx, "QMP quit failed, falling back to SIGTERM", zap.Error(err))

		err = p.cmd.Process.Signal(syscall.SIGTERM)
		if errors.Is(err, os.ErrProcessDone) {
			logger.L().Info(ctx, "stratovirt process already exited", logger.WithSandboxID(p.files.SandboxID))

			return nil
		}
		if err != nil {
			logger.L().Warn(ctx, "failed to send SIGTERM to stratovirt process", zap.Error(err))
		}
	}

	go func() {
		select {
		case <-time.After(10 * time.Second):
			err := p.cmd.Process.Kill()
			if err != nil {
				logger.L().Warn(ctx, "failed to send SIGKILL to stratovirt process", zap.Error(err))
			} else {
				logger.L().Info(ctx, "sent SIGKILL to stratovirt process because it was not responding to SIGTERM for 10 seconds", logger.WithSandboxID(p.files.SandboxID))
			}

			state, err := vmm.ProcessState(ctx, p.cmd.Process.Pid)
			if err != nil {
				logger.L().Warn(ctx, "failed to get stratovirt process state after sending SIGKILL", zap.Error(err), logger.WithSandboxID(p.files.SandboxID))
			} else if state == "D" {
				logger.L().Info(ctx, "stratovirt process is in the D state after we call SIGKILL", logger.WithSandboxID(p.files.SandboxID))
			}
		case <-p.exitOnce.Done():
			p.qmpClient.close()
			return
		}
	}()

	return nil
}

func (p *Process) Pause(ctx context.Context) error {
	ctx, childSpan := tracer.Start(ctx, "pause-stratovirt")
	defer childSpan.End()

	return p.qmpClient.stopVM(ctx)
}

// CreateSnapshot creates the VM state snapshot. Guest memory is exported
// separately through ExportMemory so Firecracker and StratoVirt share the
// existing incremental snapshot pipeline.
func (p *Process) CreateSnapshot(ctx context.Context, snapfilePath string) error {
	ctx, childSpan := tracer.Start(ctx, "create-snapshot-stratovirt")
	defer childSpan.End()

	dir, err := os.MkdirTemp(filepath.Dir(snapfilePath), ".stratovirt-snapshot-*")
	if err != nil {
		return fmt.Errorf("create stratovirt snapshot dir: %w", err)
	}
	defer os.RemoveAll(dir)

	if err := p.createSnapshotFromDir(ctx, dir); err != nil {
		return err
	}
	if err := packSnapshot(dir, snapfilePath); err != nil {
		return fmt.Errorf("pack stratovirt snapshot: %w", err)
	}

	return nil
}

func (p *Process) createSnapshotFromDir(ctx context.Context, dir string) error {
	ctx, childSpan := tracer.Start(ctx, "create-snapshot-stratovirt-dir")
	defer childSpan.End()

	if dir == "" {
		return fmt.Errorf("stratovirt create snapshot: dir is empty")
	}

	if err := p.qmpClient.migrate(ctx, "file:"+dir+",memory=external"); err != nil {
		return fmt.Errorf("stratovirt migrate (snapshot save) failed: %w", err)
	}
	return nil
}

func (p *Process) Pid() (int, error) {
	if p.cmd == nil || p.cmd.Process == nil {
		return 0, fmt.Errorf("stratovirt process not started")
	}

	return p.cmd.Process.Pid, nil
}

func (p *Process) Exit() *utils.ErrorOnce {
	return p.exitOnce
}
