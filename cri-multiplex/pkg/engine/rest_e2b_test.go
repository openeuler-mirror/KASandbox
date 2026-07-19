package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func restPodReq() *runtime.RunPodSandboxRequest {
	return &runtime.RunPodSandboxRequest{Config: &runtime.PodSandboxConfig{
		Metadata: &runtime.PodSandboxMetadata{Name: "pod-a", Namespace: "ns-a", Uid: "uid-a"},
		Labels:   map[string]string{"app": "a"},
		Annotations: map[string]string{
			annTemplateID: "tmpl-a",
		},
	}}
}

func TestRestRunPodSandbox(t *testing.T) {
	var gotPath, gotMethod, gotAPIKey string
	var gotBody createSandboxRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotAPIKey = r.Header.Get("X-API-Key")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"sandboxID":"sandbox-rest"}`))
	}))
	defer server.Close()

	e := newRestE2BEngine(server.URL, "key-a")
	resp, err := e.RunPodSandbox(context.Background(), restPodReq())
	if err != nil {
		t.Fatalf("RunPodSandbox: %v", err)
	}
	if resp.PodSandboxId != "sandbox-rest" {
		t.Fatalf("sandbox id = %q", resp.PodSandboxId)
	}
	if gotMethod != http.MethodPost || gotPath != "/sandboxes" || gotAPIKey != "key-a" || gotBody.TemplateID != "tmpl-a" {
		t.Fatalf("request mismatch: method=%s path=%s apiKey=%s body=%+v", gotMethod, gotPath, gotAPIKey, gotBody)
	}
	if pod, ok := e.tracker.Get("sandbox-rest"); !ok || pod.name != "pod-a" || pod.namespace != "ns-a" {
		t.Fatalf("tracker pod mismatch: %+v ok=%v", pod, ok)
	}
}

func TestRestRunPodSandboxErrors(t *testing.T) {
	e := newRestE2BEngine("http://127.0.0.1", "")
	req := restPodReq()
	delete(req.Config.Annotations, annTemplateID)
	if _, err := e.RunPodSandbox(context.Background(), req); err == nil {
		t.Fatal("expected missing template annotation error")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()
	e = newRestE2BEngine(server.URL, "")
	if _, err := e.RunPodSandbox(context.Background(), restPodReq()); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected 500 error, got %v", err)
	}

	emptyIDServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer emptyIDServer.Close()
	e = newRestE2BEngine(emptyIDServer.URL, "")
	if _, err := e.RunPodSandbox(context.Background(), restPodReq()); err == nil || !strings.Contains(err.Error(), "empty sandboxID") {
		t.Fatalf("expected empty sandboxID error, got %v", err)
	}
}

func TestRestStopRemovePodSandbox(t *testing.T) {
	seen := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[r.Method+" "+r.URL.Path] = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	e := newRestE2BEngine(server.URL, "")
	e.tracker.Add("sandbox-rest", &podInfo{sandboxID: "sandbox-rest"})
	if _, err := e.StopPodSandbox(context.Background(), &runtime.StopPodSandboxRequest{PodSandboxId: "sandbox-rest"}); err != nil {
		t.Fatalf("StopPodSandbox: %v", err)
	}
	if _, err := e.RemovePodSandbox(context.Background(), &runtime.RemovePodSandboxRequest{PodSandboxId: "sandbox-rest"}); err != nil {
		t.Fatalf("RemovePodSandbox: %v", err)
	}
	if !seen["POST /sandboxes/sandbox-rest/pause"] || !seen["DELETE /sandboxes/sandbox-rest"] {
		t.Fatalf("missing expected REST calls: %v", seen)
	}
	if _, ok := e.tracker.Get("sandbox-rest"); ok {
		t.Fatal("RemovePodSandbox should delete tracker entry")
	}
}

func TestRestListPodSandbox(t *testing.T) {
	started := time.Unix(10, 20).Format(time.RFC3339Nano)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/sandboxes" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]listedSandboxItem{
			{SandboxID: "tracked", State: "running", StartedAt: started},
			{SandboxID: "untracked", Alias: "alias-a", State: "paused"},
		})
	}))
	defer server.Close()

	e := newRestE2BEngine(server.URL, "")
	e.tracker.Add("tracked", &podInfo{
		sandboxID:   "tracked",
		podUID:      "uid",
		name:        "pod",
		namespace:   "ns",
		labels:      map[string]string{"app": "tracked"},
		annotations: map[string]string{"a": "b"},
	})

	resp, err := e.ListPodSandbox(context.Background(), &runtime.ListPodSandboxRequest{})
	if err != nil {
		t.Fatalf("ListPodSandbox: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("item count = %d, want 2", len(resp.Items))
	}
	if resp.Items[0].Metadata.Name != "pod" || resp.Items[0].Labels["app"] != "tracked" {
		t.Fatalf("tracked metadata mismatch: %+v", resp.Items[0])
	}
	if resp.Items[1].Metadata.Name != "alias-a" || resp.Items[1].State != runtime.PodSandboxState_SANDBOX_NOTREADY {
		t.Fatalf("untracked item mismatch: %+v", resp.Items[1])
	}
}
