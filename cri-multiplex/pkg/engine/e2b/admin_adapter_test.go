package e2b

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cri-multiplex/pkg/engine"
	"github.com/cri-multiplex/pkg/engine/util"
	"github.com/cri-multiplex/pkg/orchestrator"
)

func TestAdminCreateSandboxMapsSpecAndExtra(t *testing.T) {
	client := &fakeSandboxServiceClient{}
	e := newTestEngine(client)
	allow := true
	start := time.Unix(100, 0)

	result, err := e.AdminCreateSandbox(context.Background(), engine.SandboxSpec{
		SandboxID:     "sb1",
		TemplateID:    "tmpl",
		BuildID:       "build",
		Alias:         "alias",
		VCPU:          2,
		RamMB:         1024,
		DiskMB:        4096,
		AllowInternet: &allow,
		VolumeMounts:  []engine.VolumeMount{{ID: "vol", Name: "data", Path: "/data", Type: "pvc"}},
		StartTime:     &start,
		Extra:         map[string]string{"team_id": "team-1", "envd_access_token": "tok"},
	})
	if err != nil {
		t.Fatalf("AdminCreateSandbox: %v", err)
	}
	cfg := client.lastCreate.GetSandbox()
	if cfg.GetSandboxId() != "sb1" || cfg.GetTeamId() != "team-1" || cfg.GetVcpu() != 2 || cfg.GetTotalDiskSizeMb() != 4096 {
		t.Fatalf("mapped config = %+v", cfg)
	}
	if cfg.GetEnvdAccessToken() != "tok" || !cfg.GetAllowInternetAccess() {
		t.Fatalf("extra/allow internet not mapped: %+v", cfg)
	}
	if len(cfg.GetVolumeMounts()) != 1 || cfg.GetVolumeMounts()[0].GetPath() != "/data" || cfg.GetVolumeMounts()[0].GetName() != "data" {
		t.Fatalf("volume mounts not mapped: %+v", cfg.GetVolumeMounts())
	}
	if result.SandboxID != "sb1" || result.ClientID == "" {
		t.Fatalf("result = %+v", result)
	}
}

func TestAdminCreateSandboxRequiresTeamID(t *testing.T) {
	e := newTestEngine(&fakeSandboxServiceClient{})
	_, err := e.AdminCreateSandbox(context.Background(), engine.SandboxSpec{SandboxID: "sb1", TemplateID: "t", BuildID: "b"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestAdminPauseSandboxResolvesSingleSidedRef(t *testing.T) {
	client := &fakeSandboxServiceClient{}
	e := newTestEngine(client)
	e.tracker.Add("cri-1", &podInfo{sandboxID: "cri-1", e2bSandboxID: "e2b-1", state: stateRunning, teamID: "team-1"})

	result, err := e.AdminPauseSandbox(context.Background(),
		engine.SandboxRef{CRISandboxID: "cri-1"},
		engine.AdminOpOptions{OperationID: "op-1", Metadata: map[string]string{"team_id": "team-1", "template_id": "snap", "build_id": "b"}})
	if err != nil {
		t.Fatalf("AdminPauseSandbox: %v", err)
	}
	if !result.SnapshotPersisted || client.lastPause.GetSandboxId() != "e2b-1" {
		t.Fatalf("result = %+v, request = %+v", result, client.lastPause)
	}
}

// 产品 ID 反查、team_id 缺省沿用 pod、timeout 转秒。
func TestAdminCheckpointSandboxResolvesProductRef(t *testing.T) {
	client := &fakeSandboxServiceClient{}
	e := newTestEngine(client)
	e.tracker.Add("cri-2", &podInfo{sandboxID: "cri-2", e2bSandboxID: "e2b-2", state: stateRunning,
		teamID: "team-2", templateID: "tmpl-2", buildID: "build-2"})

	result, err := e.AdminCheckpointSandbox(context.Background(),
		engine.SandboxRef{SandboxID: "e2b-2"},
		engine.AdminOpOptions{OperationID: "op-2", Timeout: 90 * time.Second})
	if err != nil {
		t.Fatalf("AdminCheckpointSandbox: %v", err)
	}
	if !result.VMResumed || !result.SnapshotPersisted {
		t.Fatalf("result = %+v", result)
	}
	if client.lastCheckpoint.GetSandboxId() != "e2b-2" || client.lastCheckpoint.GetBuildId() != "build-2" {
		t.Fatalf("checkpoint request = %+v", client.lastCheckpoint)
	}
}

func TestAdminPauseSandboxMissingRefIsNotFound(t *testing.T) {
	e := newTestEngine(&fakeSandboxServiceClient{})
	_, err := e.AdminPauseSandbox(context.Background(),
		engine.SandboxRef{CRISandboxID: "missing"},
		engine.AdminOpOptions{OperationID: "op-x", Metadata: map[string]string{"team_id": "t", "template_id": "s", "build_id": "b"}})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("code = %v, want NotFound", status.Code(err))
	}
}

func TestAdminDeleteSandboxIsIdempotent(t *testing.T) {
	client := &fakeSandboxServiceClient{deleteErr: status.Error(codes.NotFound, "missing")}
	e := newTestEngine(client)
	if err := e.AdminDeleteSandbox(context.Background(), "sb-missing"); err != nil {
		t.Fatalf("AdminDeleteSandbox should be idempotent, got %v", err)
	}
}

func TestAdminListSandboxesMapsRunningSandbox(t *testing.T) {
	start := time.Unix(1000, 0).UTC()
	end := time.Unix(2000, 0).UTC()
	client := &fakeSandboxServiceClient{listResp: &orchestrator.SandboxListResponse{Sandboxes: []*orchestrator.RunningSandbox{{
		Config: &orchestrator.SandboxConfig{
			SandboxId:  "sb-list",
			TemplateId: "tmpl",
			BuildId:    "build",
			RuntimeNetwork: &orchestrator.SandboxRuntimeNetworkConfig{
				PodIp: "10.0.0.9",
			},
		},
		ClientId:  "client-9",
		StartTime: timestamppb.New(start),
		EndTime:   timestamppb.New(end),
	}}}}
	e := newTestEngine(client)

	got, err := e.AdminListSandboxes(context.Background())
	if err != nil {
		t.Fatalf("AdminListSandboxes: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("sandboxes = %+v", got)
	}
	s := got[0]
	if s.Backend != string(engine.EngineTypeE2B) || s.CRISandboxID != "sb-list" || s.SandboxID != "sb-list" ||
		s.State != "Running" || s.PodIP != "10.0.0.9" || s.HostIP != "192.0.2.1" {
		t.Fatalf("runtime = %+v", s)
	}
	if s.StartTime == nil || !s.StartTime.Equal(start) || s.EndTime == nil || !s.EndTime.Equal(end) {
		t.Fatalf("times = %+v / %+v", s.StartTime, s.EndTime)
	}
}

func TestAdminListCachedBuildsMapsExpiration(t *testing.T) {
	exp := time.Unix(3000, 0).UTC()
	client := &fakeSandboxServiceClient{buildsResp: &orchestrator.SandboxListCachedBuildsResponse{
		Builds: []*orchestrator.CachedBuildInfo{{BuildId: "build-1", ExpirationTime: timestamppb.New(exp)}},
	}}
	e := newTestEngine(client)

	got, err := e.AdminListCachedBuilds(context.Background())
	if err != nil {
		t.Fatalf("AdminListCachedBuilds: %v", err)
	}
	if len(got) != 1 || got[0].BuildID != "build-1" || !got[0].ExpirationTime.Equal(exp) {
		t.Fatalf("builds = %+v", got)
	}
}

func TestAdminGetSandboxRuntimeMapsFacts(t *testing.T) {
	e := newTestEngine(&fakeSandboxServiceClient{})
	e.tracker.Add("cri-3", &podInfo{
		sandboxID: "cri-3", e2bSandboxID: "e2b-3", state: stateRunning,
		name: "pod-3", namespace: "ns-3", podUID: "uid-3", nodeName: "node-3",
		templateID: "tmpl-3", buildID: "build-3",
		cniRecord:    &util.CNIRecord{NetNSPath: "/var/run/netns/e2b-cri-3", PodIP: "10.0.0.3"},
		portMappings: []util.PortMapping{{HostPort: 21000, SandboxPort: 49983}},
	})

	got, err := e.AdminGetSandboxRuntime(context.Background(), engine.SandboxRef{CRISandboxID: "cri-3"})
	if err != nil {
		t.Fatalf("AdminGetSandboxRuntime: %v", err)
	}
	if got.Backend != string(engine.EngineTypeE2B) || got.CRISandboxID != "cri-3" || got.SandboxID != "e2b-3" ||
		got.State != "Running" || got.PodIP != "10.0.0.3" || got.NetNSPath != "/var/run/netns/e2b-cri-3" ||
		got.PodName != "pod-3" || got.PodNamespace != "ns-3" || got.PodUID != "uid-3" || got.NodeName != "node-3" {
		t.Fatalf("runtime = %+v", got)
	}
	if len(got.HostPorts) != 1 || got.HostPorts[0].HostPort != 21000 || got.HostPorts[0].SandboxPort != 49983 {
		t.Fatalf("hostports = %+v", got.HostPorts)
	}
}
