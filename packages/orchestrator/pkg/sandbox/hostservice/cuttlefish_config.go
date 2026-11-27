package hostservice

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type AndroidRuntimeConfig struct {
	SandboxDir  string
	CID         int64
	ADBHostPort int
	MobileTap   string
}

func WriteSandboxCuttlefishConfig(basePath, outputPath string, runtime AndroidRuntimeConfig) error {
	data, err := os.ReadFile(basePath)
	if err != nil {
		return fmt.Errorf("read base cuttlefish config %s: %w", basePath, err)
	}

	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("parse base cuttlefish config %s: %w", basePath, err)
	}
	instances, ok := config["instances"].(map[string]any)
	if !ok {
		return fmt.Errorf("base cuttlefish config must contain an instances object")
	}
	if len(instances) != 1 {
		return fmt.Errorf("base cuttlefish config must contain only instances[\"1\"]")
	}
	instance, ok := instances["1"].(map[string]any)
	if !ok {
		return fmt.Errorf("base cuttlefish config must contain instances[\"1\"]")
	}
	if runtime.SandboxDir == "" {
		return fmt.Errorf("sandbox host directory is empty")
	}

	config["root_dir"] = runtime.SandboxDir
	instance["vsock_guest_cid"] = runtime.CID
	instance["adb_host_port"] = runtime.ADBHostPort
	instance["adb_ip_and_port"] = fmt.Sprintf("127.0.0.1:%d", runtime.ADBHostPort)
	instance["config_server_port"] = ConfigServerVsockPort
	instance["modem_simulator_ports"] = fmt.Sprintf("%d", ModemSimulatorVsockPort)
	instance["modem_simulator_host_id"] = 1000 + (runtime.CID - VsockCIDBase)
	instance["modem_simulator_instance_number"] = 1
	instance["modem_simulator_sim_type"] = 1
	instance["enable_modem_simulator"] = true
	instance["mobile_bridge_name"] = ""
	instance["mobile_tap_name"] = runtime.MobileTap

	for _, dir := range []string{
		runtime.SandboxDir,
		filepath.Join(runtime.SandboxDir, "instances"),
		filepath.Join(runtime.SandboxDir, "instances", "cvd-1"),
		filepath.Join(runtime.SandboxDir, "instances", "cvd-1", "logs"),
		filepath.Join(runtime.SandboxDir, "instances", "cvd-1", "internal"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create Android runtime directory %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("set Android runtime directory mode %s: %w", dir, err)
		}
	}

	output, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("encode sandbox cuttlefish config: %w", err)
	}
	if err := os.WriteFile(outputPath, output, 0o600); err != nil {
		return fmt.Errorf("write sandbox cuttlefish config %s: %w", outputPath, err)
	}
	if err := os.Chmod(outputPath, 0o600); err != nil {
		return fmt.Errorf("set sandbox cuttlefish config mode %s: %w", outputPath, err)
	}
	return nil
}
