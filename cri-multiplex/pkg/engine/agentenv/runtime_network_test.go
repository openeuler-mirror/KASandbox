package agentenv

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	runtimeapi "github.com/cri-multiplex/pkg/agentenv/runtime/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func newRuntimeNetworkTestEngine(t *testing.T) (*Engine, *fakeLifecycle, *fakeAdmin, string) {
	t.Helper()
	const nativeID = "019a0000-0000-7000-8000-000000000301"
	lifecycle := &fakeLifecycle{
		create: func(_ context.Context, req *runtimeapi.SandboxCreateRequest) (*runtimeapi.SandboxCreateResponse, error) {
			if req.GetSpec().GetRuntimeNetwork() != nil {
				t.Fatal("原生网络不应传外部 CNI netns")
			}
			return &runtimeapi.SandboxCreateResponse{Sandbox: &runtimeapi.SandboxInfo{NativeSandboxId: nativeID, Phase: runtimeapi.SandboxPhase_RUNNING}}, nil
		},
		get: func(context.Context, *runtimeapi.SandboxGetRequest) (*runtimeapi.SandboxInfo, error) {
			return &runtimeapi.SandboxInfo{NativeSandboxId: nativeID, Phase: runtimeapi.SandboxPhase_RUNNING}, nil
		},
	}
	e, _ := newCRITestEngine(t, lifecycle)
	path := filepath.Join(t.TempDir(), "runtime-netns")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	admin := &fakeAdmin{getRuntime: func(_ context.Context, req *runtimeapi.SandboxGetRuntimeRequest) (*runtimeapi.SandboxRuntimeInfo, error) {
		return &runtimeapi.SandboxRuntimeInfo{NativeSandboxId: req.GetNativeSandboxId(), Phase: runtimeapi.SandboxPhase_RUNNING, NetnsPath: path, Network: map[string]string{"mode": runtimeManagedNetworkMode, "pod_ip": "10.11.0.9"}}, nil
	}}
	e.admin = admin
	return e, lifecycle, admin, path
}
func TestRuntimeManagedNetworkLifecycleAndOwnership(t *testing.T) {
	e, lifecycle, _, path := newRuntimeNetworkTestEngine(t)
	defer e.Close()
	var order []string
	lifecycle.delete = func(_ context.Context, req *runtimeapi.SandboxDeleteRequest) (*runtimeapi.OperationStatus, error) {
		order = append(order, "delete")
		return successfulDeleteOperation(req), nil
	}
	req := criRunRequest(map[string]string{annTemplate: "template"})
	if _, err := e.RunPodSandbox(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunPodSandbox(context.Background(), req); err != nil {
		t.Fatalf("重复创建: %v", err)
	}
	rsp, err := e.PodSandboxStatus(context.Background(), &runtime.PodSandboxStatusRequest{PodSandboxId: "cri-1"})
	if err != nil {
		t.Fatal(err)
	}
	if rsp.GetStatus().GetNetwork().GetIp() != "10.11.0.9" || rsp.GetStatus().GetState() != runtime.PodSandboxState_SANDBOX_READY {
		t.Fatalf("Pod 状态: %+v", rsp)
	}
	restored := newWithClients(Config{}, e.store, lifecycle, e.admin)
	if err := restored.restorePods(); err != nil {
		t.Fatal(err)
	}
	rec, _ := restored.getPod("cri-1")
	if rec.RuntimeNetwork == nil || rec.CNIRecord != nil || podIPFromRecord(rec) != "10.11.0.9" {
		t.Fatalf("恢复后的网络所有权丢失: %+v", rec)
	}
	if _, err := e.PortForward(context.Background(), &runtime.PortForwardRequest{PodSandboxId: "cri-1", Port: []int32{8080}}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("原生网络 PortForward: %v", err)
	}
	order = nil
	if _, err := e.StopPodSandbox(context.Background(), &runtime.StopPodSandboxRequest{PodSandboxId: "cri-1"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"delete"}) {
		t.Fatalf("原生网络删除不应新增 HostPort 操作: %v", order)
	}
	rsp, err = e.PodSandboxStatus(context.Background(), &runtime.PodSandboxStatusRequest{PodSandboxId: "cri-1"})
	if err != nil {
		t.Fatal(err)
	}
	if rsp.GetStatus().GetNetwork().GetIp() != "" {
		t.Fatal("已释放的原生 IP 仍被回报")
	}
	if _, err := e.RemovePodSandbox(context.Background(), &runtime.RemovePodSandboxRequest{PodSandboxId: "cri-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("mux 删除了 Runtime 所有的 netns")
	}
}
func TestRuntimeManagedNetworkRejectsInvalidFacts(t *testing.T) {
	for _, ip := range []string{"", "169.254.0.21", "127.0.0.1", "0.0.0.0", "224.0.0.1", "::1", "bad"} {
		t.Run(ip, func(t *testing.T) {
			e, lifecycle, admin, _ := newRuntimeNetworkTestEngine(t)
			old := admin.getRuntime
			admin.getRuntime = func(ctx context.Context, r *runtimeapi.SandboxGetRuntimeRequest) (*runtimeapi.SandboxRuntimeInfo, error) {
				x, err := old(ctx, r)
				x.Network["pod_ip"] = ip
				return x, err
			}
			deletes := 0
			lifecycle.delete = func(_ context.Context, r *runtimeapi.SandboxDeleteRequest) (*runtimeapi.OperationStatus, error) {
				deletes++
				return successfulDeleteOperation(r), nil
			}
			_, err := e.RunPodSandbox(context.Background(), criRunRequest(map[string]string{annTemplate: "template"}))
			if status.Code(err) != codes.FailedPrecondition || deletes != 1 || len(e.listPods()) != 0 {
				t.Fatalf("非法事实必须拒绝并只回收 VM: err=%v deletes=%d", err, deletes)
			}
		})
	}
}
func TestRuntimeManagedContainerPortDoesNotCreateHostPort(t *testing.T) {
	e, _, _, _ := newRuntimeNetworkTestEngine(t)
	defer e.Close()
	err := validatePodPorts(&createPlan{podConfig: &runtime.PodSandboxConfig{
		PortMappings: []*runtime.PortMapping{{HostPort: 0, ContainerPort: 18080}},
	}})
	if err != nil {
		t.Fatalf("仅声明 containerPort 不应创建 HostPort 或 CNI 资源: err=%v", err)
	}
}

func TestRuntimeManagedRejectsHostPortBeforeCreatingVM(t *testing.T) {
	e, lifecycle, _, _ := newRuntimeNetworkTestEngine(t)
	defer e.Close()
	lifecycle.create = func(context.Context, *runtimeapi.SandboxCreateRequest) (*runtimeapi.SandboxCreateResponse, error) {
		t.Fatal("拒绝HostPort后不应创建VM")
		return nil, nil
	}
	req := criRunRequest(map[string]string{annTemplate: "template"})
	req.Config.PortMappings = []*runtime.PortMapping{{HostPort: 30080, ContainerPort: 80}}
	if _, err := e.RunPodSandbox(context.Background(), req); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("必须明确拒绝HostPort: %v", err)
	}
	if len(e.listPods()) != 0 {
		t.Fatal("无效请求产生了资源")
	}
}
func TestRuntimeManagedNetworkIdentityWithoutHostNamespaceProbe(t *testing.T) {
	e, _, admin, path := newRuntimeNetworkTestEngine(t)
	if _, err := e.RunPodSandbox(context.Background(), criRunRequest(map[string]string{annTemplate: "template"})); err != nil {
		t.Fatal(err)
	}
	rec, _ := e.getPod("cri-1")
	old := admin.getRuntime
	admin.getRuntime = func(ctx context.Context, r *runtimeapi.SandboxGetRuntimeRequest) (*runtimeapi.SandboxRuntimeInfo, error) {
		x, err := old(ctx, r)
		x.Network["pod_ip"] = "10.11.0.10"
		return x, err
	}
	if err := e.refreshRuntimeNetwork(context.Background(), &rec); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("不能接管不同地址: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if ready, err := e.podNetworkReady(rec); err != nil || !ready {
		t.Fatalf("网络状态由 Runtime 提供，mux 不探测宿主 netns: %t %v", ready, err)
	}
}

func TestRuntimeManagedStatusAndPortForwardRejectReusedSlot(t *testing.T) {
	e, _, admin, _ := newRuntimeNetworkTestEngine(t)
	defer e.Close()
	if _, err := e.RunPodSandbox(context.Background(), criRunRequest(map[string]string{annTemplate: "template"})); err != nil {
		t.Fatal(err)
	}
	old := admin.getRuntime
	admin.getRuntime = func(ctx context.Context, r *runtimeapi.SandboxGetRuntimeRequest) (*runtimeapi.SandboxRuntimeInfo, error) {
		x, err := old(ctx, r)
		x.Network["pod_ip"] = "10.11.0.10"
		return x, err
	}
	rsp, err := e.PodSandboxStatus(context.Background(), &runtime.PodSandboxStatusRequest{PodSandboxId: "cri-1"})
	if err != nil {
		t.Fatal(err)
	}
	if rsp.GetStatus().GetState() != runtime.PodSandboxState_SANDBOX_NOTREADY || rsp.GetStatus().GetNetwork().GetIp() != "" {
		t.Fatalf("不能报告复用后的旧IP: %+v", rsp)
	}
	if _, err = e.PortForward(context.Background(), &runtime.PortForwardRequest{PodSandboxId: "cri-1", Port: []int32{8080}}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("不能转发到复用IP: %v", err)
	}
}
