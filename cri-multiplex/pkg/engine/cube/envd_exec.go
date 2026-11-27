package cube

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	envdHTTPPort           = 49983
	envdConnectProtoVer    = "1"
	envdConnectContentType = "application/connect+json"
	envdConnectEndStream   = byte(0x02)
	envdConnectCompressed  = byte(0x01)
	envdMaxEnvelopeSize    = 64 * 1024 * 1024
	// Default cap for combined stdout+stderr retained in memory for ExecSync.
	// Matches the default gRPC max message size so a runaway guest command
	// cannot OOM cri-multiplex (KASandbox #67).
	envdMaxExecOutputBytesDefault = 16 * 1024 * 1024
	envdDefaultUser               = "root"
	envdDefaultTimeout            = 60 * time.Second
)

// Overridable in tests.
var envdMaxExecOutputBytes = envdMaxExecOutputBytesDefault

type envdStartRequest struct {
	Process envdProcessConfig `json:"process"`
	Stdin   *bool             `json:"stdin,omitempty"`
}

type envdProcessConfig struct {
	Cmd  string            `json:"cmd"`
	Args []string          `json:"args"`
	Envs map[string]string `json:"envs"`
	Cwd  string            `json:"cwd,omitempty"`
}

type envdStartResponse struct {
	Event *envdProcessEvent `json:"event"`
}

type envdProcessEvent struct {
	Start *struct {
		PID int `json:"pid"`
	} `json:"start,omitempty"`
	Data *struct {
		Stdout string `json:"stdout,omitempty"`
		Stderr string `json:"stderr,omitempty"`
	} `json:"data,omitempty"`
	End *envdEndEvent `json:"end,omitempty"`
}

type envdEndEvent struct {
	ExitCode      *int   `json:"exitCode,omitempty"`
	ExitCodeSnake *int   `json:"exit_code,omitempty"`
	Exited        bool   `json:"exited,omitempty"`
	Status        string `json:"status,omitempty"`
	Error         string `json:"error,omitempty"`
}

type envdExecResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int32
}

func (e *Engine) envdClient() *http.Client {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.envdHTTP != nil {
		return e.envdHTTP
	}
	e.envdHTTP = &http.Client{}
	return e.envdHTTP
}

func (e *Engine) execViaEnvd(ctx context.Context, sandboxIP string, cmd []string, timeoutSec int64) (*envdExecResult, error) {
	ip := strings.TrimSpace(sandboxIP)
	if ip == "" {
		return nil, status.Error(codes.FailedPrecondition, "sandbox has no IP for envd exec")
	}
	if len(cmd) == 0 {
		return nil, status.Error(codes.InvalidArgument, "exec command is empty")
	}

	timeout := time.Duration(timeoutSec) * time.Second
	if timeout <= 0 {
		timeout = envdDefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	stdin := false
	payload, err := json.Marshal(envdStartRequest{
		Process: envdProcessConfig{
			Cmd:  "/bin/bash",
			Args: []string{"-l", "-c", shellQuoteArgs(cmd)},
			Envs: map[string]string{},
			Cwd:  "/",
		},
		Stdin: &stdin,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "marshal envd start request: %v", err)
	}

	url := fmt.Sprintf("http://%s:%d/process.Process/Start", ip, envdHTTPPort)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, encodeConnectEnvelope(payload))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create envd request: %v", err)
	}
	req.Header.Set("Content-Type", envdConnectContentType)
	req.Header.Set("Connect-Protocol-Version", envdConnectProtoVer)
	req.Header.Set("Connect-Content-Encoding", "identity")
	req.SetBasicAuth(envdDefaultUser, "")

	resp, err := e.envdClient().Do(req)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, status.Errorf(codes.DeadlineExceeded, "envd exec timed out: %v", err)
		}
		return nil, status.Errorf(codes.Unavailable, "envd exec request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, status.Errorf(codes.Internal, "envd returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return parseEnvdProcessStream(resp.Body)
}

func parseEnvdProcessStream(r io.Reader) (*envdExecResult, error) {
	var stdout, stderr strings.Builder
	var exitCode int32 = -1
	sawEnd := false
	truncated := false
	retained := 0

	appendCapped := func(dst *strings.Builder, text string) {
		if text == "" || truncated {
			return
		}
		remain := envdMaxExecOutputBytes - retained
		if remain <= 0 {
			truncated = true
			return
		}
		if len(text) > remain {
			dst.WriteString(text[:remain])
			retained += remain
			truncated = true
			return
		}
		dst.WriteString(text)
		retained += len(text)
	}

	for {
		flags, payload, err := readConnectEnvelope(r)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return nil, status.Errorf(codes.Internal, "read envd stream: %v", err)
		}
		if flags&envdConnectCompressed != 0 {
			return nil, status.Error(codes.Internal, "unsupported compressed Connect stream message")
		}
		if flags&envdConnectEndStream != 0 {
			if err := parseConnectEndStream(payload); err != nil {
				return nil, status.Errorf(codes.Internal, "envd stream error: %v", err)
			}
			continue
		}

		var msg envdStartResponse
		if err := json.Unmarshal(payload, &msg); err != nil {
			return nil, status.Errorf(codes.Internal, "decode envd event: %v", err)
		}
		if msg.Event == nil {
			continue
		}
		if msg.Event.Data != nil {
			// Once capped, skip decoding further data chunks to avoid
			// allocating huge temporary strings while draining to End.
			if !truncated {
				if msg.Event.Data.Stdout != "" {
					text, err := decodeEnvdBytes(msg.Event.Data.Stdout)
					if err != nil {
						return nil, status.Errorf(codes.Internal, "decode stdout: %v", err)
					}
					appendCapped(&stdout, text)
				}
				if msg.Event.Data.Stderr != "" {
					text, err := decodeEnvdBytes(msg.Event.Data.Stderr)
					if err != nil {
						return nil, status.Errorf(codes.Internal, "decode stderr: %v", err)
					}
					appendCapped(&stderr, text)
				}
			}
		}
		if msg.Event.End != nil {
			code, ok := msg.Event.End.exitCode()
			if !ok {
				if msg.Event.End.Error != "" {
					return nil, status.Errorf(codes.Internal, "process failed: %s", msg.Event.End.Error)
				}
				return nil, status.Error(codes.Internal, "process EndEvent missing exit code")
			}
			exitCode = int32(code)
			sawEnd = true
		}
	}

	if !sawEnd {
		return nil, status.Error(codes.Internal, "envd stream ended without EndEvent")
	}
	return &envdExecResult{
		Stdout:   []byte(stdout.String()),
		Stderr:   []byte(stderr.String()),
		ExitCode: exitCode,
	}, nil
}

func (e *envdEndEvent) exitCode() (int, bool) {
	if e == nil {
		return 0, false
	}
	if e.ExitCode != nil {
		return *e.ExitCode, true
	}
	if e.ExitCodeSnake != nil {
		return *e.ExitCodeSnake, true
	}
	if s := strings.TrimSpace(e.Status); strings.HasPrefix(s, "exit status ") {
		if code, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(s, "exit status "))); err == nil {
			return code, true
		}
	}
	if e.Exited {
		return 0, true
	}
	return 0, false
}

func decodeEnvdBytes(value string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func encodeConnectEnvelope(payload []byte) io.Reader {
	buf := bytes.NewBuffer(make([]byte, 0, 5+len(payload)))
	var header [5]byte
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	buf.Write(header[:])
	buf.Write(payload)
	return buf
}

func readConnectEnvelope(r io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	size := binary.BigEndian.Uint32(header[1:])
	if size > envdMaxEnvelopeSize {
		return 0, nil, fmt.Errorf("Connect stream message too large: %d bytes", size)
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return header[0], payload, nil
}

func parseConnectEndStream(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	var end struct {
		Error *struct {
			Code    string `json:"code,omitempty"`
			Message string `json:"message,omitempty"`
		} `json:"error,omitempty"`
	}
	if err := json.Unmarshal(raw, &end); err != nil {
		return fmt.Errorf("decode Connect end stream: %w", err)
	}
	if end.Error == nil {
		return nil
	}
	message := strings.TrimSpace(end.Error.Message)
	if message == "" {
		message = "Connect stream error"
	}
	if end.Error.Code != "" {
		return fmt.Errorf("%s: %s", end.Error.Code, message)
	}
	return fmt.Errorf("%s", message)
}

func shellQuoteArgs(args []string) string {
	if len(args) == 0 {
		return "true"
	}
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, shellQuote(arg))
	}
	return strings.Join(quoted, " ")
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || strings.ContainsRune("_-./:=+,%@", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
