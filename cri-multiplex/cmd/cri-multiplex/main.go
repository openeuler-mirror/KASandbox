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
	"github.com/cri-multiplex/pkg/engine/android"
	"github.com/cri-multiplex/pkg/engine/containerd"
	"github.com/cri-multiplex/pkg/engine/e2b"
	"github.com/cri-multiplex/pkg/engine/util"
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

// logRegistrySnapshot 打印冻结后的后端快照（只含 Descriptor 与能力开关，不含任何密钥）。
func logRegistrySnapshot(reg *engine.Registry) {
	for _, mod := range reg.Modules() {
		desc := mod.Descriptor
		log.Printf("[backend-registry] type=%s handler=%s aliases=%v image_prefixes=%v cleaner=%t reconciler=%t restorer=%t health=%t",
			desc.Type, desc.Handler, desc.Aliases, desc.ImagePrefixes,
			mod.Cleaner != nil, mod.Reconciler != nil, mod.Restorer != nil, mod.Health != nil)
	}
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
	cniPoolEnabled := flag.Bool("cni-pool-enabled", false, "Enable the E2B CNI netns/veth prewarm pool (only takes effect together with -cni-pool-size > 0)")
	cniPoolSize := flag.Int("cni-pool-size", 0, "E2B CNI netns/veth prewarm pool size (0 disables pooling; warm entries are reused by RunPodSandbox to skip serial CNI ADD under concurrency)")
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
	unknownHandlerPolicy := flag.String("unknown-handler-policy", string(server.UnknownHandlerPassthrough), "Behaviour for an unregistered runtime handler: passthrough (fall back to the default backend) or reject")
	runtimeName := flag.String("runtime-name", "e2b-cri-shim", "Runtime name reported by the CRI Version API")
	flag.Parse()

	handlerPolicy := server.UnknownHandlerPolicy(*unknownHandlerPolicy)
	switch handlerPolicy {
	case server.UnknownHandlerPassthrough, server.UnknownHandlerReject:
	default:
		log.Fatalf("invalid -unknown-handler-policy %q (want %q or %q)",
			*unknownHandlerPolicy, server.UnknownHandlerPassthrough, server.UnknownHandlerReject)
	}

	stateStore, err := engine.NewJSONStateStore(*stateDir)
	if err != nil {
		log.Fatalf("initialize state store: %v", err)
	}

	containerEng := containerd.New(*containerdSocket)
	defer containerEng.Close()

	if *nodeIP == "" {
		*nodeIP = autoNodeIP()
		if *nodeIP == "" {
			log.Fatal("--node-ip is required (or auto-detection failed). " +
				"Example: --node-ip=$(hostname -I | awk '{print $1}')")
		}
		log.Printf("auto-detected node-ip: %s", *nodeIP)
	}
	cfg := e2b.Config{
		Deps:                  engine.Deps{Store: stateStore, NodeName: *nodeName, NodeIP: *nodeIP},
		OrchestratorAddr:      *orchestratorAddress,
		OrchestratorProxyAddr: *orchestratorProxyAddr,
		CNI: util.CNIConfig{
			Enabled:     *cniEnabled,
			ConfDir:     *cniConfDir,
			BinDir:      *cniBinDir,
			IfName:      *cniIfName,
			NetNSDir:    *cniNetNSDir,
			PoolEnabled: *cniPoolEnabled,
			PoolSize:    *cniPoolSize,
		},
		HideLabel: *hideSandboxLabel,
	}
	e2bEng, err := e2b.New(cfg)
	if err != nil {
		log.Fatalf("initialize e2b backend: %v", err)
	}
	defer e2bEng.Close()

	// Admin server 是附加管理面（Node Agent 调用），启动失败仅告警，不影响 CRI 主服务。
	adminSrv := admin.NewServer(*adminSocket, e2bEng)
	if err := adminSrv.Start(); err != nil {
		log.Printf("WARNING: admin server failed to start on %s: %v", *adminSocket, err)
	} else {
		defer adminSrv.Stop()
		log.Printf("admin server listening on %s", *adminSocket)
	}

	if *androidEnabled && *androidNodeIP == "" && !*cniEnabled {
		*androidNodeIP = autoNodeIP()
		if *androidNodeIP == "" {
			log.Fatal("--android-node-ip is required when --android-enabled is set (auto-detection failed)")
		}
		log.Printf("auto-detected android-node-ip: %s", *androidNodeIP)
	}
	androidEng, err := android.New(android.Config{
		Deps:                 engine.Deps{Store: stateStore, NodeName: *nodeName, NodeIP: *androidNodeIP},
		Enabled:              *androidEnabled,
		ArtifactsDir:         *androidArtifactsDir,
		ADBPortStart:         *androidADBPortStart,
		BaseInstanceNumStart: *androidBaseInstanceNumStart,
		WebRTCPortStart:      *androidWebRTCPortStart,
		LaunchTimeout:        *androidLaunchTimeout,
		StateDir:             *androidStateDir,
		CNI: util.CNIConfig{
			Enabled:     *cniEnabled,
			ConfDir:     *cniConfDir,
			BinDir:      *cniBinDir,
			IfName:      *cniIfName,
			NetNSDir:    *cniNetNSDir,
			NetNSPrefix: *androidCNINetNSPrefix,
		},
	})
	if err != nil {
		log.Fatalf("initialize android backend: %v", err)
	}
	defer androidEng.Close()

	// 后端装配顺序固定：Register → Freeze → CleanupManager → Mux → Restore → Reconcile → Start。
	reg := engine.NewRegistry(engine.EngineTypeContainer)
	for _, mod := range []engine.BackendModule{containerEng.Module(), e2bEng.Module(), androidEng.Module()} {
		if err := reg.Register(mod); err != nil {
			log.Fatalf("register backend %s: %v", mod.Descriptor.Type, err)
		}
	}
	if err := reg.Freeze(); err != nil {
		log.Fatalf("freeze backend registry: %v", err)
	}
	logRegistrySnapshot(reg)

	cleanupManager := engine.NewCleanupManager(stateStore, reg, engine.CleanupConfig{
		Enabled:     *orphanReconcileEnabled,
		Interval:    *orphanReconcileInterval,
		GracePeriod: *orphanGracePeriod,
		MaxRetries:  *cleanupMaxRetries,
		DryRun:      *cleanupDryRun,
	})
	defer cleanupManager.Close()

	restoreCtx, cancelRestore := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelRestore()
	mux := server.NewMuxServer(reg, stateStore, server.MuxOptions{
		UnknownHandlerPolicy: handlerPolicy,
		RuntimeName:          *runtimeName,
		Cleanup:              cleanupManager,
	})
	if err := mux.RestoreState(restoreCtx); err != nil {
		log.Fatalf("restore mux state: %v", err)
	}
	if *orphanReconcileEnabled {
		if err := cleanupManager.Reconcile(restoreCtx); err != nil {
			log.Printf("startup orphan reconcile warning: %v", err)
		}
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
