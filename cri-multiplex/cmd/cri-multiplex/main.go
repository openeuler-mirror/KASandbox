package main

import (
	"flag"
	"log"
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

func main() {
	socketPath := flag.String("socket", defaultSocketPath, "Unix socket path for cri-multiplex")
	containerdSocket := flag.String("containerd-socket", defaultContainerdSocket, "Unix socket path for containerd")
	e2bBackend := flag.String("e2b-backend", defaultE2BBackend, "E2B backend: grpc or rest")
	orchestratorAddress := flag.String("orchestrator-address", defaultOrchestratorAddress, "E2B orchestrator gRPC address (for grpc backend)")
	e2bAPIURL := flag.String("e2b-api-url", "", "E2B API base URL (for rest backend)")
	e2bAPIKey := flag.String("e2b-api-key", "", "E2B API key (for rest backend)")
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

	log.Printf("starting cri-multiplex on %s (containerd: %s, e2b backend: %s)", *socketPath, *containerdSocket, *e2bBackend)
	if err := mux.Start(*socketPath); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}
