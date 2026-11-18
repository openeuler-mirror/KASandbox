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
	"google.golang.org/protobuf/types/known/timestamppb"

	runtimeapi "github.com/cri-multiplex/pkg/agentenv/runtime/v1"
	"github.com/cri-multiplex/pkg/engine"
)

const (
	adminCreateOperationPrefix = "admin-create:"
	adminDeleteOperationPrefix = "admin-delete:"
)

func (e *Engine) AdminCreateSandbox(ctx context.Context, spec engine.SandboxSpec) (*engine.SandboxCreateResult, error) {
	runtimeSpec, err := sandboxSpecFromAdmin(spec)
	if err != nil {
		return nil, err
	}
	fingerprint, err := requestFingerprint(runtimeSpec)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "fingerprint create request: %v", err)
	}
	if err := e.ensureClients(); err != nil {
		return nil, e.mapRuntimeError(err)
	}
	unlock, err := e.lockSandbox(ctx, spec.SandboxID)
	if err != nil {
		return nil, status.FromContextError(err).Err()
	}
	defer unlock()

	response, err := e.lifecycle.Create(ctx, &runtimeapi.SandboxCreateRequest{
		OperationId:        adminCreateOperationPrefix + spec.SandboxID,
		RequestFingerprint: fingerprint,
		Spec:               runtimeSpec,
	})
	if err != nil {
		return nil, e.mapRuntimeError(err)
	}
	info := response.GetSandbox()
	nativeID := info.GetNativeSandboxId()
	if nativeID == "" {
		return nil, status.Error(codes.Internal, "agentenv runtime returned an empty native sandbox id")
	}
	if productID := info.GetProductSandboxId(); productID != "" && productID != spec.SandboxID {
		return nil, status.Errorf(codes.Internal,
			"agentenv runtime returned product sandbox id %q, want %q", productID, spec.SandboxID)
	}

	mapping := podRoute{
		Backend:          engine.EngineTypeAgentENV,
		ProductSandboxID: spec.SandboxID,
		NativeSandboxID:  nativeID,
	}
	if err := e.routes.PutPodRoute(mapping); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		_, _ = e.lifecycle.Delete(cleanupCtx, &runtimeapi.SandboxDeleteRequest{
			OperationId:     adminDeleteOperationPrefix + nativeID,
			NativeSandboxId: nativeID,
		})
		cancel()
		return nil, status.Errorf(codes.Internal, "persist AgentENV id mapping: %v", err)
	}

	return &engine.SandboxCreateResult{
		SandboxID: spec.SandboxID,
		ClientID:  nativeID,
		HostIP:    e.config.NodeIP,
	}, nil
}

func (e *Engine) AdminUpdateSandbox(ctx context.Context, sandboxID string, update engine.SandboxUpdate) error {
	if sandboxID == "" {
		return status.Error(codes.InvalidArgument, "sandbox_id is required")
	}
	mapping, ok := e.routes.PodRouteByProduct(sandboxID)
	if !ok {
		return status.Errorf(codes.NotFound, "sandbox %s not found on this node", sandboxID)
	}
	if err := e.ensureClients(); err != nil {
		return e.mapRuntimeError(err)
	}

	request := &runtimeapi.SandboxUpdateRequest{NativeSandboxId: mapping.NativeSandboxID}
	if update.EndTime != nil {
		request.ExpiresAt = timestamppb.New(*update.EndTime)
	}
	if update.Egress != nil {
		request.Egress = networkPolicyToProto(*update.Egress)
	}
	_, err := e.lifecycle.Update(ctx, request)
	return e.mapRuntimeError(err)
}

func (e *Engine) AdminListSandboxes(ctx context.Context) ([]*engine.SandboxRuntime, error) {
	if err := e.ensureClients(); err != nil {
		return nil, e.mapRuntimeError(err)
	}
	response, err := e.lifecycle.List(ctx, &runtimeapi.SandboxListRequest{IncludePaused: true})
	if err != nil {
		return nil, e.mapRuntimeError(err)
	}
	out := make([]*engine.SandboxRuntime, 0, len(response.GetSandboxes()))
	for _, info := range response.GetSandboxes() {
		mapping, _ := e.routes.PodRouteByNative(info.GetNativeSandboxId())
		out = append(out, sandboxRuntimeFromProto(info, mapping, e.config.NodeName, e.config.NodeIP))
	}
	return out, nil
}

func (e *Engine) AdminDeleteSandbox(ctx context.Context, sandboxID string) error {
	if sandboxID == "" {
		return status.Error(codes.InvalidArgument, "sandbox_id is required")
	}
	ids := e.sandboxOperationIDs(sandboxID)
	unlock, err := e.lockSandbox(ctx, ids...)
	if err != nil {
		return status.FromContextError(err).Err()
	}
	defer unlock()
	nativeID, found, err := e.resolveNativeByProduct(ctx, sandboxID)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	route, _ := e.routes.PodRouteByProduct(sandboxID)
	if err := e.ensureClients(); err != nil {
		return e.mapRuntimeError(err)
	}

	operationID := adminDeleteOperationPrefix + nativeID
	operation, err := e.lifecycle.Delete(ctx, &runtimeapi.SandboxDeleteRequest{
		OperationId:     operationID,
		NativeSandboxId: nativeID,
	})
	if err != nil {
		mappedErr := e.mapRuntimeError(err)
		if status.Code(mappedErr) != codes.NotFound {
			return mappedErr
		}
	} else if err := validateDeleteOperationStatus(operationID, operation); err != nil {
		// Delete 副作用结果不确定时保留 CRI record 与 route，供后续重试和对账。
		return err
	}
	if err := e.deletePod(route.CRISandboxID); err != nil {
		return status.Errorf(codes.Internal, "delete AgentENV CRI state: %v", err)
	}
	if err := e.routes.DeleteSandbox(sandboxID); err != nil {
		return status.Errorf(codes.Internal, "delete AgentENV id mapping: %v", err)
	}
	return nil
}

func (e *Engine) AdminListCachedBuilds(context.Context) ([]engine.CachedBuild, error) {
	return nil, status.Error(codes.Unimplemented, "AgentENV template cache listing is not implemented")
}

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

func (e *Engine) resolveNativeByProduct(ctx context.Context, productSandboxID string) (string, bool, error) {
	mapping, found := e.routes.PodRouteByProduct(productSandboxID)
	if found && mapping.NativeSandboxID != "" {
		return mapping.NativeSandboxID, true, nil
	}
	if err := e.ensureClients(); err != nil {
		return "", false, e.mapRuntimeError(err)
	}
	response, err := e.lifecycle.List(ctx, &runtimeapi.SandboxListRequest{
		ProductSandboxId: productSandboxID,
		IncludePaused:    true,
	})
	if err != nil {
		return "", false, e.mapRuntimeError(err)
	}
	if len(response.GetSandboxes()) == 0 {
		return "", false, nil
	}
	if len(response.GetSandboxes()) > 1 {
		return "", false, status.Errorf(codes.FailedPrecondition,
			"multiple AgentENV sandboxes found for product id %s", productSandboxID)
	}
	nativeID := response.GetSandboxes()[0].GetNativeSandboxId()
	if nativeID == "" {
		return "", false, status.Error(codes.Internal, "AgentENV runtime returned an empty native sandbox id")
	}
	if err := e.routes.PutPodRoute(podRoute{Backend: engine.EngineTypeAgentENV, ProductSandboxID: productSandboxID, NativeSandboxID: nativeID}); err != nil {
		return "", false, status.Errorf(codes.Internal, "persist recovered AgentENV id mapping: %v", err)
	}
	return nativeID, true, nil
}

func (e *Engine) resolveMapping(ref engine.SandboxRef) (podRoute, error) {
	if ref.CRISandboxID == "" && ref.SandboxID == "" {
		return podRoute{}, status.Error(codes.InvalidArgument, "cri_sandbox_id or sandbox_id is required")
	}
	var (
		mapping podRoute
		ok      bool
	)
	if ref.CRISandboxID != "" {
		mapping, ok = e.routes.PodRouteByCRI(ref.CRISandboxID)
	} else {
		mapping, ok = e.routes.PodRouteByProduct(ref.SandboxID)
	}
	if !ok {
		return podRoute{}, status.Errorf(codes.NotFound, "sandbox not found on this node")
	}
	if ref.CRISandboxID != "" && ref.SandboxID != "" && mapping.ProductSandboxID != ref.SandboxID {
		return podRoute{}, status.Errorf(codes.FailedPrecondition,
			"sandbox id mismatch: CRI %s maps to product %q, request has %q",
			ref.CRISandboxID, mapping.ProductSandboxID, ref.SandboxID)
	}
	return mapping, nil
}

func sandboxSpecFromAdmin(spec engine.SandboxSpec) (*runtimeapi.SandboxSpec, error) {
	if spec.SandboxID == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox_id is required")
	}
	if spec.StartTime != nil {
		return nil, status.Error(codes.InvalidArgument, "start_time is not supported by AgentENV Runtime")
	}
	if spec.TemplateID == "" && spec.Image == "" {
		return nil, status.Error(codes.InvalidArgument, "template_id or image is required")
	}
	if spec.TemplateID != "" && spec.Image != "" {
		return nil, status.Error(codes.InvalidArgument, "template_id and image are mutually exclusive")
	}
	if spec.Image != "" && spec.BuildID != "" {
		return nil, status.Error(codes.InvalidArgument, "build_id requires template_id")
	}
	if spec.MaxSandboxLengthHours < 0 {
		return nil, status.Error(codes.InvalidArgument, "max_sandbox_length_hours cannot be negative")
	}
	if err := rejectSensitiveValues("metadata", spec.Metadata); err != nil {
		return nil, err
	}
	if err := rejectSensitiveValues("extra", spec.Extra); err != nil {
		return nil, err
	}

	source := &runtimeapi.SandboxSource{}
	if spec.TemplateID != "" {
		source.Kind = &runtimeapi.SandboxSource_Template{Template: &runtimeapi.TemplateSource{
			TemplateId: spec.TemplateID,
			BuildId:    spec.BuildID,
		}}
	} else {
		source.Kind = &runtimeapi.SandboxSource_Image{Image: &runtimeapi.ImageSource{Image: spec.Image}}
	}

	volumeMounts := make([]*runtimeapi.VolumeMount, 0, len(spec.VolumeMounts))
	for _, mount := range spec.VolumeMounts {
		if mount.Path == "" {
			return nil, status.Error(codes.InvalidArgument, "volume mount path is required")
		}
		volumeMounts = append(volumeMounts, &runtimeapi.VolumeMount{
			Id:   mount.ID,
			Name: mount.Name,
			Path: mount.Path,
			Type: mount.Type,
		})
	}

	runtimeSpec := &runtimeapi.SandboxSpec{
		ProductSandboxId:      spec.SandboxID,
		Alias:                 spec.Alias,
		Source:                source,
		Resources:             &runtimeapi.ResourceSpec{Vcpu: spec.VCPU, MemoryMb: spec.RamMB, DiskMb: spec.DiskMB},
		MaxSandboxLengthHours: uint64(spec.MaxSandboxLengthHours),
		AllowInternet:         spec.AllowInternet,
		EnvVars:               cloneStringMap(spec.EnvVars),
		Metadata:              cloneStringMap(spec.Metadata),
		VolumeMounts:          volumeMounts,
		Extra:                 cloneStringMap(spec.Extra),
	}
	if spec.Network != nil {
		runtimeSpec.Network = networkPolicyToProto(*spec.Network)
	}
	if spec.EndTime != nil {
		runtimeSpec.ExpiresAt = timestamppb.New(*spec.EndTime)
	}
	return runtimeSpec, nil
}

func networkPolicyToProto(policy engine.NetworkPolicy) *runtimeapi.NetworkPolicy {
	return &runtimeapi.NetworkPolicy{
		AllowedCidrs:    append([]string(nil), policy.AllowedCIDRs...),
		DeniedCidrs:     append([]string(nil), policy.DeniedCIDRs...),
		AllowedDomains:  append([]string(nil), policy.AllowedDomains...),
		MaskRequestHost: policy.MaskRequestHost,
	}
}

func requestFingerprint(spec *runtimeapi.SandboxSpec) ([]byte, error) {
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(spec)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(encoded)
	return sum[:], nil
}

func rejectSensitiveValues(field string, values map[string]string) error {
	for key, value := range values {
		if sensitiveKey(key) || sensitiveValue(value) {
			return status.Errorf(codes.InvalidArgument,
				"%s key %q contains credential material; use AccessDescriptor instead", field, key)
		}
	}
	return nil
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

func sandboxRuntimeFromProto(info *runtimeapi.SandboxInfo, mapping podRoute, nodeName, nodeIP string) *engine.SandboxRuntime {
	runtime := &engine.SandboxRuntime{
		Backend:      string(engine.EngineTypeAgentENV),
		CRISandboxID: mapping.CRISandboxID,
		SandboxID:    info.GetProductSandboxId(),
		State:        runtimePhaseName(info.GetPhase()),
		NodeName:     nodeName,
		HostIP:       nodeIP,
		Extra:        cloneStringMap(info.GetExtra()),
	}
	if runtime.SandboxID == "" {
		runtime.SandboxID = mapping.ProductSandboxID
	}
	ensureExtra(runtime, "native_sandbox_id", info.GetNativeSandboxId())
	if info.GetCreatedAt() != nil {
		createdAt := info.GetCreatedAt().AsTime()
		runtime.StartTime = &createdAt
	}
	if info.GetExpiresAt() != nil {
		expiresAt := info.GetExpiresAt().AsTime()
		runtime.EndTime = &expiresAt
	}
	return runtime
}

func sandboxRuntimeInfoFromProto(info *runtimeapi.SandboxRuntimeInfo, mapping podRoute, nodeName, nodeIP string) *engine.SandboxRuntime {
	runtime := &engine.SandboxRuntime{
		Backend:      string(engine.EngineTypeAgentENV),
		CRISandboxID: mapping.CRISandboxID,
		SandboxID:    info.GetProductSandboxId(),
		State:        runtimePhaseName(info.GetPhase()),
		NodeName:     nodeName,
		NetNSPath:    info.GetNetnsPath(),
		HostIP:       nodeIP,
		Extra:        cloneStringMap(info.GetExtra()),
	}
	if runtime.SandboxID == "" {
		runtime.SandboxID = mapping.ProductSandboxID
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
