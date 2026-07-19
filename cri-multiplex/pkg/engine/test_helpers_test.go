package engine

import (
	"bytes"
	"context"
	"io"
	"net/http"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/cri-multiplex/pkg/orchestrator"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

type fakeCNIManager struct {
	addRecord *CNIRecord
	addErr    error
	delErr    error

	addCalls int
	delCalls int
	addIDs   []string
	delIDs   []string
}

func (f *fakeCNIManager) Add(ctx context.Context, sandboxID string, podCfg *runtime.PodSandboxConfig) (*CNIRecord, error) {
	f.addCalls++
	f.addIDs = append(f.addIDs, sandboxID)
	if f.addErr != nil {
		return nil, f.addErr
	}
	if f.addRecord != nil {
		return f.addRecord, nil
	}
	return &CNIRecord{
		SandboxID: sandboxID,
		Network:   "test-net",
		IfName:    "eth0",
		NetNSName: "test-" + shortID(sandboxID),
		NetNSPath: "/var/run/netns/test-" + shortID(sandboxID),
		PodIP:     "10.0.0.10",
		Gateway:   "10.0.0.1",
		DNS:       []string{"10.96.0.10"},
	}, nil
}

func (f *fakeCNIManager) Del(ctx context.Context, rec *CNIRecord, podCfg *runtime.PodSandboxConfig) error {
	f.delCalls++
	if rec != nil {
		f.delIDs = append(f.delIDs, rec.SandboxID)
	}
	return f.delErr
}

type fakeSandboxServiceClient struct {
	createResp *orchestrator.SandboxCreateResponse
	createErr  error
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

	lastCreate *orchestrator.SandboxCreateRequest
	lastDelete *orchestrator.SandboxDeleteRequest
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
	return &emptypb.Empty{}, nil
}

func (f *fakeSandboxServiceClient) Checkpoint(ctx context.Context, in *orchestrator.SandboxCheckpointRequest, opts ...grpc.CallOption) (*orchestrator.SandboxCheckpointResponse, error) {
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
