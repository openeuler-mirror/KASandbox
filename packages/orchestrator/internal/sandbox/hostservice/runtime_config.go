package hostservice

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

const (
	WebRTCOperatorPort = 8443
	webrtcMediaBase    = 20000
	webrtcMediaWidth   = 32
)

// WebRTCMediaRange assigns a non-overlapping ICE range from the already
// unique guest CID. TCP and UDP may safely use the same numeric range.
func WebRTCMediaRange(cid int64) (int, int, error) {
	index := int(cid - VsockCIDBase)
	start := webrtcMediaBase + index*webrtcMediaWidth
	end := start + webrtcMediaWidth - 1
	if index < 0 || end > 65535 {
		return 0, 0, fmt.Errorf("CID %d cannot be mapped to a WebRTC media range", cid)
	}
	return start, end, nil
}

// WriteRuntimeCuttlefishConfig clones the version-matched host profile and
// changes only values that must be unique for a restored sandbox. Keeping all
// unknown fields is important because the Android 14 C++ binaries consume a
// much larger schema than the Go orchestration layer.
func WriteRuntimeCuttlefishConfig(
	profilePath string,
	outputPath string,
	sandboxID string,
	cid int64,
	adbPort int,
	assetsDir string,
) (int, error) {
	data, err := os.ReadFile(profilePath)
	if err != nil {
		return 0, fmt.Errorf("read Android host profile %s: %w", profilePath, err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return 0, fmt.Errorf("parse Android host profile %s: %w", profilePath, err)
	}
	instances, ok := root["instances"].(map[string]any)
	if !ok || len(instances) == 0 {
		return 0, fmt.Errorf("Android host profile %s has no instances", profilePath)
	}
	keys := make([]string, 0, len(instances))
	for key := range instances {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	instance, ok := instances[keys[0]].(map[string]any)
	if !ok {
		return 0, fmt.Errorf("Android host profile instance %s is invalid", keys[0])
	}

	mediaStart, mediaEnd, err := WebRTCMediaRange(cid)
	if err != nil {
		return 0, err
	}
	instance["vsock_guest_cid"] = cid
	instance["adb_host_port"] = adbPort
	instance["webrtc_device_id"] = sandboxID
	instance["group_id"] = sandboxID
	instance["webrtc_assets_dir"] = assetsDir
	instance["webrtc_tcp_port_range"] = []int{mediaStart, mediaEnd}
	instance["webrtc_udp_port_range"] = []int{mediaStart, mediaEnd}
	root["webrtc_sig_server_addr"] = "127.0.0.1"
	root["webrtc_sig_server_port"] = WebRTCOperatorPort

	encoded, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("encode runtime Cuttlefish config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return 0, fmt.Errorf("create runtime config directory: %w", err)
	}
	if err := os.WriteFile(outputPath, encoded, 0o600); err != nil {
		return 0, fmt.Errorf("write runtime Cuttlefish config %s: %w", outputPath, err)
	}
	return mediaStart, nil
}
