package agentenv

import (
	"context"
	"testing"
	"time"

	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	runtimeapi "github.com/cri-multiplex/pkg/agentenv/runtime/v1"
)

func TestRunPodSandboxDifferentSandboxesRunConcurrently(t *testing.T) {
	entered := make(chan string, 2)
	release := make(chan struct{})
	lifecycle := &fakeLifecycle{create: func(_ context.Context, req *runtimeapi.SandboxCreateRequest) (*runtimeapi.SandboxCreateResponse, error) {
		productID := req.GetSpec().GetProductSandboxId()
		entered <- productID
		<-release
		return &runtimeapi.SandboxCreateResponse{Sandbox: &runtimeapi.SandboxInfo{
			NativeSandboxId:  "native-" + productID,
			ProductSandboxId: productID,
			Phase:            runtimeapi.SandboxPhase_RUNNING,
		}}, nil
	}}
	instance, _ := newCRITestEngine(t, lifecycle)

	newRequest := func(uid, productID string) *runtime.RunPodSandboxRequest {
		req := criRunRequest(map[string]string{
			annTemplate:  "template-1",
			annSandboxID: productID,
		})
		req.GetConfig().GetMetadata().Uid = uid
		req.GetConfig().GetMetadata().Name = uid
		return req
	}

	errs := make(chan error, 2)
	go func() {
		_, err := instance.RunPodSandbox(context.Background(), newRequest("cri-a", "product-a"))
		errs <- err
	}()
	go func() {
		_, err := instance.RunPodSandbox(context.Background(), newRequest("cri-b", "product-b"))
		errs <- err
	}()

	seen := make(map[string]bool)
	for len(seen) < 2 {
		select {
		case productID := <-entered:
			seen[productID] = true
		case <-time.After(2 * time.Second):
			close(release)
			t.Fatalf("only %d sandbox(es) reached Runtime Create; different sandboxes are still serialized", len(seen))
		}
	}
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("RunPodSandbox: %v", err)
		}
	}
}
