package main

import (
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/cri-multiplex/pkg/engine"
	"github.com/cri-multiplex/pkg/server"
)

const (
	defaultSocketPath          = "/run/cri-multiplex.sock"
	defaultContainerdSocket    = "/run/containerd/containerd.sock"
	defaultOrchestratorAddress = "localhost:5008"
	defaultE2BBackend          = "grpc"
)

// autoNodeIP 返回本机第一个非 lo 的 IPv4 地址，用于自动填充 --node-ip
func autoNodeIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
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
	e2bAPIURL := flag.String("e2b-api-url", "", "E2B API base URL (for rest backend)")
	e2bAPIKey := flag.String("e2b-api-key", "", "E2B API key (for rest backend)")
	nodeIP := flag.String("node-ip", "", "Node IP for host network mode (auto-detected if empty)")
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

	log.Printf("starting cri-multiplex on %s (containerd: %s, e2b backend: %s, node-ip: %s)",
		*socketPath, *containerdSocket, *e2bBackend, cfg.NodeIP)
	if err := mux.Start(*socketPath); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

