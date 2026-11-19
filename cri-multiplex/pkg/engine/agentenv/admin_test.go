package agentenv

import (
	"context"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	runtimeapi "github.com/cri-multiplex/pkg/agentenv/runtime/v1"
	core "github.com/cri-multiplex/pkg/engine"
)

type fakeLifecycle struct {
	create func(context.Context, *runtimeapi.SandboxCreateRequest) (*runtimeapi.SandboxCreateResponse, error)
	get    func(context.Context, *runtimeapi.SandboxGetRequest) (*runtimeapi.SandboxInfo, error)
	list   func(context.Context, *runtimeapi.SandboxListRequest) (*runtimeapi.SandboxListResponse, error)
	update func(context.Context, *runtimeapi.SandboxUpdateRequest) (*runtimeapi.SandboxInfo, error)
	delete func(context.Context, *runtimeapi.SandboxDeleteRequest) (*runtimeapi.OperationStatus, error)
}

func (f *fakeLifecycle) Create(ctx context.Context, req *runtimeapi.SandboxCreateRequest, _ ...grpc.CallOption) (*runtimeapi.SandboxCreateResponse, error) {
	if f.create == nil {
		return &runtimeapi.SandboxCreateResponse{}, nil
	}
	return f.create(ctx, req)
}

func (f *fakeLifecycle) Get(ctx context.Context, req *runtimeapi.SandboxGetRequest, _ ...grpc.CallOption) (*runtimeapi.SandboxInfo, error) {
	if f.get == nil {
		return nil, status.Error(codes.NotFound, "not found")
	}
	return f.get(ctx, req)
}

func (f *fakeLifecycle) List(ctx context.Context, req *runtimeapi.SandboxListRequest, _ ...grpc.CallOption) (*runtimeapi.SandboxListResponse, error) {
	if f.list == nil {
		return &runtimeapi.SandboxListResponse{}, nil
	}
	return f.list(ctx, req)
}

func (f *fakeLifecycle) Update(ctx context.Context, req *runtimeapi.SandboxUpdateRequest, _ ...grpc.CallOption) (*runtimeapi.SandboxInfo, error) {
	if f.update == nil {
		return &runtimeapi.SandboxInfo{}, nil
	}
	return f.update(ctx, req)
}

func (f *fakeLifecycle) Delete(ctx context.Context, req *runtimeapi.SandboxDeleteRequest, _ ...grpc.CallOption) (*runtimeapi.OperationStatus, error) {
	if f.delete == nil {
		return successfulDeleteOperation(req), nil
	}
	return f.delete(ctx, req)
}

func successfulDeleteOperation(req *runtimeapi.SandboxDeleteRequest) *runtimeapi.OperationStatus {
	return &runtimeapi.OperationStatus{
		OperationId: req.GetOperationId(),
		Action:      "delete",
		State:       "SUCCEEDED",
	}
}

type fakeAdmin struct {
	pause        func(context.Context, *runtimeapi.SandboxPauseRequest) (*runtimeapi.SandboxPauseResponse, error)
	checkpoint   func(context.Context, *runtimeapi.SandboxCheckpointRequest) (*runtimeapi.CheckpointResult, error)
	getRuntime   func(context.Context, *runtimeapi.SandboxGetRuntimeRequest) (*runtimeapi.SandboxRuntimeInfo, error)
	capabilities func(context.Context, *emptypb.Empty) (*runtimeapi.SandboxCapabilities, error)
}

func (f *fakeAdmin) Pause(ctx context.Context, req *runtimeapi.SandboxPauseRequest, _ ...grpc.CallOption) (*runtimeapi.SandboxPauseResponse, error) {
	if f.pause == nil {
		return &runtimeapi.SandboxPauseResponse{}, nil
	}
	return f.pause(ctx, req)
}
func (f *fakeAdmin) Checkpoint(ctx context.Context, req *runtimeapi.SandboxCheckpointRequest, _ ...grpc.CallOption) (*runtimeapi.CheckpointResult, error) {
	if f.checkpoint == nil {
		return &runtimeapi.CheckpointResult{}, nil
	}
	return f.checkpoint(ctx, req)
}

func (f *fakeAdmin) GetRuntime(ctx context.Context, req *runtimeapi.SandboxGetRuntimeRequest, _ ...grpc.CallOption) (*runtimeapi.SandboxRuntimeInfo, error) {
	if f.getRuntime == nil {
		path, _ := os.Executable()
		return &runtimeapi.SandboxRuntimeInfo{NativeSandboxId: req.GetNativeSandboxId(), Phase: runtimeapi.SandboxPhase_RUNNING, NetnsPath: path, Network: map[string]string{"mode": runtimeManagedNetworkMode, "pod_ip": "10.244.0.5"}}, nil
	}
	return f.getRuntime(ctx, req)
}

func (f *fakeAdmin) Capabilities(ctx context.Context, req *emptypb.Empty, _ ...grpc.CallOption) (*runtimeapi.SandboxCapabilities, error) {
	if f.capabilities == nil {
		return &runtimeapi.SandboxCapabilities{}, nil
	}
	return f.capabilities(ctx, req)
}

func newTestEngine(t *testing.T, lifecycle lifecycleAPI, admin adminAPI) (*Engine, *core.JSONStateStore) {
	t.Helper()
	store, err := core.NewJSONStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewJSONStateStore: %v", err)
	}
	return newWithClients(Config{RuntimeAddress: "passthrough:///agentenv-test", NodeIP: "10.0.0.1"}, store, lifecycle, admin), store
}

func TestAdminPauseUsesNativeIDAndValidatesSucceededOperation(t *testing.T) {
	const nativeID = "019a0000-0000-7000-8000-000000000721"
	var captured *runtimeapi.SandboxPauseRequest
	admin := &fakeAdmin{pause: func(_ context.Context, req *runtimeapi.SandboxPauseRequest) (*runtimeapi.SandboxPauseResponse, error) {
		captured = req
		return &runtimeapi.SandboxPauseResponse{Operation: &runtimeapi.OperationStatus{
			OperationId: req.GetOperationId(),
			Action:      "pause",
			State:       "SUCCEEDED",
		}}, nil
	}}
	instance, _ := newTestEngine(t, &fakeLifecycle{}, admin)
	if err := instance.putPod(podRecord{CRISandboxID: "product-pause",
		NativeSandboxID: nativeID,
	}); err != nil {
		t.Fatalf("seed route: %v", err)
	}

	result, err := instance.AdminPauseSandbox(
		context.Background(),
		core.SandboxRef{SandboxID: "product-pause"},
		core.AdminOpOptions{OperationID: "pause-op-1", Timeout: 2 * time.Second},
	)
	if err != nil {
		t.Fatalf("AdminPauseSandbox: %v", err)
	}
	if !result.SnapshotPersisted || result.VMResumed {
		t.Fatalf("pause result mismatch: %+v", result)
	}
	if captured.GetNativeSandboxId() != nativeID || captured.GetOperationId() != "pause-op-1" || captured.GetTimeoutSeconds() != 2 {
		t.Fatalf("pause request mismatch: %+v", captured)
	}
}

func TestAdminPauseRejectsUnknownOperation(t *testing.T) {
	const nativeID = "019a0000-0000-7000-8000-000000000722"
	admin := &fakeAdmin{pause: func(_ context.Context, req *runtimeapi.SandboxPauseRequest) (*runtimeapi.SandboxPauseResponse, error) {
		return &runtimeapi.SandboxPauseResponse{Operation: &runtimeapi.OperationStatus{
			OperationId:  req.GetOperationId(),
			Action:       "pause",
			State:        "UNKNOWN",
			ErrorCode:    "OUTCOME_UNKNOWN",
			ErrorMessage: "pause outcome is unknown",
		}}, nil
	}}
	instance, _ := newTestEngine(t, &fakeLifecycle{}, admin)
	if err := instance.putPod(podRecord{CRISandboxID: "product-pause-unknown",
		NativeSandboxID: nativeID,
	}); err != nil {
		t.Fatalf("seed route: %v", err)
	}

	_, err := instance.AdminPauseSandbox(
		context.Background(),
		core.SandboxRef{SandboxID: "product-pause-unknown"},
		core.AdminOpOptions{OperationID: "pause-op-unknown"},
	)
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("AdminPauseSandbox code = %v, want Unavailable; err=%v", status.Code(err), err)
	}
}
func TestAdminCheckpointUsesNativeIDAndRequiresRunningSource(t *testing.T) {
	const nativeID = "019a0000-0000-7000-8000-000000000724"
	var captured *runtimeapi.SandboxCheckpointRequest
	admin := &fakeAdmin{checkpoint: func(_ context.Context, req *runtimeapi.SandboxCheckpointRequest) (*runtimeapi.CheckpointResult, error) {
		captured = req
		return &runtimeapi.CheckpointResult{
			NativeSandboxId: req.GetNativeSandboxId(),
			SnapshotRef:     "snapshot-ref-1",
			Published:       true,
			CapturesMemory:  true,
			CapturesRootfs:  true,
			SourcePhase:     runtimeapi.SandboxPhase_RUNNING,
		}, nil
	}}
	instance, _ := newTestEngine(t, &fakeLifecycle{}, admin)
	if err := instance.putPod(podRecord{CRISandboxID: "product-checkpoint",
		NativeSandboxID: nativeID,
	}); err != nil {
		t.Fatalf("seed route: %v", err)
	}

	result, err := instance.AdminCheckpointSandbox(
		context.Background(),
		core.SandboxRef{SandboxID: "product-checkpoint"},
		core.AdminOpOptions{
			OperationID: "checkpoint-op-1",
			Timeout:     3 * time.Second,
			Metadata:    map[string]string{"template_id": "tpl-1"},
		},
	)
	if err != nil {
		t.Fatalf("AdminCheckpointSandbox: %v", err)
	}
	if !result.SnapshotPersisted || !result.VMResumed {
		t.Fatalf("checkpoint result mismatch: %+v", result)
	}
	if result.StartedAt.IsZero() || result.FinishedAt.IsZero() || result.FinishedAt.Before(result.StartedAt) {
		t.Fatalf("checkpoint timestamps mismatch: %+v", result)
	}
	if captured.GetNativeSandboxId() != nativeID ||
		captured.GetOperationId() != "checkpoint-op-1" ||
		captured.GetTimeoutSeconds() != 3 ||
		captured.GetMetadata()["template_id"] != "tpl-1" {
		t.Fatalf("checkpoint request mismatch: %+v", captured)
	}
}

func TestAdminCheckpointRejectsUnpublishedOrNonRunningResult(t *testing.T) {
	const nativeID = "019a0000-0000-7000-8000-000000000725"
	tests := []struct {
		name   string
		result *runtimeapi.CheckpointResult
		code   codes.Code
	}{
		{
			name:   "nil result",
			result: nil,
			code:   codes.Internal,
		},
		{
			name:   "not published",
			result: &runtimeapi.CheckpointResult{SnapshotRef: "snapshot-ref-1", SourcePhase: runtimeapi.SandboxPhase_RUNNING},
			code:   codes.Internal,
		},
		{
			name:   "empty snapshot ref",
			result: &runtimeapi.CheckpointResult{Published: true, SourcePhase: runtimeapi.SandboxPhase_RUNNING},
			code:   codes.Internal,
		},
		{
			name:   "source not running",
			result: &runtimeapi.CheckpointResult{SnapshotRef: "snapshot-ref-1", Published: true, SourcePhase: runtimeapi.SandboxPhase_PAUSED},
			code:   codes.Unavailable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			admin := &fakeAdmin{checkpoint: func(context.Context, *runtimeapi.SandboxCheckpointRequest) (*runtimeapi.CheckpointResult, error) {
				return tt.result, nil
			}}
			instance, _ := newTestEngine(t, &fakeLifecycle{}, admin)
			if err := instance.putPod(podRecord{CRISandboxID: "product-checkpoint-invalid",
				NativeSandboxID: nativeID,
			}); err != nil {
				t.Fatalf("seed route: %v", err)
			}

			_, err := instance.AdminCheckpointSandbox(
				context.Background(),
				core.SandboxRef{SandboxID: "product-checkpoint-invalid"},
				core.AdminOpOptions{OperationID: "checkpoint-op-invalid"},
			)
			if status.Code(err) != tt.code {
				t.Fatalf("AdminCheckpointSandbox code = %v, want %v; err=%v", status.Code(err), tt.code, err)
			}
		})
	}
}

func TestAdminGetSandboxRuntimeResolvesCRIToNativeID(t *testing.T) {
	const nativeID = "019a0000-0000-7000-8000-000000000003"
	var requested string
	admin := &fakeAdmin{getRuntime: func(_ context.Context, req *runtimeapi.SandboxGetRuntimeRequest) (*runtimeapi.SandboxRuntimeInfo, error) {
		requested = req.GetNativeSandboxId()
		return &runtimeapi.SandboxRuntimeInfo{
			NativeSandboxId:  nativeID,
			ProductSandboxId: "cri-3",
			Phase:            runtimeapi.SandboxPhase_PAUSED,
			Hostports:        []*runtimeapi.HostPort{{HostPort: 30000, SandboxPort: 8080}},
			Extra:            map[string]string{"reason": "test"},
		}, nil
	}}
	engine, _ := newTestEngine(t, &fakeLifecycle{}, admin)
	if err := engine.putPod(podRecord{

		CRISandboxID:    "cri-3",
		NativeSandboxID: nativeID,
	}); err != nil {
		t.Fatalf("seed mapping: %v", err)
	}

	runtimeInfo, err := engine.AdminGetSandboxRuntime(context.Background(), core.SandboxRef{CRISandboxID: "cri-3"})
	if err != nil {
		t.Fatalf("AdminGetSandboxRuntime: %v", err)
	}
	if requested != nativeID {
		t.Fatalf("runtime request native id = %q", requested)
	}
	if runtimeInfo.CRISandboxID != "cri-3" || runtimeInfo.SandboxID != "cri-3" || runtimeInfo.State != "Paused" {
		t.Fatalf("runtime mapping mismatch: %+v", runtimeInfo)
	}
	if runtimeInfo.Extra["native_sandbox_id"] != nativeID || len(runtimeInfo.HostPorts) != 1 {
		t.Fatalf("runtime details mismatch: %+v", runtimeInfo)
	}
}

func TestCleanupSandboxDeletesNativeAndCRIMapping(t *testing.T) {
	const nativeID = "019a0000-0000-7000-8000-000000000004"
	var deleted string
	lifecycle := &fakeLifecycle{delete: func(_ context.Context, req *runtimeapi.SandboxDeleteRequest) (*runtimeapi.OperationStatus, error) {
		deleted = req.GetNativeSandboxId()
		return successfulDeleteOperation(req), nil
	}}
	engine, _ := newTestEngine(t, lifecycle, &fakeAdmin{})
	if err := engine.putPod(podRecord{

		CRISandboxID:    "cri-4",
		NativeSandboxID: nativeID,
	}); err != nil {
		t.Fatalf("seed mapping: %v", err)
	}

	if err := engine.CleanupSandbox(context.Background(), "cri-4", "orphan"); err != nil {
		t.Fatalf("CleanupSandbox: %v", err)
	}
	if deleted != nativeID {
		t.Fatalf("deleted native id = %q", deleted)
	}
	if _, ok := engine.getPod("cri-4"); ok {
		t.Fatal("CRI mapping should be removed")
	}
	if _, ok := engine.getPod("product-4"); ok {
		t.Fatal("product mapping should be removed")
	}
}

func TestUnimplementedCRIMethodIsExplicit(t *testing.T) {
	instance, _ := newCRITestEngine(t, &fakeLifecycle{})
	_, err := instance.GetContainerEvents(context.Background(), nil)
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("GetContainerEvents code = %v, want Unimplemented", status.Code(err))
	}
}

func boolPointer(value bool) *bool { return &value }

// 与 Cube 一致：生命周期走 CRI，Admin 仅暴露暂停、快照和运行信息。
func TestAdminLifecycleIsNotExposed(t *testing.T) {
	if _, ok := any(&Engine{}).(core.AdminLifecycle); ok {
		t.Fatal("unexpected AdminLifecycle capability")
	}
}

func TestModuleDoesNotReconcile(t *testing.T) {
	if (&Engine{}).Module().Reconciler != nil {
		t.Fatal("unexpected active reconciler")
	}
}
