package hostservice

import (
	"context"
	"fmt"
	"sync"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/cfg"
)

// Android's bootconfig in the reusable snapshot contains fixed host vsock
// ports for config_server (6800) and modem_simulator (9600). AF_VSOCK is not
// network-namespace scoped, so these listeners must be owned once per host,
// not once per sandbox. Both upstream daemons accept multiple guest clients.
var sharedAndroidServices struct {
	sync.Mutex
	manager *Manager
}

// EnsureSharedAndroidServices starts the host-wide services needed by every
// restored Android sandbox. Their context is detached by Manager.StartAll and
// they intentionally live until the orchestrator process exits.
func EnsureSharedAndroidServices(
	ctx context.Context,
	config cfg.BuilderConfig,
	cuttlefishConfigPath string,
) error {
	sharedAndroidServices.Lock()
	defer sharedAndroidServices.Unlock()

	if sharedAndroidServices.manager != nil {
		return nil
	}

	configServer, err := BuildConfigServerService(config, VsockCIDBase, "host-shared", cuttlefishConfigPath)
	if err != nil {
		return fmt.Errorf("build shared config_server: %w", err)
	}
	modem, err := BuildModemSimulatorService(config, VsockCIDBase, "host-shared", cuttlefishConfigPath)
	if err != nil {
		for _, f := range configServer.ExtraFiles {
			_ = f.Close()
		}
		return fmt.Errorf("build shared modem_simulator: %w", err)
	}
	operator, err := BuildWebRTCOperatorService(config, cuttlefishConfigPath)
	if err != nil {
		configServer.CloseParentResources()
		modem.CloseParentResources()
		return fmt.Errorf("build shared webrtc_operator: %w", err)
	}

	manager := NewManager([]Service{configServer, modem, operator})
	if err := manager.StartAll(ctx); err != nil {
		return fmt.Errorf("start shared Android services: %w", err)
	}
	sharedAndroidServices.manager = manager
	return nil
}
