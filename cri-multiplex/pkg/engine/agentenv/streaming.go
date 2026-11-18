package agentenv

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	httpstreamspdy "k8s.io/apimachinery/pkg/util/httpstream/spdy"
	k8sremotecommand "k8s.io/apimachinery/pkg/util/remotecommand"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	runtimeapi "github.com/cri-multiplex/pkg/agentenv/runtime/v1"
	"github.com/cri-multiplex/pkg/engine"
)

const (
	channelStdIn  = 0
	channelStdOut = 1
	channelStdErr = 2
	channelErr    = 3
	channelResize = 4
	channelClose  = 255

	streamTokenTTL         = 5 * time.Minute
	streamAPITimeout       = 10 * time.Second
	streamingV5Subprotocol = "v5.channel.k8s.io"
	// ? TTY ???????? resize ?????? Exec ?????????
	spdyOptionalStreamGrace = 100 * time.Millisecond
)

var streamingSPDYProtocols = []string{
	streamingV5Subprotocol,
	k8sremotecommand.StreamProtocolV4Name,
	k8sremotecommand.StreamProtocolV3Name,
	k8sremotecommand.StreamProtocolV2Name,
	k8sremotecommand.StreamProtocolV1Name,
}

var agentenvWebsocketUpgrader = websocket.Upgrader{
	Subprotocols:    []string{streamingV5Subprotocol},
	CheckOrigin:     func(r *http.Request) bool { return true },
	ReadBufferSize:  32 * 1024,
	WriteBufferSize: 32 * 1024,
}

type streamRequest struct {
	containerID     string
	nativeSandboxID string
	command         []string
	tty             bool
	stdin           bool
	stdout          bool
	stderr          bool
	attach          bool
}

type portForwardRequest struct {
	podSandboxID string
	podIP        string
	ports        map[int32]struct{}
}

type portForwardSession struct {
	ctx      context.Context
	req      *portForwardRequest
	mu       sync.Mutex
	requests map[string]httpstream.Stream
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

type streamSession struct {
	e      *Engine
	req    *streamRequest
	record containerRecord
	conn   *websocket.Conn
	stream streamTransport
	pid    uint32

	tty    bool
	stdin  bool
	stdout bool
	stderr bool

	mu      sync.Mutex
	writeMu sync.Mutex
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

func (e *Engine) ensureStreamingServer() {
	e.streamingOnce.Do(func() {
		listenAddr := "127.0.0.1:0"
		if strings.TrimSpace(e.config.NodeIP) != "" {
			listenAddr = net.JoinHostPort(e.config.NodeIP, "0")
		}
		listener, err := net.Listen("tcp", listenAddr)
		if err != nil && listenAddr != "127.0.0.1:0" {
			listener, err = net.Listen("tcp", "127.0.0.1:0")
		}
		if err != nil {
			log.Printf("[agentenv-streaming] failed to start streaming server: %v", err)
			return
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/exec/", e.handleExecStream)
		mux.HandleFunc("/attach/", e.handleAttachStream)
		mux.HandleFunc("/portforward/", e.handlePortForwardStream)
		server := &http.Server{Handler: mux}
		e.streamingListener = listener
		e.streamingServer = server
		go func() {
			log.Printf("[agentenv-streaming] listening on %s", listener.Addr().String())
			if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
				log.Printf("[agentenv-streaming] server exited: %v", err)
			}
		}()
	})
}

func (e *Engine) streamingAddress() string {
	if e.streamingListener == nil {
		return ""
	}
	return e.streamingListener.Addr().String()
}

func (e *Engine) registerStream(req streamRequest) (string, error) {
	e.ensureStreamingServer()
	if e.streamingListener == nil {
		return "", status.Error(codes.Internal, "AgentENV streaming server is not available")
	}
	token, err := generateStreamToken()
	if err != nil {
		return "", status.Errorf(codes.Internal, "generate streaming token: %v", err)
	}
	e.streamingMu.Lock()
	if e.streamingReqs == nil {
		e.streamingReqs = make(map[string]*streamRequest)
	}
	request := req
	e.streamingReqs[token] = &request
	e.streamingMu.Unlock()
	time.AfterFunc(streamTokenTTL, func() {
		e.streamingMu.Lock()
		delete(e.streamingReqs, token)
		e.streamingMu.Unlock()
	})
	return token, nil
}

func (e *Engine) getStreamRequest(token string) (*streamRequest, bool) {
	e.streamingMu.RLock()
	defer e.streamingMu.RUnlock()
	req, ok := e.streamingReqs[token]
	return req, ok
}

func (e *Engine) Exec(ctx context.Context, req *runtime.ExecRequest) (*runtime.ExecResponse, error) {
	if req == nil || strings.TrimSpace(req.GetContainerId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "container id is required")
	}
	if len(req.GetCmd()) == 0 || strings.TrimSpace(req.GetCmd()[0]) == "" {
		return nil, status.Error(codes.InvalidArgument, "exec command is required")
	}
	record, ok := e.getContainer(req.GetContainerId())
	if !ok || record.State == containerStateRemoved {
		return nil, status.Errorf(codes.NotFound, "container %s not found", req.GetContainerId())
	}
	if record.State != containerStateRunning {
		return nil, status.Errorf(codes.FailedPrecondition, "container %s is in state %s", record.ContainerID, record.State)
	}
	pod, ok := e.getPod(record.CRISandboxID)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "pod sandbox %s not found", record.CRISandboxID)
	}
	if pod.State != criStateReady {
		return nil, status.Errorf(codes.FailedPrecondition, "pod sandbox %s is not ready", record.CRISandboxID)
	}
	mapping, err := e.resolveMapping(engine.SandboxRef{CRISandboxID: record.CRISandboxID})
	if err != nil {
		return nil, err
	}
	token, err := e.registerStream(streamRequest{
		containerID:     record.ContainerID,
		nativeSandboxID: mapping.NativeSandboxID,
		command:         append([]string(nil), req.GetCmd()...),
		tty:             req.GetTty(),
		stdin:           req.GetStdin(),
		stdout:          req.GetStdout(),
		stderr:          req.GetStderr(),
	})
	if err != nil {
		return nil, err
	}
	return &runtime.ExecResponse{Url: fmt.Sprintf("http://%s/exec/%s", e.streamingAddress(), token)}, nil
}

func (e *Engine) Attach(ctx context.Context, req *runtime.AttachRequest) (*runtime.AttachResponse, error) {
	if req == nil || strings.TrimSpace(req.GetContainerId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "container id is required")
	}
	record, ok := e.getContainer(req.GetContainerId())
	if !ok || record.State == containerStateRemoved {
		return nil, status.Errorf(codes.NotFound, "container %s not found", req.GetContainerId())
	}
	if record.State != containerStateRunning {
		return nil, status.Errorf(codes.FailedPrecondition, "container %s is in state %s", record.ContainerID, record.State)
	}
	if record.NativeProcessID == 0 {
		return nil, status.Errorf(codes.FailedPrecondition, "container %s has no attachable process", record.ContainerID)
	}
	pod, ok := e.getPod(record.CRISandboxID)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "pod sandbox %s not found", record.CRISandboxID)
	}
	if pod.State != criStateReady {
		return nil, status.Errorf(codes.FailedPrecondition, "pod sandbox %s is not ready", record.CRISandboxID)
	}
	mapping, err := e.resolveMapping(engine.SandboxRef{CRISandboxID: record.CRISandboxID})
	if err != nil {
		return nil, err
	}
	token, err := e.registerStream(streamRequest{
		containerID:     record.ContainerID,
		nativeSandboxID: mapping.NativeSandboxID,
		tty:             req.GetTty(),
		stdin:           req.GetStdin(),
		stdout:          req.GetStdout(),
		stderr:          req.GetStderr(),
		attach:          true,
	})
	if err != nil {
		return nil, err
	}
	return &runtime.AttachResponse{Url: fmt.Sprintf("http://%s/attach/%s", e.streamingAddress(), token)}, nil
}

func (s *streamSession) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.stream != nil {
		_ = s.stream.close()
	} else if s.conn != nil {
		_ = s.conn.Close()
	}
}

func (s *streamSession) sendChannelData(channel byte, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.stream == nil {
		return fmt.Errorf("stream transport is not available")
	}
	return s.stream.writeChannel(channel, data)
}

func (s *streamSession) sendStatus(value remoteCommandStatus) {
	payload, err := json.Marshal(value)
	if err != nil {
		return
	}
	if err := s.sendChannelData(channelErr, payload); err != nil {
		log.Printf("[agentenv-streaming] write status failed: %v", err)
	}
}

func (s *streamSession) sendError(message string) {
	s.sendStatus(remoteCommandStatus{Status: "Failure", Message: message})
	s.close()
}

func (s *streamSession) sendExitStatus(exitCode int32) {
	if exitCode == 0 {
		s.sendStatus(remoteCommandStatus{Status: "Success"})
		return
	}
	s.sendStatus(remoteCommandStatus{
		Status: "Failure",
		Reason: "NonZeroExitCode",
		Details: &remoteCommandStatusDetails{Causes: []remoteCommandStatusCause{{
			Reason:  "ExitCode",
			Message: fmt.Sprintf("%d", exitCode),
		}}},
	})
}

func (s *streamSession) startProcess(ctx context.Context) error {
	if err := s.e.ensureClients(); err != nil {
		return s.e.mapRuntimeError(err)
	}
	if s.e.exec == nil {
		return status.Error(codes.Internal, "AgentENV exec client is not initialized")
	}
	if s.req.attach {
		s.pid = s.record.NativeProcessID
		return nil
	}
	var size *runtimeapi.SandboxProcessTerminalSize
	if s.tty {
		size = &runtimeapi.SandboxProcessTerminalSize{Cols: 80, Rows: 24}
	}
	response, err := s.e.exec.StartProcess(ctx, &runtimeapi.SandboxProcessStartRequest{
		NativeSandboxId: s.req.nativeSandboxID,
		Command:         append([]string(nil), s.req.command...),
		Envs:            cloneStringMap(s.record.Env),
		Cwd:             stringPointer(s.record.WorkingDir),
		Tty:             s.tty,
		Stdin:           s.stdin,
		TerminalSize:    size,
	})
	if err != nil {
		return s.e.mapRuntimeError(err)
	}
	if response.GetPid() == 0 {
		return status.Error(codes.Internal, "AgentENV Runtime returned an empty process pid")
	}
	s.pid = response.GetPid()
	return nil
}

func (s *streamSession) runRuntimeStream(ctx context.Context) {
	if err := s.e.ensureClients(); err != nil {
		s.sendError(s.e.mapRuntimeError(err).Error())
		return
	}
	stream, err := s.e.exec.ConnectProcess(ctx, &runtimeapi.SandboxProcessConnectRequest{
		NativeSandboxId: s.req.nativeSandboxID,
		Pid:             s.pid,
		AfterSequence:   0,
	})
	if err != nil {
		s.sendError(s.e.mapRuntimeError(err).Error())
		return
	}
	defer s.close()
	for {
		response, err := stream.Recv()
		if err == io.EOF {
			s.sendStatus(remoteCommandStatus{Status: "Success"})
			return
		}
		if err != nil {
			s.sendError(fmt.Sprintf("receive AgentENV process stream: %v", err))
			return
		}
		switch event := response.GetEvent().(type) {
		case *runtimeapi.SandboxProcessConnectResponse_Output:
			chunk := event.Output
			channel := channelStdOut
			if chunk.GetStream() == runtimeapi.SandboxProcessOutputStream_SANDBOX_PROCESS_OUTPUT_STREAM_STDERR {
				channel = channelStdErr
			}
			if (channel == channelStdOut && s.stdout) || (channel == channelStdErr && s.stderr) {
				if err := s.sendChannelData(byte(channel), chunk.GetData()); err != nil {
					log.Printf("[agentenv-streaming] write output failed: %v", err)
					return
				}
			}
		case *runtimeapi.SandboxProcessConnectResponse_Exit:
			exit := event.Exit
			if exit.GetErrorMessage() != "" || exit.GetOutputTruncated() {
				message := exit.GetErrorMessage()
				if message == "" {
					message = "process output was truncated"
				}
				s.sendError(message)
				return
			}
			s.sendExitStatus(exit.GetExitCode())
			return
		}
	}
}

func (s *streamSession) sendRuntimeInput(ctx context.Context, data []byte, pty bool) error {
	apiCtx, cancel := context.WithTimeout(ctx, streamAPITimeout)
	defer cancel()
	_, err := s.e.exec.SendProcessInput(apiCtx, &runtimeapi.SandboxProcessInputRequest{
		NativeSandboxId: s.req.nativeSandboxID,
		Pid:             s.pid,
		Data:            append([]byte(nil), data...),
		Pty:             pty,
	})
	if err != nil {
		return s.e.mapRuntimeError(err)
	}
	return nil
}

func (s *streamSession) resizeRuntime(ctx context.Context, cols, rows uint32) error {
	apiCtx, cancel := context.WithTimeout(ctx, streamAPITimeout)
	defer cancel()
	_, err := s.e.exec.ResizeProcess(apiCtx, &runtimeapi.SandboxProcessResizeRequest{
		NativeSandboxId: s.req.nativeSandboxID,
		Pid:             s.pid,
		Cols:            cols,
		Rows:            rows,
	})
	if err != nil {
		return s.e.mapRuntimeError(err)
	}
	return nil
}

func (s *streamSession) closeRuntimeStdin(ctx context.Context) error {
	if s.tty {
		return s.sendRuntimeInput(ctx, []byte{0x04}, true)
	}
	apiCtx, cancel := context.WithTimeout(ctx, streamAPITimeout)
	defer cancel()
	_, err := s.e.exec.CloseProcessStdin(apiCtx, &runtimeapi.SandboxProcessCloseStdinRequest{
		NativeSandboxId: s.req.nativeSandboxID,
		Pid:             s.pid,
	})
	if err != nil {
		return s.e.mapRuntimeError(err)
	}
	return nil
}

func (s *streamSession) forwardWebSocketToRuntime(ctx context.Context) {
	defer s.close()
	for {
		_, data, err := s.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Printf("[agentenv-streaming] websocket read failed: %v", err)
			}
			return
		}
		if len(data) == 0 {
			continue
		}
		channel := data[0]
		payload := data[1:]
		switch channel {
		case channelStdIn:
			if !s.stdin {
				continue
			}
			if err := s.sendRuntimeInput(ctx, payload, s.tty); err != nil {
				log.Printf("[agentenv-streaming] stdin failed: %v", err)
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
				continue
			}
			if err := s.resizeRuntime(ctx, uint32(size.Width), uint32(size.Height)); err != nil {
				log.Printf("[agentenv-streaming] resize failed: %v", err)
			}
		case channelClose:
			if err := s.closeRuntimeStdin(ctx); err != nil {
				log.Printf("[agentenv-streaming] close stdin failed: %v", err)
			}
		}
	}
}

func (s *streamSession) forwardSPDYStdin(ctx context.Context, stream httpstream.Stream) {
	buf := make([]byte, 32*1024)
	for {
		n, err := stream.Read(buf)
		if n > 0 {
			if sendErr := s.sendRuntimeInput(ctx, buf[:n], s.tty); sendErr != nil {
				log.Printf("[agentenv-streaming] spdy stdin failed: %v", sendErr)
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				log.Printf("[agentenv-streaming] spdy stdin read failed: %v", err)
			}
			if ctx.Err() == nil {
				if closeErr := s.closeRuntimeStdin(ctx); closeErr != nil {
					log.Printf("[agentenv-streaming] spdy close stdin failed: %v", closeErr)
				}
			}
			return
		}
	}
}

func (s *streamSession) forwardSPDYResize(ctx context.Context, stream httpstream.Stream) {
	decoder := json.NewDecoder(stream)
	for {
		var size struct {
			Width  uint16
			Height uint16
		}
		if err := decoder.Decode(&size); err != nil {
			return
		}
		if err := s.resizeRuntime(ctx, uint32(size.Width), uint32(size.Height)); err != nil && ctx.Err() == nil {
			log.Printf("[agentenv-streaming] spdy resize failed: %v", err)
		}
	}
}

func (s *streamSession) upgradeSPDY(w http.ResponseWriter, r *http.Request) (*spdyStreamTransport, error) {
	protocol, err := httpstream.Handshake(r, w, streamingSPDYProtocols)
	if err != nil {
		return nil, err
	}
	log.Printf("[agentenv-streaming] negotiated SPDY protocol %q", protocol)
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

func (s *streamSession) collectSPDYStreams(ctx context.Context, transport *spdyStreamTransport, streamCh <-chan spdyStreamAndReply) error {
	required := map[string]bool{corev1.StreamTypeError: true}
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
	graceDuration := s.spdyOptionalGrace()
	if graceDuration <= 0 {
		return nil
	}
	grace := time.NewTimer(graceDuration)
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

// spdyOptionalGrace ?????? resize ???? TTY ?????? grace?
func (s *streamSession) spdyOptionalGrace() time.Duration {
	if s != nil && s.tty {
		return spdyOptionalStreamGrace
	}
	return 0
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
	}
}

func (e *Engine) PortForward(ctx context.Context, req *runtime.PortForwardRequest) (*runtime.PortForwardResponse, error) {
	if req == nil || strings.TrimSpace(req.GetPodSandboxId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "pod sandbox id is required")
	}
	ports := make(map[int32]struct{}, len(req.GetPort()))
	for _, port := range req.GetPort() {
		if port < 1 || port > 65535 {
			return nil, status.Errorf(codes.InvalidArgument, "port %d is outside 1..65535", port)
		}
		ports[port] = struct{}{}
	}
	record, ok := e.getPod(req.GetPodSandboxId())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "pod sandbox %s not found", req.GetPodSandboxId())
	}
	if record.State != criStateReady || podIPFromRecord(record) == "" {
		return nil, status.Errorf(codes.FailedPrecondition, "pod sandbox %s is not network-ready", req.GetPodSandboxId())
	}
	if record.RuntimeNetwork != nil {
		if err := e.ensureClients(); err != nil {
			return nil, e.mapRuntimeError(err)
		}
		if err := e.refreshRuntimeNetwork(ctx, &record); err != nil {
			return nil, err
		}
	}
	e.ensureStreamingServer()
	if e.streamingListener == nil {
		return nil, status.Error(codes.Internal, "AgentENV streaming server is not available")
	}
	token, err := generateStreamToken()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "generate port-forward token: %v", err)
	}
	request := &portForwardRequest{
		podSandboxID: record.CRISandboxID,
		podIP:        podIPFromRecord(record),
		ports:        ports,
	}
	e.streamingMu.Lock()
	if e.portForwardReqs == nil {
		e.portForwardReqs = make(map[string]*portForwardRequest)
	}
	e.portForwardReqs[token] = request
	e.streamingMu.Unlock()
	time.AfterFunc(streamTokenTTL, func() {
		e.streamingMu.Lock()
		delete(e.portForwardReqs, token)
		e.streamingMu.Unlock()
	})
	return &runtime.PortForwardResponse{Url: fmt.Sprintf("http://%s/portforward/%s", e.streamingAddress(), token)}, nil
}

func (e *Engine) getPortForwardRequest(token string) (*portForwardRequest, bool) {
	e.streamingMu.RLock()
	defer e.streamingMu.RUnlock()
	req, ok := e.portForwardReqs[token]
	return req, ok
}

func (e *Engine) handlePortForwardStream(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/portforward/")
	req, ok := e.getPortForwardRequest(token)
	if !ok {
		http.Error(w, "invalid or expired port-forward token", http.StatusForbidden)
		return
	}
	pod, ok := e.getPod(req.podSandboxID)
	if !ok || pod.State != criStateReady {
		http.Error(w, "pod sandbox not ready", http.StatusNotFound)
		return
	}
	if !httpstream.IsUpgradeRequest(r) {
		http.Error(w, "spdy upgrade required", http.StatusBadRequest)
		return
	}
	if _, err := httpstream.Handshake(r, w, []string{"portforward.k8s.io"}); err != nil {
		return
	}
	session := &portForwardSession{
		ctx:      r.Context(),
		req:      req,
		requests: make(map[string]httpstream.Stream),
	}
	upgrader := httpstreamspdy.NewResponseUpgrader()
	conn := upgrader.UpgradeResponse(w, r, func(stream httpstream.Stream, _ <-chan struct{}) error {
		switch stream.Headers().Get(corev1.StreamType) {
		case corev1.StreamTypeError:
			requestID := stream.Headers().Get("requestID")
			session.mu.Lock()
			session.requests[requestID] = stream
			session.mu.Unlock()
		case corev1.StreamTypeData:
			go session.proxyDataStream(stream)
		default:
			_ = stream.Reset()
		}
		return nil
	})
	if conn == nil {
		return
	}
	<-conn.CloseChan()
}

func (s *portForwardSession) errorStream(requestID string) httpstream.Stream {
	deadline := time.NewTimer(k8sremotecommand.DefaultStreamCreationTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		stream := s.requests[requestID]
		s.mu.Unlock()
		if stream != nil {
			return stream
		}
		select {
		case <-s.ctx.Done():
			return nil
		case <-deadline.C:
			return nil
		case <-ticker.C:
		}
	}
}

func (s *portForwardSession) proxyDataStream(stream httpstream.Stream) {
	defer stream.Reset()
	requestID := stream.Headers().Get("requestID")
	portValue := stream.Headers().Get("port")
	port, err := strconv.Atoi(portValue)
	if err != nil || port < 1 || port > 65535 {
		s.writePortForwardError(requestID, fmt.Errorf("invalid port %q", portValue))
		return
	}
	if len(s.req.ports) > 0 {
		if _, ok := s.req.ports[int32(port)]; !ok {
			s.writePortForwardError(requestID, fmt.Errorf("port %d was not authorized", port))
			return
		}
	}
	target := net.JoinHostPort(s.req.podIP, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		s.writePortForwardError(requestID, fmt.Errorf("connect %s: %w", target, err))
		return
	}
	defer conn.Close()
	errStream := s.errorStream(requestID)
	if errStream != nil {
		defer errStream.Close()
	}

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(conn, stream)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(stream, conn)
		done <- struct{}{}
	}()
	select {
	case <-s.ctx.Done():
	case <-done:
	}
}

func (s *portForwardSession) writePortForwardError(requestID string, forwardingErr error) {
	if stream := s.errorStream(requestID); stream != nil {
		_, _ = fmt.Fprintf(stream, "%v", forwardingErr)
		_ = stream.Close()
	}
}

func (e *Engine) handleExecStream(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/exec/")
	req, ok := e.getStreamRequest(token)
	if !ok {
		http.Error(w, "invalid or expired exec token", http.StatusForbidden)
		return
	}
	e.handleStream(w, r, req)
}

func (e *Engine) handleAttachStream(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/attach/")
	req, ok := e.getStreamRequest(token)
	if !ok {
		http.Error(w, "invalid or expired attach token", http.StatusForbidden)
		return
	}
	e.handleStream(w, r, req)
}

func (e *Engine) handleStream(w http.ResponseWriter, r *http.Request, req *streamRequest) {
	record, ok := e.getContainer(req.containerID)
	if !ok || record.State != containerStateRunning {
		http.Error(w, "container not running", http.StatusNotFound)
		return
	}
	session := &streamSession{
		e:      e,
		req:    req,
		record: record,
		tty:    req.tty,
		stdin:  req.stdin,
		stdout: req.stdout,
		stderr: req.stderr,
	}
	if err := session.startProcess(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if websocket.IsWebSocketUpgrade(r) {
		conn, err := agentenvWebsocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		session.conn = conn
		session.stream = &websocketStreamTransport{conn: conn}
		defer session.close()
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		go session.runRuntimeStream(ctx)
		session.forwardWebSocketToRuntime(ctx)
		return
	}
	if httpstream.IsUpgradeRequest(r) {
		transport, err := session.upgradeSPDY(w, r)
		if err != nil {
			log.Printf("[agentenv-streaming] spdy upgrade failed: %v", err)
			return
		}
		defer session.close()
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		if transport.stdin != nil {
			go session.forwardSPDYStdin(ctx, transport.stdin)
		}
		if transport.resize != nil {
			go session.forwardSPDYResize(ctx, transport.resize)
		}
		go session.runRuntimeStream(ctx)
		<-transport.conn.CloseChan()
		return
	}
	http.Error(w, "websocket or spdy upgrade required", http.StatusBadRequest)
}
