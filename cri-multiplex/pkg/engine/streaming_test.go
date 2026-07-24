package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/cri-multiplex/pkg/envd/process"
)

type fakeStreamTransport struct {
	mu     sync.Mutex
	writes []struct {
		channel byte
		data    []byte
	}
	closes int
}

func (f *fakeStreamTransport) writeChannel(channel byte, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, struct {
		channel byte
		data    []byte
	}{channel: channel, data: append([]byte(nil), data...)})
	return nil
}

func (f *fakeStreamTransport) close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return nil
}

func TestShellQuoteAndBuildCommand(t *testing.T) {
	if got := shellQuote("abc_1-./:=+,%@"); got != "abc_1-./:=+,%@" {
		t.Fatalf("safe shellQuote = %q", got)
	}
	if got := shellQuote(""); got != "''" {
		t.Fatalf("empty shellQuote = %q", got)
	}
	if got := shellQuote("a b'c"); got != "'a b'\\''c'" {
		t.Fatalf("special shellQuote = %q", got)
	}
	if got := shellQuoteArgs([]string{"echo", "a b"}); got != "echo 'a b'" {
		t.Fatalf("shellQuoteArgs = %q", got)
	}
	if got := buildShellCommand(nil, nil); got != "sleep 3600" {
		t.Fatalf("empty buildShellCommand = %q", got)
	}
	if got := buildShellCommand([]string{"echo"}, []string{"hello world"}); got != "echo 'hello world'" {
		t.Fatalf("buildShellCommand = %q", got)
	}
}

func TestProcessConfigBuilders(t *testing.T) {
	cfg := processConfigFromCommand([]string{"echo", "hello"})
	if cfg.Cmd != "/bin/bash" || len(cfg.Args) != 3 || cfg.Args[0] != "-l" || cfg.Args[1] != "-c" || cfg.Args[2] != "echo hello" {
		t.Fatalf("processConfigFromCommand = %+v", cfg)
	}
	if cfg.Cwd == nil || *cfg.Cwd != "/" {
		t.Fatalf("cwd = %v, want /", cfg.Cwd)
	}

	shell := processConfigForInteractiveShell()
	if shell.Cmd != "/bin/bash" || len(shell.Args) != 1 || shell.Args[0] != "-l" {
		t.Fatalf("processConfigForInteractiveShell = %+v", shell)
	}
}

func TestEnvdFrameRoundTripAndErrors(t *testing.T) {
	frame := writeGRPCFrameToBuf([]byte("payload"))
	payload, err := readEnvdFrame(bytes.NewReader(frame))
	if err != nil {
		t.Fatalf("readEnvdFrame: %v", err)
	}
	if string(payload) != "payload" {
		t.Fatalf("payload = %q", payload)
	}
	if _, err := readEnvdFrame(stringsReader("")); err != io.EOF {
		t.Fatalf("empty frame err = %v, want EOF", err)
	}
	tooLarge := []byte{0, 1, 0, 0, 0}
	if _, err := readEnvdFrame(bytes.NewReader(tooLarge)); err == nil {
		t.Fatal("expected too large frame error")
	}
}

func stringsReader(s string) io.Reader {
	return bytes.NewBufferString(s)
}

func TestStreamingSessionStatusAndClose(t *testing.T) {
	transport := &fakeStreamTransport{}
	session := &streamingSession{stream: transport}

	session.sendSuccess()
	session.sendExitStatus(7)
	session.sendError("failed")
	session.close()

	if transport.closes != 1 {
		t.Fatalf("close count = %d, want 1", transport.closes)
	}
	if len(transport.writes) != 3 {
		t.Fatalf("write count = %d, want 3", len(transport.writes))
	}
	for i, write := range transport.writes {
		if write.channel != channelErr {
			t.Fatalf("write %d channel = %d, want error channel", i, write.channel)
		}
		var status remoteCommandStatus
		if err := json.Unmarshal(write.data, &status); err != nil {
			t.Fatalf("unmarshal status %d: %v", i, err)
		}
	}
	var exit remoteCommandStatus
	if err := json.Unmarshal(transport.writes[1].data, &exit); err != nil {
		t.Fatal(err)
	}
	if exit.Reason != "NonZeroExitCode" || exit.Details.Causes[0].Message != "7" {
		t.Fatalf("exit status mismatch: %+v", exit)
	}
}

func TestStreamingSessionAccessToken(t *testing.T) {
	pod := &podInfo{envdAccessToken: "pod-token"}
	session := &streamingSession{pod: pod}
	if got := session.accessToken(); got != "pod-token" {
		t.Fatalf("pod token = %q", got)
	}
	session.execReq = &execStreamRequest{accessToken: "exec-token"}
	if got := session.accessToken(); got != "exec-token" {
		t.Fatalf("exec token = %q", got)
	}
	session.execReq = nil
	session.attachReq = &attachStreamRequest{accessToken: "attach-token"}
	if got := session.accessToken(); got != "attach-token" {
		t.Fatalf("attach token = %q", got)
	}
}

func TestStreamingSessionWaitPID(t *testing.T) {
	session := &streamingSession{}
	session.pid = 123
	if got := session.waitPID(context.Background()); got != 123 {
		t.Fatalf("waitPID existing = %d", got)
	}

	session = &streamingSession{}
	go func() {
		time.Sleep(20 * time.Millisecond)
		session.mu.Lock()
		session.pid = 456
		session.mu.Unlock()
	}()
	if got := session.waitPID(context.Background()); got != 456 {
		t.Fatalf("waitPID delayed = %d", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := (&streamingSession{}).waitPID(ctx); got != 0 {
		t.Fatalf("waitPID canceled = %d, want 0", got)
	}
}

func TestForwardEnvdToChannelsStartStream(t *testing.T) {
	var body bytes.Buffer
	events := []*process.ProcessEvent{
		{Event: &process.ProcessEvent_Start{Start: &process.ProcessEvent_StartEvent{Pid: 321}}},
		{Event: &process.ProcessEvent_Data{Data: &process.ProcessEvent_DataEvent{
			Output: &process.ProcessEvent_DataEvent_Stdout{Stdout: []byte("stdout")},
		}}},
		{Event: &process.ProcessEvent_Data{Data: &process.ProcessEvent_DataEvent{
			Output: &process.ProcessEvent_DataEvent_Stderr{Stderr: []byte("stderr")},
		}}},
		{Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{ExitCode: 0}}},
	}
	for _, event := range events {
		payload, err := proto.Marshal(&process.StartResponse{Event: event})
		if err != nil {
			t.Fatalf("marshal StartResponse: %v", err)
		}
		body.Write(writeGRPCFrameToBuf(payload))
	}

	transport := &fakeStreamTransport{}
	session := &streamingSession{stream: transport, stdout: true, stderr: true}
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body.Bytes()))}

	session.forwardEnvdToChannels(resp, true)

	if session.currentPID() != 321 {
		t.Fatalf("pid = %d, want 321", session.currentPID())
	}
	if transport.closes != 1 {
		t.Fatalf("close count = %d, want 1", transport.closes)
	}
	if len(transport.writes) != 3 {
		t.Fatalf("write count = %d, want stdout/stderr/status", len(transport.writes))
	}
	if transport.writes[0].channel != channelStdOut || string(transport.writes[0].data) != "stdout" {
		t.Fatalf("stdout write mismatch: %+v", transport.writes[0])
	}
	if transport.writes[1].channel != channelStdErr || string(transport.writes[1].data) != "stderr" {
		t.Fatalf("stderr write mismatch: %+v", transport.writes[1])
	}
	var status remoteCommandStatus
	if err := json.Unmarshal(transport.writes[2].data, &status); err != nil {
		t.Fatalf("unmarshal final status: %v", err)
	}
	if status.Status != "Success" {
		t.Fatalf("final status = %+v, want Success", status)
	}
}

func TestForwardEnvdToChannelsConnectTTYStreamFailureExit(t *testing.T) {
	var body bytes.Buffer
	events := []*process.ProcessEvent{
		{Event: &process.ProcessEvent_Data{Data: &process.ProcessEvent_DataEvent{
			Output: &process.ProcessEvent_DataEvent_Pty{Pty: []byte("pty-out")},
		}}},
		{Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{ExitCode: 9}}},
	}
	for _, event := range events {
		payload, err := proto.Marshal(&process.ConnectResponse{Event: event})
		if err != nil {
			t.Fatalf("marshal ConnectResponse: %v", err)
		}
		body.Write(writeGRPCFrameToBuf(payload))
	}

	transport := &fakeStreamTransport{}
	session := &streamingSession{stream: transport, tty: true, stdout: true, stderr: true}
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body.Bytes()))}

	session.forwardEnvdToChannels(resp, false)

	if len(transport.writes) != 2 {
		t.Fatalf("write count = %d, want pty output + status", len(transport.writes))
	}
	if transport.writes[0].channel != channelStdOut || string(transport.writes[0].data) != "pty-out" {
		t.Fatalf("pty write mismatch: %+v", transport.writes[0])
	}
	var status remoteCommandStatus
	if err := json.Unmarshal(transport.writes[1].data, &status); err != nil {
		t.Fatalf("unmarshal exit status: %v", err)
	}
	if status.Reason != "NonZeroExitCode" || status.Details.Causes[0].Message != "9" {
		t.Fatalf("exit status mismatch: %+v", status)
	}
}

func TestEnvdStreamRequestBuilders(t *testing.T) {
	var seen []string
	e := &grpcE2BEngine{envdHTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		seen = append(seen, req.URL.Path)
		if req.Header.Get("X-Access-Token") != "token-a" {
			t.Fatalf("missing access token for %s", req.URL.Path)
		}
		return httpResponse(http.StatusOK, nil), nil
	})}}
	pod := &podInfo{sandboxID: "sandbox-a", envdAccessToken: "token-a"}

	session := &streamingSession{
		e:       e,
		pod:     pod,
		tty:     true,
		stdin:   true,
		stdout:  true,
		stderr:  true,
		execReq: &execStreamRequest{sandboxID: "sandbox-a", cmd: []string{"echo", "hi"}, accessToken: "token-a"},
	}
	if resp, err := session.startEnvdExec(context.Background()); err != nil {
		t.Fatalf("startEnvdExec: %v", err)
	} else {
		_ = resp.Body.Close()
	}
	session.execReq = nil
	session.attachReq = &attachStreamRequest{sandboxID: "sandbox-a", accessToken: "token-a"}
	if resp, err := session.startEnvdAttachShell(context.Background()); err != nil {
		t.Fatalf("startEnvdAttachShell: %v", err)
	} else {
		_ = resp.Body.Close()
	}
	if resp, err := session.connectEnvdAttach(context.Background(), 123); err != nil {
		t.Fatalf("connectEnvdAttach: %v", err)
	} else {
		_ = resp.Body.Close()
	}

	want := []string{"/process.Process/Start", "/process.Process/Start", "/process.Process/Connect"}
	if len(seen) != len(want) {
		t.Fatalf("request count = %d, want %d: %v", len(seen), len(want), seen)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("request %d path = %s, want %s", i, seen[i], want[i])
		}
	}
}

func TestStreamingSessionSendInputAndUpdatePTYSize(t *testing.T) {
	var paths []string
	e := &grpcE2BEngine{envdHTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		switch req.URL.Path {
		case "/process.Process/SendInput":
			var input process.SendInputRequest
			if err := proto.Unmarshal(body, &input); err != nil {
				t.Fatalf("unmarshal SendInputRequest: %v", err)
			}
			if input.Process.GetPid() != 123 || string(input.Input.GetPty()) != "abc" {
				t.Fatalf("send input mismatch: %+v", &input)
			}
		case "/process.Process/Update":
			var update process.UpdateRequest
			if err := proto.Unmarshal(body, &update); err != nil {
				t.Fatalf("unmarshal UpdateRequest: %v", err)
			}
			if update.Process.GetPid() != 123 || update.Pty.GetSize().GetCols() != 100 || update.Pty.GetSize().GetRows() != 40 {
				t.Fatalf("update mismatch: %+v", &update)
			}
		default:
			t.Fatalf("unexpected path: %s", req.URL.Path)
		}
		return httpResponse(http.StatusOK, nil), nil
	})}}
	session := &streamingSession{e: e, pod: &podInfo{sandboxID: "sandbox-a", envdAccessToken: "token-a"}, pid: 123}
	if err := session.sendInputToEnvd(context.Background(), []byte("abc"), true); err != nil {
		t.Fatalf("sendInputToEnvd: %v", err)
	}
	if err := session.updatePTYSize(context.Background(), 100, 40); err != nil {
		t.Fatalf("updatePTYSize: %v", err)
	}
	if len(paths) != 2 {
		t.Fatalf("request paths = %v, want 2 calls", paths)
	}
}

func TestWaitTCPReady(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			_ = conn.Close()
		}
	}()

	host, portText, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	if err := waitTCPReady(context.Background(), host, port, nil); err != nil {
		t.Fatalf("waitTCPReady: %v", err)
	}

	exitCh := make(chan error, 1)
	exitCh <- io.EOF
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := waitTCPReady(ctx, "127.0.0.1", 1, exitCh); err == nil {
		t.Fatal("expected exit before ready error")
	}
}
