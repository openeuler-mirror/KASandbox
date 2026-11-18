package agentenv

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	httpstreamspdy "k8s.io/apimachinery/pkg/util/httpstream/spdy"
	k8sremotecommand "k8s.io/apimachinery/pkg/util/remotecommand"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

const streamTokenTTL = 5 * time.Minute

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
