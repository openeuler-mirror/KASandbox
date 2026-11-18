package agentenv

import (
	"context"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	runtimeapi "github.com/cri-multiplex/pkg/agentenv/runtime/v1"
	"github.com/cri-multiplex/pkg/engine"
)

// ExecSync 将 CRI 同步执行投影到 AgentENV Runtime。
// adapter 只传递 native ID、容器环境和工作目录；Runtime 负责经 Node Proxy 调用 envd。
func (e *Engine) ExecSync(ctx context.Context, req *runtime.ExecSyncRequest) (*runtime.ExecSyncResponse, error) {
	if req == nil || strings.TrimSpace(req.GetContainerId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "container id is required")
	}
	if len(req.GetCmd()) == 0 || strings.TrimSpace(req.GetCmd()[0]) == "" {
		return nil, status.Error(codes.InvalidArgument, "exec command is required")
	}
	if req.GetTimeout() < 0 {
		return nil, status.Error(codes.InvalidArgument, "exec timeout cannot be negative")
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
	if err := e.ensureClients(); err != nil {
		return nil, e.mapRuntimeError(err)
	}
	if e.exec == nil {
		return nil, status.Error(codes.Internal, "AgentENV exec client is not initialized")
	}

	response, err := e.exec.ExecSync(ctx, &runtimeapi.SandboxExecSyncRequest{
		NativeSandboxId: mapping.NativeSandboxID,
		Command:         append([]string(nil), req.GetCmd()...),
		Envs:            cloneStringMap(record.Env),
		Cwd:             stringPointer(record.WorkingDir),
		TimeoutSeconds:  uint64(req.GetTimeout()),
	})
	if err != nil {
		return nil, e.mapRuntimeError(err)
	}
	return &runtime.ExecSyncResponse{
		Stdout:   append([]byte(nil), response.GetStdout()...),
		Stderr:   append([]byte(nil), response.GetStderr()...),
		ExitCode: response.GetExitCode(),
	}, nil
}

func stringPointer(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
