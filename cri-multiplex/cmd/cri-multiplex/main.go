package main

import (
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cri-multiplex/pkg/engine"
	"github.com/cri-multiplex/pkg/server"
)

const (
	defaultSocketPath            = "/run/cri-multiplex.sock"
	defaultContainerdSocket      = "/run/containerd/containerd.sock"
	defaultOrchestratorAddress   = "localhost:5008"
	defaultOrchestratorProxyAddr = "localhost:5007"
	defaultE2BBackend            = "grpc"
)

// autoNodeIP 返回本机第一个非 lo 的 IPv4 地址，用于自动填充 --node-ip
func autoNodeIP() string {
	// 虚拟网卡前缀黑名单
	virtualPrefixes := []string{"veth", "docker", "br-", "tun", "virbr", "vnet", "flannel", "cali", "cni", "kube"}

	interfaces, err := net.Interfaces()
	if err != nil {
		return ""
	}

	for _, iface := range interfaces {
		// 跳过 down 状态和回环
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		// 跳过虚拟网卡
		skip := false
		for _, prefix := range virtualPrefixes {
			if strings.HasPrefix(iface.Name, prefix) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok {
				if ip := ipnet.IP.To4(); ip != nil {
					return ip.String()
				}
			}
		}
	}
	return ""
}

func main() {
	socketPath := flag.String("socket", defaultSocketPath, "Unix socket path for cri-multiplex")
	containerdSocket := flag.String("containerd-socket", defaultContainerdSocket, "Unix socket path for containerd")
	e2bBackend := flag.String("e2b-backend", defaultE2BBackend, "E2B backend: grpc or rest")
	orchestratorAddress := flag.String("orchestrator-address", defaultOrchestratorAddress, "E2B orchestrator gRPC address (for grpc backend)")
	orchestratorProxyAddr := flag.String("orchestrator-proxy-address", defaultOrchestratorProxyAddr, "E2B orchestrator HTTP proxy address (for envd interaction)")
	e2bAPIURL := flag.String("e2b-api-url", "", "E2B API base URL (for rest backend)")
	e2bAPIKey := flag.String("e2b-api-key", "", "E2B API key (for rest backend)")
	nodeIP := flag.String("node-ip", "", "Node IP for host network mode (auto-detected if empty)")
	e2bCNIEnabled := flag.Bool("e2b-cni-enabled", false, "Enable CNI networking for E2B pod sandboxes")
	cniConfDir := flag.String("cni-conf-dir", "/etc/cni/net.d", "CNI configuration directory")
	cniBinDir := flag.String("cni-bin-dir", "/opt/cni/bin", "CNI plugin binary directory")
	cniIfName := flag.String("cni-ifname", "eth0", "CNI interface name inside the pod netns")
	cniNetNSDir := flag.String("cni-netns-dir", "/var/run/netns", "Directory for named CNI network namespaces")
	androidEnabled := flag.Bool("android-enabled", false, "Enable Android Cuttlefish runtime")
	androidArtifactsDir := flag.String("android-artifacts-dir", "/home/fjq/cf17", "Android Cuttlefish artifacts directory")
	androidNodeIP := flag.String("android-node-ip", "", "Node IP for Android ADB/WebRTC access (auto-detected if empty)")
	androidADBPortStart := flag.Int("android-adb-port-start", 6520, "Android ADB host port start")
	androidBaseInstanceNumStart := flag.Int("android-base-instance-num-start", 1, "Android Cuttlefish base_instance_num start")
	androidWebRTCPortStart := flag.Int("android-webrtc-port-start", 0, "Android WebRTC host port start (0 disables allocation)")
	androidLaunchTimeout := flag.Duration("android-launch-timeout", 180*time.Second, "Android launch readiness timeout")
	androidStateDir := flag.String("android-state-dir", "/var/lib/cri-multiplex/android", "Android runtime state directory")
	androidCVDGroup := flag.String("android-cvd-group", "cvdnetwork", "Supplementary group for Android Cuttlefish commands")
	flag.Parse()

	containerEng := engine.NewContainerEngine(*containerdSocket)
	defer containerEng.Close()

	cfg := &engine.E2BConfig{}
	switch *e2bBackend {
	case "rest":
		cfg.Backend = engine.BackendREST
		cfg.APIBaseURL = *e2bAPIURL
		cfg.APIKey = *e2bAPIKey
	default:
		cfg.Backend = engine.BackendGRPC
		cfg.OrchestratorAddr = *orchestratorAddress
		cfg.OrchestratorProxyAddr = *orchestratorProxyAddr
		// grpc 后端需要 node-ip 用于 PodSandboxStatus 网络状态报告
		if *nodeIP == "" {
			*nodeIP = autoNodeIP()
			if *nodeIP == "" {
				log.Fatal("--node-ip is required (or auto-detection failed). " +
					"Example: --node-ip=$(hostname -I | awk '{print $1}')")
			}
			log.Printf("auto-detected node-ip: %s", *nodeIP)
		}
		cfg.NodeIP = *nodeIP
		cfg.CNI = engine.CNIConfig{
			Enabled:  *e2bCNIEnabled,
			ConfDir:  *cniConfDir,
			BinDir:   *cniBinDir,
			IfName:   *cniIfName,
			NetNSDir: *cniNetNSDir,
		}
	}
	e2bEng := engine.NewE2BEngine(cfg)
	defer e2bEng.Close()

	if *androidEnabled && *androidNodeIP == "" {
		*androidNodeIP = autoNodeIP()
		if *androidNodeIP == "" {
			log.Fatal("--android-node-ip is required when --android-enabled is set (auto-detection failed)")
		}
		log.Printf("auto-detected android-node-ip: %s", *androidNodeIP)
	}
	androidEng := engine.NewAndroidEngine(engine.AndroidConfig{
		Enabled:              *androidEnabled,
		ArtifactsDir:         *androidArtifactsDir,
		NodeIP:               *androidNodeIP,
		ADBPortStart:         *androidADBPortStart,
		BaseInstanceNumStart: *androidBaseInstanceNumStart,
		WebRTCPortStart:      *androidWebRTCPortStart,
		LaunchTimeout:        *androidLaunchTimeout,
		StateDir:             *androidStateDir,
		CVDGroup:             *androidCVDGroup,
	})
	defer androidEng.Close()

	mux := server.NewMuxServer(containerEng, e2bEng, androidEng)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("received shutdown signal, stopping...")
		mux.Stop()
	}()

	log.Printf("starting cri-multiplex on %s (containerd: %s, e2b backend: %s, node-ip: %s, proxy: %s, android-enabled: %v, android-node-ip: %s)",
		*socketPath, *containerdSocket, *e2bBackend, cfg.NodeIP, cfg.OrchestratorProxyAddr, *androidEnabled, *androidNodeIP)
	if err := mux.Start(*socketPath); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}
