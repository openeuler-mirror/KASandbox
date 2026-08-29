package admin

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/cri-multiplex/pkg/engine"
	"github.com/cri-multiplex/pkg/orchestrator"
)

type fakeEngine struct {
	pauseErr      error
	checkpointErr error
	runtimeErr    error

	lastPauseOp      engine.E2BOperation
	lastCheckpointOp engine.E2BOperation
	pauseTimeout     uint64
	pauseCalls       int
	checkpointCalls  int
	runtimeCalls     int
	lastRuntimeCriID string
	lastRuntimeE2BID string

	createErr  error
	createResp *orchestrator.SandboxCreateResponse
	updateErr  error
	listResp   *orchestrator.SandboxListResponse
	listErr    error
	deleteErr  error
	buildsResp *orchestrator.SandboxListCachedBuildsResponse
	buildsErr  error

	createCalls int
	updateCalls int
	listCalls   int
	deleteCalls int
	buildsCalls int

	lastCreateReq *orchestrator.SandboxCreateRequest
	lastUpdateReq *orchestrator.SandboxUpdateRequest
	lastDeleteReq *orchestrator.SandboxDeleteRequest
}

func (f *fakeEngine) AdminPause(ctx context.Context, op engine.E2BOperation, timeoutSeconds uint64) (time.Time, time.Time, error) {
	f.pauseCalls++
	f.lastPauseOp = op
	f.pauseTimeout = timeoutSeconds
	if f.pauseErr != nil {
		return time.Time{}, time.Time{}, f.pauseErr
	}
	return time.Unix(100, 0), time.Unix(200, 0), nil
}

func (f *fakeEngine) AdminCheckpoint(ctx context.Context, op engine.E2BOperation, timeoutSeconds uint64) (bool, time.Time, time.Time, error) {
	f.checkpointCalls++
	f.lastCheckpointOp = op
	if f.checkpointErr != nil {
		return false, time.Time{}, time.Time{}, f.checkpointErr
	}
	return true, time.Unix(100, 0), time.Unix(200, 0), nil
}

func (f *fakeEngine) AdminGetRuntime(criID, e2bID string) (*engine.RuntimeInfo, []engine.E2BOperation, error) {
	f.runtimeCalls++
	f.lastRuntimeCriID = criID
	f.lastRuntimeE2BID = e2bID
	if f.runtimeErr != nil {
		return nil, nil, f.runtimeErr
	}
	return &engine.RuntimeInfo{
			CRISandboxID: "cri-a", E2BSandboxID: "e2b-a", TeamID: "team-a",
			PodName: "pod-a", PodNamespace: "ns-a", PodUID: "uid-a", NodeName: "node-a",
			TemplateID: "tmpl-a", BuildID: "build-a", ExecutionID: "exec-a",
			RuntimeState: "Running",
			CNI:          &engine.CNIRecord{NetNSPath: "/var/run/netns/x", IfName: "eth0", PodIP: "10.0.0.10", Gateway: "10.0.0.1", DNS: []string{"10.96.0.10"}},
			HostPorts:    []engine.PortMapping{{HostPort: 20001, SandboxPort: 49983}},
		}, []engine.E2BOperation{{
			OperationID: "op-1", Action: "Pause", CRISandboxID: "cri-a", E2BSandboxID: "e2b-a",
			TemplateID: "tmpl-snap", BuildID: "build-snap", State: engine.OperationStateRunning,
			StartedAt: time.Unix(100, 0),
		}}, nil
}

func (f *fakeEngine) AdminCreate(ctx context.Context, req *orchestrator.SandboxCreateRequest) (*orchestrator.SandboxCreateResponse, error) {
	f.createCalls++
	f.lastCreateReq = req
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.createResp != nil {
		return f.createResp, nil
	}
	return &orchestrator.SandboxCreateResponse{ClientId: "client-a", HostIp: "172.16.0.2"}, nil
}

func (f *fakeEngine) AdminUpdate(ctx context.Context, req *orchestrator.SandboxUpdateRequest) (*emptypb.Empty, error) {
	f.updateCalls++
	f.lastUpdateReq = req
	return &emptypb.Empty{}, f.updateErr
}

func (f *fakeEngine) AdminList(ctx context.Context) (*orchestrator.SandboxListResponse, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.listResp != nil {
		return f.listResp, nil
	}
	return &orchestrator.SandboxListResponse{}, nil
}

func (f *fakeEngine) AdminDelete(ctx context.Context, req *orchestrator.SandboxDeleteRequest) (*emptypb.Empty, error) {
	f.deleteCalls++
	f.lastDeleteReq = req
	return &emptypb.Empty{}, f.deleteErr
}

func (f *fakeEngine) AdminListCachedBuilds(ctx context.Context) (*orchestrator.SandboxListCachedBuildsResponse, error) {
	f.buildsCalls++
	if f.buildsErr != nil {
		return nil, f.buildsErr
	}
	if f.buildsResp != nil {
		return f.buildsResp, nil
	}
	return &orchestrator.SandboxListCachedBuildsResponse{}, nil
}

func TestPauseSandboxValidation(t *testing.T) {
	eng := &fakeEngine{pauseErr: status.Error(codes.InvalidArgument, "operation_id, cri_sandbox_id and e2b_sandbox_id are required")}
	s := NewServer("/tmp/unused.sock", eng)
	if _, err := s.PauseSandbox(context.Background(), &PauseSandboxRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty request code = %v, want InvalidArgument", status.Code(err))
	}
	if eng.pauseCalls != 1 {
		t.Fatalf("server should delegate validation to engine, calls=%d", eng.pauseCalls)
	}
}

func TestPauseSandboxDelegatesToEngine(t *testing.T) {
	eng := &fakeEngine{}
	s := NewServer("/tmp/unused.sock", eng)
	resp, err := s.PauseSandbox(context.Background(), &PauseSandboxRequest{
		OperationId: "op-1", CriSandboxId: "cri-a", E2BSandboxId: "e2b-a", TeamId: "team-a",
		TemplateId: "tmpl-snap", BuildId: "build-snap", TimeoutSeconds: 30,
	})
	if err != nil {
		t.Fatalf("PauseSandbox: %v", err)
	}
	if !resp.SnapshotPersisted || resp.StartedAtUnix != 100 || resp.FinishedAtUnix != 200 {
		t.Fatalf("response mismatch: %+v", resp)
	}
	if eng.pauseCalls != 1 || eng.lastPauseOp.OperationID != "op-1" || eng.lastPauseOp.TeamID != "team-a" ||
		eng.lastPauseOp.TemplateID != "tmpl-snap" || eng.lastPauseOp.BuildID != "build-snap" || eng.pauseTimeout != 30 {
		t.Fatalf("engine op mismatch: %+v timeout=%d", eng.lastPauseOp, eng.pauseTimeout)
	}
}

func TestPauseSandboxErrorPassthrough(t *testing.T) {
	eng := &fakeEngine{pauseErr: status.Error(codes.Aborted, "conflict")}
	s := NewServer("/tmp/unused.sock", eng)
	_, err := s.PauseSandbox(context.Background(), &PauseSandboxRequest{
		OperationId: "op-1", CriSandboxId: "cri-a", E2BSandboxId: "e2b-a", TeamId: "team-a",
		TemplateId: "tmpl-snap", BuildId: "build-snap",
	})
	if status.Code(err) != codes.Aborted {
		t.Fatalf("code = %v, want Aborted", status.Code(err))
	}
}

func TestCheckpointSandboxDelegates(t *testing.T) {
	eng := &fakeEngine{}
	s := NewServer("/tmp/unused.sock", eng)
	resp, err := s.CheckpointSandbox(context.Background(), &CheckpointSandboxRequest{
		OperationId: "op-1", CriSandboxId: "cri-a", E2BSandboxId: "e2b-a", TeamId: "team-a", BuildId: "build-snap",
	})
	if err != nil {
		t.Fatalf("CheckpointSandbox: %v", err)
	}
	if !resp.SnapshotPersisted || !resp.VmResumed || resp.StartedAtUnix != 100 || resp.FinishedAtUnix != 200 {
		t.Fatalf("response mismatch: %+v", resp)
	}
	if eng.lastCheckpointOp.TemplateID != "" {
		t.Fatalf("checkpoint op should not carry template id: %+v", eng.lastCheckpointOp)
	}
}

func TestGetSandboxRuntimeMapping(t *testing.T) {
	eng := &fakeEngine{}
	s := NewServer("/tmp/unused.sock", eng)

	if _, err := s.GetSandboxRuntime(context.Background(), &GetSandboxRuntimeRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty target code = %v, want InvalidArgument", status.Code(err))
	}

	resp, err := s.GetSandboxRuntime(context.Background(), &GetSandboxRuntimeRequest{
		Target: &GetSandboxRuntimeRequest_E2BSandboxId{E2BSandboxId: "e2b-a"},
	})
	if err != nil {
		t.Fatalf("GetSandboxRuntime: %v", err)
	}
	if eng.lastRuntimeE2BID != "e2b-a" || eng.lastRuntimeCriID != "" {
		t.Fatalf("engine target mismatch: cri=%q e2b=%q", eng.lastRuntimeCriID, eng.lastRuntimeE2BID)
	}
	if resp.CriSandboxId != "cri-a" || resp.E2BSandboxId != "e2b-a" || resp.TeamId != "team-a" ||
		resp.PodName != "pod-a" || resp.PodNamespace != "ns-a" || resp.PodUid != "uid-a" ||
		resp.NodeName != "node-a" || resp.TemplateId != "tmpl-a" || resp.BuildId != "build-a" ||
		resp.ExecutionId != "exec-a" || resp.RuntimeState != "Running" {
		t.Fatalf("response mismatch: %+v", resp)
	}
	if resp.Cni == nil || resp.Cni.NetnsPath != "/var/run/netns/x" || resp.Cni.IfName != "eth0" ||
		resp.Cni.PodIp != "10.0.0.10" || resp.Cni.Gateway != "10.0.0.1" ||
		len(resp.Cni.DnsServers) != 1 || resp.Cni.DnsServers[0] != "10.96.0.10" {
		t.Fatalf("cni mismatch: %+v", resp.Cni)
	}
	if len(resp.Hostports) != 1 || resp.Hostports[0].HostPort != 20001 || resp.Hostports[0].SandboxPort != 49983 {
		t.Fatalf("hostports mismatch: %+v", resp.Hostports)
	}
	if len(resp.ActiveOperations) != 1 || resp.ActiveOperations[0].OperationId != "op-1" ||
		resp.ActiveOperations[0].State != engine.OperationStateRunning || resp.ActiveOperations[0].StartedAtUnix != 100 {
		t.Fatalf("active operations mismatch: %+v", resp.ActiveOperations)
	}

	// cri target
	if _, err := s.GetSandboxRuntime(context.Background(), &GetSandboxRuntimeRequest{
		Target: &GetSandboxRuntimeRequest_CriSandboxId{CriSandboxId: "cri-a"},
	}); err != nil {
		t.Fatalf("GetSandboxRuntime by cri id: %v", err)
	}
	if eng.lastRuntimeCriID != "cri-a" || eng.lastRuntimeE2BID != "" {
		t.Fatalf("engine target mismatch: cri=%q e2b=%q", eng.lastRuntimeCriID, eng.lastRuntimeE2BID)
	}
}

func TestE2BSandboxServiceDelegatesToEngine(t *testing.T) {
	eng := &fakeEngine{
		listResp:   &orchestrator.SandboxListResponse{Sandboxes: []*orchestrator.RunningSandbox{{ClientId: "client-a"}}},
		buildsResp: &orchestrator.SandboxListCachedBuildsResponse{Builds: []*orchestrator.CachedBuildInfo{{BuildId: "build-a"}}},
	}
	s := NewServer("/tmp/unused.sock", eng)
	ctx := context.Background()

	createReq := &orchestrator.SandboxCreateRequest{Sandbox: &orchestrator.SandboxConfig{
		SandboxId: "sbx-a", TemplateId: "tmpl-a", BuildId: "build-a", TeamId: "team-a",
	}}
	createResp, err := s.Create(ctx, createReq)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if eng.createCalls != 1 || eng.lastCreateReq != createReq {
		t.Fatalf("create delegation mismatch: calls=%d req=%+v", eng.createCalls, eng.lastCreateReq)
	}
	if createResp.ClientId != "client-a" || createResp.HostIp != "172.16.0.2" {
		t.Fatalf("create response mismatch: %+v", createResp)
	}

	updateReq := &orchestrator.SandboxUpdateRequest{SandboxId: "sbx-a"}
	if _, err := s.Update(ctx, updateReq); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if eng.updateCalls != 1 || eng.lastUpdateReq != updateReq {
		t.Fatalf("update delegation mismatch: calls=%d req=%+v", eng.updateCalls, eng.lastUpdateReq)
	}

	listResp, err := s.List(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if eng.listCalls != 1 || len(listResp.Sandboxes) != 1 || listResp.Sandboxes[0].ClientId != "client-a" {
		t.Fatalf("list delegation mismatch: calls=%d resp=%+v", eng.listCalls, listResp)
	}

	deleteReq := &orchestrator.SandboxDeleteRequest{SandboxId: "sbx-a"}
	if _, err := s.Delete(ctx, deleteReq); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if eng.deleteCalls != 1 || eng.lastDeleteReq != deleteReq {
		t.Fatalf("delete delegation mismatch: calls=%d req=%+v", eng.deleteCalls, eng.lastDeleteReq)
	}

	buildsResp, err := s.ListCachedBuilds(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatalf("ListCachedBuilds: %v", err)
	}
	if eng.buildsCalls != 1 || len(buildsResp.Builds) != 1 || buildsResp.Builds[0].BuildId != "build-a" {
		t.Fatalf("builds delegation mismatch: calls=%d resp=%+v", eng.buildsCalls, buildsResp)
	}
}

func TestE2BSandboxServiceErrorPassthrough(t *testing.T) {
	eng := &fakeEngine{createErr: status.Error(codes.InvalidArgument, "config.sandbox_id is required")}
	s := NewServer("/tmp/unused.sock", eng)
	if _, err := s.Create(context.Background(), &orchestrator.SandboxCreateRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("create error code = %v, want InvalidArgument", status.Code(err))
	}
}
