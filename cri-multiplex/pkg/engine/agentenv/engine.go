package agentenv

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/cri-multiplex/pkg/agentenv/runtime/v1"
	"github.com/cri-multiplex/pkg/engine"
)

type lifecycleAPI interface {
	Create(context.Context, *v1.SandboxCreateRequest, ...grpc.CallOption) (*v1.SandboxCreateResponse, error)
	Get(context.Context, *v1.SandboxGetRequest, ...grpc.CallOption) (*v1.SandboxInfo, error)
	List(context.Context, *v1.SandboxListRequest, ...grpc.CallOption) (*v1.SandboxListResponse, error)
	Update(context.Context, *v1.SandboxUpdateRequest, ...grpc.CallOption) (*v1.SandboxInfo, error)
	Delete(context.Context, *v1.SandboxDeleteRequest, ...grpc.CallOption) (*v1.OperationStatus, error)
}

type adminAPI interface {
	Pause(context.Context, *v1.SandboxPauseRequest, ...grpc.CallOption) (*v1.SandboxPauseResponse, error)
	Checkpoint(context.Context, *v1.SandboxCheckpointRequest, ...grpc.CallOption) (*v1.CheckpointResult, error)
	GetRuntime(context.Context, *v1.SandboxGetRuntimeRequest, ...grpc.CallOption) (*v1.SandboxRuntimeInfo, error)
	Capabilities(context.Context, *emptypb.Empty, ...grpc.CallOption) (*v1.SandboxCapabilities, error)
	GetStats(context.Context, *v1.SandboxStatsRequest, ...grpc.CallOption) (*v1.SandboxStatsResponse, error)
}

type execAPI interface {
	StartProcess(context.Context, *v1.SandboxProcessStartRequest, ...grpc.CallOption) (*v1.SandboxProcessStartResponse, error)
	GetProcess(context.Context, *v1.SandboxProcessGetRequest, ...grpc.CallOption) (*v1.SandboxProcessGetResponse, error)
	ConnectProcess(context.Context, *v1.SandboxProcessConnectRequest, ...grpc.CallOption) (ProcessEventStream, error)
	StopProcess(context.Context, *v1.SandboxProcessStopRequest, ...grpc.CallOption) (*v1.SandboxProcessStopResponse, error)
}

// Config 是 AgentENV engine 的连接和节点身份配置。
type Config struct {
	RuntimeAddress string
	DialOptions    []grpc.DialOption
	NodeName       string
	NodeIP         string
}

// Engine 是 KASandbox 到 AgentENV Runtime 的内部 adapter。
// 容器和镜像仅做 CRI 本地投影；真实执行环境始终是 PodSandbox 对应的 microVM。
type Engine struct {
	config Config
	store  engine.StateStore

	// sandboxOps 按 sandbox ID 加锁，不同 sandbox 的创建/清理可以并行。
	sandboxOps keyedLockSet

	podMu        sync.RWMutex
	podPersistMu sync.Mutex
	// podStateVersion 由 podMu 保护；persistedPodStateVersion 由 podPersistMu 保护。
	podStateVersion          uint64
	persistedPodStateVersion uint64
	pods                     map[string]*podRecord
	containers               map[string]*containerRecord

	dialOnce  sync.Once
	dialErr   error
	connMu    sync.Mutex
	conn      *grpc.ClientConn
	lifecycle lifecycleAPI
	admin     adminAPI
	exec      execAPI

	streamingOnce     sync.Once
	streamingListener net.Listener
	streamingServer   *http.Server
	streamingMu       sync.RWMutex
	portForwardReqs   map[string]*portForwardRequest
	containerOps      sync.Map
}

func New(config Config, deps engine.Deps) (*Engine, error) {
	if config.RuntimeAddress == "" {
		return nil, fmt.Errorf("agentenv: runtime address is required")
	}
	instance := &Engine{
		config:          config,
		store:           deps.Store,
		pods:            make(map[string]*podRecord),
		containers:      make(map[string]*containerRecord),
		portForwardReqs: make(map[string]*portForwardRequest),
	}
	return instance, nil
}

func newWithClients(config Config, store engine.StateStore, lifecycle lifecycleAPI, admin adminAPI) *Engine {
	e := &Engine{
		config:          config,
		store:           store,
		pods:            make(map[string]*podRecord),
		containers:      make(map[string]*containerRecord),
		portForwardReqs: make(map[string]*portForwardRequest),
		lifecycle:       lifecycle,
		admin:           admin,
	}
	e.dialOnce.Do(func() {})
	return e
}

func (e *Engine) lockContainer(containerID string) func() {
	value, _ := e.containerOps.LoadOrStore(containerID, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

func (e *Engine) Type() engine.EngineType { return engine.EngineTypeAgentENV }

func (e *Engine) ensureClients() error {
	e.dialOnce.Do(func() {
		options := append([]grpc.DialOption(nil), e.config.DialOptions...)
		if len(options) == 0 {
			options = append(options, grpc.WithTransportCredentials(insecure.NewCredentials()))
		}
		conn, err := grpc.NewClient(e.config.RuntimeAddress, options...)
		if err != nil {
			e.dialErr = fmt.Errorf("agentenv: create runtime client: %w", err)
			return
		}
		e.connMu.Lock()
		e.conn = conn
		e.connMu.Unlock()
		e.lifecycle = NewLifecycleClient(conn)
		e.admin = NewAdminClient(conn)
		e.exec = NewExecClient(conn)
	})
	return e.dialErr
}

const defaultRuntimeReconnectTimeout = 2 * time.Second

func (e *Engine) Health(ctx context.Context) error {
	if err := e.ensureClients(); err != nil {
		return err
	}
	if e.admin == nil {
		return fmt.Errorf("agentenv: admin client is not initialized")
	}
	_, err := e.admin.Capabilities(ctx, &emptypb.Empty{})
	if status.Code(err) != codes.Unavailable || ctx.Err() != nil {
		return e.mapRuntimeError(err)
	}
	firstErr := err

	// 健康检查是幂等调用；Runtime 重启后等待一次有界重连，避免 kubelet 因 channel backoff 误判长期不可用。
	e.reconnectRuntime()
	retryCtx, cancel := context.WithTimeout(ctx, defaultRuntimeReconnectTimeout)
	defer cancel()
	_, err = e.admin.Capabilities(retryCtx, &emptypb.Empty{}, grpc.WaitForReady(true))
	if status.Code(err) == codes.DeadlineExceeded && ctx.Err() == nil {
		return e.mapRuntimeError(firstErr)
	}
	return e.mapRuntimeError(err)
}

// reconnectRuntime 唤醒同一个 gRPC channel 并清空失败退避；不重建 channel，避免并发替换和连接泄漏。
func (e *Engine) reconnectRuntime() {
	e.connMu.Lock()
	conn := e.conn
	e.connMu.Unlock()
	if conn == nil {
		return
	}
	conn.Connect()
	conn.ResetConnectBackoff()
}
func (e *Engine) Close() error {
	if e.streamingServer != nil {
		_ = e.streamingServer.Close()
	}
	if e.streamingListener != nil {
		_ = e.streamingListener.Close()
	}
	e.connMu.Lock()
	conn := e.conn
	e.conn = nil
	e.connMu.Unlock()
	if conn == nil {
		return nil
	}
	return conn.Close()
}

func generateStreamToken() (string, error) {
	var token [24]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("generate streaming token: %w", err)
	}
	return hex.EncodeToString(token[:]), nil
}
