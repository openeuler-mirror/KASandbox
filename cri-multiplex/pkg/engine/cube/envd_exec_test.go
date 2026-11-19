package cube

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func connectEnvelope(flags byte, payload string) []byte {
	raw := []byte(payload)
	header := make([]byte, 5)
	header[0] = flags
	header[1] = byte(len(raw) >> 24)
	header[2] = byte(len(raw) >> 16)
	header[3] = byte(len(raw) >> 8)
	header[4] = byte(len(raw))
	return append(header, raw...)
}

func TestExecSyncViaEnvd(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", envdConnectContentType)
		stdout := base64.StdEncoding.EncodeToString([]byte("hi\n"))
		_, _ = w.Write(connectEnvelope(0, `{"event":{"data":{"stdout":"`+stdout+`"}}}`))
		_, _ = w.Write(connectEnvelope(0, `{"event":{"end":{"exitCode":0,"exited":true,"status":"exit status 0"}}}`))
		_, _ = w.Write(connectEnvelope(envdConnectEndStream, `{}`))
	}))
	defer srv.Close()

	fake := &fakeClient{}
	eng := newTestEngine(t, fake)
	// httptest is host:port; execViaEnvd always dials SandboxIP:49983. Rewrite
	// the outbound host so the fake envd receives the request.
	eng.envdHTTP = &http.Client{Transport: rewriteHostTransport{
		base: srv.Client().Transport,
		host: strings.TrimPrefix(srv.URL, "http://"),
	}}
	if _, err := eng.RunPodSandbox(context.Background(), podRequest()); err != nil {
		t.Fatal(err)
	}

	created, err := eng.CreateContainer(context.Background(), &runtime.CreateContainerRequest{
		PodSandboxId: "uid-1",
		Config:       &runtime.ContainerConfig{Metadata: &runtime.ContainerMetadata{Name: "app"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.StartContainer(context.Background(), &runtime.StartContainerRequest{ContainerId: created.ContainerId}); err != nil {
		t.Fatal(err)
	}

	resp, err := eng.ExecSync(context.Background(), &runtime.ExecSyncRequest{
		ContainerId: created.ContainerId,
		Cmd:         []string{"echo", "hi"},
		Timeout:     5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ExitCode != 0 || string(resp.Stdout) != "hi\n" {
		t.Fatalf("exec response = exit=%d stdout=%q stderr=%q", resp.ExitCode, resp.Stdout, resp.Stderr)
	}
	if len(fake.execs) != 0 {
		t.Fatalf("CubeboxMgr.Exec should not be used, got %+v", fake.execs)
	}
	if gotPath != "/process.Process/Start" {
		t.Fatalf("path = %q", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "Basic ") {
		t.Fatalf("auth = %q", gotAuth)
	}
	// Request body is Connect-framed JSON.
	if len(gotBody) < 5 {
		t.Fatalf("body too short: %d", len(gotBody))
	}
	var start envdStartRequest
	if err := json.Unmarshal(gotBody[5:], &start); err != nil {
		t.Fatalf("decode start body: %v body=%q", err, gotBody[5:])
	}
	if start.Process.Cmd != "/bin/bash" || len(start.Process.Args) < 3 || start.Process.Args[2] != "echo hi" {
		t.Fatalf("process = %+v", start.Process)
	}
}

type rewriteHostTransport struct {
	base http.RoundTripper
	host string
}

func (t rewriteHostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = t.host
	clone.Host = t.host
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

func TestParseEnvdProcessStreamExitZeroOmitted(t *testing.T) {
	var buf bytes.Buffer
	// proto3 JSON may omit exitCode=0; recover from status / exited.
	_, _ = buf.Write(connectEnvelope(0, `{"event":{"end":{"exited":true,"status":"exit status 0"}}}`))
	_, _ = buf.Write(connectEnvelope(envdConnectEndStream, `{}`))
	result, err := parseEnvdProcessStream(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit = %d", result.ExitCode)
	}
}

func TestShellQuoteArgs(t *testing.T) {
	if got := shellQuoteArgs([]string{"echo", "a b"}); got != "echo 'a b'" {
		t.Fatalf("got %q", got)
	}
}

func TestExecSyncRequiresRunningContainer(t *testing.T) {
	eng := newTestEngine(t, &fakeClient{})
	if _, err := eng.RunPodSandbox(context.Background(), podRequest()); err != nil {
		t.Fatal(err)
	}
	_, err := eng.ExecSync(context.Background(), &runtime.ExecSyncRequest{
		ContainerId: "missing",
		Cmd:         []string{"true"},
	})
	if err == nil {
		t.Fatal("expected not found")
	}
}
