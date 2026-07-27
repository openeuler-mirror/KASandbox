package sandbox

import (
	"context"
	"path/filepath"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/cfg"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/hostservice"
)

// buildAndroidHostServices attaches per-sandbox ADB and WebRTC processes to
// the host-wide config_server, modem_simulator and signaling operator.
func buildAndroidHostServices(
	ctx context.Context,
	config cfg.BuilderConfig,
	cid int64,
	sandboxID string,
	sandboxDir string,
) ([]hostservice.Service, hostservice.Ports, error) {
	// The ranchu composer treats the device config as mandatory and crashes if
	// it cannot fetch it. Use the matching Android 14 host package profile while
	// the remaining optional companion services are brought up independently.
	configPath := filepath.Join(config.CvdHostPackageDir, "cuttlefish", "assembly", "cuttlefish_config.json")
	if err := hostservice.EnsureSharedAndroidServices(ctx, config, configPath); err != nil {
		return nil, hostservice.Ports{}, err
	}

	adbProxy, adbPort, err := hostservice.BuildVsockProxyService(config, cid, sandboxID)
	if err != nil {
		return nil, hostservice.Ports{}, err
	}

	runtimeConfigPath := filepath.Join(sandboxDir, "cuttlefish_config.json")
	assetsDir := filepath.Join(config.CvdHostPackageDir, "usr", "share", "webrtc", "assets")
	streamingPort, err := hostservice.WriteRuntimeCuttlefishConfig(
		configPath,
		runtimeConfigPath,
		sandboxID,
		cid,
		adbPort,
		assetsDir,
	)
	if err != nil {
		adbProxy.CloseParentResources()
		return nil, hostservice.Ports{}, err
	}

	webrtc, err := hostservice.BuildWebRTCService(config, sandboxID, sandboxDir, runtimeConfigPath, streamingPort)
	if err != nil {
		adbProxy.CloseParentResources()
		return nil, hostservice.Ports{}, err
	}

	return []hostservice.Service{adbProxy, webrtc}, hostservice.Ports{
		AdbPort:             adbPort,
		ModemSimulatorPort:  hostservice.ModemSimulatorVsockPort(hostservice.VsockCIDBase),
		WebrtcHttpPort:      hostservice.WebRTCOperatorPort,
		WebrtcStreamingPort: streamingPort,
	}, nil
}
