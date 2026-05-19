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
	defaultSocketPath       = "/run/cri-multiplex.sock"
	defaultContainerdSocket = "/run/containerd/containerd.sock"
)

func main() {
	socketPath := flag.String("socket", defaultSocketPath, "Unix socket path for cri-multiplex")
	containerdSocket := flag.String("containerd-socket", defaultContainerdSocket, "Unix socket path for containerd")
	flag.Parse()

	containerEng := engine.NewContainerEngine(*containerdSocket)
	defer containerEng.Close()

	e2bEng := engine.NewE2BEngine()

	mux := server.NewMuxServer(containerEng, e2bEng)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("received shutdown signal, stopping...")
		mux.Stop()
	}()

	log.Printf("starting cri-multiplex on %s (containerd: %s)", *socketPath, *containerdSocket)
	if err := mux.Start(*socketPath); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}