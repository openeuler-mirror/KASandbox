package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cri-multiplex/pkg/admin"
	"github.com/cri-multiplex/pkg/engine"
	"github.com/cri-multiplex/pkg/server"
)

const (
	defaultSocketPath            = "/run/cri-multiplex.sock"
	defaultContainerdSocket      = "/run/containerd/containerd.sock"
	defaultOrchestratorAddress   = "localhost:5008"
	defaultOrchestratorProxyAddr = "localhost:5007"
	defaultStateDir              = "/var/lib/cri-multiplex/state"
	defaultAdminSocket           = "/run/cri-multiplex/admin.sock"
)

type stateRestorer interface {
	RestoreState(context.Context) error
}

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
	orchestratorAddress := flag.String("orchestrator-address", defaultOrchestratorAddress, "E2B orchestrator gRPC address")
	orchestratorProxyAddr := flag.String("orchestrator-proxy-address", defaultOrchestratorProxyAddr, "E2B orchestrator HTTP proxy address (for envd interaction)")
	nodeIP := flag.String("node-ip", "", "Node IP for host network mode (auto-detected if empty)")
	nodeName := flag.String("node-name", os.Getenv("NODE_NAME"), "Kubernetes node name (defaults to NODE_NAME env)")
	adminSocket := flag.String("admin-socket", defaultAdminSocket, "Unix socket path for the node-local admin service")
	stateDir := flag.String("state-dir", defaultStateDir, "cri-multiplex persistent state directory")
	cniEnabled := flag.Bool("cni-enabled", false, "Enable CNI networking for E2B and Android pod sandboxes")
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
	androidLaunchTimeout := flag.Duration("android-launch-timeout", 30*time.Second, "Android launch readiness timeout")
	androidStateDir := flag.String("android-state-dir", "/var/lib/cri-multiplex/android", "Android runtime state directory")
	androidCNINetNSPrefix := flag.String("android-cni-netns-prefix", "android-", "Android CNI netns name prefix")
	orphanReconcileEnabled := flag.Bool("orphan-reconcile-enabled", true, "Enable orphan resource reconciliation")
	orphanReconcileInterval := flag.Duration("orphan-reconcile-interval", 60*time.Second, "Orphan reconciliation interval")
	orphanGracePeriod := flag.Duration("orphan-grace-period", 120*time.Second, "Grace period before reclaiming orphan resources")
	cleanupMaxRetries := flag.Int("cleanup-max-retries", 10, "Maximum cleanup retry attempts")
	cleanupDryRun := flag.Bool("cleanup-dry-run", false, "Log orphan cleanup actions without deleting resources")
	hideSandboxLabel := flag.String("hide-sandbox-label", "", "Hide E2B sandboxes carrying this label (key=value, e.g. flux-sandbox.io/direct=true) from ListPodSandbox/ListContainers, so kubelet does not garbage-collect them as orphans; empty keeps them visible")
	flag.Parse()

	stateStore, err := engine.NewJSONStateStore(*stateDir)
	if err != nil {
		log.Fatalf("initialize state store: %v", err)
	}

	containerEng := engine.NewContainerEngine(*containerdSocket)
	defer containerEng.Close()

	if *nodeIP == "" {
		*nodeIP = autoNodeIP()
		if *nodeIP == "" {
			log.Fatal("--node-ip is required (or auto-detection failed). " +
				"Example: --node-ip=$(hostname -I | awk '{print $1}')")
		}
		log.Printf("auto-detected node-ip: %s", *nodeIP)
	}
	cfg := &engine.E2BConfig{
		OrchestratorAddr:      *orchestratorAddress,
		OrchestratorProxyAddr: *orchestratorProxyAddr,
		NodeIP:                *nodeIP,
		NodeName:              *nodeName,
		CNI: engine.CNIConfig{
			Enabled:  *cniEnabled,
			ConfDir:  *cniConfDir,
			BinDir:   *cniBinDir,
			IfName:   *cniIfName,
			NetNSDir: *cniNetNSDir,
		},
		StateStore: stateStore,
		HideLabel:  *hideSandboxLabel,
	}
	e2bEng := engine.NewE2BEngine(cfg)
	defer e2bEng.Close()

	// Admin server 是附加管理面（Node Agent 调用），启动失败仅告警，不影响 CRI 主服务。
	if adminEng, ok := e2bEng.(admin.Engine); ok {
		adminSrv := admin.NewServer(*adminSocket, adminEng)
		if err := adminSrv.Start(); err != nil {
			log.Printf("WARNING: admin server failed to start on %s: %v", *adminSocket, err)
		} else {
			defer adminSrv.Stop()
			log.Printf("admin server listening on %s", *adminSocket)
		}
	}

	if *androidEnabled && *androidNodeIP == "" && !*cniEnabled {
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
		CNI: engine.CNIConfig{
			Enabled:     *cniEnabled,
			ConfDir:     *cniConfDir,
			BinDir:      *cniBinDir,
			IfName:      *cniIfName,
			NetNSDir:    *cniNetNSDir,
			NetNSPrefix: *androidCNINetNSPrefix,
		},
		StateStore: stateStore,
	})
	defer androidEng.Close()
	cleanupManager := engine.NewCleanupManager(stateStore, e2bEng, androidEng, engine.CleanupConfig{
		Enabled:     *orphanReconcileEnabled,
		Interval:    *orphanReconcileInterval,
		GracePeriod: *orphanGracePeriod,
		MaxRetries:  *cleanupMaxRetries,
		DryRun:      *cleanupDryRun,
	})
	defer cleanupManager.Close()

	restoreCtx, cancelRestore := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelRestore()
	if restorer, ok := e2bEng.(stateRestorer); ok {
		if err := restorer.RestoreState(restoreCtx); err != nil {
			log.Fatalf("restore e2b state: %v", err)
		}
	}
	if err := androidEng.RestoreState(restoreCtx); err != nil {
		log.Fatalf("restore android state: %v", err)
	}
	if *orphanReconcileEnabled {
		if err := cleanupManager.Reconcile(restoreCtx); err != nil {
			log.Printf("startup orphan reconcile warning: %v", err)
		}
	}

	mux := server.NewMuxServer(containerEng, e2bEng, androidEng, stateStore)
	if err := mux.RestoreState(); err != nil {
		log.Fatalf("restore mux state: %v", err)
	}
	cleanupManager.Start(context.Background())

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("received shutdown signal, stopping...")
		mux.Stop()
	}()

	log.Printf("starting cri-multiplex on %s (containerd: %s, orchestrator: %s, node-ip: %s, proxy: %s, state-dir: %s, android-enabled: %v, cni-enabled: %v, android-node-ip: %s)",
		*socketPath, *containerdSocket, cfg.OrchestratorAddr, cfg.NodeIP, cfg.OrchestratorProxyAddr, *stateDir, *androidEnabled, *cniEnabled, *androidNodeIP)
	if err := mux.Start(*socketPath); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}
