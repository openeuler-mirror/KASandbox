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

	SandboxID      string
	SandboxDir     string
	NetNSName      string
	MobileTap      string
	AndroidVersion string
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

	baseConfigPath := filepath.Join(params.Config.CvdHostPackageDirForVersion(params.AndroidVersion), "cuttlefish", "assembly", "cuttlefish_config.json")
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

	// secure_env is required on Android 15+. BuildSecureEnvService creates
	// the virtio-serial FIFO pair endpoints internally and owns their full
	// lifecycle (Manager closes them at StopAll/startup-rollback via
	// CloseParentResources). See secure_env.go for the FIFO/secure_env
	// protocol details.
	secureEnvEnabled := vmm.AndroidVersion(params.AndroidVersion).RequiresSecureEnv()

	// Version-gate config_server so a missing binary on a version that
	// requires it fails the build instead of failing at runtime.
	configServerEnabled := vmm.AndroidVersion(params.AndroidVersion).RequiresConfigServer()
	var configServer Service
	if configServerEnabled {
		configServer, err = BuildConfigServerService(params.Config, params.AndroidVersion, runtimeConfigPath, params.NetNSName, configListener)
		if err != nil {
			return nil, fmt.Errorf("build config_server service: %w", err)
		}
	} else {
		logger.L().Debug(ctx, "skipping config_server; runtime config reaches guest through a different channel",
			zap.String("sandbox_id", params.SandboxID),
			zap.String("android_version", params.AndroidVersion),
		)
		_ = configListener.Close()
	}
	modem, err := BuildModemSimulatorService(params.Config, params.AndroidVersion, runtimeConfigPath, params.NetNSName, modemListener)
	if err != nil {
		return nil, err
	}
	adbProxy, err := BuildVsockProxyService(params.Config, params.AndroidVersion, allocatedCID, params.SandboxID, runtimeConfigPath, params.NetNSName, adbListener)
	if err != nil {
		return nil, err
	}

	serviceList := []Service{}
	if configServerEnabled {
		serviceList = append(serviceList, configServer)
	}
	serviceList = append(serviceList, modem, adbProxy)
	if secureEnvEnabled {
		secureEnv, buildErr := BuildSecureEnvService(params.Config, params.AndroidVersion, runtimeConfigPath, params.NetNSName, params.SandboxDir)
		if buildErr != nil {
			return nil, buildErr
		}
		serviceList = append(serviceList, secureEnv)
	}

	manager := NewManager(serviceList, params.Config.ReadyCheckTimeout)
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
