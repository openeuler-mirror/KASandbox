package agentenv

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

type failingProjectionStore struct {
	*memoryStateStore
	fail bool
}

func (s *failingProjectionStore) SaveBackendState(path string, value any) error {
	if err := s.memoryStateStore.SaveBackendState(path, value); err != nil {
		return err
	}
	if s.fail {
		return errors.New("mock persistence failure")
	}
	return nil
}

func TestProjectionPersistenceRollback(t *testing.T) {
	e, base := newReadyContainerTestEngine(t)
	store := &failingProjectionStore{memoryStateStore: base}
	e.store = store
	request := criCreateContainerRequest("cri-1", "app", "agentenv.dev/app:v1", 0)
	// CRI 容器只投影状态，不执行 command/args。
	request.Config.Command = nil
	request.Config.Args = nil
	request.Config.LogPath = "app/0.log"
	created, err := e.CreateContainer(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	id := created.GetContainerId()
	info, err := e.ContainerStatus(context.Background(), &runtime.ContainerStatusRequest{ContainerId: id})
	if err != nil || info.GetStatus().GetLogPath() != "" {
		t.Fatal("逻辑容器不应发布虚假的日志路径")
	}
	store.fail = true
	if _, err := e.StartContainer(context.Background(), &runtime.StartContainerRequest{ContainerId: id}); err == nil {
		t.Fatal("未报告持久化失败")
	}
	record, _ := e.getContainer(id)
	if record.State != containerStateCreated {
		t.Fatal("失败写入污染状态")
	}
	var saved persistedAgentENVState
	if _, err := store.LoadBackendState(podStatePath, &saved); err != nil || len(saved.Containers) != 1 || saved.Containers[0].State != containerStateCreated {
		t.Fatal("失败写入污染共享 StateStore")
	}
	store.fail = false
	if _, err := e.StartContainer(context.Background(), &runtime.StartContainerRequest{ContainerId: id}); err != nil {
		t.Fatal(err)
	}
	// 无连接也能读取逻辑统计；不伪造 CPU、内存或磁盘指标。
	e.admin = nil
	stats, err := e.ContainerStats(context.Background(), &runtime.ContainerStatsRequest{ContainerId: id})
	if err != nil || stats.GetStats().GetAttributes().GetId() != id {
		t.Fatalf("统计身份: %v %v", stats, err)
	}
	if stats.Stats.Cpu != nil || stats.Stats.Memory != nil || stats.Stats.WritableLayer != nil {
		t.Fatal("逻辑投影不应返回 VM 指标")
	}
	list, err := e.ListContainerStats(context.Background(), &runtime.ListContainerStatsRequest{Filter: &runtime.ContainerStatsFilter{Id: "other"}})
	if err != nil || len(list.Stats) != 0 {
		t.Fatal("统计过滤失败")
	}
	if _, err := e.StopContainer(context.Background(), &runtime.StopContainerRequest{ContainerId: id}); err != nil {
		t.Fatal(err)
	}
	restored := newWithClients(Config{}, store, nil, nil)
	if err := restored.restorePods(); err != nil {
		t.Fatal(err)
	}
	record, ok := restored.getContainer(id)
	if !ok || record.State != containerStateExited {
		t.Fatal("停止状态未持久化")
	}
}

func TestConcurrentProjectionWritesSurviveRestore(t *testing.T) {
	store := newMemoryStateStore()
	e := newWithClients(Config{}, store, nil, nil)
	var group sync.WaitGroup
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			if err := e.putPod(podRecord{CRISandboxID: fmt.Sprint(i), State: criStateCreating}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	group.Wait()
	restored := newWithClients(Config{}, store, nil, nil)
	if err := restored.restorePods(); err != nil {
		t.Fatal(err)
	}
	if len(restored.listPods()) != 20 {
		t.Fatal("并发写入丢失")
	}
}
