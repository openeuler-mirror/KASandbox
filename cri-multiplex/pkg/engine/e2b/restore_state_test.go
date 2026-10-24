package e2b

import (
	"context"
	"testing"

	"github.com/cri-multiplex/pkg/engine"
)

// 重启恢复必须把 pod 与容器路由写回路由表，否则 ListPodSandbox 之后的
// PodSandboxStatus/RemovePodSandbox 会按"未知 sandbox"处理。
func TestRestoreStateRebuildsPodAndContainerRoutes(t *testing.T) {
	store := newTestStateStore(t)
	client := &fakeSandboxServiceClient{}
	pod := &podInfo{
		sandboxID:     "cri-1",
		e2bSandboxID:  "e2b-1",
		podUID:        "uid-1",
		state:         stateRunning,
		containerName: "main",
	}
	if err := saveE2BPodTo(store, pod.toPersistedState()); err != nil {
		t.Fatalf("persist pod: %v", err)
	}

	restored := newTestEngine(client)
	restored.stateStore = store
	if err := restored.RestoreState(context.Background()); err != nil {
		t.Fatalf("RestoreState: %v", err)
	}

	routes, err := loadRoutesFrom(t, store)
	if err != nil {
		t.Fatalf("LoadRoutes: %v", err)
	}
	byKey := map[string]engine.RouteRecord{}
	for _, r := range routes {
		byKey[r.Kind+"/"+r.ID] = r
	}
	podRoute, ok := byKey["pod/cri-1"]
	if !ok || podRoute.Engine != engine.EngineTypeE2B {
		t.Fatalf("pod route missing: %+v", routes)
	}
	containerRoute, ok := byKey["container/cri-1-c"]
	if !ok || containerRoute.Engine != engine.EngineTypeE2B || containerRoute.SandboxID != "cri-1" {
		t.Fatalf("container route missing: %+v", routes)
	}
}
