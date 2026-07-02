package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	httpstreamspdy "k8s.io/apimachinery/pkg/util/httpstream/spdy"
	k8sremotecommand "k8s.io/apimachinery/pkg/util/remotecommand"

	"github.com/cri-multiplex/pkg/envd/process"
)

// Kubernetes CRI streaming protocol v5 channel IDs.
const (
	channelStdIn  = 0
	channelStdOut = 1
	channelStdErr = 2
	channelErr    = 3
	channelResize = 4
	channelClose  = 255
)

// CRI streaming v5 subprotocol used by kubectl >= 1.29.
const streamingV5Subprotocol = "v5.channel.k8s.io"

var streamingSPDYProtocols = []string{
	streamingV5Subprotocol,
	k8sremotecommand.StreamProtocolV4Name,
	k8sremotecommand.StreamProtocolV3Name,
	k8sremotecommand.StreamProtocolV2Name,
	k8sremotecommand.StreamProtocolV1Name,
}

var websocketUpgrader = websocket.Upgrader{
	Subprotocols:    []string{streamingV5Subprotocol},
	CheckOrigin:     func(r *http.Request) bool { return true },
	ReadBufferSize:  32 * 1024,
	WriteBufferSize: 32 * 1024,
}

type streamTransport interface {
	writeChannel(channel byte, data []byte) error
	close() error
}

type websocketStreamTransport struct {
	conn *websocket.Conn
}

func (t *websocketStreamTransport) writeChannel(channel byte, data []byte) error {
	payload := make([]byte, len(data)+1)
	payload[0] = channel
	copy(payload[1:], data)
	return t.conn.WriteMessage(websocket.BinaryMessage, payload)
}

func (t *websocketStreamTransport) close() error {
	return t.conn.Close()
}

type spdyStreamTransport struct {
	conn      httpstream.Connection
	stdin     httpstream.Stream
	stdout    httpstream.Stream
	stderr    httpstream.Stream
	errStream httpstream.Stream
	resize    httpstream.Stream
}

func (t *spdyStreamTransport) writeChannel(channel byte, data []byte) error {
	var stream httpstream.Stream
	switch channel {
	case channelStdOut:
		stream = t.stdout
	case channelStdErr:
		stream = t.stderr
	case channelErr:
		stream = t.errStream
	default:
		return nil
	}
	if stream == nil {
		return nil
	}
	_, err := stream.Write(data)
	return err
}

func (t *spdyStreamTransport) close() error {
	if t.conn == nil {
		return nil
	}
	return t.conn.Close()
}

type spdyStreamAndReply struct {
	stream    httpstream.Stream
	replySent <-chan struct{}
}

// streamingSession 表示一次 CRI streaming 会话（Exec 或 Attach）。
type streamingSession struct {
	e      *grpcE2BEngine
	pod    *podInfo
	conn   *websocket.Conn
	stream streamTransport
	tty    bool
	stdin  bool
	stdout bool
	stderr bool

	// exec-only
	execReq *execStreamRequest
	// attach-only
	attachReq *attachStreamRequest

	mu      sync.Mutex
	writeMu sync.Mutex
	pid     uint32
	closed  bool
}

type remoteCommandStatus struct {
	Status  string                      `json:"status,omitempty"`
	Message string                      `json:"message,omitempty"`
	Reason  string                      `json:"reason,omitempty"`
	Details *remoteCommandStatusDetails `json:"details,omitempty"`
}

type remoteCommandStatusDetails struct {
	Causes []remoteCommandStatusCause `json:"causes,omitempty"`
}

type remoteCommandStatusCause struct {
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// closeSession 幂等地关闭 WebSocket 连接。
func (s *streamingSession) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.stream != nil {
		_ = s.stream.close()
		return
	}
	if s.conn != nil {
		_ = s.conn.Close()
	}
}

// sendChannelData 将数据发送到指定 channel。
func (s *streamingSession) sendChannelData(channel byte, data []byte) error {
	if s.stream == nil {
		return fmt.Errorf("stream transport is not available")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.stream.writeChannel(channel, data)
}

func (s *streamingSession) sendStatus(st remoteCommandStatus) {
	payload, err := json.Marshal(st)
	if err != nil {
		log.Printf("[streaming] marshal status error: %v", err)
		return
	}
	if err := s.sendChannelData(channelErr, payload); err != nil {
		log.Printf("[streaming] write status error: %v", err)
	}
}

func (s *streamingSession) sendSuccess() {
	s.sendStatus(remoteCommandStatus{Status: "Success"})
}

func (s *streamingSession) sendExitStatus(exitCode int32) {
	if exitCode == 0 {
		s.sendSuccess()
		return
	}
	s.sendStatus(remoteCommandStatus{
		Status: "Failure",
		Reason: "NonZeroExitCode",
		Details: &remoteCommandStatusDetails{
			Causes: []remoteCommandStatusCause{{
				Reason:  "ExitCode",
				Message: fmt.Sprintf("%d", exitCode),
			}},
		},
	})
}

// sendError 向 error channel 发送 Kubernetes remotecommand status 并关闭连接。
func (s *streamingSession) sendError(msg string) {
	s.sendStatus(remoteCommandStatus{Status: "Failure", Message: msg})
	s.close()
}

func (s *streamingSession) accessToken() string {
	if s.execReq != nil && s.execReq.accessToken != "" {
		return s.execReq.accessToken
	}
	if s.attachReq != nil && s.attachReq.accessToken != "" {
		return s.attachReq.accessToken
	}
	return s.pod.envdAccessToken
}

func (s *streamingSession) currentPID() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pid
}

func (s *streamingSession) waitPID(ctx context.Context) uint32 {
	if pid := s.currentPID(); pid != 0 {
		return pid
	}
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return 0
		case <-timeout.C:
			return s.currentPID()
		case <-ticker.C:
			if pid := s.currentPID(); pid != 0 {
				return pid
			}
		}
	}
}

// readEnvdFrame 从 envd HTTP 响应体读取一个 gRPC 帧。
func readEnvdFrame(r io.Reader) ([]byte, error) {
	prefix := make([]byte, 5)
	if _, err := io.ReadFull(r, prefix); err != nil {
		if err == io.EOF {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("read gRPC prefix: %w", err)
	}
	length := int(binary.BigEndian.Uint32(prefix[1:5]))
	if length == 0 {
		return nil, nil
	}
	const maxMessageSize = 16 * 1024 * 1024
	if length > maxMessageSize {
		return nil, fmt.Errorf("gRPC message too large: %d bytes", length)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, fmt.Errorf("read gRPC payload: %w", err)
	}
	return payload, nil
}

// writeGRPCFrameToBuf 把 protobuf payload 写入 gRPC 帧格式缓冲区。
func writeGRPCFrameToBuf(payload []byte) []byte {
	prefix := make([]byte, 5)
	prefix[0] = 0
	binary.BigEndian.PutUint32(prefix[1:5], uint32(len(payload)))
	return append(prefix, payload...)
}

func processConfigFromCommand(cmd []string) *process.ProcessConfig {
	cwd := "/"
	return &process.ProcessConfig{
		Cmd:  "/bin/bash",
		Args: []string{"-l", "-c", shellQuoteArgs(cmd)},
		Envs: map[string]string{},
		Cwd:  &cwd,
	}
}

func buildShellCommand(command, args []string) string {
	argv := make([]string, 0, len(command)+len(args))
	argv = append(argv, command...)
	argv = append(argv, args...)
	if len(argv) == 0 {
		argv = []string{"sleep", "3600"}
	}
	return shellQuoteArgs(argv)
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

// startEnvdExec 启动 envd Start stream，返回 HTTP 响应和请求体。
func (s *streamingSession) startEnvdExec(ctx context.Context) (*http.Response, error) {
	stdin := s.stdin
	startReq := &process.StartRequest{
		Process: processConfigFromCommand(s.execReq.cmd),
		Stdin:   &stdin,
	}
	if s.tty {
		startReq.Pty = &process.PTY{
			Size: &process.PTY_Size{Cols: 80, Rows: 24},
		}
	}

	payload, err := proto.Marshal(startReq)
	if err != nil {
		return nil, fmt.Errorf("marshal StartRequest: %w", err)
	}
	body := writeGRPCFrameToBuf(payload)

	req, err := newEnvdRequest(ctx, http.MethodPost, s.pod.envdSandboxID(), "/process.Process/Start", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create envd start request: %w", err)
	}
	req.Header.Set("Content-Type", "application/connect+proto")
	req.Header.Set("Accept", "application/connect+proto")
	req.Header.Set("Connect-Protocol-Version", "1")
	if token := s.accessToken(); token != "" {
		req.Header.Set("X-Access-Token", token)
	}

	resp, err := s.e.getEnvdHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("envd start failed: %w", err)
	}
	return resp, nil
}

// connectEnvdAttach 启动 envd Connect stream。
func (s *streamingSession) connectEnvdAttach(ctx context.Context, pid uint32) (*http.Response, error) {
	connectReq := &process.ConnectRequest{
		Process: &process.ProcessSelector{
			Selector: &process.ProcessSelector_Pid{Pid: pid},
		},
	}
	payload, err := proto.Marshal(connectReq)
	if err != nil {
		return nil, fmt.Errorf("marshal ConnectRequest: %w", err)
	}
	body := writeGRPCFrameToBuf(payload)

	req, err := newEnvdRequest(ctx, http.MethodPost, s.pod.envdSandboxID(), "/process.Process/Connect", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create envd connect request: %w", err)
	}
	req.Header.Set("Content-Type", "application/connect+proto")
	req.Header.Set("Accept", "application/connect+proto")
	req.Header.Set("Connect-Protocol-Version", "1")
	if token := s.accessToken(); token != "" {
		req.Header.Set("X-Access-Token", token)
	}

	resp, err := s.e.getEnvdHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("envd connect failed: %w", err)
	}
	return resp, nil
}

// sendInputToEnvd 将输入数据发送到 envd。
func (s *streamingSession) sendInputToEnvd(ctx context.Context, data []byte, isTTY bool) error {
	pid := s.waitPID(ctx)
	if pid == 0 {
		return fmt.Errorf("process pid not available")
	}

	var input *process.ProcessInput
	if isTTY {
		input = &process.ProcessInput{Input: &process.ProcessInput_Pty{Pty: data}}
	} else {
		input = &process.ProcessInput{Input: &process.ProcessInput_Stdin{Stdin: data}}
	}
	inputReq := &process.SendInputRequest{
		Process: &process.ProcessSelector{Selector: &process.ProcessSelector_Pid{Pid: pid}},
		Input:   input,
	}
	payload, err := proto.Marshal(inputReq)
	if err != nil {
		return fmt.Errorf("marshal SendInput: %w", err)
	}

	req, err := newEnvdRequest(ctx, http.MethodPost, s.pod.envdSandboxID(), "/process.Process/SendInput", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create SendInput request: %w", err)
	}
	req.Header.Set("Content-Type", "application/proto")
	req.Header.Set("Accept", "application/proto")
	req.Header.Set("Connect-Protocol-Version", "1")
	if token := s.accessToken(); token != "" {
		req.Header.Set("X-Access-Token", token)
	}

	resp, err := s.e.getEnvdHTTPClient().Do(req)
	if err != nil {
		return fmt.Errorf("sendInput request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("sendInput envd error %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// updatePTYSize 更新 envd PTY 尺寸。
func (s *streamingSession) updatePTYSize(ctx context.Context, cols, rows uint16) error {
	s.mu.Lock()
	pid := s.pid
	s.mu.Unlock()
	if pid == 0 {
		return nil
	}

	updateReq := &process.UpdateRequest{
		Process: &process.ProcessSelector{Selector: &process.ProcessSelector_Pid{Pid: pid}},
		Pty: &process.PTY{
			Size: &process.PTY_Size{Cols: uint32(cols), Rows: uint32(rows)},
		},
	}
	payload, err := proto.Marshal(updateReq)
	if err != nil {
		return fmt.Errorf("marshal Update: %w", err)
	}

	req, err := newEnvdRequest(ctx, http.MethodPost, s.pod.envdSandboxID(), "/process.Process/Update", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create Update request: %w", err)
	}
	req.Header.Set("Content-Type", "application/proto")
	req.Header.Set("Accept", "application/proto")
	req.Header.Set("Connect-Protocol-Version", "1")
	if token := s.accessToken(); token != "" {
		req.Header.Set("X-Access-Token", token)
	}

	resp, err := s.e.getEnvdHTTPClient().Do(req)
	if err != nil {
		return fmt.Errorf("update request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("update envd error %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func (s *streamingSession) upgradeSPDY(w http.ResponseWriter, r *http.Request) (*spdyStreamTransport, error) {
	protocol, err := httpstream.Handshake(r, w, streamingSPDYProtocols)
	if err != nil {
		return nil, err
	}
	log.Printf("[streaming] negotiated SPDY protocol %q", protocol)

	streamCh := make(chan spdyStreamAndReply, 8)
	upgrader := httpstreamspdy.NewResponseUpgrader()
	conn := upgrader.UpgradeResponse(w, r, func(stream httpstream.Stream, replySent <-chan struct{}) error {
		streamCh <- spdyStreamAndReply{stream: stream, replySent: replySent}
		return nil
	})
	if conn == nil {
		return nil, fmt.Errorf("spdy upgrade failed")
	}

	transport := &spdyStreamTransport{conn: conn}
	if err := s.collectSPDYStreams(r.Context(), transport, streamCh); err != nil {
		_ = transport.close()
		return nil, err
	}
	s.stream = transport
	return transport, nil
}

func (s *streamingSession) collectSPDYStreams(ctx context.Context, transport *spdyStreamTransport, streamCh <-chan spdyStreamAndReply) error {
	required := map[string]bool{
		corev1.StreamTypeError: true,
	}
	if s.stdin {
		required[corev1.StreamTypeStdin] = true
	}
	if s.stdout {
		required[corev1.StreamTypeStdout] = true
	}
	if s.stderr && !s.tty {
		required[corev1.StreamTypeStderr] = true
	}

	got := make(map[string]bool, len(required))
	deadline := time.NewTimer(k8sremotecommand.DefaultStreamCreationTimeout)
	defer deadline.Stop()

	for len(got) < len(required) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("timed out waiting for SPDY streams: got %v, want %v", got, required)
		case item := <-streamCh:
			streamType := registerSPDYStream(transport, item.stream)
			waitSPDYReply(item.replySent)
			if required[streamType] {
				got[streamType] = true
			}
		}
	}

	// Resize is optional and is created only when the client has a terminal size
	// queue. Drain briefly so interactive TTY sessions can send resize events.
	grace := time.NewTimer(500 * time.Millisecond)
	defer grace.Stop()
	for {
		select {
		case item := <-streamCh:
			registerSPDYStream(transport, item.stream)
			waitSPDYReply(item.replySent)
		case <-grace.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func registerSPDYStream(transport *spdyStreamTransport, stream httpstream.Stream) string {
	streamType := stream.Headers().Get(corev1.StreamType)
	switch streamType {
	case corev1.StreamTypeStdin:
		transport.stdin = stream
	case corev1.StreamTypeStdout:
		transport.stdout = stream
	case corev1.StreamTypeStderr:
		transport.stderr = stream
	case corev1.StreamTypeError:
		transport.errStream = stream
	case corev1.StreamTypeResize:
		transport.resize = stream
	default:
		log.Printf("[streaming] ignoring unknown SPDY stream type %q", streamType)
		_ = stream.Reset()
	}
	return streamType
}

func waitSPDYReply(replySent <-chan struct{}) {
	if replySent == nil {
		return
	}
	select {
	case <-replySent:
	case <-time.After(5 * time.Second):
		log.Printf("[streaming] timed out waiting for SPDY stream reply")
	}
}

func (s *streamingSession) forwardSPDYStdin(ctx context.Context, stream httpstream.Stream) {
	buf := make([]byte, 32*1024)
	for {
		n, err := stream.Read(buf)
		if n > 0 {
			if sendErr := s.sendInputToEnvd(ctx, append([]byte(nil), buf[:n]...), s.tty); sendErr != nil {
				log.Printf("[streaming] spdy stdin sendInput error: %v", sendErr)
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				log.Printf("[streaming] spdy stdin read error: %v", err)
			}
			return
		}
	}
}

func (s *streamingSession) forwardSPDYResize(ctx context.Context, stream httpstream.Stream) {
	decoder := json.NewDecoder(stream)
	for {
		var size struct {
			Width  uint16
			Height uint16
		}
		if err := decoder.Decode(&size); err != nil {
			if err != io.EOF {
				log.Printf("[streaming] spdy resize decode error: %v", err)
			}
			return
		}
		if err := s.updatePTYSize(ctx, size.Width, size.Height); err != nil {
			log.Printf("[streaming] spdy updatePTYSize error: %v", err)
		}
	}
}

// forwardEnvdToChannels 读取 envd gRPC 流并转发到 WebSocket channel。
// 适用于 Exec 的 StartResponse 和 Attach 的 ConnectResponse。
func (s *streamingSession) forwardEnvdToChannels(resp *http.Response, isStart bool) {
	defer s.close()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		s.sendError(fmt.Sprintf("envd returned %d: %s", resp.StatusCode, string(body)))
		return
	}

	for {
		framePayload, err := readEnvdFrame(resp.Body)
		if err == io.EOF {
			s.sendSuccess()
			return
		}
		if err != nil {
			log.Printf("[streaming] read envd frame error: %v", err)
			s.sendError(fmt.Sprintf("read envd frame: %v", err))
			return
		}
		if len(framePayload) == 0 {
			continue
		}

		var event *process.ProcessEvent
		if isStart {
			var msg process.StartResponse
			if err := proto.Unmarshal(framePayload, &msg); err == nil && msg.Event != nil {
				event = msg.Event
			}
		} else {
			var msg process.ConnectResponse
			if err := proto.Unmarshal(framePayload, &msg); err == nil && msg.Event != nil {
				event = msg.Event
			}
		}
		if event == nil {
			continue
		}

		if start := event.GetStart(); start != nil {
			s.mu.Lock()
			s.pid = start.GetPid()
			s.mu.Unlock()
			log.Printf("[streaming] envd process started, pid=%d", start.GetPid())
		} else if data := event.GetData(); data != nil {
			if s.tty {
				if out := data.GetPty(); len(out) > 0 && s.stdout {
					if err := s.sendChannelData(channelStdOut, out); err != nil {
						log.Printf("[streaming] write pty stdout error: %v", err)
						return
					}
				}
			} else {
				if out := data.GetStdout(); len(out) > 0 && s.stdout {
					if err := s.sendChannelData(channelStdOut, out); err != nil {
						log.Printf("[streaming] write stdout error: %v", err)
						return
					}
				}
				if out := data.GetStderr(); len(out) > 0 && s.stderr {
					if err := s.sendChannelData(channelStdErr, out); err != nil {
						log.Printf("[streaming] write stderr error: %v", err)
						return
					}
				}
			}
		} else if end := event.GetEnd(); end != nil {
			log.Printf("[streaming] envd process ended, exit_code=%d", end.GetExitCode())
			if end.GetError() != "" && end.GetExitCode() == 0 {
				s.sendError(end.GetError())
			} else {
				s.sendExitStatus(end.GetExitCode())
			}
			return
		}
	}
}

// forwardWebSocketToEnvd 读取 WebSocket 消息并转发到 envd。
func (s *streamingSession) forwardWebSocketToEnvd(ctx context.Context) {
	defer s.close()

	for {
		_, data, err := s.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Printf("[streaming] websocket read error: %v", err)
			}
			return
		}
		if len(data) == 0 {
			continue
		}

		if data[0] == channelClose {
			if len(data) != 2 {
				log.Printf("[streaming] invalid close frame length: %d", len(data))
				return
			}
			// v5 close is a half-close for the addressed stream. envd's
			// unary SendInput API has no matching half-close primitive, and
			// kubectl attach/exec tests send explicit commands such as "exit".
			continue
		}

		channel := data[0]
		payload := data[1:]

		switch channel {
		case channelStdIn:
			if !s.stdin {
				continue
			}
			if err := s.sendInputToEnvd(ctx, payload, s.tty); err != nil {
				log.Printf("[streaming] sendInput error: %v", err)
				return
			}
		case channelResize:
			if !s.tty || len(payload) == 0 {
				continue
			}
			var size struct {
				Width  uint16
				Height uint16
			}
			if err := json.Unmarshal(payload, &size); err != nil {
				log.Printf("[streaming] resize decode error: %v", err)
				continue
			}
			if err := s.updatePTYSize(ctx, size.Width, size.Height); err != nil {
				log.Printf("[streaming] updatePTYSize error: %v", err)
			}
		default:
			// 忽略客户端发送到其他 channel 的数据
		}
	}
}

func (s *streamingSession) resolveAttachPID(ctx context.Context) (uint32, bool) {
	pid := s.pod.mainPID
	if pid == 0 {
		listResp, err := s.e.doListRequest(ctx, s.pod.envdSandboxID(), s.accessToken())
		if err != nil {
			s.sendError(fmt.Sprintf("list processes failed: %v", err))
			return 0, false
		}
		if len(listResp.Processes) == 0 {
			s.sendError("no running processes in sandbox")
			return 0, false
		}
		pid = listResp.Processes[0].GetPid()
	}
	s.mu.Lock()
	s.pid = pid
	s.mu.Unlock()

	sandboxID := s.pod.sandboxID
	if s.attachReq != nil && s.attachReq.sandboxID != "" {
		sandboxID = s.attachReq.sandboxID
	}
	log.Printf("[streaming] attach to sandbox cri_id=%s, e2b_id=%s, pid=%d", sandboxID, s.pod.envdSandboxID(), pid)
	return pid, true
}

// handleExecStream 处理 CRI Exec streaming 请求（支持 WebSocket v5）。
func (e *grpcE2BEngine) handleExecStream(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/exec/")
	if token == "" {
		http.Error(w, "missing token", http.StatusBadRequest)
		return
	}

	e.streamingMu.RLock()
	sreq, ok := e.streamingReqs[token]
	e.streamingMu.RUnlock()
	if !ok {
		http.Error(w, "invalid or expired exec token", http.StatusForbidden)
		return
	}

	pod, ok := e.tracker.Get(sreq.sandboxID)
	if !ok || pod.state != stateRunning {
		http.Error(w, "sandbox not running", http.StatusNotFound)
		return
	}

	if websocket.IsWebSocketUpgrade(r) {
		conn, err := websocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("[streaming] exec websocket upgrade error: %v", err)
			return
		}

		session := &streamingSession{
			e:       e,
			pod:     pod,
			conn:    conn,
			stream:  &websocketStreamTransport{conn: conn},
			tty:     sreq.tty,
			stdin:   sreq.stdin,
			stdout:  sreq.stdout,
			stderr:  sreq.stderr,
			execReq: sreq,
		}
		defer session.close()

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		resp, err := session.startEnvdExec(ctx)
		if err != nil {
			session.sendError(err.Error())
			return
		}

		go session.forwardEnvdToChannels(resp, true)
		session.forwardWebSocketToEnvd(ctx)
		return
	}

	if httpstream.IsUpgradeRequest(r) {
		session := &streamingSession{
			e:       e,
			pod:     pod,
			tty:     sreq.tty,
			stdin:   sreq.stdin,
			stdout:  sreq.stdout,
			stderr:  sreq.stderr,
			execReq: sreq,
		}
		transport, err := session.upgradeSPDY(w, r)
		if err != nil {
			log.Printf("[streaming] exec spdy upgrade error: %v", err)
			return
		}
		defer session.close()

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		resp, err := session.startEnvdExec(ctx)
		if err != nil {
			session.sendError(err.Error())
			return
		}
		if transport.stdin != nil {
			go session.forwardSPDYStdin(ctx, transport.stdin)
		}
		if transport.resize != nil {
			go session.forwardSPDYResize(ctx, transport.resize)
		}
		go session.forwardEnvdToChannels(resp, true)
		<-transport.conn.CloseChan()
		return
	}

	http.Error(w, "websocket or spdy upgrade required", http.StatusBadRequest)
}

// handleAttachStream 处理 CRI Attach streaming 请求（支持 WebSocket v5）。
func (e *grpcE2BEngine) handleAttachStream(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/attach/")
	if token == "" {
		http.Error(w, "missing token", http.StatusBadRequest)
		return
	}

	e.streamingMu.RLock()
	areq, ok := e.attachReqs[token]
	e.streamingMu.RUnlock()
	if !ok {
		http.Error(w, "invalid or expired attach token", http.StatusForbidden)
		return
	}

	pod, ok := e.tracker.Get(areq.sandboxID)
	if !ok || pod.state != stateRunning {
		http.Error(w, "sandbox not running", http.StatusNotFound)
		return
	}

	if websocket.IsWebSocketUpgrade(r) {
		conn, err := websocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("[streaming] attach websocket upgrade error: %v", err)
			return
		}

		session := &streamingSession{
			e:         e,
			pod:       pod,
			conn:      conn,
			stream:    &websocketStreamTransport{conn: conn},
			tty:       areq.tty,
			stdin:     areq.stdin,
			stdout:    areq.stdout,
			stderr:    areq.stderr,
			attachReq: areq,
		}
		defer session.close()

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		pid, ok := session.resolveAttachPID(ctx)
		if !ok {
			return
		}

		resp, err := session.connectEnvdAttach(ctx, pid)
		if err != nil {
			session.sendError(err.Error())
			return
		}

		go session.forwardEnvdToChannels(resp, false)
		session.forwardWebSocketToEnvd(ctx)
		return
	}

	if httpstream.IsUpgradeRequest(r) {
		session := &streamingSession{
			e:         e,
			pod:       pod,
			tty:       areq.tty,
			stdin:     areq.stdin,
			stdout:    areq.stdout,
			stderr:    areq.stderr,
			attachReq: areq,
		}
		transport, err := session.upgradeSPDY(w, r)
		if err != nil {
			log.Printf("[streaming] attach spdy upgrade error: %v", err)
			return
		}
		defer session.close()

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		pid, ok := session.resolveAttachPID(ctx)
		if !ok {
			return
		}

		resp, err := session.connectEnvdAttach(ctx, pid)
		if err != nil {
			session.sendError(err.Error())
			return
		}
		if transport.stdin != nil {
			go session.forwardSPDYStdin(ctx, transport.stdin)
		}
		if transport.resize != nil {
			go session.forwardSPDYResize(ctx, transport.resize)
		}
		go session.forwardEnvdToChannels(resp, false)
		<-transport.conn.CloseChan()
		return
	}

	http.Error(w, "websocket or spdy upgrade required", http.StatusBadRequest)
}
