package main

import (
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

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

	mux := server.NewMuxServer(containerEng, e2bEng)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("received shutdown signal, stopping...")
		mux.Stop()
	}()

	log.Printf("starting cri-multiplex on %s (containerd: %s, e2b backend: %s, node-ip: %s, proxy: %s)",
		*socketPath, *containerdSocket, *e2bBackend, cfg.NodeIP, cfg.OrchestratorProxyAddr)
	if err := mux.Start(*socketPath); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}
