package engine

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cri-multiplex/pkg/orchestrator"
)

func adminCreateReq(sandboxID string) *orchestrator.SandboxCreateRequest {
	alias := "alias-a"
	token := "tok-1"
	return &orchestrator.SandboxCreateRequest{Sandbox: &orchestrator.SandboxConfig{
		SandboxId:       sandboxID,
		TemplateId:      "tmpl-a",
		BuildId:         "build-a",
		TeamId:          "team-a",
		Alias:           &alias,
		EnvdAccessToken: &token,
		Metadata: map[string]string{
			annExposePorts: "49983,8080",
		},
	}}
}

func TestAdminCreateFullLifecycle(t *testing.T) {
	client := &fakeSandboxServiceClient{createResp: &orchestrator.SandboxCreateResponse{
		ClientId: "client-a",
		HostIp:   "172.16.0.2",
	}}
	fakeCNI := &fakeCNIManager{addRecord: &CNIRecord{
		SandboxID: "sbx-1",
		Network:   "calico",
		IfName:    "eth0",
		NetNSPath: "/var/run/netns/e2b-sbx-1",
		PodIP:     "10.0.0.20",
		Gateway:   "10.0.0.1",
		DNS:       []string{"10.96.0.10"},
	}}
	var setupCalls []PortMapping
	store, err := NewJSONStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewJSONStateStore: %v", err)
	}
	e := newTestGRPCE2BEngine(client)
	e.cniConfig.Enabled = true
	e.cniManager = fakeCNI
	e.stateStore = store
	e.hostPortOps = hostPortMappingOps{
		setup: func(nodeIP string, hostPort int, sandboxIP string, sandboxPort int) error {
			setupCalls = append(setupCalls, PortMapping{HostPort: hostPort, SandboxPort: sandboxPort})
			return nil
		},
		cleanup: func(nodeIP string, hostPort int, sandboxIP string, sandboxPort int) error { return nil },
	}

	fixedEnd := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	req := adminCreateReq("sbx-1")
	req.EndTime = timestamppb.New(fixedEnd)

	resp, err := e.AdminCreate(context.Background(), req)
	if err != nil {
		t.Fatalf("AdminCreate: %v", err)
	}
	// 出参原样返回 orchestrator 响应
	if resp.ClientId != "client-a" || resp.HostIp != "172.16.0.2" {
		t.Fatalf("create response mismatch: %+v", resp)
	}
	// CNI ADD 被调用且 RuntimeNetwork 注入
	if fakeCNI.addCalls != 1 || fakeCNI.addIDs[0] != "sbx-1" {
		t.Fatalf("CNI add mismatch: calls=%d ids=%v", fakeCNI.addCalls, fakeCNI.addIDs)
	}
	rn := client.lastCreate.Sandbox.RuntimeNetwork
	if rn == nil || rn.Mode != orchestrator.SandboxRuntimeNetworkConfig_CNI_EXTERNAL_NETNS ||
		rn.PodIp != "10.0.0.20" || rn.NetnsPath != "/var/run/netns/e2b-sbx-1" {
		t.Fatalf("runtime network mismatch: %+v", rn)
	}
	// end_time 优先入参
	if !client.lastCreate.EndTime.AsTime().Equal(fixedEnd) {
		t.Fatalf("end_time = %v, want %v", client.lastCreate.EndTime.AsTime(), fixedEnd)
	}
	// HostPort 按 metadata["e2b.dev/expose-ports"] 分配
	if len(setupCalls) != 2 {
		t.Fatalf("hostport setup calls = %d, want 2: %+v", len(setupCalls), setupCalls)
	}
	// tracker 登记：cri/e2b id 同值，name=alias，namespace="e2b-admin"，labels=nil
	pod, ok := e.tracker.Get("sbx-1")
	if !ok {
		t.Fatal("tracker should contain sbx-1")
	}
	if pod.e2bSandboxID != "sbx-1" || pod.podUID != "sbx-1" || pod.name != "alias-a" ||
		pod.namespace != adminSandboxNamespace || pod.labels != nil ||
		pod.templateID != "tmpl-a" || pod.buildID != "build-a" || pod.teamID != "team-a" ||
		pod.envdAccessToken != "tok-1" || pod.state != stateRunning {
		t.Fatalf("tracker pod mismatch: %+v", pod)
	}
	if pod.hostPort == 0 || len(pod.portMappings) != 2 {
		t.Fatalf("pod hostport fields mismatch: hostPort=%d mappings=%+v", pod.hostPort, pod.portMappings)
	}
	// stateStore 登记
	pods, err := store.LoadE2BPods()
	if err != nil || len(pods) != 1 || pods[0].SandboxID != "sbx-1" {
		t.Fatalf("stateStore pods = %+v err=%v", pods, err)
	}
}

func TestAdminCreateDefaultEndTimeFallback(t *testing.T) {
	client := &fakeSandboxServiceClient{}
	e := newTestGRPCE2BEngine(client)

	before := time.Now()
	if _, err := e.AdminCreate(context.Background(), adminCreateReq("sbx-1")); err != nil {
		t.Fatalf("AdminCreate: %v", err)
	}
	after := time.Now()
	// 未设置 end_time 时回落到 startTime + MaxSandboxLength（默认 24h，与 CRI 路径一致）
	got := client.lastCreate.EndTime.AsTime()
	lo := before.Add(time.Duration(defaultSandboxConfig.MaxSandboxLength) * time.Hour)
	hi := after.Add(time.Duration(defaultSandboxConfig.MaxSandboxLength) * time.Hour)
	if got.Before(lo) || got.After(hi) {
		t.Fatalf("end_time = %v, want within [%v, %v]", got, lo, hi)
	}
	if client.lastCreate.Sandbox.MaxSandboxLength != defaultSandboxConfig.MaxSandboxLength {
		t.Fatalf("max_sandbox_length = %d, want default %d",
			client.lastCreate.Sandbox.MaxSandboxLength, defaultSandboxConfig.MaxSandboxLength)
	}
}

func TestAdminCreateValidation(t *testing.T) {
	client := &fakeSandboxServiceClient{}
	e := newTestGRPCE2BEngine(client)

	if _, err := e.AdminCreate(context.Background(), &orchestrator.SandboxCreateRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("nil sandbox code = %v, want InvalidArgument", status.Code(err))
	}
	if _, err := e.AdminCreate(context.Background(), &orchestrator.SandboxCreateRequest{
		Sandbox: &orchestrator.SandboxConfig{TemplateId: "t", BuildId: "b", TeamId: "team"},
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty sandbox_id code = %v, want InvalidArgument", status.Code(err))
	}
	if _, err := e.AdminCreate(context.Background(), &orchestrator.SandboxCreateRequest{
		Sandbox: &orchestrator.SandboxConfig{SandboxId: "sbx-1"},
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing template/build/team code = %v, want InvalidArgument", status.Code(err))
	}
	if client.createCalls != 0 {
		t.Fatalf("validation failures must not reach orchestrator, create calls = %d", client.createCalls)
	}
}

func TestAdminCreateCNIRollbackOnCreateFailure(t *testing.T) {
	client := &fakeSandboxServiceClient{createErr: status.Error(codes.Internal, "create failed")}
	fakeCNI := &fakeCNIManager{}
	e := newTestGRPCE2BEngine(client)
	e.cniConfig.Enabled = true
	e.cniManager = fakeCNI

	if _, err := e.AdminCreate(context.Background(), adminCreateReq("sbx-1")); status.Code(err) != codes.Internal {
		t.Fatalf("AdminCreate error code = %v, want Internal", status.Code(err))
	}
	if fakeCNI.addCalls != 1 || fakeCNI.delCalls != 1 {
		t.Fatalf("CNI calls = add:%d del:%d, want 1/1", fakeCNI.addCalls, fakeCNI.delCalls)
	}
	if _, ok := e.tracker.Get("sbx-1"); ok {
		t.Fatal("tracker should not contain failed sandbox")
	}
}

func TestAdminCreateIdempotentExisting(t *testing.T) {
	client := &fakeSandboxServiceClient{listResp: &orchestrator.SandboxListResponse{
		Sandboxes: []*orchestrator.RunningSandbox{{Config: &orchestrator.SandboxConfig{SandboxId: "sbx-1"}}},
	}}
	e := newTestGRPCE2BEngine(client)
	e.tracker.Add("sbx-1", &podInfo{
		sandboxID:    "sbx-1",
		e2bSandboxID: "sbx-1",
		state:        stateRunning,
		hostIP:       "172.16.0.9",
	})

	resp, err := e.AdminCreate(context.Background(), adminCreateReq("sbx-1"))
	if err != nil {
		t.Fatalf("AdminCreate: %v", err)
	}
	if client.createCalls != 0 {
		t.Fatalf("idempotent hit must not re-create, create calls = %d", client.createCalls)
	}
	if resp.HostIp != "172.16.0.9" {
		t.Fatalf("idempotent response host_ip = %q", resp.HostIp)
	}
}

func TestAdminDeleteFullCleanup(t *testing.T) {
	client := &fakeSandboxServiceClient{}
	fakeCNI := &fakeCNIManager{}
	var cleanupCalls []PortMapping
	store, err := NewJSONStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewJSONStateStore: %v", err)
	}
	e := newTestGRPCE2BEngine(client)
	e.cniConfig.Enabled = true
	e.cniManager = fakeCNI
	e.stateStore = store
	e.hostPortOps = hostPortMappingOps{
		setup: func(nodeIP string, hostPort int, sandboxIP string, sandboxPort int) error { return nil },
		cleanup: func(nodeIP string, hostPort int, sandboxIP string, sandboxPort int) error {
			cleanupCalls = append(cleanupCalls, PortMapping{HostPort: hostPort, SandboxPort: sandboxPort})
			return nil
		},
	}
	pod := &podInfo{
		sandboxID:    "sbx-1",
		e2bSandboxID: "sbx-1",
		state:        stateRunning,
		hostIP:       "172.16.0.2",
		portMappings: []PortMapping{{HostPort: 20001, SandboxPort: 49983}, {HostPort: 20002, SandboxPort: 8080}},
		cniEnabled:   true,
		cniRecord:    &CNIRecord{SandboxID: "sbx-1", NetNSPath: "/var/run/netns/e2b-sbx-1"},
	}
	e.tracker.Add("sbx-1", pod)
	if err := store.SaveE2BPod(pod.toPersistedState()); err != nil {
		t.Fatalf("SaveE2BPod: %v", err)
	}

	if _, err := e.AdminDelete(context.Background(), &orchestrator.SandboxDeleteRequest{SandboxId: "sbx-1"}); err != nil {
		t.Fatalf("AdminDelete: %v", err)
	}
	if client.deleteCalls != 1 || client.lastDelete.SandboxId != "sbx-1" {
		t.Fatalf("orchestrator delete mismatch: calls=%d req=%+v", client.deleteCalls, client.lastDelete)
	}
	if fakeCNI.delCalls != 1 {
		t.Fatalf("CNI del calls = %d, want 1", fakeCNI.delCalls)
	}
	if len(cleanupCalls) != 2 {
		t.Fatalf("hostport cleanup calls = %d, want 2: %+v", len(cleanupCalls), cleanupCalls)
	}
	if _, ok := e.tracker.Get("sbx-1"); ok {
		t.Fatal("tracker should delete pod")
	}
	pods, err := store.LoadE2BPods()
	if err != nil || len(pods) != 0 {
		t.Fatalf("stateStore pods after delete = %+v err=%v", pods, err)
	}

	// 重复删除返回 OK 且不再触碰 orchestrator/CNI
	if _, err := e.AdminDelete(context.Background(), &orchestrator.SandboxDeleteRequest{SandboxId: "sbx-1"}); err != nil {
		t.Fatalf("repeat AdminDelete: %v", err)
	}
	if client.deleteCalls != 1 || fakeCNI.delCalls != 1 {
		t.Fatalf("repeat delete should be a no-op: delete=%d cniDel=%d", client.deleteCalls, fakeCNI.delCalls)
	}
}

func TestAdminDeleteNotFoundReturnsOK(t *testing.T) {
	client := &fakeSandboxServiceClient{}
	store, err := NewJSONStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewJSONStateStore: %v", err)
	}
	e := newTestGRPCE2BEngine(client)
	e.stateStore = store

	if _, err := e.AdminDelete(context.Background(), &orchestrator.SandboxDeleteRequest{SandboxId: "ghost"}); err != nil {
		t.Fatalf("AdminDelete of unknown sandbox: %v", err)
	}
	if client.deleteCalls != 0 {
		t.Fatalf("unknown sandbox must not call orchestrator delete, calls = %d", client.deleteCalls)
	}
	if _, err := e.AdminDelete(context.Background(), &orchestrator.SandboxDeleteRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty sandbox_id code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestAdminDeleteBlocksOnOperationLock(t *testing.T) {
	client := &fakeSandboxServiceClient{}
	e := newTestGRPCE2BEngine(client)
	e.tracker.Add("sbx-1", &podInfo{sandboxID: "sbx-1", e2bSandboxID: "sbx-1", state: stateRunning})

	// 模拟进行中的 AdminPause/Checkpoint 持有 sandbox 操作锁
	mu, locked := e.tryLockSandbox("sbx-1")
	if !locked {
		t.Fatal("tryLockSandbox should succeed")
	}

	done := make(chan error, 1)
	go func() {
		_, err := e.AdminDelete(context.Background(), &orchestrator.SandboxDeleteRequest{SandboxId: "sbx-1"})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("AdminDelete should block on the operation lock, returned early: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	mu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("AdminDelete after lock release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AdminDelete did not finish after lock release")
	}
	if client.deleteCalls != 1 {
		t.Fatalf("orchestrator delete calls = %d, want 1", client.deleteCalls)
	}
	if _, ok := e.tracker.Get("sbx-1"); ok {
		t.Fatal("tracker should delete pod")
	}
}

func TestAdminDeleteLockWaitCanceled(t *testing.T) {
	e := newTestGRPCE2BEngine(&fakeSandboxServiceClient{})
	e.tracker.Add("sbx-1", &podInfo{sandboxID: "sbx-1", e2bSandboxID: "sbx-1", state: stateRunning})

	mu, locked := e.tryLockSandbox("sbx-1")
	if !locked {
		t.Fatal("tryLockSandbox should succeed")
	}
	defer mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := e.AdminDelete(ctx, &orchestrator.SandboxDeleteRequest{SandboxId: "sbx-1"}); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("lock wait timeout code = %v, want DeadlineExceeded", status.Code(err))
	}
}

func TestAdminUpdateListCachedBuildsForwarding(t *testing.T) {
	client := &fakeSandboxServiceClient{
		listResp:   &orchestrator.SandboxListResponse{Sandboxes: []*orchestrator.RunningSandbox{{ClientId: "client-a"}}},
		buildsResp: &orchestrator.SandboxListCachedBuildsResponse{Builds: []*orchestrator.CachedBuildInfo{{BuildId: "build-a"}}},
	}
	e := newTestGRPCE2BEngine(client)
	ctx := context.Background()

	endTime := timestamppb.New(time.Now().Add(time.Hour))
	upd := &orchestrator.SandboxUpdateRequest{SandboxId: "sbx-1", EndTime: endTime}
	if _, err := e.AdminUpdate(ctx, upd); err != nil {
		t.Fatalf("AdminUpdate: %v", err)
	}
	if client.updateCalls != 1 || client.lastUpdate != upd {
		t.Fatalf("update forwarding mismatch: calls=%d req=%+v", client.updateCalls, client.lastUpdate)
	}

	list, err := e.AdminList(ctx)
	if err != nil {
		t.Fatalf("AdminList: %v", err)
	}
	if client.listCalls != 1 || len(list.Sandboxes) != 1 || list.Sandboxes[0].ClientId != "client-a" {
		t.Fatalf("list forwarding mismatch: calls=%d resp=%+v", client.listCalls, list)
	}

	builds, err := e.AdminListCachedBuilds(ctx)
	if err != nil {
		t.Fatalf("AdminListCachedBuilds: %v", err)
	}
	if client.buildsCalls != 1 || len(builds.Builds) != 1 || builds.Builds[0].BuildId != "build-a" {
		t.Fatalf("builds forwarding mismatch: calls=%d resp=%+v", client.buildsCalls, builds)
	}

	// 错误按 mapE2BError 映射透传
	errClient := &fakeSandboxServiceClient{updateErr: status.Error(codes.NotFound, "sandbox missing")}
	e2 := newTestGRPCE2BEngine(errClient)
	if _, err := e2.AdminUpdate(ctx, upd); status.Code(err) != codes.NotFound {
		t.Fatalf("update error code = %v, want NotFound", status.Code(err))
	}
}
