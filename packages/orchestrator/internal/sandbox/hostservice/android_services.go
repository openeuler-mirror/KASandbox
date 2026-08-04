package hostservice

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/cfg"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/vmm"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

type cleanupRegistrar interface {
	Add(context.Context, func(context.Context) error)
	AddPriority(context.Context, func(context.Context) error)
}

type vsockConfigurer interface {
	SetVsockConfig(int64)
}

type AndroidServicesParams struct {
	Config  cfg.BuilderConfig
	CIDPool *CIDPool
	Process vmm.Process
	Cleanup cleanupRegistrar
	Mux     *VsockMux

	SandboxID  string
	SandboxDir string
	NetNSName  string
	MobileTap  string
}

type AndroidServices struct {
	ADBAddress string

	cid     uint32
	manager *Manager
	mux     *VsockMux

	configListener *UnixListener
	modemListener  *UnixListener
	sandboxDir     string
	cidReleasable  *atomic.Bool

	stopOnce sync.Once
	stopErr  error
}

func StartAndroidServices(ctx context.Context, params AndroidServicesParams) (_ *AndroidServices, err error) {
	if params.CIDPool == nil {
		return nil, fmt.Errorf("Android host services require a CID pool")
	}
	if params.Mux == nil {
		return nil, fmt.Errorf("Android host services require a vsock mux")
	}
	if params.Cleanup == nil {
		return nil, fmt.Errorf("Android host services require cleanup registration")
	}
	if params.NetNSName == "" {
		return nil, fmt.Errorf("Android host services require a named network namespace")
	}

	allocatedCID, err := params.CIDPool.Allocate()
	if err != nil {
		return nil, fmt.Errorf("allocate vsock CID: %w", err)
	}
	cidReleasable := &atomic.Bool{}
	cidReleasable.Store(true)
	params.Cleanup.AddPriority(ctx, func(cleanupCtx context.Context) error {
		if !cidReleasable.Load() {
			logger.L().Error(cleanupCtx, "withholding CID release while vsock connections are still active",
				zap.Int64("guest_cid", allocatedCID),
			)
			return fmt.Errorf("CID %d still has active vsock connections", allocatedCID)
		}
		params.CIDPool.Release(allocatedCID)
		return nil
	})

	if configurable, ok := params.Process.(vsockConfigurer); ok {
		configurable.SetVsockConfig(allocatedCID)
	}
	if err := params.Mux.Start(ctx); err != nil {
		return nil, fmt.Errorf("start global Android vsock mux: %w", err)
	}

	adbListener, adbAddress, err := NewADBListener()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = adbListener.Close()
		}
	}()

	baseConfigPath := filepath.Join(params.Config.CvdHostPackageDir, "cuttlefish", "assembly", "cuttlefish_config.json")
	runtimeConfigPath := filepath.Join(params.SandboxDir, "cuttlefish_config.json")
	adbPort := 0
	if _, scanErr := fmt.Sscanf(adbAddress, "127.0.0.1:%d", &adbPort); scanErr != nil {
		return nil, fmt.Errorf("parse allocated ADB address %s: %w", adbAddress, scanErr)
	}
	if err := WriteSandboxCuttlefishConfig(baseConfigPath, runtimeConfigPath, AndroidRuntimeConfig{
		SandboxDir:  params.SandboxDir,
		CID:         allocatedCID,
		ADBHostPort: adbPort,
		MobileTap:   params.MobileTap,
	}); err != nil {
		return nil, err
	}

	configListener, err := NewUnixListener(filepath.Join(params.SandboxDir, "config-server.sock"))
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = configListener.Close()
		}
	}()
	modemListener, err := NewUnixListener(filepath.Join(params.SandboxDir, "modem-simulator.sock"))
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = modemListener.Close()
		}
	}()

	configServer, err := BuildConfigServerService(params.Config, runtimeConfigPath, params.NetNSName, configListener)
	if err != nil {
		return nil, err
	}
	modem, err := BuildModemSimulatorService(params.Config, runtimeConfigPath, params.NetNSName, modemListener)
	if err != nil {
		return nil, err
	}
	adbProxy, err := BuildVsockProxyService(params.Config, allocatedCID, params.SandboxID, runtimeConfigPath, adbListener)
	if err != nil {
		return nil, err
	}

	manager := NewManager([]Service{configServer, modem, adbProxy}, params.Config.ReadyCheckTimeout)
	if err := manager.StartAll(ctx); err != nil {
		return nil, fmt.Errorf("start Android host services: %w", err)
	}

	services := &AndroidServices{
		ADBAddress:     adbAddress,
		cid:            uint32(allocatedCID),
		manager:        manager,
		mux:            params.Mux,
		configListener: configListener,
		modemListener:  modemListener,
		sandboxDir:     params.SandboxDir,
		cidReleasable:  cidReleasable,
	}
	if err := params.Mux.Register(SandboxRoute{
		CID:               uint32(allocatedCID),
		SandboxID:         params.SandboxID,
		ConfigBackendPath: configListener.Path,
		ModemBackendPath:  modemListener.Path,
	}); err != nil {
		stopErr := services.Stop(ctx)
		return nil, errors.Join(fmt.Errorf("register Android vsock route: %w", err), stopErr)
	}
	cidReleasable.Store(false)
	params.Cleanup.AddPriority(ctx, services.Stop)

	logger.L().Info(ctx, "Android host services configured",
		zap.Int64("cid", allocatedCID),
		zap.String("adb_address", adbAddress),
		zap.String("sandbox_id", params.SandboxID),
	)
	return services, nil
}

func (s *AndroidServices) WaitForModemConnection(ctx context.Context) error {
	if s == nil {
		return nil
	}
	return s.mux.WaitForConnection(ctx, s.cid, ModemSimulatorVsockPort)
}

func (s *AndroidServices) Stop(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.stopOnce.Do(func() {
		var errs []error
		if err := s.mux.Unregister(ctx, s.cid); err != nil {
			errs = append(errs, fmt.Errorf("unregister Android vsock route: %w", err))
		} else if s.cidReleasable != nil {
			s.cidReleasable.Store(true)
		}
		if err := s.manager.StopAll(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop Android host processes: %w", err))
		}
		if err := s.configListener.Close(); err != nil {
			errs = append(errs, err)
		}
		if err := s.modemListener.Close(); err != nil {
			errs = append(errs, err)
		}
		if err := os.RemoveAll(s.sandboxDir); err != nil {
			errs = append(errs, fmt.Errorf("remove Android sandbox host directory %s: %w", s.sandboxDir, err))
		}
		s.stopErr = errors.Join(errs...)
	})
	return s.stopErr
}
