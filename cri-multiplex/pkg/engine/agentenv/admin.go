package agentenv

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	runtimeapi "github.com/cri-multiplex/pkg/agentenv/runtime/v1"
	"github.com/cri-multiplex/pkg/engine"
)

func (e *Engine) AdminPauseSandbox(ctx context.Context, ref engine.SandboxRef, opts engine.AdminOpOptions) (*engine.AdminOpResult, error) {
	mapping, err := e.resolveMapping(ref)
	if err != nil {
		return nil, err
	}
	operationID := strings.TrimSpace(opts.OperationID)
	if operationID == "" {
		return nil, status.Error(codes.InvalidArgument, "operation_id is required for AgentENV pause")
	}
	if err := e.ensureClients(); err != nil {
		return nil, e.mapRuntimeError(err)
	}

	var timeoutSeconds uint64
	if opts.Timeout > 0 {
		timeoutSeconds = uint64(opts.Timeout / time.Second)
	}
	response, err := e.admin.Pause(ctx, &runtimeapi.SandboxPauseRequest{
		OperationId:     operationID,
		NativeSandboxId: mapping.NativeSandboxID,
		TimeoutSeconds:  timeoutSeconds,
	})
	if err != nil {
		return nil, e.mapRuntimeError(err)
	}
	if err := validatePauseOperationStatus(operationID, response.GetOperation()); err != nil {
		return nil, err
	}
	result := &engine.AdminOpResult{SnapshotPersisted: true}
	if startedAt := response.GetOperation().GetStartedAt(); startedAt != nil {
		result.StartedAt = startedAt.AsTime()
	}
	if finishedAt := response.GetOperation().GetFinishedAt(); finishedAt != nil {
		result.FinishedAt = finishedAt.AsTime()
	}
	return result, nil
}

func validatePauseOperationStatus(expectedOperationID string, operation *runtimeapi.OperationStatus) error {
	if operation == nil {
		return status.Error(codes.Internal, "AgentENV sandbox pause returned a nil operation status")
	}
	if actual := operation.GetOperationId(); actual != expectedOperationID {
		return status.Errorf(codes.Internal,
			"AgentENV sandbox pause operation id mismatch: got %q, want %q", actual, expectedOperationID)
	}
	if state := strings.ToUpper(strings.TrimSpace(operation.GetState())); state != "SUCCEEDED" {
		detail := strings.TrimSpace(operation.GetErrorMessage())
		if code := strings.TrimSpace(operation.GetErrorCode()); code != "" {
			if detail != "" {
				detail = code + ": " + detail
			} else {
				detail = code
			}
		}
		if detail == "" {
			detail = "no error detail"
		}
		return status.Errorf(codes.Unavailable,
			"AgentENV sandbox pause operation %s did not succeed: state=%s detail=%s",
			expectedOperationID, state, detail)
	}
	return nil
}

func (e *Engine) AdminCheckpointSandbox(ctx context.Context, ref engine.SandboxRef, opts engine.AdminOpOptions) (*engine.AdminOpResult, error) {
	mapping, err := e.resolveMapping(ref)
	if err != nil {
		return nil, err
	}
	operationID := strings.TrimSpace(opts.OperationID)
	if operationID == "" {
		return nil, status.Error(codes.InvalidArgument, "operation_id is required for AgentENV checkpoint")
	}
	if err := e.ensureClients(); err != nil {
		return nil, e.mapRuntimeError(err)
	}

	var timeoutSeconds uint64
	if opts.Timeout > 0 {
		timeoutSeconds = uint64(opts.Timeout / time.Second)
	}
	startedAt := time.Now()
	response, err := e.admin.Checkpoint(ctx, &runtimeapi.SandboxCheckpointRequest{
		OperationId:     operationID,
		NativeSandboxId: mapping.NativeSandboxID,
		TimeoutSeconds:  timeoutSeconds,
		Metadata:        cloneStringMap(opts.Metadata),
	})
	if err != nil {
		return nil, e.mapRuntimeError(err)
	}
	if err := validateCheckpointResult(response); err != nil {
		return nil, err
	}

	return &engine.AdminOpResult{
		SnapshotPersisted: response.GetPublished(),
		VMResumed:         response.GetSourcePhase() == runtimeapi.SandboxPhase_RUNNING,
		StartedAt:         startedAt,
		FinishedAt:        time.Now(),
	}, nil
}

func validateCheckpointResult(result *runtimeapi.CheckpointResult) error {
	if result == nil {
		return status.Error(codes.Internal, "AgentENV sandbox checkpoint returned an empty result")
	}
	if !result.GetPublished() {
		return status.Error(codes.Internal, "AgentENV sandbox checkpoint did not publish the snapshot")
	}
	if strings.TrimSpace(result.GetSnapshotRef()) == "" {
		return status.Error(codes.Internal, "AgentENV sandbox checkpoint returned an empty snapshot reference")
	}
	if result.GetSourcePhase() != runtimeapi.SandboxPhase_RUNNING {
		return status.Errorf(codes.Unavailable,
			"AgentENV sandbox checkpoint did not leave the source sandbox running: phase=%s",
			result.GetSourcePhase().String())
	}
	return nil
}

func (e *Engine) AdminGetSandboxRuntime(ctx context.Context, ref engine.SandboxRef) (*engine.SandboxRuntime, error) {
	mapping, err := e.resolveMapping(ref)
	if err != nil {
		return nil, err
	}
	if err := e.ensureClients(); err != nil {
		return nil, e.mapRuntimeError(err)
	}
	info, err := e.admin.GetRuntime(ctx, &runtimeapi.SandboxGetRuntimeRequest{NativeSandboxId: mapping.NativeSandboxID})
	if err != nil {
		return nil, e.mapRuntimeError(err)
	}
	return sandboxRuntimeInfoFromProto(info, mapping, e.config.NodeName, e.config.NodeIP), nil
}

func (e *Engine) resolveMapping(ref engine.SandboxRef) (podRecord, error) {
	if ref.CRISandboxID == "" && ref.SandboxID == "" {
		return podRecord{}, status.Error(codes.InvalidArgument, "cri_sandbox_id or sandbox_id is required")
	}
	var (
		mapping podRecord
		ok      bool
	)
	if ref.CRISandboxID != "" {
		mapping, ok = e.getPod(ref.CRISandboxID)
	} else {
		mapping, ok = e.getPod(ref.SandboxID)
	}
	if !ok {
		return podRecord{}, status.Errorf(codes.NotFound, "sandbox not found on this node")
	}
	if ref.CRISandboxID != "" && ref.SandboxID != "" && mapping.CRISandboxID != ref.SandboxID {
		return podRecord{}, status.Errorf(codes.FailedPrecondition,
			"sandbox id mismatch: CRI %q, sandbox %q",
			ref.CRISandboxID, ref.SandboxID)
	}
	return mapping, nil
}

func requestFingerprint(spec *runtimeapi.SandboxSpec) ([]byte, error) {
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(spec)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(encoded)
	return sum[:], nil
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func sandboxRuntimeInfoFromProto(info *runtimeapi.SandboxRuntimeInfo, mapping podRecord, nodeName, nodeIP string) *engine.SandboxRuntime {
	runtime := &engine.SandboxRuntime{
		Backend:      string(engine.EngineTypeAgentENV),
		CRISandboxID: mapping.CRISandboxID,
		SandboxID:    mapping.CRISandboxID,
		State:        runtimePhaseName(info.GetPhase()),
		NodeName:     nodeName,
		NetNSPath:    info.GetNetnsPath(),
		HostIP:       nodeIP,
		Extra:        cloneStringMap(info.GetExtra()),
	}
	ensureExtra(runtime, "native_sandbox_id", info.GetNativeSandboxId())
	for key, value := range info.GetVersions() {
		ensureExtra(runtime, "version."+key, value)
	}
	for key, value := range info.GetNetwork() {
		ensureExtra(runtime, "network."+key, value)
	}
	for key, value := range info.GetStorage() {
		ensureExtra(runtime, "storage."+key, value)
	}
	for _, hostPort := range info.GetHostports() {
		runtime.HostPorts = append(runtime.HostPorts, engine.SandboxHostPort{
			HostPort:    int(hostPort.GetHostPort()),
			SandboxPort: int(hostPort.GetSandboxPort()),
		})
	}
	for _, operation := range info.GetActiveOperations() {
		item := engine.SandboxOperation{
			OperationID: operation.GetOperationId(),
			Action:      operation.GetAction(),
			State:       operation.GetState(),
			Error:       operation.GetErrorMessage(),
		}
		if operation.GetStartedAt() != nil {
			item.StartedAt = operation.GetStartedAt().AsTime()
		}
		if operation.GetFinishedAt() != nil {
			item.FinishedAt = operation.GetFinishedAt().AsTime()
		}
		runtime.ActiveOps = append(runtime.ActiveOps, item)
	}
	return runtime
}

func runtimePhaseName(phase runtimeapi.SandboxPhase) string {
	switch phase {
	case runtimeapi.SandboxPhase_RUNNING:
		return "Running"
	case runtimeapi.SandboxPhase_PAUSED:
		return "Paused"
	case runtimeapi.SandboxPhase_PAUSING:
		return "Pausing"
	case runtimeapi.SandboxPhase_RESUMING:
		return "Resuming"
	case runtimeapi.SandboxPhase_SNAPSHOTTING:
		return "Snapshotting"
	case runtimeapi.SandboxPhase_DELETING:
		return "Deleting"
	case runtimeapi.SandboxPhase_FAILED:
		return "Failed"
	default:
		return "Creating"
	}
}

func ensureExtra(runtime *engine.SandboxRuntime, key, value string) {
	if value == "" {
		return
	}
	if runtime.Extra == nil {
		runtime.Extra = make(map[string]string)
	}
	if _, exists := runtime.Extra[key]; !exists {
		runtime.Extra[key] = value
	}
}

func (e *Engine) mapRuntimeError(err error) error {
	if err == nil {
		return nil
	}
	if status.Code(err) == codes.Unavailable {
		e.reconnectRuntime()
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	return status.Error(codes.Internal, fmt.Sprintf("AgentENV runtime error: %v", err))
}
