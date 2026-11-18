package agentenv

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	runtimeapi "github.com/cri-multiplex/pkg/agentenv/runtime/v1"
)

const podNetworkCleanupTimeout = 30 * time.Second
const runtimeManagedNetworkMode = "runtime-managed"

// validatePodPorts 允许 containerPort 声明，但拒绝额外宿主端口绑定。
func validatePodPorts(plan *createPlan) error {
	if plan == nil || plan.podConfig == nil {
		return status.Error(codes.InvalidArgument, "pod sandbox config is required")
	}
	for _, mapping := range plan.podConfig.GetPortMappings() {
		if mapping.GetHostPort() != 0 {
			return status.Error(codes.InvalidArgument, "HostPort is not supported with AgentENV runtime-managed networking; use the AgentENV proxy")
		}
	}
	return nil
}
func (e *Engine) ensurePodNetwork(record podRecord) error { return e.ensurePodNetworkFacts(record) }

// podNetworkReady 不分配或回收网络；网络生命周期完全属于 Runtime。
func (e *Engine) podNetworkReady(record podRecord) (bool, error) {
	if record.RuntimeNetwork == nil || record.RuntimeDeleted || record.RuntimeNetwork.Mode != runtimeManagedNetworkMode || !validRuntimePodIP(record.RuntimeNetwork.PodIP) {
		return false, nil
	}
	info, err := os.Stat(record.RuntimeNetwork.NetNSPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !info.IsDir(), nil
}
func podIPFromRecord(record podRecord) string {
	if record.RuntimeNetwork == nil {
		return ""
	}
	return record.RuntimeNetwork.PodIP
}

// refreshRuntimeNetwork 只接受活实例返回的宿主可达地址，不使用重复的 guest link-local IP。
func (e *Engine) refreshRuntimeNetwork(ctx context.Context, record *podRecord) error {
	if record.RuntimeNetwork == nil {
		return status.Error(codes.FailedPrecondition, "Runtime network projection is missing")
	}
	info, err := e.admin.GetRuntime(ctx, &runtimeapi.SandboxGetRuntimeRequest{NativeSandboxId: record.NativeSandboxID})
	if err != nil {
		return e.mapRuntimeError(err)
	}
	if info.GetNativeSandboxId() != record.NativeSandboxID || info.GetPhase() != runtimeapi.SandboxPhase_RUNNING || info.GetNetwork()["mode"] != runtimeManagedNetworkMode {
		return status.Error(codes.FailedPrecondition, "Runtime does not expose a live runtime-managed network")
	}
	ip, path := info.GetNetwork()["pod_ip"], info.GetNetnsPath()
	if !validRuntimePodIP(ip) || !filepath.IsAbs(path) {
		return status.Error(codes.FailedPrecondition, "Runtime returned an invalid Pod IP or netns path")
	}
	// 活实例身份改变时拒绝旧地址，避免 slot 复用误路由。
	if record.RuntimeNetwork.PodIP != "" && (record.RuntimeNetwork.PodIP != ip || record.RuntimeNetwork.NetNSPath != path) {
		return status.Error(codes.FailedPrecondition, "Runtime network identity changed")
	}
	record.RuntimeNetwork = &runtimeNetworkRecord{Mode: runtimeManagedNetworkMode, PodIP: ip, NetNSPath: path}
	return e.ensurePodNetworkFacts(*record)
}
func (e *Engine) ensurePodNetworkFacts(record podRecord) error {
	ready, err := e.podNetworkReady(record)
	if err != nil || !ready {
		return status.Errorf(codes.FailedPrecondition, "Runtime network is unavailable: %v", err)
	}
	return nil
}
func validRuntimePodIP(value string) bool {
	ip := net.ParseIP(value)
	return ip != nil && ip.To4() != nil && ip.IsGlobalUnicast() && !ip.IsLinkLocalUnicast() && !ip.IsLoopback()
}

// currentPodNetworkReady 核对活实例归属，不能把被复用的原生 slot 当成旧网络。
func (e *Engine) currentPodNetworkReady(ctx context.Context, record podRecord) (bool, error) {
	if record.RuntimeNetwork != nil {
		if record.RuntimeDeleted {
			return false, nil
		}
		if err := e.refreshRuntimeNetwork(ctx, &record); err != nil {
			if status.Code(err) == codes.FailedPrecondition || status.Code(err) == codes.NotFound {
				return false, nil
			}
			return false, err
		}
	}
	return e.podNetworkReady(record)
}
