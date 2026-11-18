package agentenv

import (
	"testing"

	"github.com/cri-multiplex/pkg/engine"
)

func TestPrivateMappingEmptyDeletePreservesPartialIdentity(t *testing.T) {
	routes := newRouteTable(nil)
	if err := routes.PutPodRoute(podRoute{Backend: engine.EngineTypeAgentENV, NativeSandboxID: "native"}); err != nil {
		t.Fatal(err)
	}
	if err := routes.DeleteSandbox(""); err != nil {
		t.Fatal(err)
	}
	if _, ok := routes.PodRouteByNative("native"); !ok {
		t.Fatal("空 ID 删除了未绑定的映射")
	}
}

func TestPrivateMappingRestoresWithoutSharedRoutes(t *testing.T) {
	store := newMemoryStateStore()
	routes := newRouteTable(store)
	wanted := podRoute{Backend: engine.EngineTypeAgentENV, CRISandboxID: "cri", ProductSandboxID: "product", NativeSandboxID: "native"}
	if err := routes.PutPodRoute(wanted); err != nil {
		t.Fatal(err)
	}
	restored := newRouteTable(store)
	if err := restored.Restore(); err != nil {
		t.Fatal(err)
	}
	got, ok := restored.PodRouteByCRI("cri")
	if !ok || got.NativeSandboxID != "native" {
		t.Fatalf("映射未恢复: %+v", got)
	}
	if len(store.routes) != 0 {
		t.Fatal("写入了公共路由")
	}
	if err := restored.DeleteSandbox("product"); err != nil {
		t.Fatal(err)
	}
	if err := routes.Restore(); err != nil {
		t.Fatal(err)
	}
	if _, ok := routes.PodRouteByNative("native"); ok {
		t.Fatal("删除未持久化")
	}
}

func TestPrivateMappingRejectsMergingDifferentSandboxes(t *testing.T) {
	routes := newRouteTable(nil)
	for _, r := range []podRoute{
		{Backend: engine.EngineTypeAgentENV, ProductSandboxID: "p1"},
		{Backend: engine.EngineTypeAgentENV, ProductSandboxID: "p2", NativeSandboxID: "n2"},
	} {
		if err := routes.PutPodRoute(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := routes.PutPodRoute(podRoute{Backend: engine.EngineTypeAgentENV, ProductSandboxID: "p1", NativeSandboxID: "n2"}); err == nil {
		t.Fatal("错误合并了两个 Sandbox 的身份")
	}
	if err := routes.PutPodRoute(podRoute{Backend: engine.EngineTypeAgentENV, CRISandboxID: "n2", ProductSandboxID: "p3"}); err == nil {
		t.Fatal("允许了跨字段身份冲突")
	}
	if len(routes.Snapshot()) != 2 {
		t.Fatal("冲突请求修改了已有映射")
	}
}
