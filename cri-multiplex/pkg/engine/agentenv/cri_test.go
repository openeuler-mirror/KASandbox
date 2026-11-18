package agentenv

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	runtimeapi "github.com/cri-multiplex/pkg/agentenv/runtime/v1"
	"github.com/cri-multiplex/pkg/engine"
)

type memoryStateStore struct {
	mu              sync.Mutex
	routes          []engine.RouteRecord
	sections        map[string][]byte
	saveRouteCalls  int
	combinedCalls   int
	failSaveRouteAt int
}

func newMemoryStateStore() *memoryStateStore {
	return &memoryStateStore{sections: make(map[string][]byte)}
}

func (s *memoryStateStore) SaveRoute(record engine.RouteRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveRouteCalls++
	if s.failSaveRouteAt > 0 && s.saveRouteCalls == s.failSaveRouteAt {
		return errors.New("injected route save failure")
	}
	for i := range s.routes {
		if s.routes[i].Kind == record.Kind && s.routes[i].ID == record.ID {
			s.routes[i] = record
			return nil
		}
	}
	s.routes = append(s.routes, record)
	return nil
}

func (s *memoryStateStore) SaveRouteAndBackendState(record engine.RouteRecord, path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveRouteCalls++
	if s.failSaveRouteAt > 0 && s.saveRouteCalls == s.failSaveRouteAt {
		return errors.New("injected route save failure")
	}
	replaced := false
	for i := range s.routes {
		if s.routes[i].Kind == record.Kind && s.routes[i].ID == record.ID {
			s.routes[i] = record
			replaced = true
			break
		}
	}
	if !replaced {
		s.routes = append(s.routes, record)
	}
	s.sections[path] = data
	s.combinedCalls++
	return nil
}

func (s *memoryStateStore) DeleteRoute(kind, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.routes[:0]
	for _, record := range s.routes {
		if record.Kind != kind || record.ID != id {
			out = append(out, record)
		}
	}
	s.routes = out
	return nil
}

func (s *memoryStateStore) LoadRoutes() ([]engine.RouteRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]engine.RouteRecord(nil), s.routes...), nil
}

func (s *memoryStateStore) SaveCleanupTask(engine.CleanupTask) error { return nil }
func (s *memoryStateStore) DeleteCleanupTask(string) error           { return nil }
func (s *memoryStateStore) LoadCleanupTasks() ([]engine.CleanupTask, error) {
	return nil, nil
}

func (s *memoryStateStore) SaveBackendState(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sections[path] = data
	return nil
}

func (s *memoryStateStore) LoadBackendState(path string, dst any) (bool, error) {
	s.mu.Lock()
	data, ok := s.sections[path]
	data = append([]byte(nil), data...)
	s.mu.Unlock()
	if !ok {
		return false, nil
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return false, err
	}
	return true, nil
}

func (s *memoryStateStore) DeleteBackendState(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sections, path)
	return nil
}

func newCRITestEngine(t *testing.T, lifecycle lifecycleAPI) (*Engine, *memoryStateStore, *engine.RouteTable) {
	t.Helper()
	store := newMemoryStateStore()
	routes := engine.NewRouteTable(store)
	instance := newWithClients(
		Config{RuntimeAddress: "passthrough:///agentenv-test", NodeName: "node-1", NodeIP: "10.0.0.1"},
		store,
		routes,
		lifecycle,
		&fakeAdmin{},
	)
	return instance, store, routes
}

func criRunRequest(annotations map[string]string) *runtime.RunPodSandboxRequest {
	return &runtime.RunPodSandboxRequest{
		RuntimeHandler: "agentenv",
		Config: &runtime.PodSandboxConfig{
			Metadata: &runtime.PodSandboxMetadata{
				Name:      "pod-1",
				Uid:       "cri-1",
				Namespace: "default",
			},
			Labels:      map[string]string{"app": "demo"},
			Annotations: annotations,
		},
	}
}

func TestRunPodSandboxCreatesTemplateAndPersistsIDs(t *testing.T) {
	const nativeID = "019a0000-0000-7000-8000-000000000101"
	var captured *runtimeapi.SandboxCreateRequest
	lifecycle := &fakeLifecycle{create: func(_ context.Context, req *runtimeapi.SandboxCreateRequest) (*runtimeapi.SandboxCreateResponse, error) {
		captured = req
		return &runtimeapi.SandboxCreateResponse{Sandbox: &runtimeapi.SandboxInfo{
			NativeSandboxId:  nativeID,
			ProductSandboxId: "product-1",
			Phase:            runtimeapi.SandboxPhase_RUNNING,
		}}, nil
	}}
	instance, store, routes := newCRITestEngine(t, lifecycle)

	resp, err := instance.RunPodSandbox(context.Background(), criRunRequest(map[string]string{
		annBackend:           "agentenv",
		annTemplate:          "template-alias",
		annCanonicalTemplate: "template-canonical",
		annSandboxID:         "product-1",
		annBuild:             "build-1",
		annAutoPause:         "false",
		annTimeoutSeconds:    "120",
		annVCPU:              "2",
		annMemoryMB:          "4096",
		annDiskMB:            "8192",
		annEnvVars:           `{"MODE":"test"}`,
		annMetadata:          `{"team":"kas"}`,
	}))
	if err != nil {
		t.Fatalf("RunPodSandbox: %v", err)
	}
	if resp.GetPodSandboxId() != "cri-1" {
		t.Fatalf("pod sandbox id = %q", resp.GetPodSandboxId())
	}
	if captured == nil || captured.GetSpec().GetSource().GetImage() != nil {
		t.Fatalf("create request did not use template source: %+v", captured)
	}
	spec := captured.GetSpec()
	if spec.GetProductSandboxId() != "product-1" || spec.GetNativeSandboxId() != "" {
		t.Fatalf("ID contract mismatch: %+v", spec)
	}
	if spec.GetSource().GetTemplate().GetTemplateId() != "template-canonical" ||
		spec.GetSource().GetTemplate().GetBuildId() != "build-1" {
		t.Fatalf("template source mismatch: %+v", spec.GetSource())
	}
	if spec.AutoPause == nil || spec.GetAutoPause() {
		t.Fatalf("auto_pause = %+v, want false", spec.AutoPause)
	}
	if captured.GetTimeoutSeconds() != 120 {
		t.Fatalf("timeout = %d", captured.GetTimeoutSeconds())
	}
	if spec.GetMaxSandboxLengthHours() != defaultCRISandboxMaxLengthHours {
		t.Fatalf("max sandbox length = %d, want %d", spec.GetMaxSandboxLengthHours(), defaultCRISandboxMaxLengthHours)
	}
	if spec.GetResources().GetVcpu() != 2 || spec.GetResources().GetMemoryMb() != 4096 || spec.GetResources().GetDiskMb() != 8192 {
		t.Fatalf("resources mismatch: %+v", spec.GetResources())
	}
	if spec.GetEnvVars()["MODE"] != "test" || spec.GetMetadata()["team"] != "kas" {
		t.Fatalf("annotation maps mismatch: env=%v metadata=%v", spec.GetEnvVars(), spec.GetMetadata())
	}

	for name, lookup := range map[string]func() (engine.PodRoute, bool){
		"CRI":     func() (engine.PodRoute, bool) { return routes.PodRouteByCRI("cri-1") },
		"product": func() (engine.PodRoute, bool) { return routes.PodRouteByProduct("product-1") },
		"native":  func() (engine.PodRoute, bool) { return routes.PodRouteByNative(nativeID) },
	} {
		route, ok := lookup()
		if !ok || route.CRISandboxID != "cri-1" || route.ProductSandboxID != "product-1" || route.NativeSandboxID != nativeID {
			t.Fatalf("%s route mismatch: %+v ok=%v", name, route, ok)
		}
	}
	record, ok := instance.getPod("cri-1")
	if !ok || record.NativeSandboxID != nativeID || record.TemplateID != "template-canonical" || record.State != criStateReady {
		t.Fatalf("pod state mismatch: %+v ok=%v", record, ok)
	}

	var persisted persistedAgentENVState
	found, err := store.LoadBackendState(podStatePath, &persisted)
	if err != nil || !found || len(persisted.Pods) != 1 {
		t.Fatalf("pod state not persisted: found=%v err=%v state=%+v", found, err, persisted)
	}
	if store.combinedCalls != 1 {
		t.Fatalf("combined route/pod persistence calls = %d, want 1", store.combinedCalls)
	}
}
func TestRunPodSandboxRejectsMissingTemplateAndAutoPause(t *testing.T) {
	instance, _, _ := newCRITestEngine(t, &fakeLifecycle{})

	if _, err := instance.RunPodSandbox(context.Background(), criRunRequest(nil)); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing template code = %v, want InvalidArgument", status.Code(err))
	}
	if _, err := instance.RunPodSandbox(context.Background(), criRunRequest(map[string]string{
		annTemplate:  "template-1",
		annAutoPause: "true",
	})); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("auto-pause code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestRunPodSandboxIsIdempotentForSamePodUID(t *testing.T) {
	const nativeID = "019a0000-0000-7000-8000-000000000102"
	createCalls := 0
	lifecycle := &fakeLifecycle{
		create: func(_ context.Context, _ *runtimeapi.SandboxCreateRequest) (*runtimeapi.SandboxCreateResponse, error) {
			createCalls++
			return &runtimeapi.SandboxCreateResponse{Sandbox: &runtimeapi.SandboxInfo{
				NativeSandboxId:  nativeID,
				ProductSandboxId: "product-idem",
				Phase:            runtimeapi.SandboxPhase_RUNNING,
			}}, nil
		},
		get: func(_ context.Context, req *runtimeapi.SandboxGetRequest) (*runtimeapi.SandboxInfo, error) {
			if req.GetNativeSandboxId() != nativeID {
				return nil, status.Error(codes.NotFound, "not found")
			}
			return &runtimeapi.SandboxInfo{NativeSandboxId: nativeID, ProductSandboxId: "product-idem", Phase: runtimeapi.SandboxPhase_RUNNING}, nil
		},
	}
	instance, _, _ := newCRITestEngine(t, lifecycle)
	req := criRunRequest(map[string]string{annTemplate: "template-1", annSandboxID: "product-idem"})
	if _, err := instance.RunPodSandbox(context.Background(), req); err != nil {
		t.Fatalf("first RunPodSandbox: %v", err)
	}
	if _, err := instance.RunPodSandbox(context.Background(), req); err != nil {
		t.Fatalf("second RunPodSandbox: %v", err)
	}
	if createCalls != 1 {
		t.Fatalf("create calls = %d, want 1", createCalls)
	}
}

func TestRunPodSandboxRollsBackReservationOnCreateFailure(t *testing.T) {
	lifecycle := &fakeLifecycle{create: func(context.Context, *runtimeapi.SandboxCreateRequest) (*runtimeapi.SandboxCreateResponse, error) {
		return nil, status.Error(codes.Unavailable, "runtime unavailable")
	}}
	instance, _, routes := newCRITestEngine(t, lifecycle)
	_, err := instance.RunPodSandbox(context.Background(), criRunRequest(map[string]string{annTemplate: "template-1"}))
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("create error code = %v", status.Code(err))
	}
	if _, ok := routes.PodRouteByCRI("cri-1"); ok {
		t.Fatal("reservation survived failed create")
	}
	if _, ok := instance.getPod("cri-1"); ok {
		t.Fatal("pod state survived failed create")
	}
}

func TestPodSandboxLifecycleProjection(t *testing.T) {
	const nativeID = "019a0000-0000-7000-8000-000000000103"
	deleted := false
	lifecycle := &fakeLifecycle{
		create: func(_ context.Context, _ *runtimeapi.SandboxCreateRequest) (*runtimeapi.SandboxCreateResponse, error) {
			return &runtimeapi.SandboxCreateResponse{Sandbox: &runtimeapi.SandboxInfo{
				NativeSandboxId:  nativeID,
				ProductSandboxId: "product-life",
				Phase:            runtimeapi.SandboxPhase_RUNNING,
			}}, nil
		},
		get: func(_ context.Context, _ *runtimeapi.SandboxGetRequest) (*runtimeapi.SandboxInfo, error) {
			if deleted {
				return nil, status.Error(codes.NotFound, "deleted")
			}
			return &runtimeapi.SandboxInfo{NativeSandboxId: nativeID, ProductSandboxId: "product-life", Phase: runtimeapi.SandboxPhase_RUNNING}, nil
		},
		list: func(context.Context, *runtimeapi.SandboxListRequest) (*runtimeapi.SandboxListResponse, error) {
			if deleted {
				return &runtimeapi.SandboxListResponse{}, nil
			}
			return &runtimeapi.SandboxListResponse{Sandboxes: []*runtimeapi.SandboxInfo{{
				NativeSandboxId: nativeID, ProductSandboxId: "product-life", Phase: runtimeapi.SandboxPhase_RUNNING,
			}}}, nil
		},
		delete: func(_ context.Context, req *runtimeapi.SandboxDeleteRequest) (*runtimeapi.OperationStatus, error) {
			if req.GetNativeSandboxId() != nativeID {
				t.Fatalf("delete native id = %q", req.GetNativeSandboxId())
			}
			deleted = true
			return successfulDeleteOperation(req), nil
		},
	}
	instance, _, routes := newCRITestEngine(t, lifecycle)
	req := criRunRequest(map[string]string{annTemplate: "template-1", annSandboxID: "product-life"})
	if _, err := instance.RunPodSandbox(context.Background(), req); err != nil {
		t.Fatalf("RunPodSandbox: %v", err)
	}

	statusResp, err := instance.PodSandboxStatus(context.Background(), &runtime.PodSandboxStatusRequest{PodSandboxId: "cri-1"})
	if err != nil {
		t.Fatalf("PodSandboxStatus: %v", err)
	}
	if statusResp.GetStatus().GetState() != runtime.PodSandboxState_SANDBOX_READY || statusResp.GetStatus().GetCreatedAt() <= 0 {
		t.Fatalf("ready status mismatch: %+v", statusResp.GetStatus())
	}

	listResp, err := instance.ListPodSandbox(context.Background(), &runtime.ListPodSandboxRequest{Filter: &runtime.PodSandboxFilter{
		State:         &runtime.PodSandboxStateValue{State: runtime.PodSandboxState_SANDBOX_READY},
		LabelSelector: map[string]string{"app": "demo"},
	}})
	if err != nil {
		t.Fatalf("ListPodSandbox: %v", err)
	}
	if len(listResp.GetItems()) != 1 || listResp.GetItems()[0].GetId() != "cri-1" {
		t.Fatalf("list items = %+v", listResp.GetItems())
	}

	if _, err := instance.StopPodSandbox(context.Background(), &runtime.StopPodSandboxRequest{PodSandboxId: "cri-1"}); err != nil {
		t.Fatalf("StopPodSandbox: %v", err)
	}
	statusResp, err = instance.PodSandboxStatus(context.Background(), &runtime.PodSandboxStatusRequest{PodSandboxId: "cri-1"})
	if err != nil {
		t.Fatalf("PodSandboxStatus after stop: %v", err)
	}
	if statusResp.GetStatus().GetState() != runtime.PodSandboxState_SANDBOX_NOTREADY {
		t.Fatalf("stopped state = %v", statusResp.GetStatus().GetState())
	}

	if _, err := instance.RemovePodSandbox(context.Background(), &runtime.RemovePodSandboxRequest{PodSandboxId: "cri-1"}); err != nil {
		t.Fatalf("RemovePodSandbox: %v", err)
	}
	if _, ok := instance.getPod("cri-1"); ok {
		t.Fatal("pod state survived remove")
	}
	if _, ok := routes.PodRouteByCRI("cri-1"); ok {
		t.Fatal("route survived remove")
	}
}

func TestRestoreStateRestoresRoutesAndCRIProjection(t *testing.T) {
	const nativeID = "019a0000-0000-7000-8000-000000000104"
	lifecycle := &fakeLifecycle{create: func(context.Context, *runtimeapi.SandboxCreateRequest) (*runtimeapi.SandboxCreateResponse, error) {
		return &runtimeapi.SandboxCreateResponse{Sandbox: &runtimeapi.SandboxInfo{
			NativeSandboxId: nativeID, ProductSandboxId: "product-restore", Phase: runtimeapi.SandboxPhase_RUNNING,
		}}, nil
	}}
	instance, store, _ := newCRITestEngine(t, lifecycle)
	if _, err := instance.RunPodSandbox(context.Background(), criRunRequest(map[string]string{
		annTemplate: "template-1", annSandboxID: "product-restore",
	})); err != nil {
		t.Fatalf("RunPodSandbox: %v", err)
	}

	routes := engine.NewRouteTable(store)
	restored := newWithClients(
		Config{RuntimeAddress: "passthrough:///agentenv-test", NodeName: "node-1", NodeIP: "10.0.0.1"},
		store,
		routes,
		lifecycle,
		&fakeAdmin{},
	)
	if err := restored.RestoreState(context.Background()); err != nil {
		t.Fatalf("RestoreState: %v", err)
	}
	route, ok := routes.PodRouteByNative(nativeID)
	if !ok || route.CRISandboxID != "cri-1" || route.ProductSandboxID != "product-restore" {
		t.Fatalf("restored route mismatch: %+v ok=%v", route, ok)
	}
	record, ok := restored.getPod("cri-1")
	if !ok || record.NativeSandboxID != nativeID || record.TemplateID != "template-1" {
		t.Fatalf("restored pod mismatch: %+v ok=%v", record, ok)
	}
}

func TestStopThenRemoveDoesNotRepeatRuntimeDelete(t *testing.T) {
	const nativeID = "019a0000-0000-7000-8000-000000000720"
	deleteCalls := 0
	lifecycle := &fakeLifecycle{
		create: func(context.Context, *runtimeapi.SandboxCreateRequest) (*runtimeapi.SandboxCreateResponse, error) {
			return &runtimeapi.SandboxCreateResponse{Sandbox: &runtimeapi.SandboxInfo{
				NativeSandboxId:  nativeID,
				ProductSandboxId: "product-stop-remove",
				Phase:            runtimeapi.SandboxPhase_RUNNING,
			}}, nil
		},
		delete: func(_ context.Context, req *runtimeapi.SandboxDeleteRequest) (*runtimeapi.OperationStatus, error) {
			deleteCalls++
			return successfulDeleteOperation(req), nil
		},
	}
	instance, _, routes := newCRITestEngine(t, lifecycle)
	if _, err := instance.RunPodSandbox(context.Background(), criRunRequest(map[string]string{
		annTemplate:  "template-stop-remove",
		annSandboxID: "product-stop-remove",
	})); err != nil {
		t.Fatalf("RunPodSandbox: %v", err)
	}

	for i := 0; i < 2; i++ {
		if _, err := instance.StopPodSandbox(context.Background(), &runtime.StopPodSandboxRequest{PodSandboxId: "cri-1"}); err != nil {
			t.Fatalf("StopPodSandbox attempt %d: %v", i+1, err)
		}
	}
	if deleteCalls != 1 {
		t.Fatalf("runtime delete calls after repeated stop = %d, want 1", deleteCalls)
	}
	record, ok := instance.getPod("cri-1")
	if !ok || !record.RuntimeDeleted || record.State != criStateNotReady {
		t.Fatalf("stopped record = %+v ok=%v", record, ok)
	}

	if _, err := instance.RemovePodSandbox(context.Background(), &runtime.RemovePodSandboxRequest{PodSandboxId: "cri-1"}); err != nil {
		t.Fatalf("RemovePodSandbox: %v", err)
	}
	if deleteCalls != 1 {
		t.Fatalf("runtime delete calls after remove = %d, want 1", deleteCalls)
	}
	if _, ok := instance.getPod("cri-1"); ok {
		t.Fatal("pod projection survived remove")
	}
	if _, ok := routes.PodRouteByCRI("cri-1"); ok {
		t.Fatal("pod route survived remove")
	}
}

func TestValidateDeleteOperationStatus(t *testing.T) {
	const expected = "cri-stop:cri-1"
	tests := []struct {
		name      string
		operation *runtimeapi.OperationStatus
		code      codes.Code
	}{
		{name: "succeeded", operation: &runtimeapi.OperationStatus{OperationId: expected, State: "SUCCEEDED"}, code: codes.OK},
		{name: "nil", operation: nil, code: codes.Internal},
		{name: "id mismatch", operation: &runtimeapi.OperationStatus{OperationId: "other", State: "SUCCEEDED"}, code: codes.Internal},
		{name: "unknown", operation: &runtimeapi.OperationStatus{OperationId: expected, State: "UNKNOWN", ErrorCode: "OUTCOME_UNKNOWN"}, code: codes.Unavailable},
		{name: "empty state", operation: &runtimeapi.OperationStatus{OperationId: expected}, code: codes.Unavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateDeleteOperationStatus(expected, tt.operation)
			if got := status.Code(err); got != tt.code {
				t.Fatalf("code = %v, want %v; err=%v", got, tt.code, err)
			}
		})
	}
}

func TestStopPodSandboxRejectsUnknownDeleteOperation(t *testing.T) {
	const nativeID = "019a0000-0000-7000-8000-000000000719"
	lifecycle := &fakeLifecycle{
		create: func(context.Context, *runtimeapi.SandboxCreateRequest) (*runtimeapi.SandboxCreateResponse, error) {
			return &runtimeapi.SandboxCreateResponse{Sandbox: &runtimeapi.SandboxInfo{
				NativeSandboxId:  nativeID,
				ProductSandboxId: "product-unknown-delete",
				Phase:            runtimeapi.SandboxPhase_RUNNING,
			}}, nil
		},
		delete: func(_ context.Context, req *runtimeapi.SandboxDeleteRequest) (*runtimeapi.OperationStatus, error) {
			return &runtimeapi.OperationStatus{
				OperationId:  req.GetOperationId(),
				Action:       "delete",
				State:        "UNKNOWN",
				ErrorCode:    "OUTCOME_UNKNOWN",
				ErrorMessage: "Runtime restarted before delete reached a terminal state",
			}, nil
		},
	}
	instance, _, routes := newCRITestEngine(t, lifecycle)
	request := criRunRequest(map[string]string{
		annTemplate:  "template-unknown-delete",
		annSandboxID: "product-unknown-delete",
	})
	if _, err := instance.RunPodSandbox(context.Background(), request); err != nil {
		t.Fatalf("RunPodSandbox: %v", err)
	}

	if _, err := instance.StopPodSandbox(context.Background(), &runtime.StopPodSandboxRequest{PodSandboxId: "cri-1"}); status.Code(err) != codes.Unavailable {
		t.Fatalf("StopPodSandbox code = %v, want Unavailable; err=%v", status.Code(err), err)
	}
	record, ok := instance.getPod("cri-1")
	if !ok || record.State != criStateReady {
		t.Fatalf("pod record after failed stop = %+v, ok=%v; want READY projection retained", record, ok)
	}
	if _, ok := routes.PodRouteByCRI("cri-1"); !ok {
		t.Fatal("route was removed after UNKNOWN delete operation")
	}

	if _, err := instance.RemovePodSandbox(context.Background(), &runtime.RemovePodSandboxRequest{PodSandboxId: "cri-1"}); status.Code(err) != codes.Unavailable {
		t.Fatalf("RemovePodSandbox code = %v, want Unavailable; err=%v", status.Code(err), err)
	}
	if _, ok := instance.getPod("cri-1"); !ok {
		t.Fatal("pod record was removed after UNKNOWN delete operation")
	}
	if _, ok := routes.PodRouteByCRI("cri-1"); !ok {
		t.Fatal("route was removed after UNKNOWN delete operation")
	}
}
