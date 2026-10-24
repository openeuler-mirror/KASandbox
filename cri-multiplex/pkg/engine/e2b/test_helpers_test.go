package e2b

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/cri-multiplex/pkg/engine"
	"github.com/cri-multiplex/pkg/engine/util"
	"github.com/cri-multiplex/pkg/orchestrator"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

type fakeCNIManager struct {
	addRecord *util.CNIRecord
	addErr    error
	delErr    error

	addCalls  int
	delCalls  int
	addIDs    []string
	delIDs    []string
	delCtxErr error
}

func (f *fakeCNIManager) Add(ctx context.Context, sandboxID string, podCfg *runtime.PodSandboxConfig) (*util.CNIRecord, error) {
	f.addCalls++
	f.addIDs = append(f.addIDs, sandboxID)
	if f.addErr != nil {
		return nil, f.addErr
	}
	if f.addRecord != nil {
		return f.addRecord, nil
	}
	return &util.CNIRecord{
		SandboxID: sandboxID,
		Network:   "test-net",
		IfName:    "eth0",
		NetNSName: "test-" + util.ShortID(sandboxID),
		NetNSPath: "/var/run/netns/test-" + util.ShortID(sandboxID),
		PodIP:     "10.0.0.10",
		Gateway:   "10.0.0.1",
		DNS:       []string{"10.96.0.10"},
	}, nil
}

func (f *fakeCNIManager) NetNSName(sandboxID string) string {
	if f.addRecord != nil && f.addRecord.NetNSName != "" {
		return f.addRecord.NetNSName
	}
	return "test-" + util.ShortID(sandboxID)
}

func (f *fakeCNIManager) Del(ctx context.Context, rec *util.CNIRecord, podCfg *runtime.PodSandboxConfig) error {
	f.delCalls++
	f.delCtxErr = ctx.Err()
	if rec != nil {
		f.delIDs = append(f.delIDs, rec.SandboxID)
	}
	return f.delErr
}

type fakeSandboxServiceClient struct {
	createResp    *orchestrator.SandboxCreateResponse
	createErr     error
	updateErr     error
	listResp      *orchestrator.SandboxListResponse
	listErr       error
	deleteErr     error
	pauseErr      error
	checkpointErr error
	buildsResp    *orchestrator.SandboxListCachedBuildsResponse
	buildsErr     error

	createCalls     int
	updateCalls     int
	listCalls       int
	deleteCalls     int
	pauseCalls      int
	checkpointCalls int
	buildsCalls     int

	lastCreate     *orchestrator.SandboxCreateRequest
	lastUpdate     *orchestrator.SandboxUpdateRequest
	lastDelete     *orchestrator.SandboxDeleteRequest
	lastPause      *orchestrator.SandboxPauseRequest
	lastCheckpoint *orchestrator.SandboxCheckpointRequest
}

func (f *fakeSandboxServiceClient) Create(ctx context.Context, in *orchestrator.SandboxCreateRequest, opts ...grpc.CallOption) (*orchestrator.SandboxCreateResponse, error) {
	f.createCalls++
	f.lastCreate = in
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.createResp != nil {
		return f.createResp, nil
	}
	return &orchestrator.SandboxCreateResponse{ClientId: "client-a"}, nil
}

func (f *fakeSandboxServiceClient) Update(ctx context.Context, in *orchestrator.SandboxUpdateRequest, opts ...grpc.CallOption) (*emptypb.Empty, error) {
	f.updateCalls++
	f.lastUpdate = in
	return &emptypb.Empty{}, f.updateErr
}

func (f *fakeSandboxServiceClient) List(ctx context.Context, in *emptypb.Empty, opts ...grpc.CallOption) (*orchestrator.SandboxListResponse, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.listResp != nil {
		return f.listResp, nil
	}
	return &orchestrator.SandboxListResponse{}, nil
}

func (f *fakeSandboxServiceClient) Delete(ctx context.Context, in *orchestrator.SandboxDeleteRequest, opts ...grpc.CallOption) (*emptypb.Empty, error) {
	f.deleteCalls++
	f.lastDelete = in
	return &emptypb.Empty{}, f.deleteErr
}

func (f *fakeSandboxServiceClient) Pause(ctx context.Context, in *orchestrator.SandboxPauseRequest, opts ...grpc.CallOption) (*emptypb.Empty, error) {
	f.pauseCalls++
	f.lastPause = in
	return &emptypb.Empty{}, f.pauseErr
}

func (f *fakeSandboxServiceClient) Checkpoint(ctx context.Context, in *orchestrator.SandboxCheckpointRequest, opts ...grpc.CallOption) (*orchestrator.SandboxCheckpointResponse, error) {
	f.checkpointCalls++
	f.lastCheckpoint = in
	if f.checkpointErr != nil {
		return nil, f.checkpointErr
	}
	return &orchestrator.SandboxCheckpointResponse{}, nil
}

func (f *fakeSandboxServiceClient) ListCachedBuilds(ctx context.Context, in *emptypb.Empty, opts ...grpc.CallOption) (*orchestrator.SandboxListCachedBuildsResponse, error) {
	f.buildsCalls++
	if f.buildsErr != nil {
		return nil, f.buildsErr
	}
	if f.buildsResp != nil {
		return f.buildsResp, nil
	}
	return &orchestrator.SandboxListCachedBuildsResponse{}, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func httpResponse(statusCode int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

// newTestStateStore 返回一个临时目录下的 JSONStateStore（root 包的同名 helper 不跨包可见）。
func newTestStateStore(t *testing.T) engine.StateStore {
	t.Helper()
	store, err := engine.NewJSONStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewJSONStateStore: %v", err)
	}
	return store
}

func loadPodsFrom(t *testing.T, store engine.StateStore) ([]E2BPodState, error) {
	t.Helper()
	var pods []E2BPodState
	_, err := store.LoadBackendState("e2b.pods", &pods)
	return pods, err
}

func loadImagesFrom(t *testing.T, store engine.StateStore) ([]string, error) {
	t.Helper()
	var images []string
	_, err := store.LoadBackendState("e2b.images", &images)
	return images, err
}

func loadRoutesFrom(t *testing.T, store engine.StateStore) ([]engine.RouteRecord, error) {
	t.Helper()
	return store.LoadRoutes()
}

func saveE2BPodTo(store engine.StateStore, pods ...E2BPodState) error {
	var existing []E2BPodState
	if _, err := store.LoadBackendState("e2b.pods", &existing); err != nil {
		return err
	}
	for _, p := range pods {
		existing = upsertE2BPodState(existing, p)
	}
	return store.SaveBackendState("e2b.pods", existing)
}

func deleteE2BPodFrom(store engine.StateStore, sandboxID string) error {
	var existing []E2BPodState
	if _, err := store.LoadBackendState("e2b.pods", &existing); err != nil {
		return err
	}
	return store.SaveBackendState("e2b.pods", deleteE2BPodState(existing, sandboxID))
}

func saveE2BImageTo(store engine.StateStore, imageRef string) error {
	return store.SaveBackendState("e2b.images", []string{imageRef})
}

func saveRouteTo(store engine.StateStore, rec engine.RouteRecord) error {
	return store.SaveRoute(rec)
}

// waitTCPReady 是 root 包同名 helper 的测试副本（android 尚未迁包）。
func waitTCPReady(ctx context.Context, host string, port int, exitCh <-chan error) error {
	if host == "" {
		host = "127.0.0.1"
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	var lastErr error
	for {
		dialer := net.Dialer{Timeout: 1 * time.Second}
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err
		select {
		case err := <-exitCh:
			if err != nil {
				return fmt.Errorf("launch_cvd exited before ADB ready: %w", err)
			}
			return fmt.Errorf("launch_cvd exited before ADB ready")
		case <-ctx.Done():
			return lastErr
		case <-ticker.C:
		}
	}
}
