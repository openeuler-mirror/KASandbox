package agentenv

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	runtimeapi "github.com/cri-multiplex/pkg/agentenv/runtime/v1"
	"github.com/cri-multiplex/pkg/engine"
)

const (
	annTemplate                       = "sandbox.cri/template"
	templateRefMaxLength              = 512
	defaultCRIOperationTimeoutSeconds = 300
	defaultCRIDeleteTimeoutSeconds    = 60
	// CRI sandbox 生命周期由 kubelet 管理，不能继承 Runtime 原生 API 的 15 秒默认 TTL。
	defaultCRISandboxMaxLengthHours = 24
)

// validateDeleteOperationStatus 防止 Runtime Delete RPC 无 error 但 operation
// 未成功时被 CRI adapter 误判为已完成。UNKNOWN 表示副作用结果不确定，必须保留
// CRI record 并让 kubelet 后续重试。
func validateDeleteOperationStatus(expectedOperationID string, operation *runtimeapi.OperationStatus) error {
	if operation == nil {
		return status.Error(codes.Internal, "AgentENV sandbox delete returned a nil operation status")
	}
	if actual := operation.GetOperationId(); actual != expectedOperationID {
		return status.Errorf(codes.Internal,
			"AgentENV sandbox delete operation id mismatch: got %q, want %q", actual, expectedOperationID)
	}
	state := strings.ToUpper(strings.TrimSpace(operation.GetState()))
	if state == "SUCCEEDED" {
		return nil
	}
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
		"AgentENV sandbox delete operation %s did not succeed: state=%s detail=%s",
		expectedOperationID, state, detail)
}

type createPlan struct {
	criID       string
	name        string
	namespace   string
	templateID  string
	labels      map[string]string
	annotations map[string]string
	podConfig   *runtime.PodSandboxConfig
	spec        *runtimeapi.SandboxSpec
}

// RunPodSandbox 是 AgentENV 的 CRI 创建主路径，只接受 template source。
func (e *Engine) RunPodSandbox(ctx context.Context, req *runtime.RunPodSandboxRequest) (*runtime.RunPodSandboxResponse, error) {
	plan, err := buildCreatePlan(req)
	if err != nil {
		return nil, err
	}
	if err := validatePodPorts(plan); err != nil {
		return nil, err
	}
	if err := e.ensureClients(); err != nil {
		return nil, e.mapRuntimeError(err)
	}
	lockStarted := time.Now()
	unlock, err := e.lockSandbox(ctx, plan.criID)
	if err != nil {
		return nil, status.FromContextError(err).Err()
	}
	defer unlock()
	lockWaitMs := time.Since(lockStarted).Milliseconds()

	fingerprint, err := requestFingerprint(plan.spec)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "fingerprint AgentENV create request: %v", err)
	}
	if existing, ok := e.getPod(plan.criID); ok {
		if existing.PodUID != plan.criID {
			return nil, status.Error(codes.AlreadyExists, "sandbox id belongs to another Pod")
		}
		if existing.RequestFingerprint != "" && existing.RequestFingerprint != string(fingerprint) {
			return nil, status.Errorf(codes.AlreadyExists,
				"AgentENV sandbox %s already has a different create request", plan.criID)
		}
		if existing.NativeSandboxID != "" {
			found, err := e.runtimeSandboxExists(ctx, existing.NativeSandboxID)
			if err != nil {
				return nil, err
			}
			if found {
				if err := e.refreshRuntimeNetwork(ctx, &existing); err != nil {
					return nil, err
				}
				if err := e.ensurePodNetwork(existing); err != nil {
					return nil, err
				}
				if existing.State != criStateReady || existing.LastError != "" {
					existing.State = criStateReady
					existing.LastError = ""
					if err := e.putPod(existing); err != nil {
						return nil, status.Errorf(codes.Internal, "persist recovered AgentENV sandbox: %v", err)
					}
				}
				return &runtime.RunPodSandboxResponse{PodSandboxId: existing.CRISandboxID}, nil
			}
			if err := e.rollbackPodCreate(ctx, existing); err != nil {
				return nil, status.Errorf(codes.Internal,
					"reclaim stale AgentENV sandbox %s: %v", existing.CRISandboxID, err)
			}
		} else {
			return e.completePodCreate(ctx, plan, existing, string(fingerprint))
		}
	}
	now := time.Now()
	record := podRecord{
		CRISandboxID:       plan.criID,
		PodUID:             plan.criID,
		Name:               plan.name,
		Namespace:          plan.namespace,
		LogDirectory:       plan.podConfig.GetLogDirectory(),
		TemplateID:         plan.templateID,
		Labels:             cloneStringMap(plan.labels),
		Annotations:        cloneStringMap(plan.annotations),
		CreateOperationID:  "cri-create:" + plan.criID,
		RequestFingerprint: string(fingerprint),
		CreateStartedAt:    now,
		State:              criStateCreating,
		CreatedAt:          now,
	}
	record.RuntimeNetwork = &runtimeNetworkRecord{Mode: runtimeManagedNetworkMode}
	if err := e.putPod(record); err != nil {
		return nil, status.Errorf(codes.Internal, "persist pending AgentENV CRI sandbox: %v", err)
	}
	completeStarted := time.Now()
	response, err := e.completePodCreate(ctx, plan, record, string(fingerprint))
	log.Printf("[PerfTrace] sandbox=%s network_mode=runtime-managed lock_wait_ms=%d runpod_total_ms=%d success=%t", plan.criID, lockWaitMs, time.Since(completeStarted).Milliseconds(), err == nil)
	return response, err
}
func (e *Engine) completePodCreate(ctx context.Context, plan *createPlan, record podRecord, fingerprint string) (*runtime.RunPodSandboxResponse, error) {
	adapterStarted := time.Now()
	plan.spec.RuntimeNetwork = nil
	runtimeCreateStarted := time.Now()
	response, err := e.lifecycle.Create(ctx, &runtimeapi.SandboxCreateRequest{
		OperationId:        record.CreateOperationID,
		RequestFingerprint: []byte(fingerprint),
		Spec:               plan.spec,
		TimeoutSeconds:     defaultCRIOperationTimeoutSeconds,
	})
	if err != nil {
		mapped := e.mapRuntimeError(err)
		if code := status.Code(mapped); code != codes.DeadlineExceeded && code != codes.Unknown {
			return nil, e.rollbackFailedCreate(ctx, record, mapped)
		}
		record.LastError = mapped.Error()
		if persistErr := e.putPod(record); persistErr != nil {
			return nil, status.Errorf(codes.Internal,
				"AgentENV create outcome is unknown and pending state persistence failed: %v; original: %v",
				persistErr, mapped)
		}
		return nil, mapped
	}
	runtimeCreateMs := time.Since(runtimeCreateStarted).Milliseconds()

	info := response.GetSandbox()
	nativeID := info.GetNativeSandboxId()
	if nativeID == "" {
		recovered, recoverErr := e.findRuntimeSandboxByProduct(ctx, plan.criID)
		if recoverErr != nil {
			return nil, e.rollbackFailedCreate(ctx, record, recoverErr)
		}
		if recovered != nil {
			nativeID = recovered.GetNativeSandboxId()
		}
	}
	if nativeID == "" {
		cause := status.Error(codes.Internal, "AgentENV Runtime returned an empty native sandbox id")
		return nil, e.rollbackFailedCreate(ctx, record, cause)
	}
	if productID := info.GetProductSandboxId(); productID != "" && productID != plan.criID {
		cause := status.Errorf(codes.Internal,
			"AgentENV Runtime returned product sandbox id %q, want %q", productID, plan.criID)
		record.NativeSandboxID = nativeID
		return nil, e.rollbackFailedCreate(ctx, record, cause)
	}

	record.NativeSandboxID = nativeID
	if err := e.refreshRuntimeNetwork(ctx, &record); err != nil {
		return nil, e.rollbackFailedCreate(ctx, record, err)
	}
	record.State = criStateCreating
	record.LastError = ""
	// 保存 ready 投影。
	record.State = criStateReady
	record.LastError = ""
	persistStarted := time.Now()
	if err := e.putPod(record); err != nil {
		return nil, e.rollbackFailedCreate(ctx, record,
			status.Errorf(codes.Internal, "persist AgentENV CRI sandbox state: %v", err))
	}
	persistMs := time.Since(persistStarted).Milliseconds()
	log.Printf("[PerfTrace] sandbox=%s native=%s runtime_create_ms=%d persist_ms=%d adapter_total_ms=%d",
		plan.criID, nativeID, runtimeCreateMs,
		persistMs, time.Since(adapterStarted).Milliseconds())
	return &runtime.RunPodSandboxResponse{PodSandboxId: plan.criID}, nil
}

func (e *Engine) rollbackFailedCreate(ctx context.Context, record podRecord, cause error) error {
	if err := e.rollbackPodCreate(ctx, record); err != nil {
		return status.Errorf(codes.Internal, "%v; rollback failed: %v", cause, err)
	}
	return cause
}

// rollbackPodCreate 回收一次未完成 Create 的全部资源；失败时保留 cleanup-pending 供后续 CRI 清理重试。
func (e *Engine) rollbackPodCreate(ctx context.Context, record podRecord) error {
	rollbackCtx := context.WithoutCancel(ctx)
	var errs []error
	if record.NativeSandboxID != "" {
		if err := e.deleteRuntimeSandbox(rollbackCtx, record.CRISandboxID, record.NativeSandboxID); err != nil {
			errs = append(errs, fmt.Errorf("delete Runtime sandbox: %w", err))
		}
	}
	if len(errs) > 0 {
		record.State = criStateCleanupPending
		record.LastError = errors.Join(errs...).Error()
		if persistErr := e.putPod(record); persistErr != nil {
			errs = append(errs, fmt.Errorf("persist cleanup-pending state: %w", persistErr))
		}
		return errors.Join(errs...)
	}

	record.State = criStateCleanupPending
	record.LastError = ""
	if err := e.putPod(record); err != nil {
		return fmt.Errorf("persist completed rollback: %w", err)
	}
	if err := e.deletePod(record.CRISandboxID); err != nil {
		return err
	}
	return nil
}

func (e *Engine) findRuntimeSandboxByProduct(ctx context.Context, productID string) (*runtimeapi.SandboxInfo, error) {
	if productID == "" {
		return nil, nil
	}
	response, err := e.lifecycle.List(ctx, &runtimeapi.SandboxListRequest{
		IncludePaused:    true,
		ProductSandboxId: productID,
	})
	if err != nil {
		return nil, e.mapRuntimeError(err)
	}
	for _, info := range response.GetSandboxes() {
		if info.GetProductSandboxId() == productID && info.GetNativeSandboxId() != "" {
			return info, nil
		}
	}
	return nil, nil
}
func (e *Engine) PodSandboxStatus(ctx context.Context, req *runtime.PodSandboxStatusRequest) (*runtime.PodSandboxStatusResponse, error) {
	criID := req.GetPodSandboxId()
	if criID == "" {
		return nil, status.Error(codes.InvalidArgument, "pod sandbox id is required")
	}
	record, found := e.getPod(criID)
	if !found {
		return nil, status.Errorf(codes.NotFound, "AgentENV pod sandbox %s not found", criID)
	}

	state := criStateNotReady
	if record.NativeSandboxID != "" {
		if err := e.ensureClients(); err != nil {
			return nil, e.mapRuntimeError(err)
		}
		info, err := e.lifecycle.Get(ctx, &runtimeapi.SandboxGetRequest{NativeSandboxId: record.NativeSandboxID})
		if err == nil {
			networkReady, err := e.currentPodNetworkReady(ctx, record)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "check AgentENV pod network: %v", err)
			}
			if info.GetPhase() == runtimeapi.SandboxPhase_RUNNING && networkReady {
				state = criStateReady
			}
		} else if status.Code(e.mapRuntimeError(err)) != codes.NotFound {
			return nil, e.mapRuntimeError(err)
		}
	}
	if record.State != criStateCreating && record.State != criStateCleanupPending && record.State != state {
		if err := e.setPodState(criID, state); err != nil {
			return nil, status.Errorf(codes.Internal, "persist AgentENV CRI sandbox state: %v", err)
		}
		record.State = state
	}
	createdAt := record.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	network := &runtime.PodSandboxNetworkStatus{}
	if podIP := podIPFromRecord(record); podIP != "" && !record.RuntimeDeleted && record.State == criStateReady {
		network.Ip = podIP
	}
	return &runtime.PodSandboxStatusResponse{
		Status: &runtime.PodSandboxStatus{
			Id:             record.CRISandboxID,
			Metadata:       podMetadata(record),
			State:          criState(record.State),
			CreatedAt:      createdAt.UnixNano(),
			Network:        network,
			Labels:         cloneStringMap(record.Labels),
			Annotations:    cloneStringMap(record.Annotations),
			RuntimeHandler: string(engine.EngineTypeAgentENV),
		},
		Timestamp: time.Now().UnixNano(),
	}, nil
}

func (e *Engine) ListPodSandbox(ctx context.Context, req *runtime.ListPodSandboxRequest) (*runtime.ListPodSandboxResponse, error) {
	records := e.listPods()
	if len(records) == 0 {
		return &runtime.ListPodSandboxResponse{}, nil
	}
	if err := e.ensureClients(); err != nil {
		return nil, e.mapRuntimeError(err)
	}
	runtimeStates := make(map[string]*runtimeapi.SandboxInfo)
	response, err := e.lifecycle.List(ctx, &runtimeapi.SandboxListRequest{IncludePaused: true})
	if err != nil {
		return nil, e.mapRuntimeError(err)
	}
	for _, info := range response.GetSandboxes() {
		runtimeStates[info.GetNativeSandboxId()] = info
	}

	filter := req.GetFilter()
	items := make([]*runtime.PodSandbox, 0, len(records))
	for _, record := range records {
		state := criStateNotReady
		if info, ok := runtimeStates[record.NativeSandboxID]; ok {
			if info.GetPhase() == runtimeapi.SandboxPhase_RUNNING {
				networkReady, err := e.currentPodNetworkReady(ctx, record)
				if err != nil {
					return nil, status.Errorf(codes.Internal, "check AgentENV pod network: %v", err)
				}
				if networkReady {
					state = criStateReady
				} else {
					state = criStateNotReady
				}
			} else {
				state = criStateNotReady
			}
		}
		if filter.GetId() != "" && filter.GetId() != record.CRISandboxID {
			continue
		}
		if filter.GetState() != nil && filter.GetState().GetState() != criState(state) {
			continue
		}
		if !labelsMatch(record.Labels, filter.GetLabelSelector()) {
			continue
		}
		items = append(items, &runtime.PodSandbox{
			Id:             record.CRISandboxID,
			Metadata:       podMetadata(record),
			State:          criState(state),
			CreatedAt:      record.CreatedAt.UnixNano(),
			Labels:         cloneStringMap(record.Labels),
			Annotations:    cloneStringMap(record.Annotations),
			RuntimeHandler: string(engine.EngineTypeAgentENV),
		})
	}
	return &runtime.ListPodSandboxResponse{Items: items}, nil
}
func (e *Engine) StopPodSandbox(ctx context.Context, req *runtime.StopPodSandboxRequest) (resp *runtime.StopPodSandboxResponse, retErr error) {
	criID := req.GetPodSandboxId()
	lockStarted := time.Now()
	unlock, err := e.lockSandbox(ctx, criID)
	if err != nil {
		return nil, status.FromContextError(err).Err()
	}
	defer unlock()
	lockWaitMs := time.Since(lockStarted).Milliseconds()
	started := time.Now()
	defer func() {
		log.Printf("[PerfTrace] sandbox=%s stop_total_ms=%d lock_wait_ms=%d success=%t",
			criID, time.Since(started).Milliseconds(), lockWaitMs, retErr == nil)
	}()
	return e.stopPodSandboxLocked(ctx, req)
}

func (e *Engine) stopPodSandboxLocked(ctx context.Context, req *runtime.StopPodSandboxRequest) (*runtime.StopPodSandboxResponse, error) {
	criID := req.GetPodSandboxId()
	if criID == "" {
		return nil, status.Error(codes.InvalidArgument, "pod sandbox id is required")
	}
	record, found := e.getPod(criID)
	if !found {
		return &runtime.StopPodSandboxResponse{}, nil
	}
	if record.State == criStateNotReady && record.RuntimeDeleted {
		return &runtime.StopPodSandboxResponse{}, nil
	}
	if record.NativeSandboxID == "" {
		_ = e.deletePod(criID)
		return &runtime.StopPodSandboxResponse{}, nil
	}
	if err := e.ensureClients(); err != nil {
		return nil, e.mapRuntimeError(err)
	}
	operationID := "cri-stop:" + criID
	deleteStarted := time.Now()
	operation, err := e.lifecycle.Delete(ctx, &runtimeapi.SandboxDeleteRequest{
		OperationId:     operationID,
		NativeSandboxId: record.NativeSandboxID,
		TimeoutSeconds:  defaultCRIDeleteTimeoutSeconds,
	})
	deleteOK := err == nil || status.Code(e.mapRuntimeError(err)) == codes.NotFound
	log.Printf("[PerfTrace] sandbox=%s runtime_delete_ms=%d success=%t",
		criID, time.Since(deleteStarted).Milliseconds(), deleteOK)
	if err != nil && status.Code(e.mapRuntimeError(err)) != codes.NotFound {
		return nil, e.mapRuntimeError(err)
	}
	if err == nil {
		if err := validateDeleteOperationStatus(operationID, operation); err != nil {
			return nil, err
		}
	}
	if err := e.setPodRuntimeDeleted(criID); err != nil {
		return nil, status.Errorf(codes.Internal, "persist stopped AgentENV CRI sandbox: %v", err)
	}
	return &runtime.StopPodSandboxResponse{}, nil
}

func (e *Engine) RemovePodSandbox(ctx context.Context, req *runtime.RemovePodSandboxRequest) (resp *runtime.RemovePodSandboxResponse, retErr error) {
	criID := req.GetPodSandboxId()
	lockStarted := time.Now()
	unlock, err := e.lockSandbox(ctx, criID)
	if err != nil {
		return nil, status.FromContextError(err).Err()
	}
	defer unlock()
	lockWaitMs := time.Since(lockStarted).Milliseconds()
	removeStarted := time.Now()
	defer func() {
		log.Printf("[PerfTrace] sandbox=%s remove_total_ms=%d lock_wait_ms=%d success=%t",
			criID, time.Since(removeStarted).Milliseconds(), lockWaitMs, retErr == nil)
	}()
	if criID == "" {
		return nil, status.Error(codes.InvalidArgument, "pod sandbox id is required")
	}
	if _, err := e.stopPodSandboxLocked(ctx, &runtime.StopPodSandboxRequest{PodSandboxId: criID}); err != nil {
		return nil, err
	}
	var errs []error
	if err := e.deletePod(criID); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return nil, status.Errorf(codes.Internal, "remove AgentENV CRI sandbox: %v", errors.Join(errs...))
	}
	return &runtime.RemovePodSandboxResponse{}, nil
}
func buildCreatePlan(req *runtime.RunPodSandboxRequest) (*createPlan, error) {
	if req == nil || req.GetConfig() == nil || req.GetConfig().GetMetadata() == nil {
		return nil, status.Error(codes.InvalidArgument, "pod sandbox config and metadata are required")
	}
	config := req.GetConfig()
	meta := config.GetMetadata()
	templateID := strings.TrimSpace(config.GetAnnotations()[annTemplate])
	if templateID == "" {
		return nil, status.Errorf(codes.InvalidArgument, "%s is required", annTemplate)
	}
	if len(templateID) > templateRefMaxLength {
		return nil, status.Errorf(codes.InvalidArgument, "%s exceeds %d bytes", annTemplate, templateRefMaxLength)
	}
	if meta.GetUid() == "" {
		return nil, status.Error(codes.InvalidArgument, "pod sandbox UID is required")
	}

	autoPause := false
	return &createPlan{
		criID:       meta.GetUid(),
		name:        meta.GetName(),
		namespace:   meta.GetNamespace(),
		templateID:  templateID,
		labels:      cloneStringMap(config.GetLabels()),
		annotations: cloneStringMap(config.GetAnnotations()),
		podConfig:   config,
		spec: &runtimeapi.SandboxSpec{
			ProductSandboxId: meta.GetUid(),
			Alias:            meta.GetName(),
			Source: &runtimeapi.SandboxSource{Kind: &runtimeapi.SandboxSource_Template{Template: &runtimeapi.TemplateSource{
				TemplateId: templateID,
			}}},
			Resources:             &runtimeapi.ResourceSpec{},
			MaxSandboxLengthHours: defaultCRISandboxMaxLengthHours,
			AutoPause:             &autoPause,
			AutoResume:            &runtimeapi.AutoResumeConfig{},
		},
	}, nil
}

func (e *Engine) runtimeSandboxExists(ctx context.Context, nativeID string) (bool, error) {
	if nativeID == "" {
		return false, nil
	}
	_, err := e.lifecycle.Get(ctx, &runtimeapi.SandboxGetRequest{NativeSandboxId: nativeID})
	if err == nil {
		return true, nil
	}
	mapped := e.mapRuntimeError(err)
	if status.Code(mapped) == codes.NotFound {
		return false, nil
	}
	return false, mapped
}

func (e *Engine) deleteRuntimeSandbox(ctx context.Context, criID, nativeID string) error {
	if nativeID == "" {
		return nil
	}
	_, err := e.lifecycle.Delete(ctx, &runtimeapi.SandboxDeleteRequest{
		OperationId:     "cri-rollback:" + criID,
		NativeSandboxId: nativeID,
		TimeoutSeconds:  defaultCRIDeleteTimeoutSeconds,
	})
	if err != nil && status.Code(e.mapRuntimeError(err)) != codes.NotFound {
		return e.mapRuntimeError(err)
	}
	return nil
}
func podMetadata(record podRecord) *runtime.PodSandboxMetadata {
	return &runtime.PodSandboxMetadata{
		Name:      record.Name,
		Uid:       record.PodUID,
		Namespace: record.Namespace,
		Attempt:   0,
	}
}

func criState(state string) runtime.PodSandboxState {
	if state == criStateReady {
		return runtime.PodSandboxState_SANDBOX_READY
	}
	return runtime.PodSandboxState_SANDBOX_NOTREADY
}

func labelsMatch(labels, selector map[string]string) bool {
	for key, value := range selector {
		if labels[key] != value {
			return false
		}
	}
	return true
}

func unimplemented(method string) error {
	return status.Errorf(codes.Unimplemented, "AgentENV CRI method %s is not implemented", method)
}

func (e *Engine) GetContainerEvents(context.Context, *runtime.GetEventsRequest, ...grpc.CallOption) (runtime.RuntimeService_GetContainerEventsClient, error) {
	return nil, unimplemented("GetContainerEvents")
}

const podNetworkCleanupTimeout = 30 * time.Second
const runtimeManagedNetworkMode = "runtime-managed"

// validatePodPorts 允许 containerPort 声明，但拒绝额外宿主端口绑定。
func validatePodPorts(plan *createPlan) error {
	if plan == nil || plan.podConfig == nil {
		return status.Error(codes.InvalidArgument, "pod sandbox config is required")
	}
	for _, mapping := range plan.podConfig.GetPortMappings() {
		if mapping.GetHostPort() != 0 {
			return status.Error(codes.InvalidArgument, "HostPort is not supported with AgentENV runtime-managed networking; use the AgentENV proxy")
		}
	}
	return nil
}
func (e *Engine) ensurePodNetwork(record podRecord) error { return e.ensurePodNetworkFacts(record) }

// podNetworkReady 不分配或回收网络；网络生命周期完全属于 Runtime。
func (e *Engine) podNetworkReady(record podRecord) (bool, error) {
	if record.RuntimeNetwork == nil || record.RuntimeDeleted || record.RuntimeNetwork.Mode != runtimeManagedNetworkMode || !validRuntimePodIP(record.RuntimeNetwork.PodIP) {
		return false, nil
	}
	return true, nil
}
func podIPFromRecord(record podRecord) string {
	if record.RuntimeNetwork == nil {
		return ""
	}
	return record.RuntimeNetwork.PodIP
}

// refreshRuntimeNetwork 只接受活实例返回的宿主可达地址，不使用重复的 guest link-local IP。
func (e *Engine) refreshRuntimeNetwork(ctx context.Context, record *podRecord) error {
	if record.RuntimeNetwork == nil {
		return status.Error(codes.FailedPrecondition, "Runtime network projection is missing")
	}
	info, err := e.admin.GetRuntime(ctx, &runtimeapi.SandboxGetRuntimeRequest{NativeSandboxId: record.NativeSandboxID})
	if err != nil {
		return e.mapRuntimeError(err)
	}
	if info.GetNativeSandboxId() != record.NativeSandboxID || info.GetPhase() != runtimeapi.SandboxPhase_RUNNING || info.GetNetwork()["mode"] != runtimeManagedNetworkMode {
		return status.Error(codes.FailedPrecondition, "Runtime does not expose a live runtime-managed network")
	}
	ip, path := info.GetNetwork()["pod_ip"], info.GetNetnsPath()
	if !validRuntimePodIP(ip) || !filepath.IsAbs(path) {
		return status.Error(codes.FailedPrecondition, "Runtime returned an invalid Pod IP or netns path")
	}
	// 活实例身份改变时拒绝旧地址，避免 slot 复用误路由。
	if record.RuntimeNetwork.PodIP != "" && (record.RuntimeNetwork.PodIP != ip || record.RuntimeNetwork.NetNSPath != path) {
		return status.Error(codes.FailedPrecondition, "Runtime network identity changed")
	}
	record.RuntimeNetwork = &runtimeNetworkRecord{Mode: runtimeManagedNetworkMode, PodIP: ip, NetNSPath: path}
	return e.ensurePodNetworkFacts(*record)
}
func (e *Engine) ensurePodNetworkFacts(record podRecord) error {
	ready, err := e.podNetworkReady(record)
	if err != nil || !ready {
		return status.Errorf(codes.FailedPrecondition, "Runtime network is unavailable: %v", err)
	}
	return nil
}
func validRuntimePodIP(value string) bool {
	ip := net.ParseIP(value)
	return ip != nil && ip.To4() != nil && ip.IsGlobalUnicast() && !ip.IsLinkLocalUnicast() && !ip.IsLoopback()
}

// currentPodNetworkReady 核对活实例归属，不能把被复用的原生 slot 当成旧网络。
func (e *Engine) currentPodNetworkReady(ctx context.Context, record podRecord) (bool, error) {
	if record.RuntimeNetwork != nil {
		if record.RuntimeDeleted {
			return false, nil
		}
		if err := e.refreshRuntimeNetwork(ctx, &record); err != nil {
			if status.Code(err) == codes.FailedPrecondition || status.Code(err) == codes.NotFound {
				return false, nil
			}
			return false, err
		}
	}
	return e.podNetworkReady(record)
}

// ExecSync 不提供 CRI 命令执行；业务命令统一通过 SDK 调用。
func (e *Engine) ExecSync(context.Context, *runtime.ExecSyncRequest) (*runtime.ExecSyncResponse, error) {
	return nil, unimplemented("ExecSync; use the AgentENV SDK")
}

// Exec 不提供 CRI 命令执行；业务命令统一通过 SDK 调用。
func (e *Engine) Exec(context.Context, *runtime.ExecRequest) (*runtime.ExecResponse, error) {
	return nil, unimplemented("Exec; use the AgentENV SDK")
}

// Attach 不提供 CRI 命令执行；业务命令统一通过 SDK 调用。
func (e *Engine) Attach(context.Context, *runtime.AttachRequest) (*runtime.AttachResponse, error) {
	return nil, unimplemented("Attach; use the AgentENV SDK")
}

// PortForward 不代理业务流量，直接使用 AgentENV SDK/proxy。
func (e *Engine) PortForward(context.Context, *runtime.PortForwardRequest) (*runtime.PortForwardResponse, error) {
	return nil, status.Error(codes.Unimplemented, "use AgentENV SDK/proxy")
}
