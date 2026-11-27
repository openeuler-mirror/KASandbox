package cube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/cri-multiplex/pkg/engine"
)

type destroyCall struct {
	id  string
	ann map[string]string
}

type fakeClient struct {
	creates   []createRequest
	destroys  []destroyCall
	updates   []map[string]string
	execs     [][]string
	createFn  func(createRequest) (createResult, error)
	updateErr error
	list      []listedSandbox
	listErr   error
}

func (f *fakeClient) Create(_ context.Context, req createRequest) (createResult, error) {
	f.creates = append(f.creates, req)
	if f.createFn != nil {
		return f.createFn(req)
	}
	if req.Resume != nil {
		return createResult{SandboxID: req.Resume.SandboxID, SandboxIP: "10.1.0.3"}, nil
	}
	return createResult{SandboxID: "sbx-1", SandboxUUID: "uuid-1", SandboxIP: "10.1.0.2"}, nil
}

func (f *fakeClient) Destroy(_ context.Context, sandboxID string, annotations map[string]string) error {
	f.destroys = append(f.destroys, destroyCall{id: sandboxID, ann: annotations})
	return nil
}

func (f *fakeClient) List(context.Context) ([]listedSandbox, error) {
	return f.list, f.listErr
}

func (f *fakeClient) Update(_ context.Context, _ string, annotations map[string]string) error {
	f.updates = append(f.updates, annotations)
	return f.updateErr
}

func (f *fakeClient) Exec(_ context.Context, _ string, args []string) error {
	f.execs = append(f.execs, append([]string(nil), args...))
	return nil
}

func (f *fakeClient) Commit(context.Context, string, string, string) (string, error) {
	return "/snapshots/snap", nil
}

func (f *fakeClient) Close() error { return nil }

func newTestEngine(t *testing.T, client cubeClient) *Engine {
	t.Helper()
	store, err := engine.NewJSONStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	eng := newEngine(Config{
		Enabled:     true,
		CubeletAddr: "127.0.0.1:9999",
		NodeName:    "node-a",
		Timeout:     time.Second,
		StateStore:  store,
	}, client)
	attachStubEnvd(t, eng)
	t.Cleanup(func() { _ = eng.Close() })
	return eng
}

func attachStubEnvd(t *testing.T, eng *Engine) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/init" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	eng.envdHTTP = &http.Client{Transport: rewriteHostTransport{
		base: srv.Client().Transport,
		host: strings.TrimPrefix(srv.URL, "http://"),
	}}
}

func podRequest() *runtime.RunPodSandboxRequest {
	return &runtime.RunPodSandboxRequest{
		RuntimeHandler: "cube",
		Config: &runtime.PodSandboxConfig{
			Metadata: &runtime.PodSandboxMetadata{Name: "app", Uid: "uid-1", Namespace: "default"},
			Labels:   map[string]string{"app": "demo"},
			Annotations: map[string]string{
				annTemplateID: "tpl-base",
				annVCPU:       "2",
				annRAMMB:      "2048",
			},
		},
	}
}

func TestRunPodSandboxRequiresTemplate(t *testing.T) {
	fake := &fakeClient{}
	eng := newTestEngine(t, fake)
	req := podRequest()
	req.Config.Annotations = nil
	_, err := eng.RunPodSandbox(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, err = %v", status.Code(err), err)
	}
	if len(fake.creates) != 0 {
		t.Fatalf("create calls = %d", len(fake.creates))
	}
}

func TestColdCreateWaitsForStateFlush(t *testing.T) {
	store, err := engine.NewJSONStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan error, 1)
	started := make(chan struct{}, 1)
	var blockOnce sync.Once
	realWrite := store.WriteSnapshotForTest()
	store.SetWriteSnapshotForTest(func(data []byte) error {
		var wait bool
		blockOnce.Do(func() { wait = true })
		if wait {
			select {
			case started <- struct{}{}:
			default:
			}
			select {
			case err := <-release:
				if err != nil {
					return err
				}
			case <-time.After(5 * time.Second):
				return context.DeadlineExceeded
			}
		}
		return realWrite(data)
	})

	fake := &fakeClient{}
	eng := newEngine(Config{
		Enabled:     true,
		CubeletAddr: "127.0.0.1:9999",
		NodeName:    "node-a",
		Timeout:     time.Second,
		StateStore:  store,
	}, fake)
	attachStubEnvd(t, eng)
	t.Cleanup(func() { _ = eng.Close() })

	done := make(chan error, 1)
	go func() {
		_, err := eng.RunPodSandbox(context.Background(), podRequest())
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("state flush was not scheduled")
	}
	select {
	case <-done:
		t.Fatal("RunPodSandbox returned before state flush finished")
	case <-time.After(100 * time.Millisecond):
	}
	release <- nil
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("create: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunPodSandbox did not finish after flush")
	}
}

func TestColdCreatePersistFailureDestroysSandbox(t *testing.T) {
	store, err := engine.NewJSONStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store.SetWriteSnapshotForTest(func([]byte) error {
		return context.DeadlineExceeded
	})
	fake := &fakeClient{}
	eng := newEngine(Config{
		Enabled:     true,
		CubeletAddr: "127.0.0.1:9999",
		NodeName:    "node-a",
		Timeout:     time.Second,
		StateStore:  store,
	}, fake)
	attachStubEnvd(t, eng)
	t.Cleanup(func() { _ = eng.Close() })

	_, err = eng.RunPodSandbox(context.Background(), podRequest())
	if err == nil {
		t.Fatal("expected create to fail when persist fails")
	}
	if len(fake.destroys) == 0 || fake.destroys[0].id != "sbx-1" {
		t.Fatalf("expected cubelet destroy after persist fail, got %+v", fake.destroys)
	}
	if _, ok := eng.get("uid-1"); ok {
		t.Fatal("local record should be cleared after persist fail")
	}
}

func TestColdCreateStatusAndIdempotentRerun(t *testing.T) {
	fake := &fakeClient{}
	eng := newTestEngine(t, fake)
	resp, err := eng.RunPodSandbox(context.Background(), podRequest())
	if err != nil {
		t.Fatal(err)
	}
	if resp.PodSandboxId != "uid-1" {
		t.Fatalf("cri id = %s", resp.PodSandboxId)
	}
	st, err := eng.PodSandboxStatus(context.Background(), &runtime.PodSandboxStatusRequest{PodSandboxId: "uid-1"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Status.Network.GetIp() != "10.1.0.2" || st.Status.State != runtime.PodSandboxState_SANDBOX_READY {
		t.Fatalf("status = %+v", st.Status)
	}
	if _, err := eng.RunPodSandbox(context.Background(), podRequest()); err != nil {
		t.Fatal(err)
	}
	if len(fake.creates) != 1 {
		t.Fatalf("create calls = %d", len(fake.creates))
	}
	cold := fake.creates[0].Cold
	if cold == nil || cold.TemplateID != "tpl-base" || cold.CPU != "2" || cold.Memory != "2048Mi" || cold.DiskLimit != defaultDiskLimit {
		t.Fatalf("cold spec = %+v", cold)
	}
}

func TestCreateContainerCopiesKubeletHash(t *testing.T) {
	fake := &fakeClient{}
	eng := newTestEngine(t, fake)
	if _, err := eng.RunPodSandbox(context.Background(), podRequest()); err != nil {
		t.Fatal(err)
	}
	resp, err := eng.CreateContainer(context.Background(), &runtime.CreateContainerRequest{
		PodSandboxId: "uid-1",
		Config: &runtime.ContainerConfig{
			Metadata:    &runtime.ContainerMetadata{Name: "app"},
			Image:       &runtime.ImageSpec{Image: "cube.dev/ignored"},
			Labels:      map[string]string{"io.kubernetes.container.name": "app"},
			Annotations: map[string]string{labelHash: "abc", labelRestart: "0"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ContainerId != "uid-1-c" {
		t.Fatalf("container id = %s", resp.ContainerId)
	}
	if _, err := eng.StartContainer(context.Background(), &runtime.StartContainerRequest{ContainerId: resp.ContainerId}); err != nil {
		t.Fatal(err)
	}
	listed, err := eng.ListContainers(context.Background(), &runtime.ListContainersRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Containers) != 1 || listed.Containers[0].Labels[labelHash] != "abc" || listed.Containers[0].State != runtime.ContainerState_CONTAINER_RUNNING {
		t.Fatalf("containers = %+v", listed.Containers)
	}
}

func TestEmptySandboxIPIsRejected(t *testing.T) {
	fake := &fakeClient{createFn: func(createRequest) (createResult, error) {
		return createResult{SandboxID: "sbx-empty"}, nil
	}}
	eng := newTestEngine(t, fake)
	_, err := eng.RunPodSandbox(context.Background(), podRequest())
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %v, err = %v", status.Code(err), err)
	}
	if len(fake.destroys) != 1 || fake.destroys[0].id != "sbx-empty" {
		t.Fatalf("destroys = %+v", fake.destroys)
	}
	_, err = eng.PodSandboxStatus(context.Background(), &runtime.PodSandboxStatusRequest{PodSandboxId: "uid-1"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("status code = %v", status.Code(err))
	}
}

func TestPauseResumeAndOriginNode(t *testing.T) {
	fake := &fakeClient{}
	eng := newTestEngine(t, fake)
	if _, err := eng.RunPodSandbox(context.Background(), podRequest()); err != nil {
		t.Fatal(err)
	}
	snap, err := eng.Pause(context.Background(), "uid-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(snap, "snap-") || len(snap) != len("snap-")+24 {
		t.Fatalf("snapshot id = %s", snap)
	}
	if fake.updates[0][annUpdateAction] != "pause" || fake.updates[0][annPauseSnapshotID] != snap {
		t.Fatalf("pause annotations = %+v", fake.updates[0])
	}
	st, err := eng.PodSandboxStatus(context.Background(), &runtime.PodSandboxStatusRequest{PodSandboxId: "uid-1"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Status.State != runtime.PodSandboxState_SANDBOX_NOTREADY {
		t.Fatalf("paused state = %s", st.Status.State)
	}

	eng.cfg.NodeName = "node-b"
	_, err = eng.RunPodSandbox(context.Background(), podRequest())
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("remote resume code = %v, err = %v", status.Code(err), err)
	}
	eng.cfg.NodeName = "node-a"
	if _, err := eng.RunPodSandbox(context.Background(), podRequest()); err != nil {
		t.Fatal(err)
	}
	st, err = eng.PodSandboxStatus(context.Background(), &runtime.PodSandboxStatusRequest{PodSandboxId: "uid-1"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Status.Network.GetIp() != "10.1.0.3" || st.Status.State != runtime.PodSandboxState_SANDBOX_READY {
		t.Fatalf("resumed status = %+v", st.Status)
	}
	if fake.creates[1].Resume == nil || fake.creates[1].Resume.SandboxID != "sbx-1" || fake.creates[1].Resume.PauseSnapshotID != snap {
		t.Fatalf("resume request = %+v", fake.creates[1].Resume)
	}
}

func TestPauseFailureKeepsRunning(t *testing.T) {
	fake := &fakeClient{updateErr: status.Error(codes.Internal, "pause failed")}
	eng := newTestEngine(t, fake)
	if _, err := eng.RunPodSandbox(context.Background(), podRequest()); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Pause(context.Background(), "uid-1"); err == nil {
		t.Fatal("expected pause error")
	}
	st, err := eng.PodSandboxStatus(context.Background(), &runtime.PodSandboxStatusRequest{PodSandboxId: "uid-1"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Status.State != runtime.PodSandboxState_SANDBOX_READY {
		t.Fatalf("state = %s", st.Status.State)
	}
}

func TestPauseRemoveKeepsSnapshotForResume(t *testing.T) {
	fake := &fakeClient{}
	eng := newTestEngine(t, fake)
	if _, err := eng.RunPodSandbox(context.Background(), podRequest()); err != nil {
		t.Fatal(err)
	}
	snap, err := eng.Pause(context.Background(), "uid-1")
	if err != nil {
		t.Fatal(err)
	}
	// The agent issues Stop then Remove immediately after pause.
	if _, err := eng.StopPodSandbox(context.Background(), &runtime.StopPodSandboxRequest{PodSandboxId: "uid-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.RemovePodSandbox(context.Background(), &runtime.RemovePodSandboxRequest{PodSandboxId: "uid-1"}); err != nil {
		t.Fatal(err)
	}
	if len(fake.destroys) != 0 {
		t.Fatalf("pause cleanup destroyed the snapshot: %+v", fake.destroys)
	}
	req := podRequest()
	req.Config.Annotations[annTemplateID] = "csnap-268d988dc802"
	if _, err := eng.RunPodSandbox(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(fake.creates) != 2 || fake.creates[1].Resume == nil {
		t.Fatalf("resume create = %+v", fake.creates)
	}
	if fake.creates[1].Resume.PauseSnapshotID != snap || fake.creates[1].Cold != nil {
		t.Fatalf("resume used catalog id, request = %+v", fake.creates[1])
	}
}

func TestLaterRemoveOfPausedSandboxTombstones(t *testing.T) {
	orig := pauseDetachGrace
	pauseDetachGrace = 0
	t.Cleanup(func() { pauseDetachGrace = orig })

	fake := &fakeClient{}
	eng := newTestEngine(t, fake)
	if _, err := eng.RunPodSandbox(context.Background(), podRequest()); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Pause(context.Background(), "uid-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.StopPodSandbox(context.Background(), &runtime.StopPodSandboxRequest{PodSandboxId: "uid-1"}); err != nil {
		t.Fatal(err)
	}
	if len(fake.destroys) != 0 {
		t.Fatalf("stop parked, destroys = %+v", fake.destroys)
	}
	if _, err := eng.RemovePodSandbox(context.Background(), &runtime.RemovePodSandboxRequest{PodSandboxId: "uid-1"}); err != nil {
		t.Fatal(err)
	}
	if len(fake.destroys) != 1 || fake.destroys[0].ann[annDeleteTombstone] != "true" {
		t.Fatalf("destroys = %+v", fake.destroys)
	}
	st, err := eng.PodSandboxStatus(context.Background(), &runtime.PodSandboxStatusRequest{PodSandboxId: "uid-1"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Status.State != runtime.PodSandboxState_SANDBOX_NOTREADY {
		t.Fatalf("state = %s", st.Status.State)
	}
	if _, err := eng.RemovePodSandbox(context.Background(), &runtime.RemovePodSandboxRequest{PodSandboxId: "uid-1"}); err != nil {
		t.Fatal(err)
	}
	if len(fake.destroys) != 1 {
		t.Fatalf("second remove called cubelet, destroys = %+v", fake.destroys)
	}
}

func TestRunningDeleteStaysVisibleAsNotReady(t *testing.T) {
	fake := &fakeClient{}
	eng := newTestEngine(t, fake)
	if _, err := eng.RunPodSandbox(context.Background(), podRequest()); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.StopPodSandbox(context.Background(), &runtime.StopPodSandboxRequest{PodSandboxId: "uid-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.RemovePodSandbox(context.Background(), &runtime.RemovePodSandboxRequest{PodSandboxId: "uid-1"}); err != nil {
		t.Fatal(err)
	}
	if len(fake.destroys) != 1 || fake.destroys[0].ann[annDeleteTombstone] == "true" {
		t.Fatalf("destroys = %+v", fake.destroys)
	}
	listed, err := eng.ListPodSandbox(context.Background(), &runtime.ListPodSandboxRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Items) != 1 || listed.Items[0].State != runtime.PodSandboxState_SANDBOX_NOTREADY {
		t.Fatalf("list = %+v", listed.Items)
	}
}

func TestCatalogSnapshotWithoutLocalPauseIsRejected(t *testing.T) {
	fake := &fakeClient{}
	eng := newTestEngine(t, fake)
	req := podRequest()
	req.Config.Annotations[annTemplateID] = "csnap-268d988dc802"
	_, err := eng.RunPodSandbox(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "catalog snapshot") {
		t.Fatalf("err = %v", err)
	}
	if len(fake.creates) != 0 {
		t.Fatalf("create calls = %d", len(fake.creates))
	}
}

// ExecSync coverage lives in envd_exec_test.go (guest envd path).

func TestRestoreReconcilesPausedFromList(t *testing.T) {
	fake := &fakeClient{}
	eng := newTestEngine(t, fake)
	if _, err := eng.RunPodSandbox(context.Background(), podRequest()); err != nil {
		t.Fatal(err)
	}
	if err := eng.waitPersist(); err != nil {
		t.Fatal(err)
	}
	restored := newEngine(eng.cfg, &fakeClient{list: []listedSandbox{{ID: "sbx-1", Paused: true}}})
	t.Cleanup(func() { _ = restored.Close() })
	if err := restored.RestoreState(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, err := restored.PodSandboxStatus(context.Background(), &runtime.PodSandboxStatusRequest{PodSandboxId: "uid-1"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Status.State != runtime.PodSandboxState_SANDBOX_NOTREADY || st.Status.Network.GetIp() != "10.1.0.2" {
		t.Fatalf("restored status = %+v", st.Status)
	}
}

func TestRestoreDestroysUnknownCubeletSandbox(t *testing.T) {
	store, err := engine.NewJSONStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeClient{list: []listedSandbox{{ID: "orphan-sbx"}}}
	eng := newEngine(Config{
		Enabled:     true,
		CubeletAddr: "127.0.0.1:9999",
		NodeName:    "node-a",
		Timeout:     time.Second,
		StateStore:  store,
	}, fake)
	t.Cleanup(func() { _ = eng.Close() })
	if err := eng.RestoreState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fake.destroys) != 1 || fake.destroys[0].id != "orphan-sbx" {
		t.Fatalf("destroys = %+v", fake.destroys)
	}
}

func TestReconcileDestroysOrphanAfterGrace(t *testing.T) {
	fake := &fakeClient{list: []listedSandbox{{ID: "orphan-sbx"}}}
	eng := newEngine(Config{
		Enabled:     true,
		CubeletAddr: "127.0.0.1:9999",
		NodeName:    "node-a",
		Timeout:     time.Second,
		StateStore:  nil,
	}, fake)
	t.Cleanup(func() { _ = eng.Close() })

	if err := eng.Reconcile(context.Background(), engine.ReconcileOptions{GracePeriod: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if len(fake.destroys) != 0 {
		t.Fatalf("should honor grace, destroys=%+v", fake.destroys)
	}
	eng.orphanMu.Lock()
	eng.orphanSeen["orphan-sbx"] = time.Now().Add(-2 * time.Hour)
	eng.orphanMu.Unlock()
	if err := eng.Reconcile(context.Background(), engine.ReconcileOptions{GracePeriod: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if len(fake.destroys) != 1 || fake.destroys[0].id != "orphan-sbx" {
		t.Fatalf("destroys = %+v", fake.destroys)
	}
}

func TestAdminPauseCheckpointAndRuntime(t *testing.T) {
	eng := newTestEngine(t, &fakeClient{})
	if _, err := eng.RunPodSandbox(context.Background(), podRequest()); err != nil {
		t.Fatal(err)
	}
	ref := engine.SandboxRef{CRISandboxID: "uid-1", SandboxID: "sbx-1"}
	paused, err := eng.AdminPauseSandbox(context.Background(), ref, engine.AdminOpOptions{OperationID: "op-1", Timeout: time.Second})
	if err != nil || !paused.SnapshotPersisted {
		t.Fatalf("pause = %+v, %v", paused, err)
	}
	again, err := eng.AdminPauseSandbox(context.Background(), ref, engine.AdminOpOptions{OperationID: "op-1"})
	if err != nil || !again.SnapshotPersisted {
		t.Fatalf("replay pause = %+v, %v", again, err)
	}
	if _, err := eng.RunPodSandbox(context.Background(), podRequest()); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := eng.AdminCheckpointSandbox(context.Background(), ref, engine.AdminOpOptions{OperationID: "op-2"})
	if err != nil || !checkpoint.SnapshotPersisted || !checkpoint.VMResumed {
		t.Fatalf("checkpoint = %+v, %v", checkpoint, err)
	}
	rt, err := eng.AdminGetSandboxRuntime(context.Background(), engine.SandboxRef{SandboxID: "sbx-1"})
	if err != nil {
		t.Fatal(err)
	}
	if rt.SandboxID != "sbx-1" || rt.CRISandboxID != "uid-1" || rt.PodIP != "10.1.0.3" || rt.Extra["envd_port"] != "49983" {
		t.Fatalf("runtime = %+v", rt)
	}
}

func TestDisabledCubeRejectsRun(t *testing.T) {
	eng := newEngine(Config{Enabled: false, NodeName: "node-a"}, nil)
	t.Cleanup(func() { _ = eng.Close() })
	_, err := eng.RunPodSandbox(context.Background(), podRequest())
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, err = %v", status.Code(err), err)
	}
}

func TestHideSandboxLabelOmitsDirectFromCRIList(t *testing.T) {
	store, err := engine.NewJSONStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	eng := newEngine(Config{
		Enabled:     true,
		CubeletAddr: "127.0.0.1:9999",
		NodeName:    "node-a",
		Timeout:     time.Second,
		StateStore:  store,
		HideLabel:   "flux-sandbox.io/direct=true",
	}, &fakeClient{})
	attachStubEnvd(t, eng)
	t.Cleanup(func() { _ = eng.Close() })

	req := podRequest()
	req.Config.Labels["flux-sandbox.io/direct"] = "true"
	if _, err := eng.RunPodSandbox(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.CreateContainer(context.Background(), &runtime.CreateContainerRequest{
		PodSandboxId: "uid-1",
		Config: &runtime.ContainerConfig{
			Metadata: &runtime.ContainerMetadata{Name: "app"},
			Image:    &runtime.ImageSpec{Image: "cube.dev/ignored"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	listed, err := eng.ListPodSandbox(context.Background(), &runtime.ListPodSandboxRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Items) != 0 {
		t.Fatalf("empty list should hide direct sandbox, got %+v", listed.Items)
	}
	containers, err := eng.ListContainers(context.Background(), &runtime.ListContainersRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(containers.Containers) != 0 {
		t.Fatalf("empty container list should hide direct sandbox, got %+v", containers.Containers)
	}

	revealed, err := eng.ListPodSandbox(context.Background(), &runtime.ListPodSandboxRequest{
		Filter: &runtime.PodSandboxFilter{LabelSelector: map[string]string{"flux-sandbox.io/direct": "true"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(revealed.Items) != 1 || revealed.Items[0].Id != "uid-1" {
		t.Fatalf("selector reveal = %+v", revealed.Items)
	}
	st, err := eng.PodSandboxStatus(context.Background(), &runtime.PodSandboxStatusRequest{PodSandboxId: "uid-1"})
	if err != nil || st.Status.Id != "uid-1" {
		t.Fatalf("status by id must stay reachable: %+v, %v", st, err)
	}
}

func TestHideSandboxLabelIgnoresNonMatching(t *testing.T) {
	store, err := engine.NewJSONStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	eng := newEngine(Config{
		Enabled:     true,
		CubeletAddr: "127.0.0.1:9999",
		NodeName:    "node-a",
		Timeout:     time.Second,
		StateStore:  store,
		HideLabel:   "flux-sandbox.io/direct=true",
	}, &fakeClient{})
	attachStubEnvd(t, eng)
	t.Cleanup(func() { _ = eng.Close() })
	if _, err := eng.RunPodSandbox(context.Background(), podRequest()); err != nil {
		t.Fatal(err)
	}
	listed, err := eng.ListPodSandbox(context.Background(), &runtime.ListPodSandboxRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Items) != 1 {
		t.Fatalf("non-direct sandbox must stay visible, got %+v", listed.Items)
	}
}
