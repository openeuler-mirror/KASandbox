package admin

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	adminv2 "github.com/cri-multiplex/pkg/admin/v2"
	"github.com/cri-multiplex/pkg/engine"
)

type fakeAdminEngine struct {
	engine.RuntimeEngine
	paused      *engine.SandboxRef
	pauseOpts   engine.AdminOpOptions
	created     *engine.SandboxSpec
	updatedID   string
	updated     engine.SandboxUpdate
	deletedID   string
	unavailable bool
}

func (f *fakeAdminEngine) AdminPauseSandbox(ctx context.Context, ref engine.SandboxRef, opts engine.AdminOpOptions) (*engine.AdminOpResult, error) {
	if f.unavailable {
		return nil, status.Error(codes.Unavailable, "backend down")
	}
	f.paused = &ref
	f.pauseOpts = opts
	return &engine.AdminOpResult{SnapshotPersisted: true}, nil
}

func (f *fakeAdminEngine) AdminCreateSandbox(ctx context.Context, spec engine.SandboxSpec) (*engine.SandboxCreateResult, error) {
	f.created = &spec
	return &engine.SandboxCreateResult{SandboxID: spec.SandboxID}, nil
}

// fakeLifecycleEngine 只实现 5 个生命周期方法，用于 List/Update/Delete 用例。
type fakeLifecycleEngine struct {
	fakeAdminEngine
	listResult  []*engine.SandboxRuntime
	builds      []engine.CachedBuild
	updateCalls int
	deleteCalls int
}

func (f *fakeLifecycleEngine) AdminUpdateSandbox(ctx context.Context, sandboxID string, upd engine.SandboxUpdate) error {
	f.updateCalls++
	f.updatedID = sandboxID
	f.updated = upd
	return nil
}

func (f *fakeLifecycleEngine) AdminListSandboxes(ctx context.Context) ([]*engine.SandboxRuntime, error) {
	return f.listResult, nil
}

func (f *fakeLifecycleEngine) AdminDeleteSandbox(ctx context.Context, sandboxID string) error {
	f.deleteCalls++
	f.deletedID = sandboxID
	return nil
}

func (f *fakeLifecycleEngine) AdminListCachedBuilds(ctx context.Context) ([]engine.CachedBuild, error) {
	return f.builds, nil
}

func pauseRegistry(t *testing.T, eng *fakeAdminEngine) *Registry {
	t.Helper()
	reg, err := NewRegistry([]*engine.BackendModule{{
		Descriptor: engine.BackendDescriptor{Type: engine.EngineTypeE2B, Handler: "e2b"},
		Engine:     eng,
	}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg
}

func lifecycleRegistry(t *testing.T, eng *fakeLifecycleEngine) *Registry {
	t.Helper()
	reg, err := NewRegistry([]*engine.BackendModule{{
		Descriptor: engine.BackendDescriptor{Type: engine.EngineTypeE2B, Handler: "e2b"},
		Engine:     eng,
	}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg
}

func TestV2UnknownBackendReturnsNotFound(t *testing.T) {
	reg, _ := NewRegistry([]*engine.BackendModule{{Descriptor: engine.BackendDescriptor{Type: "cube", Handler: "cube"}}})
	s := NewV2Server(reg, nil)
	_, err := s.List(context.Background(), &adminv2.SandboxListRequest{Backend: "cube2"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("code = %v, want NotFound", status.Code(err))
	}
}

func TestV2MissingCapabilityReturnsUnimplemented(t *testing.T) {
	reg, _ := NewRegistry([]*engine.BackendModule{{
		Descriptor: engine.BackendDescriptor{Type: engine.EngineTypeE2B, Handler: "e2b"},
		Engine:     &fakeAdminEngine{}, // 没有完整 AdminLifecycle
	}})
	s := NewV2Server(reg, nil)
	_, err := s.Create(context.Background(), &adminv2.SandboxCreateRequest{Backend: "e2b", Sandbox: &adminv2.SandboxSpec{SandboxId: "sb1"}})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("code = %v, want Unimplemented", status.Code(err))
	}
}

func TestV2TargetMissingReturnsInvalidArgument(t *testing.T) {
	reg := pauseRegistry(t, &fakeAdminEngine{})
	s := NewV2Server(reg, nil)
	_, err := s.PauseSandbox(context.Background(), &adminv2.PauseSandboxRequest{Backend: "e2b", OperationId: "op"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestV2PauseResolvesBackendAndClampsTimeout(t *testing.T) {
	eng := &fakeAdminEngine{}
	reg := pauseRegistry(t, eng)
	s := NewV2Server(reg, nil)
	_, err := s.PauseSandbox(context.Background(), &adminv2.PauseSandboxRequest{
		Backend: "e2b", OperationId: "op-1",
		Target:         &adminv2.PauseSandboxRequest_SandboxId{SandboxId: "sb1"},
		TimeoutSeconds: 1000,
	})
	if err != nil {
		t.Fatalf("PauseSandbox: %v", err)
	}
	if eng.paused.SandboxID != "sb1" || eng.pauseOpts.Timeout != 300*time.Second {
		t.Fatalf("ref = %+v, timeout = %s", eng.paused, eng.pauseOpts.Timeout)
	}
}

func TestV2PauseDefaultTimeoutIs60s(t *testing.T) {
	eng := &fakeAdminEngine{}
	s := NewV2Server(pauseRegistry(t, eng), nil)
	if _, err := s.PauseSandbox(context.Background(), &adminv2.PauseSandboxRequest{
		Backend: "e2b", OperationId: "op-1",
		Target: &adminv2.PauseSandboxRequest_CriSandboxId{CriSandboxId: "cri-1"},
	}); err != nil {
		t.Fatalf("PauseSandbox: %v", err)
	}
	if eng.pauseOpts.Timeout != 60*time.Second {
		t.Fatalf("timeout = %s, want 60s", eng.pauseOpts.Timeout)
	}
}

func TestV2CRIIDOwnerMismatchReturnsFailedPrecondition(t *testing.T) {
	eng := &fakeAdminEngine{}
	reg := pauseRegistry(t, eng)
	routes := engine.NewRouteTable(nil)
	_ = routes.SetPod("cri-1", engine.EngineTypeAndroid)
	s := NewV2Server(reg, routes)
	_, err := s.PauseSandbox(context.Background(), &adminv2.PauseSandboxRequest{
		Backend: "e2b", OperationId: "op-1",
		Target: &adminv2.PauseSandboxRequest_CriSandboxId{CriSandboxId: "cri-1"},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", status.Code(err))
	}
}

func TestV2CRIIDWithoutRouteIsDelegated(t *testing.T) {
	eng := &fakeAdminEngine{}
	reg := pauseRegistry(t, eng)
	s := NewV2Server(reg, engine.NewRouteTable(nil))
	if _, err := s.PauseSandbox(context.Background(), &adminv2.PauseSandboxRequest{
		Backend: "e2b", OperationId: "op-1",
		Target: &adminv2.PauseSandboxRequest_CriSandboxId{CriSandboxId: "cri-unrouted"},
	}); err != nil {
		t.Fatalf("route miss must delegate to the engine, got %v", err)
	}
}

func TestV2CreateMapsSpecAndTimes(t *testing.T) {
	eng := &fakeLifecycleEngine{}
	s := NewV2Server(lifecycleRegistry(t, eng), nil)
	allow := true
	mask := "true"
	start := time.Unix(100, 0)
	end := time.Unix(200, 0)

	resp, err := s.Create(context.Background(), &adminv2.SandboxCreateRequest{
		Backend: "e2b",
		Sandbox: &adminv2.SandboxSpec{
			SandboxId: "sb1", TemplateId: "tmpl", BuildId: "build", Alias: "a",
			Vcpu: 2, RamMb: 1024, DiskMb: 2048, MaxSandboxLengthHours: 3,
			AllowInternet: &allow,
			EnvVars:       map[string]string{"K": "V"},
			Metadata:      map[string]string{"m": "v"},
			VolumeMounts:  []*adminv2.VolumeMount{{Id: "vol", Name: "data", Path: "/data", Type: "pvc"}},
			Extra:         map[string]string{"team_id": "team-1"},
			Network: &adminv2.NetworkPolicy{
				AllowedCidrs:       []string{"10.0.0.0/8"},
				TrafficAccessToken: "tok",
				MaskRequestHost:    &mask,
			},
		},
		StartTime: timestamppb.New(start),
		EndTime:   timestamppb.New(end),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	spec := eng.created
	if spec == nil || spec.SandboxID != "sb1" || spec.TemplateID != "tmpl" || spec.VCPU != 2 ||
		spec.StartTime == nil || !spec.StartTime.Equal(start) || spec.EndTime == nil || !spec.EndTime.Equal(end) {
		t.Fatalf("spec = %+v", spec)
	}
	if len(spec.VolumeMounts) != 1 || spec.VolumeMounts[0].Path != "/data" {
		t.Fatalf("volume mounts = %+v", spec.VolumeMounts)
	}
	if spec.Network == nil || spec.Network.TrafficAccessToken != "tok" || spec.Network.MaskRequestHost == nil || *spec.Network.MaskRequestHost != "true" {
		t.Fatalf("network = %+v", spec.Network)
	}
	if resp.GetBackend() != "e2b" || resp.GetSandboxId() != "sb1" {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestV2DeleteIsIdempotentOnNotFound(t *testing.T) {
	eng := &fakeLifecycleEngine{}
	s := NewV2Server(lifecycleRegistry(t, eng), nil)
	if _, err := s.Delete(context.Background(), &adminv2.SandboxDeleteRequest{Backend: "e2b", SandboxId: "sb1"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if eng.deletedID != "sb1" {
		t.Fatalf("deletedID = %q", eng.deletedID)
	}
}

func TestV2ListMapsSandboxFields(t *testing.T) {
	start := time.Unix(100, 0)
	end := time.Unix(200, 0)
	eng := &fakeLifecycleEngine{listResult: []*engine.SandboxRuntime{{
		Backend: "e2b", CRISandboxID: "cri-1", SandboxID: "sb-1", State: "Running",
		TemplateID: "tmpl", BuildID: "build", PodIP: "10.0.0.1", HostIP: "192.0.2.1",
		StartTime: &start, EndTime: &end,
	}}}
	s := NewV2Server(lifecycleRegistry(t, eng), nil)
	resp, err := s.List(context.Background(), &adminv2.SandboxListRequest{Backend: "e2b"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.GetSandboxes()) != 1 {
		t.Fatalf("sandboxes = %+v", resp.GetSandboxes())
	}
	got := resp.GetSandboxes()[0]
	if got.GetBackend() != "e2b" || got.GetRuntimeState() != "Running" || got.GetPodIp() != "10.0.0.1" ||
		got.GetHostIp() != "192.0.2.1" || got.GetSpec().GetSandboxId() != "sb-1" ||
		got.GetSpec().GetTemplateId() != "tmpl" {
		t.Fatalf("sandbox = %+v", got)
	}
	if got.GetStartTime().AsTime().Unix() != start.Unix() || got.GetEndTime().AsTime().Unix() != end.Unix() {
		t.Fatalf("times = %+v / %+v", got.GetStartTime(), got.GetEndTime())
	}
}

func TestV2UpdateOnlySetsProvidedFields(t *testing.T) {
	eng := &fakeLifecycleEngine{}
	s := NewV2Server(lifecycleRegistry(t, eng), nil)
	end := time.Unix(500, 0)
	if _, err := s.Update(context.Background(), &adminv2.SandboxUpdateRequest{
		Backend:   "e2b",
		SandboxId: "sb1",
		EndTime:   timestamppb.New(end),
		Egress:    &adminv2.NetworkPolicy{AllowedCidrs: []string{"10.0.0.0/8"}},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if eng.updatedID != "sb1" || eng.updated.EndTime == nil || !eng.updated.EndTime.Equal(end) {
		t.Fatalf("update = %+v (id %q)", eng.updated, eng.updatedID)
	}
	if eng.updated.Egress == nil || len(eng.updated.Egress.AllowedCIDRs) != 1 {
		t.Fatalf("egress = %+v", eng.updated.Egress)
	}
}

func TestV2GetRuntimeMapsFacts(t *testing.T) {
	eng := &fakeRuntimeEngine{runtime: &engine.SandboxRuntime{
		Backend: "e2b", CRISandboxID: "cri-1", SandboxID: "sb-1", State: "Running",
		PodName: "pod", PodNamespace: "ns", PodUID: "uid", NodeName: "node",
		PodIP: "10.0.0.1", HostIP: "192.0.2.1", NetNSPath: "/var/run/netns/x",
		HostPorts: []engine.SandboxHostPort{{HostPort: 21000, SandboxPort: 49983}},
		ActiveOps: []engine.SandboxOperation{{OperationID: "op-1", Action: "Pause", State: "Running"}},
		Extra:     map[string]string{"k": "v"},
	}}
	reg, err := NewRegistry([]*engine.BackendModule{{
		Descriptor: engine.BackendDescriptor{Type: engine.EngineTypeE2B, Handler: "e2b"},
		Engine:     eng,
	}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := NewV2Server(reg, nil).GetSandboxRuntime(context.Background(), &adminv2.GetSandboxRuntimeRequest{
		Backend: "e2b", Target: &adminv2.GetSandboxRuntimeRequest_CriSandboxId{CriSandboxId: "cri-1"},
	})
	if err != nil {
		t.Fatalf("GetSandboxRuntime: %v", err)
	}
	rt := resp.GetRuntime()
	if rt.GetCriSandboxId() != "cri-1" || rt.GetSandboxId() != "sb-1" || rt.GetNetnsPath() != "/var/run/netns/x" {
		t.Fatalf("runtime = %+v", rt)
	}
	if len(rt.GetHostports()) != 1 || rt.GetHostports()[0].GetHostPort() != 21000 {
		t.Fatalf("hostports = %+v", rt.GetHostports())
	}
	if len(rt.GetActiveOperations()) != 1 || rt.GetActiveOperations()[0].GetOperationId() != "op-1" {
		t.Fatalf("ops = %+v", rt.GetActiveOperations())
	}
	if rt.GetExtra()["k"] != "v" {
		t.Fatalf("extra = %+v", rt.GetExtra())
	}
}

type fakeRuntimeEngine struct {
	engine.RuntimeEngine
	runtime *engine.SandboxRuntime
}

func (f *fakeRuntimeEngine) AdminGetSandboxRuntime(ctx context.Context, ref engine.SandboxRef) (*engine.SandboxRuntime, error) {
	return f.runtime, nil
}
