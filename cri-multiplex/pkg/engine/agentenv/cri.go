package agentenv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
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
	annBackend           = "sandbox.cri/backend"
	annSandboxID         = "sandbox.cri/sandbox-id"
	annTemplate          = "sandbox.cri/template"
	annBuild             = "sandbox.cri/build"
	annMetadata          = "sandbox.cri/metadata"
	annVCPU              = "sandbox.cri/vcpu"
	annMemoryMB          = "sandbox.cri/memory-mb"
	annDiskMB            = "sandbox.cri/disk-mb"
	annCanonicalTemplate = "aenv.dev/template-id"
	annTimeoutSeconds    = "aenv.dev/timeout-seconds"
	annAutoPause         = "aenv.dev/auto-pause"
	annAutoResume        = "aenv.dev/auto-resume"
	annSecure            = "aenv.dev/secure"
	annAllowInternet     = "aenv.dev/allow-internet"
	annNetwork           = "aenv.dev/network"
	annEnvVars           = "aenv.dev/env-vars"
	annCustomParams      = "aenv.dev/custom-extension-params"
	annMaxLengthHours    = "aenv.dev/max-sandbox-length-hours"
	annVolumeMounts      = "aenv.dev/volume-mounts"

	defaultCRIOperationTimeoutSeconds = 300
	maxCRIOperationTimeoutSeconds     = 86400
	maxCRISandboxLengthHours          = 8760
	maxAnnotationJSONBytes            = 32 * 1024
	maxAutoResumeJSONBytes            = 1024
	maxStringMapEntries               = 256
	maxCustomParamsFields             = 64
	templateRefMaxLength              = 512
	buildRefMaxLength                 = 128
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
	productID   string
	podUID      string
	name        string
	namespace   string
	templateID  string
	buildID     string
	labels      map[string]string
	annotations map[string]string
	podConfig   *runtime.PodSandboxConfig
	spec        *runtimeapi.SandboxSpec
	timeout     uint64
}

type networkAnnotation struct {
	AllowedCIDRs    []string `json:"allowed_cidrs"`
	DeniedCIDRs     []string `json:"denied_cidrs"`
	AllowedDomains  []string `json:"allowed_domains"`
	AllowPublic     *bool    `json:"allow_public_traffic"`
	MaskRequestHost *string  `json:"mask_request_host"`
}

type autoResumeAnnotation struct {
	Enabled bool `json:"enabled"`
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
	unlock, err := e.lockSandbox(ctx, plan.criID, plan.productID)
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
		if existing.RequestFingerprint != "" && existing.RequestFingerprint != string(fingerprint) {
			return nil, status.Errorf(codes.AlreadyExists,
				"AgentENV sandbox %s already has a different create request", plan.criID)
		}
		if existing.ProductSandboxID != "" && existing.ProductSandboxID != plan.productID {
			return nil, status.Errorf(codes.AlreadyExists,
				"AgentENV sandbox %s is already bound to product %s", plan.criID, existing.ProductSandboxID)
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
				if err := e.ensurePodRoute(existing); err != nil {
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
	if route, ok := e.routes.PodRouteByCRI(plan.criID); ok {
		if route.NativeSandboxID == "" {
			// 崩溃窗口可能只留下 reservation；先确认 Runtime 侧不存在，再清理残留路由。
			info, err := e.findRuntimeSandboxByProduct(ctx, plan.productID)
			if err != nil {
				return nil, err
			}
			if info != nil {
				return nil, status.Errorf(codes.FailedPrecondition,
					"AgentENV runtime exists for product %s but local runtime projection is missing", plan.productID)
			}
			if err := e.routes.DeleteSandbox(plan.criID); err != nil {
				return nil, status.Errorf(codes.Internal, "delete stale AgentENV route: %v", err)
			}
		} else {
			found, err := e.runtimeSandboxExists(ctx, route.NativeSandboxID)
			if err != nil {
				return nil, err
			}
			if found {
				return nil, status.Errorf(codes.FailedPrecondition,
					"AgentENV CRI sandbox %s has a runtime route but no local state", plan.criID)
			}
			if err := e.routes.DeleteSandbox(plan.criID); err != nil {
				return nil, status.Errorf(codes.Internal, "delete stale AgentENV route: %v", err)
			}
		}
	}

	if route, ok := e.routes.PodRouteByProduct(plan.productID); ok {
		if route.CRISandboxID != "" && route.CRISandboxID != plan.criID {
			return nil, status.Errorf(codes.FailedPrecondition,
				"product sandbox id %s is already bound to CRI sandbox %s", plan.productID, route.CRISandboxID)
		}
		if route.NativeSandboxID == "" {
			return nil, status.Errorf(codes.FailedPrecondition,
				"AgentENV sandbox creation for product %s has an unknown in-flight outcome", plan.productID)
		}
		found, err := e.runtimeSandboxExists(ctx, route.NativeSandboxID)
		if err != nil {
			return nil, err
		}
		if found {
			if _, _, err := e.routes.BindPodRoute(plan.productID, plan.criID, engine.EngineTypeAgentENV); err != nil {
				return nil, status.Errorf(codes.Internal, "bind AgentENV sandbox route: %v", err)
			}
			return nil, status.Errorf(codes.FailedPrecondition,
				"AgentENV runtime exists for product %s but local runtime projection is missing", plan.productID)
		}
		if err := e.routes.DeleteSandbox(plan.productID); err != nil {
			return nil, status.Errorf(codes.Internal, "delete stale AgentENV product route: %v", err)
		}
	}
	reservation := podRoute{
		Backend:          engine.EngineTypeAgentENV,
		CRISandboxID:     plan.criID,
		ProductSandboxID: plan.productID,
	}
	if err := e.routes.PutPodRoute(reservation); err != nil {
		_ = e.routes.DeleteSandbox(plan.criID)
		return nil, status.Errorf(codes.FailedPrecondition, "reserve AgentENV sandbox ids: %v", err)
	}
	now := time.Now()
	record := podRecord{
		CRISandboxID:       plan.criID,
		ProductSandboxID:   plan.productID,
		PodUID:             plan.podUID,
		Name:               plan.name,
		Namespace:          plan.namespace,
		LogDirectory:       plan.podConfig.GetLogDirectory(),
		TemplateID:         plan.templateID,
		BuildID:            plan.buildID,
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
		routeErr := e.routes.DeleteSandbox(plan.criID)
		return nil, status.Errorf(codes.Internal, "persist pending AgentENV CRI sandbox: %v; cleanup: %v",
			err, routeErr)
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
		TimeoutSeconds:     plan.timeout,
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
		recovered, recoverErr := e.findRuntimeSandboxByProduct(ctx, plan.productID)
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
	if productID := info.GetProductSandboxId(); productID != "" && productID != plan.productID {
		cause := status.Errorf(codes.Internal,
			"AgentENV Runtime returned product sandbox id %q, want %q", productID, plan.productID)
		record.NativeSandboxID = nativeID
		return nil, e.rollbackFailedCreate(ctx, record, cause)
	}

	record.NativeSandboxID = nativeID
	if err := e.refreshRuntimeNetwork(ctx, &record); err != nil {
		return nil, e.rollbackFailedCreate(ctx, record, err)
	}
	record.State = criStateCreating
	record.LastError = ""
	// 保存 ready 投影及后端私有 ID 对照。
	record.State = criStateReady
	record.LastError = ""
	persistStarted := time.Now()
	route := podRoute{
		Backend:          engine.EngineTypeAgentENV,
		CRISandboxID:     record.CRISandboxID,
		ProductSandboxID: record.ProductSandboxID,
		NativeSandboxID:  record.NativeSandboxID,
	}
	if err := e.putPodWithRoute(record, route); err != nil {
		return nil, e.rollbackFailedCreate(ctx, record,
			status.Errorf(codes.Internal, "persist AgentENV CRI sandbox state: %v", err))
	}
	persistMs := time.Since(persistStarted).Milliseconds()
	log.Printf("[PerfTrace] sandbox=%s native=%s runtime_create_ms=%d persist_ms=%d adapter_total_ms=%d",
		plan.criID, nativeID, runtimeCreateMs,
		persistMs, time.Since(adapterStarted).Milliseconds())
	return &runtime.RunPodSandboxResponse{PodSandboxId: plan.criID}, nil
}

func (e *Engine) ensurePodRoute(record podRecord) error {
	if record.CRISandboxID == "" || record.NativeSandboxID == "" {
		return status.Error(codes.FailedPrecondition, "CRI and native sandbox ids are required for route")
	}
	if err := e.routes.PutPodRoute(podRoute{
		Backend:          engine.EngineTypeAgentENV,
		CRISandboxID:     record.CRISandboxID,
		ProductSandboxID: record.ProductSandboxID,
		NativeSandboxID:  record.NativeSandboxID,
	}); err != nil {
		return status.Errorf(codes.Internal, "persist AgentENV sandbox route: %v", err)
	}
	return nil
}

func (e *Engine) rollbackFailedCreate(ctx context.Context, record podRecord, cause error) error {
	if err := e.rollbackPodCreate(ctx, record); err != nil {
		return status.Errorf(codes.Internal, "%v; rollback failed: %v", cause, err)
	}
	return cause
}

// rollbackPodCreate 回收一次未完成 Create 的全部资源；失败时保留 cleanup-pending 供 reconciler 重试。
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
	if err := e.routes.DeleteSandbox(record.CRISandboxID); err != nil {
		return fmt.Errorf("delete AgentENV route: %w", err)
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
		route, ok := e.routes.PodRouteByCRI(criID)
		if !ok {
			return nil, status.Errorf(codes.NotFound, "AgentENV pod sandbox %s not found", criID)
		}
		record = podRecord{
			CRISandboxID:     criID,
			ProductSandboxID: route.ProductSandboxID,
			NativeSandboxID:  route.NativeSandboxID,
			State:            criStateNotReady,
		}
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
	ids := e.sandboxOperationIDs(criID)
	unlock, err := e.lockSandbox(ctx, ids...)
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
		route, ok := e.routes.PodRouteByCRI(criID)
		if !ok {
			return &runtime.StopPodSandboxResponse{}, nil
		}
		record = podRecord{CRISandboxID: criID, ProductSandboxID: route.ProductSandboxID, NativeSandboxID: route.NativeSandboxID}
	}
	if record.State == criStateNotReady && record.RuntimeDeleted {
		return &runtime.StopPodSandboxResponse{}, nil
	}
	if record.NativeSandboxID == "" {
		_ = e.deletePod(criID)
		_ = e.routes.DeleteSandbox(criID)
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
	ids := e.sandboxOperationIDs(criID)
	unlock, err := e.lockSandbox(ctx, ids...)
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
	if err := e.routes.DeleteSandbox(criID); err != nil {
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
	meta := req.GetConfig().GetMetadata()
	annotations := cloneStringMap(req.GetConfig().GetAnnotations())
	if err := validateAgentENVAnnotations(annotations); err != nil {
		return nil, err
	}
	if backend := strings.TrimSpace(annotations[annBackend]); backend != "" && backend != string(engine.EngineTypeAgentENV) {
		return nil, status.Errorf(codes.InvalidArgument, "%s=%q is inconsistent with runtime handler agentenv", annBackend, backend)
	}
	templateEntry := strings.TrimSpace(annotations[annTemplate])
	if templateEntry == "" {
		return nil, status.Errorf(codes.InvalidArgument, "%s is required; image/TEMPLATE_NAME/Pod name fallback is forbidden", annTemplate)
	}
	if meta.GetUid() == "" {
		return nil, status.Error(codes.InvalidArgument, "pod sandbox UID is required")
	}
	criID := meta.GetUid()
	productID := strings.TrimSpace(annotations[annSandboxID])
	if productID == "" {
		productID = criID
	}
	templateID := strings.TrimSpace(annotations[annCanonicalTemplate])
	if templateID == "" {
		templateID = templateEntry
	}

	autoPause := false
	if raw, ok := annotations[annAutoPause]; ok && raw != "" {
		parsed, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid %s: %v", annAutoPause, err)
		}
		if parsed {
			return nil, status.Errorf(codes.InvalidArgument, "%s=true is forbidden on CRI sandboxes", annAutoPause)
		}
		autoPause = parsed
	}
	autoResume := autoResumeAnnotation{}
	if raw := strings.TrimSpace(annotations[annAutoResume]); raw != "" {
		if len(raw) > maxAutoResumeJSONBytes {
			return nil, status.Errorf(codes.InvalidArgument, "%s exceeds %d bytes", annAutoResume, maxAutoResumeJSONBytes)
		}
		if err := decodeStrictJSON(annAutoResume, raw, &autoResume); err != nil {
			return nil, err
		}
	}
	envVars, err := parseStringMapAnnotation(annotations[annEnvVars])
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid %s JSON: %v", annEnvVars, err)
	}
	metadata, err := parseStringMapAnnotation(annotations[annMetadata])
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid %s JSON: %v", annMetadata, err)
	}
	if err := rejectSensitiveValues("metadata", metadata); err != nil {
		return nil, err
	}
	if err := rejectSensitiveValues("env_vars", envVars); err != nil {
		return nil, err
	}
	network, err := parseNetworkAnnotation(annotations[annNetwork])
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid %s JSON: %v", annNetwork, err)
	}
	customParams := strings.TrimSpace(annotations[annCustomParams])
	if customParams != "" {
		if len(customParams) > maxAnnotationJSONBytes {
			return nil, status.Errorf(codes.InvalidArgument, "%s exceeds %d bytes", annCustomParams, maxAnnotationJSONBytes)
		}
		var fields map[string]json.RawMessage
		if err := decodeStrictJSON(annCustomParams, customParams, &fields); err != nil {
			return nil, err
		}
		if len(fields) > maxCustomParamsFields {
			return nil, status.Errorf(codes.InvalidArgument, "%s exceeds %d top-level fields", annCustomParams, maxCustomParamsFields)
		}
		if err := rejectSensitiveJSON(annCustomParams, customParams); err != nil {
			return nil, err
		}
	}
	vcpu, err := parseInt64Annotation(annotations, annVCPU)
	if err != nil {
		return nil, err
	}
	memoryMB, err := parseInt64Annotation(annotations, annMemoryMB)
	if err != nil {
		return nil, err
	}
	diskMB, err := parseInt64Annotation(annotations, annDiskMB)
	if err != nil {
		return nil, err
	}
	maxLength, err := parseUint64Annotation(annotations, annMaxLengthHours)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(annotations[annMaxLengthHours]) == "" {
		maxLength = defaultCRISandboxMaxLengthHours
	} else if maxLength == 0 || maxLength > maxCRISandboxLengthHours {
		return nil, status.Errorf(codes.InvalidArgument, "%s must be between 1 and %d", annMaxLengthHours, maxCRISandboxLengthHours)
	}
	timeout, err := parseTimeout(annotations[annTimeoutSeconds])
	if err != nil {
		return nil, err
	}
	secure, err := parseOptionalBoolAnnotation(annotations, annSecure)
	if err != nil {
		return nil, err
	}
	allowInternet, err := parseOptionalBoolAnnotation(annotations, annAllowInternet)
	if err != nil {
		return nil, err
	}

	spec := &runtimeapi.SandboxSpec{
		ProductSandboxId: productID,
		Alias:            meta.GetName(),
		Source: &runtimeapi.SandboxSource{Kind: &runtimeapi.SandboxSource_Template{Template: &runtimeapi.TemplateSource{
			TemplateId: templateID,
			BuildId:    strings.TrimSpace(annotations[annBuild]),
		}}},
		Resources:                 &runtimeapi.ResourceSpec{Vcpu: vcpu, MemoryMb: memoryMB, DiskMb: diskMB},
		MaxSandboxLengthHours:     maxLength,
		AllowInternet:             allowInternet,
		Secure:                    secure,
		AutoPause:                 &autoPause,
		AutoResume:                &runtimeapi.AutoResumeConfig{Enabled: autoResume.Enabled},
		EnvVars:                   envVars,
		Metadata:                  metadata,
		Network:                   network,
		CustomExtensionParamsJson: customParams,
	}
	return &createPlan{
		criID:       criID,
		productID:   productID,
		podUID:      meta.GetUid(),
		name:        meta.GetName(),
		namespace:   meta.GetNamespace(),
		templateID:  templateID,
		buildID:     strings.TrimSpace(annotations[annBuild]),
		labels:      cloneStringMap(req.GetConfig().GetLabels()),
		annotations: annotations,
		podConfig:   req.GetConfig(),
		spec:        spec,
		timeout:     timeout,
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

func validateAgentENVAnnotations(annotations map[string]string) error {
	allowed := map[string]struct{}{
		annCanonicalTemplate: {}, annTimeoutSeconds: {}, annAutoPause: {}, annAutoResume: {},
		annSecure: {}, annAllowInternet: {}, annNetwork: {}, annEnvVars: {},
		annCustomParams: {}, annMaxLengthHours: {},
	}
	for key := range annotations {
		if strings.HasPrefix(key, "aenv.dev/") {
			if _, ok := allowed[key]; !ok {
				return status.Errorf(codes.InvalidArgument, "unknown AgentENV annotation %q", key)
			}
		}
	}
	if _, exists := annotations[annVolumeMounts]; exists {
		return status.Errorf(codes.InvalidArgument, "%s is reserved and not implemented", annVolumeMounts)
	}
	limits := map[string]int{
		annTemplate: templateRefMaxLength, annBuild: buildRefMaxLength,
		annCanonicalTemplate: templateRefMaxLength, annAutoResume: maxAutoResumeJSONBytes,
		annNetwork: maxAnnotationJSONBytes, annEnvVars: maxAnnotationJSONBytes,
		annMetadata: maxAnnotationJSONBytes, annCustomParams: maxAnnotationJSONBytes,
	}
	for key, limit := range limits {
		if len(strings.TrimSpace(annotations[key])) > limit {
			return status.Errorf(codes.InvalidArgument, "%s exceeds %d bytes", key, limit)
		}
	}
	return nil
}

func decodeStrictJSON(field, raw string, dst any) error {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return status.Errorf(codes.InvalidArgument, "%s must be a JSON object", field)
	}
	if err := validateJSONNoDuplicateKeys(raw); err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid %s JSON: %v", field, err)
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(dst); err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid %s JSON: %v", field, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return status.Errorf(codes.InvalidArgument, "invalid %s JSON: trailing data", field)
		}
		return status.Errorf(codes.InvalidArgument, "invalid %s JSON: %v", field, err)
	}
	return nil
}

func validateJSONNoDuplicateKeys(raw string) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if err := walkJSONValue(decoder); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("trailing data")
		}
		return err
	}
	return nil
}

func walkJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate key %q", key)
			}
			seen[key] = struct{}{}
			if err := walkJSONValue(decoder); err != nil {
				return err
			}
		}
		if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
			return errors.New("object is not closed")
		}
	case '[':
		for decoder.More() {
			if err := walkJSONValue(decoder); err != nil {
				return err
			}
		}
		if end, err := decoder.Token(); err != nil || end != json.Delim(']') {
			return errors.New("array is not closed")
		}
	default:
		return fmt.Errorf("unexpected delimiter %q", delim)
	}
	return nil
}

func rejectSensitiveJSON(field, raw string) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid %s JSON: %v", field, err)
	}
	return walkSensitiveJSON(field, "", value)
}

func walkSensitiveJSON(field, path string, value any) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if sensitiveKey(key) {
				return status.Errorf(codes.InvalidArgument, "%s key %q contains credential material; use AccessDescriptor instead", field, key)
			}
			childPath := key
			if path != "" {
				childPath = path + "." + key
			}
			if err := walkSensitiveJSON(field, childPath, child); err != nil {
				return err
			}
		}
	case []any:
		for index, child := range typed {
			if err := walkSensitiveJSON(field, fmt.Sprintf("%s[%d]", path, index), child); err != nil {
				return err
			}
		}
	case string:
		if sensitiveValue(typed) {
			return status.Errorf(codes.InvalidArgument, "%s field %q contains credential material; use AccessDescriptor instead", field, path)
		}
	}
	return nil
}

func sensitiveKey(key string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	for _, marker := range []string{"password", "passwd", "token", "secret", "api_key", "apikey", "private_key", "credential"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func sensitiveValue(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	if strings.HasPrefix(lower, "bearer ") {
		return true
	}
	return strings.Contains(lower, "-----begin") && strings.Contains(lower, "private key-----")
}

func parseStringMapAnnotation(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if len(raw) > maxAnnotationJSONBytes {
		return nil, fmt.Errorf("JSON exceeds %d bytes", maxAnnotationJSONBytes)
	}
	var values map[string]string
	if err := decodeStrictJSON("string map", raw, &values); err != nil {
		return nil, err
	}
	if len(values) > maxStringMapEntries {
		return nil, fmt.Errorf("map exceeds %d entries", maxStringMapEntries)
	}
	return cloneStringMap(values), nil
}

func parseNetworkAnnotation(raw string) (*runtimeapi.NetworkPolicy, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if len(raw) > maxAnnotationJSONBytes {
		return nil, fmt.Errorf("JSON exceeds %d bytes", maxAnnotationJSONBytes)
	}
	var value networkAnnotation
	if err := decodeStrictJSON(annNetwork, raw, &value); err != nil {
		return nil, err
	}
	return &runtimeapi.NetworkPolicy{
		AllowedCidrs:       append([]string(nil), value.AllowedCIDRs...),
		DeniedCidrs:        append([]string(nil), value.DeniedCIDRs...),
		AllowedDomains:     append([]string(nil), value.AllowedDomains...),
		AllowPublicTraffic: value.AllowPublic,
		MaskRequestHost:    value.MaskRequestHost,
	}, nil
}

func parseInt64Annotation(annotations map[string]string, key string) (int64, error) {
	raw := strings.TrimSpace(annotations[key])
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, status.Errorf(codes.InvalidArgument, "%s must be a non-negative integer", key)
	}
	return value, nil
}

func parseUint64Annotation(annotations map[string]string, key string) (uint64, error) {
	raw := strings.TrimSpace(annotations[key])
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, status.Errorf(codes.InvalidArgument, "%s must be a non-negative integer", key)
	}
	return value, nil
}

func parseOptionalBoolAnnotation(annotations map[string]string, key string) (*bool, error) {
	raw, ok := annotations[key]
	if !ok || strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	value, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid %s: %v", key, err)
	}
	return &value, nil
}

func parseTimeout(raw string) (uint64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultCRIOperationTimeoutSeconds, nil
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || value == 0 || value > maxCRIOperationTimeoutSeconds {
		return 0, status.Errorf(codes.InvalidArgument, "%s must be between 1 and %d", annTimeoutSeconds, maxCRIOperationTimeoutSeconds)
	}
	return value, nil
}

func unimplemented(method string) error {
	return status.Errorf(codes.Unimplemented, "AgentENV CRI method %s is not implemented", method)
}

func (e *Engine) GetContainerEvents(context.Context, *runtime.GetEventsRequest, ...grpc.CallOption) (runtime.RuntimeService_GetContainerEventsClient, error) {
	return nil, unimplemented("GetContainerEvents")
}
