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
	const wantToken = "test-envd-token-69"
	var gotPath string
	var gotAuth string
	var gotAccessToken string
	var gotInitToken string
	var gotBody []byte
	var tokenSet bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/init":
			var body struct {
				AccessToken string `json:"accessToken"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotInitToken = body.AccessToken
			tokenSet = body.AccessToken != ""
			w.WriteHeader(http.StatusNoContent)
			return
		case r.URL.Path == "/process.Process/Start":
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			gotAccessToken = r.Header.Get(envdAccessTokenHeader)
			gotBody, _ = io.ReadAll(r.Body)
			if tokenSet && gotAccessToken != gotInitToken {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", envdConnectContentType)
			stdout := base64.StdEncoding.EncodeToString([]byte("hi\n"))
			_, _ = w.Write(connectEnvelope(0, `{"event":{"data":{"stdout":"`+stdout+`"}}}`))
			_, _ = w.Write(connectEnvelope(0, `{"event":{"end":{"exitCode":0,"exited":true,"status":"exit status 0"}}}`))
			_, _ = w.Write(connectEnvelope(envdConnectEndStream, `{}`))
			return
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	fake := &fakeClient{}
	eng := newTestEngine(t, fake)
	// httptest is host:port; cube always dials SandboxIP:49983. Rewrite host.
	eng.envdHTTP = &http.Client{Transport: rewriteHostTransport{
		base: srv.Client().Transport,
		host: strings.TrimPrefix(srv.URL, "http://"),
	}}
	req := podRequest()
	req.Config.Annotations[annEnvdAccessToken] = wantToken
	if _, err := eng.RunPodSandbox(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	pod, ok := eng.get("uid-1")
	if !ok || pod.EnvdAccessToken != wantToken {
		t.Fatalf("stored token = %q ok=%v", pod.EnvdAccessToken, ok)
	}
	if gotInitToken != wantToken {
		t.Fatalf("init token = %q, want %q", gotInitToken, wantToken)
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
	if gotAccessToken != wantToken {
		t.Fatalf("X-Access-Token = %q, want %q", gotAccessToken, wantToken)
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

func TestEnvdRejectsExecWithoutToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/init" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Header.Get(envdAccessTokenHeader) == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	eng := newTestEngine(t, &fakeClient{})
	eng.envdHTTP = &http.Client{Transport: rewriteHostTransport{
		base: srv.Client().Transport,
		host: strings.TrimPrefix(srv.URL, "http://"),
	}}
	if _, err := eng.RunPodSandbox(context.Background(), podRequest()); err != nil {
		t.Fatal(err)
	}
	// Clear stored token so ExecSync omits X-Access-Token.
	eng.mu.Lock()
	p := eng.pods["uid-1"]
	p.EnvdAccessToken = ""
	p.ContainerID = "uid-1-c"
	p.ContainerState = ctrRunning
	eng.mu.Unlock()

	_, err := eng.ExecSync(context.Background(), &runtime.ExecSyncRequest{
		ContainerId: "uid-1-c",
		Cmd:         []string{"id"},
		Timeout:     2,
	})
	if err == nil {
		t.Fatal("expected unauthorized exec to fail")
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

func TestParseEnvdProcessStreamCapsOutput(t *testing.T) {
	prev := envdMaxExecOutputBytes
	envdMaxExecOutputBytes = 8
	t.Cleanup(func() { envdMaxExecOutputBytes = prev })

	chunk := base64.StdEncoding.EncodeToString([]byte("abcdefghij")) // 10 bytes
	var buf bytes.Buffer
	_, _ = buf.Write(connectEnvelope(0, `{"event":{"data":{"stdout":"`+chunk+`"}}}`))
	_, _ = buf.Write(connectEnvelope(0, `{"event":{"data":{"stdout":"`+chunk+`"}}}`))
	_, _ = buf.Write(connectEnvelope(0, `{"event":{"end":{"exitCode":0,"exited":true,"status":"exit status 0"}}}`))
	_, _ = buf.Write(connectEnvelope(envdConnectEndStream, `{}`))

	result, err := parseEnvdProcessStream(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit = %d", result.ExitCode)
	}
	if got := len(result.Stdout) + len(result.Stderr); got != 8 {
		t.Fatalf("retained = %d, want 8 (stdout=%q stderr=%q)", got, result.Stdout, result.Stderr)
	}
	if string(result.Stdout) != "abcdefgh" {
		t.Fatalf("stdout = %q", result.Stdout)
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
