package agentenv

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	runtimeapi "github.com/cri-multiplex/pkg/agentenv/runtime/v1"
	"github.com/cri-multiplex/pkg/engine"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func TestNativeCreateUnknownOutcomeRetriesSameOperation(t *testing.T) {
	for _, code := range []codes.Code{codes.Unknown, codes.DeadlineExceeded} {
		t.Run(code.String(), func(t *testing.T) {
			e, lifecycle, _, path := newRuntimeNetworkTestEngine(t)
			defer e.Close()
			original := lifecycle.create
			calls := 0
			var operation string
			var fingerprint []byte
			lifecycle.create = func(ctx context.Context, r *runtimeapi.SandboxCreateRequest) (*runtimeapi.SandboxCreateResponse, error) {
				calls++
				if r.Spec.RuntimeNetwork != nil {
					t.Fatal("不应附加外部网络")
				}
				if calls == 1 {
					operation = r.OperationId
					fingerprint = append([]byte(nil), r.RequestFingerprint...)
					return nil, status.Error(code, "unknown outcome")
				}
				if operation != r.OperationId || !bytes.Equal(fingerprint, r.RequestFingerprint) {
					t.Fatal("重试改变了幂等标识")
				}
				return original(ctx, r)
			}
			req := criRunRequest(map[string]string{annTemplate: "template"})
			if _, err := e.RunPodSandbox(context.Background(), req); status.Code(err) != code {
				t.Fatalf("首次结果: %v", err)
			}
			rec, ok := e.getPod("cri-1")
			if !ok || rec.State != criStateCreating || rec.RuntimeNetwork == nil {
				t.Fatal("未保留原生创建中状态")
			}
			if _, err := e.RunPodSandbox(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			if calls != 2 {
				t.Fatalf("创建次数=%d", calls)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("重试触碰了 Runtime 网络", err)
			}
		})
	}
}

func TestNativeDeleteFailureKeepsProjectionAndRetries(t *testing.T) {
	e, lifecycle, _, path := newRuntimeNetworkTestEngine(t)
	defer e.Close()
	ctx := context.Background()
	if _, err := e.RunPodSandbox(ctx, criRunRequest(map[string]string{annTemplate: "template"})); err != nil {
		t.Fatal(err)
	}
	calls := 0
	lifecycle.delete = func(_ context.Context, r *runtimeapi.SandboxDeleteRequest) (*runtimeapi.OperationStatus, error) {
		calls++
		if calls == 1 {
			return nil, status.Error(codes.Unavailable, "retry")
		}
		return successfulDeleteOperation(r), nil
	}
	req := &runtime.RemovePodSandboxRequest{PodSandboxId: "cri-1"}
	if _, err := e.RemovePodSandbox(ctx, req); status.Code(err) != codes.Unavailable {
		t.Fatal(err)
	}
	if rec, ok := e.getPod("cri-1"); !ok || rec.RuntimeDeleted {
		t.Fatal("删除失败丢失投影")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("删除失败触碰了 Runtime 网络", err)
	}
	if _, err := e.RemovePodSandbox(ctx, req); err != nil {
		t.Fatal(err)
	}
	if len(e.listPods()) != 0 || calls != 2 {
		t.Fatal("删除重试未完成")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("mux 不应自行删除原生 netns", err)
	}
}

func TestNativeRestoreRejectsLegacyNetworkWithoutChangingState(t *testing.T) {
	for _, extra := range []string{`,"cni_record":{}`, `,"host_ports":[{"host_port":30080}]`} {
		t.Run(extra, func(t *testing.T) {
			store := newMemoryStateStore()
			raw := []byte(`{"pods":[{"cri_sandbox_id":"old","native_sandbox_id":"native","state":"ready"` + extra + `}]}`)
			store.sections[podStatePath] = raw
			e := newWithClients(Config{}, store, newRouteTable(store), &fakeLifecycle{}, &fakeAdmin{})
			defer e.Close()
			if err := e.restorePods(); err == nil {
				t.Fatal("应拒绝旧网络状态")
			}
			if !bytes.Equal(store.sections[podStatePath], raw) || len(e.listPods()) != 0 {
				t.Fatal("拒绝加载不应改写或部分接管状态")
			}
		})
	}
}

func TestNativeReconcileRecoversPendingAndReclaimsExpired(t *testing.T) {
	e, lifecycle, _, path := newRuntimeNetworkTestEngine(t)
	defer e.Close()
	ctx := context.Background()
	req := criRunRequest(map[string]string{annTemplate: "template"})
	if _, err := e.RunPodSandbox(ctx, req); err != nil {
		t.Fatal(err)
	}
	rec, _ := e.getPod("cri-1")
	native := rec.NativeSandboxID
	rec.NativeSandboxID = ""
	rec.State = criStateCreating
	if err := e.putPod(rec); err != nil {
		t.Fatal(err)
	}
	lifecycle.list = func(context.Context, *runtimeapi.SandboxListRequest) (*runtimeapi.SandboxListResponse, error) {
		return &runtimeapi.SandboxListResponse{Sandboxes: []*runtimeapi.SandboxInfo{{NativeSandboxId: native, ProductSandboxId: rec.ProductSandboxID, Phase: runtimeapi.SandboxPhase_RUNNING}}}, nil
	}
	if err := e.Reconcile(ctx, engine.ReconcileOptions{}); err != nil {
		t.Fatal(err)
	}
	restored, _ := e.getPod("cri-1")
	if restored.State != criStateReady || restored.NativeSandboxID != native {
		t.Fatal("未恢复原生投影")
	}
	lifecycle.list = nil
	if err := e.Reconcile(ctx, engine.ReconcileOptions{GracePeriod: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if len(e.listPods()) != 1 {
		t.Fatal("宽限期内被回收")
	}
	if err := e.Reconcile(ctx, engine.ReconcileOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(e.listPods()) != 0 {
		t.Fatal("过期投影未回收")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("Reconcile 不应自行删除原生 netns", err)
	}
}
