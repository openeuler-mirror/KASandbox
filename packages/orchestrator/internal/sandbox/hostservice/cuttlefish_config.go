package hostservice

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// CuttlefishConfig is the JSON-decoded subset of cuttlefish_config.json that
// the host-service package needs. Only the first instance is ever read
// (one sandbox = one CVD instance). Unknown fields are silently ignored.
type CuttlefishConfig struct {
	Instances          map[string]CuttlefishInstance `json:"instances"`
	SigServerProxyPort int                           `json:"sig_server_proxy_port,omitempty"`
}

// CuttlefishInstance models the per-instance fields we consume.
type CuttlefishInstance struct {
	VsockGuestCID        int    `json:"vsock_guest_cid"`
	ADBHostPort          int    `json:"adb_host_port"`
	FastbootHostPort     int    `json:"fastboot_host_port,omitempty"`
	ModemSimulatorPorts  string `json:"modem_simulator_ports,omitempty"`   // "9600,9601,..." or ""
	ModemSimulatorHostID int    `json:"modem_simulator_host_id,omitempty"` // 4-digit ID
	WebrtcDeviceID       string `json:"webrtc_device_id,omitempty"`
	WebrtcAssetsDir      string `json:"webrtc_assets_dir,omitempty"`
	WebrtcTcpPortRange   [2]int `json:"webrtc_tcp_port_range,omitempty"` // [start, end] inclusive
	WebrtcUdpPortRange   [2]int `json:"webrtc_udp_port_range,omitempty"` // [start, end] inclusive
}

// LoadCuttlefishConfig parses the cuttlefish_config.json at the given path.
func LoadCuttlefishConfig(path string) (*CuttlefishConfig, error) {
	if path == "" {
		return nil, fmt.Errorf("cuttlefish config path is empty")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read cuttlefish config %s: %w", path, err)
	}
	var cfg CuttlefishConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse cuttlefish config %s: %w", path, err)
	}
	if len(cfg.Instances) == 0 {
		return nil, fmt.Errorf("cuttlefish config %s has no instances", path)
	}
	return &cfg, nil
}

// FirstInstance returns the first instance from the config. For a
// single-instance config (the only supported case) this is the right one.
func (c *CuttlefishConfig) FirstInstance() (CuttlefishInstance, string, error) {
	for name, inst := range c.Instances {
		return inst, name, nil
	}
	return CuttlefishInstance{}, "", fmt.Errorf("cuttlefish config has no instances")
}

// ParsePortRange parses a "start-end" string. Currently unused but kept for
// future use if we need to parse the string form from CLI flags.
func ParsePortRange(s string) (int, int, error) {
	var start, end int
	if _, err := fmt.Sscanf(s, "%d-%d", &start, &end); err != nil {
		return 0, 0, fmt.Errorf("parse port range %q: %w", s, err)
	}
	return start, end, nil
}

// SynthesizeCuttlefishConfig writes a minimal cuttlefish_config.json that the
// four CVD host services (config_server, modem_simulator, webrtc_bin,
// socket_vsock_proxy) consume via CUTTLEFISH_CONFIG_FILE env var. It is the
// e2b equivalent of running assemble_cvd, but skips disk assembly because
// the direct-boot Stratovirt path already wires up os / persistent / sdcard
// via the existing template.Disks() flow.
//
// Per-sandbox values baked into the config:
//   - vsock_guest_cid       = cid (matches -device vhost-vsock-pci,guest-cid=<cid>)
//   - adb_host_port         = adbPort (host TCP port allocated for socket_vsock_proxy)
//   - modem_simulator_ports = "<modemVsockPort>" (single-modem vsock port)
//   - webrtc_tcp_port_range = [webrtcTcpPort, webrtcTcpPort] (single-port range)
//   - webrtc_udp_port_range = [webrtcUdpPort, webrtcUdpPort] (single-port range)
//   - webrtc_device_id      = sandboxID (webrtc_bin identification)
//   - webrtc_assets_dir     = webrtcAssetsDir (icons / device assets)
//
// The instance name is fixed to "sd0" (sandbox device 0) — only the first
// instance is ever read by LoadCuttlefishConfig.FirstInstance().
//
// NOTE: only the subset of fields the orchestrator-side Go code reads is
// written. The webrtc_bin C++ binary may read additional fields from the
// config at runtime (signaling server, ICE, codecs, ...). If webrtc_bin
// fails to start because of a missing field, extend CuttlefishInstance with
// the additional JSON tags and re-run.
func SynthesizeCuttlefishConfig(
	configPath string,
	sandboxID string,
	cid int64,
	adbPort int,
	modemVsockPort int,
	webrtcTcpPort int,
	webrtcUdpPort int,
	webrtcAssetsDir string,
) error {
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return fmt.Errorf("create config dir %s: %w", filepath.Dir(configPath), err)
	}

	cfg := CuttlefishConfig{
		Instances: map[string]CuttlefishInstance{
			"sd0": {
				VsockGuestCID:       int(cid),
				ADBHostPort:         adbPort,
				ModemSimulatorPorts: fmt.Sprintf("%d", modemVsockPort),
				WebrtcDeviceID:      sandboxID,
				WebrtcAssetsDir:     webrtcAssetsDir,
				WebrtcTcpPortRange:  [2]int{webrtcTcpPort, webrtcTcpPort},
				WebrtcUdpPortRange:  [2]int{webrtcUdpPort, webrtcUdpPort},
			},
		},
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cuttlefish config: %w", err)
	}

	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		return fmt.Errorf("write cuttlefish config %s: %w", configPath, err)
	}

	return nil
}
