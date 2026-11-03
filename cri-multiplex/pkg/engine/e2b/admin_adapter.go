package e2b

import (
	"context"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cri-multiplex/pkg/engine"
	"github.com/cri-multiplex/pkg/orchestrator"
)

// E2B 引擎完整实现 8 个中性 Admin 方法（v2 接口），v1 与 v2 都通过它们落到
// 既有的 AdminCreate/AdminPause/... 实现上。
var (
	_ engine.AdminLifecycle        = (*Engine)(nil)
	_ engine.AdminPauser           = (*Engine)(nil)
	_ engine.AdminCheckpointer     = (*Engine)(nil)
	_ engine.AdminRuntimeDescriber = (*Engine)(nil)
	_ engine.AdminEngine           = (*Engine)(nil)
)

// Extra / Metadata 中 E2B 专有字段的 key（设计 §3.3.7.2）。
const (
	extraKeyTeamID          = "team_id"
	extraKeyEnvdAccessToken = "envd_access_token"
	extraKeyKernelVersion   = "kernel_version"
	extraKeyFirecrackerVer  = "firecracker_version"
	extraKeyEnvdVersion     = "envd_version"
	extraKeyBaseTemplateID  = "base_template_id"
	extraKeyExecutionID     = "execution_id"
	extraKeyHugePages       = "huge_pages"
	extraKeySnapshot        = "snapshot"
	extraKeyAutoPause       = "auto_pause"
	extraKeyAutoResume      = "auto_resume_policy"

	metaKeyTemplateID = "template_id"
	metaKeyBuildID    = "build_id"
)

// AdminCreateSandbox 把中性 SandboxSpec 映射成 orchestrator SandboxConfig，
// 再走既有 AdminCreate 的完整生命周期。
func (e *Engine) AdminCreateSandbox(ctx context.Context, spec engine.SandboxSpec) (*engine.SandboxCreateResult, error) {
	if spec.SandboxID == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox_id is required")
	}
	cfg, err := sandboxConfigFromSpec(spec)
	if err != nil {
		return nil, err
	}
	req := &orchestrator.SandboxCreateRequest{Sandbox: cfg}
	if spec.StartTime != nil {
		req.StartTime = timestamppb.New(*spec.StartTime)
	}
	if spec.EndTime != nil {
		req.EndTime = timestamppb.New(*spec.EndTime)
	}
	resp, err := e.AdminCreate(ctx, req)
	if err != nil {
		return nil, err
	}
	return &engine.SandboxCreateResult{
		SandboxID: spec.SandboxID,
		ClientID:  resp.GetClientId(),
		HostIP:    resp.GetHostIp(),
	}, nil
}

// AdminUpdateSandbox 只设置非 nil 的 EndTime/Egress。
func (e *Engine) AdminUpdateSandbox(ctx context.Context, sandboxID string, upd engine.SandboxUpdate) error {
	if sandboxID == "" {
		return status.Error(codes.InvalidArgument, "sandbox_id is required")
	}
	req := &orchestrator.SandboxUpdateRequest{SandboxId: sandboxID}
	if upd.EndTime != nil {
		req.EndTime = timestamppb.New(*upd.EndTime)
	}
	if upd.Egress != nil {
		req.Egress = egressConfigFromPolicy(upd.Egress)
	}
	_, err := e.AdminUpdate(ctx, req)
	return err
}

// AdminListSandboxes 把 orchestrator RunningSandbox 映射成中性运行事实。
func (e *Engine) AdminListSandboxes(ctx context.Context) ([]*engine.SandboxRuntime, error) {
	resp, err := e.AdminList(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*engine.SandboxRuntime, 0, len(resp.GetSandboxes()))
	for _, sb := range resp.GetSandboxes() {
		cfg := sb.GetConfig()
		rt := &engine.SandboxRuntime{
			Backend:      string(engine.EngineTypeE2B),
			CRISandboxID: cfg.GetSandboxId(),
			SandboxID:    cfg.GetSandboxId(),
			State:        "Running",
			TemplateID:   cfg.GetTemplateId(),
			BuildID:      cfg.GetBuildId(),
			PodIP:        cfg.GetRuntimeNetwork().GetPodIp(),
			HostIP:       e.nodeIP,
		}
		if t := sb.GetStartTime(); t != nil {
			ts := t.AsTime()
			rt.StartTime = &ts
		}
		if t := sb.GetEndTime(); t != nil {
			ts := t.AsTime()
			rt.EndTime = &ts
		}
		out = append(out, rt)
	}
	return out, nil
}

// AdminDeleteSandbox 复用 AdminDelete（Stop+Remove 合一），不存在视为成功。
func (e *Engine) AdminDeleteSandbox(ctx context.Context, sandboxID string) error {
	if sandboxID == "" {
		return status.Error(codes.InvalidArgument, "sandbox_id is required")
	}
	_, err := e.AdminDelete(ctx, &orchestrator.SandboxDeleteRequest{SandboxId: sandboxID})
	if status.Code(err) == codes.NotFound {
		return nil
	}
	return err
}

// AdminListCachedBuilds 薄转发 orchestrator ListCachedBuilds，并把
// build_id / expiration_time 映射成中性结构；不触碰本地状态。
func (e *Engine) AdminListCachedBuilds(ctx context.Context) ([]engine.CachedBuild, error) {
	if err := e.ensureConn(); err != nil {
		return nil, mapE2BError(err)
	}
	resp, err := e.client.ListCachedBuilds(ctx, &emptypb.Empty{})
	if err != nil {
		return nil, mapE2BError(err)
	}
	out := make([]engine.CachedBuild, 0, len(resp.GetBuilds()))
	for _, b := range resp.GetBuilds() {
		item := engine.CachedBuild{BuildID: b.GetBuildId()}
		if t := b.GetExpirationTime(); t != nil {
			item.ExpirationTime = t.AsTime()
		}
		out = append(out, item)
	}
	return out, nil
}

// AdminPauseSandbox 把单边 SandboxRef 补成 (CRI ID, E2B ID) 后调用既有 AdminPause。
func (e *Engine) AdminPauseSandbox(ctx context.Context, ref engine.SandboxRef, opts engine.AdminOpOptions) (*engine.AdminOpResult, error) {
	pod, err := e.resolveAdminRef(ref)
	if err != nil {
		return nil, err
	}
	startedAt, finishedAt, err := e.AdminPause(ctx, adminOperationFrom(pod, opts), adminTimeoutSeconds(opts.Timeout))
	if err != nil {
		return nil, err
	}
	return &engine.AdminOpResult{SnapshotPersisted: true, StartedAt: startedAt, FinishedAt: finishedAt}, nil
}

// AdminCheckpointSandbox 同 Pause；checkpoint 返回即视为 VM 已恢复运行。
func (e *Engine) AdminCheckpointSandbox(ctx context.Context, ref engine.SandboxRef, opts engine.AdminOpOptions) (*engine.AdminOpResult, error) {
	pod, err := e.resolveAdminRef(ref)
	if err != nil {
		return nil, err
	}
	resumed, startedAt, finishedAt, err := e.AdminCheckpoint(ctx, adminOperationFrom(pod, opts), adminTimeoutSeconds(opts.Timeout))
	if err != nil {
		return nil, err
	}
	return &engine.AdminOpResult{SnapshotPersisted: true, VMResumed: resumed, StartedAt: startedAt, FinishedAt: finishedAt}, nil
}

// AdminGetSandboxRuntime 把 E2B 运行事实映射成中性视图（含端口与进行中操作）。
func (e *Engine) AdminGetSandboxRuntime(ctx context.Context, ref engine.SandboxRef) (*engine.SandboxRuntime, error) {
	pod, err := e.resolveAdminRef(ref)
	if err != nil {
		return nil, err
	}
	info, active, err := e.AdminGetRuntime(pod.sandboxID, pod.e2bSandboxID)
	if err != nil {
		return nil, err
	}
	rt := &engine.SandboxRuntime{
		Backend:      string(engine.EngineTypeE2B),
		CRISandboxID: info.CRISandboxID,
		SandboxID:    info.E2BSandboxID,
		State:        info.RuntimeState,
		TemplateID:   info.TemplateID,
		BuildID:      info.BuildID,
		PodName:      info.PodName,
		PodNamespace: info.PodNamespace,
		PodUID:       info.PodUID,
		NodeName:     info.NodeName,
		PodIP:        info.PodIP,
		HostIP:       e.nodeIP,
		NetNSPath:    info.NetNSPath,
	}
	for _, hp := range info.HostPorts {
		rt.HostPorts = append(rt.HostPorts, engine.SandboxHostPort{HostPort: hp.HostPort, SandboxPort: hp.SandboxPort})
	}
	for _, op := range active {
		rt.ActiveOps = append(rt.ActiveOps, engine.SandboxOperation{
			OperationID: op.OperationID,
			Action:      op.Action,
			State:       op.State,
			Error:       op.Error,
			StartedAt:   op.StartedAt,
			FinishedAt:  op.FinishedAt,
		})
	}
	return rt, nil
}

// resolveAdminRef 把单边 SandboxRef 解析成本节点上的 podInfo，并校验双边一致性。
func (e *Engine) resolveAdminRef(ref engine.SandboxRef) (*podInfo, error) {
	switch {
	case ref.CRISandboxID != "":
		pod, ok := e.tracker.Get(ref.CRISandboxID)
		if !ok || pod.state == stateRemoved {
			return nil, status.Errorf(codes.NotFound, "sandbox %s not found on this node", ref.CRISandboxID)
		}
		if ref.SandboxID != "" && pod.e2bSandboxID != ref.SandboxID {
			return nil, status.Errorf(codes.FailedPrecondition,
				"sandbox id mismatch: cri sandbox %s maps to e2b sandbox %q, request has %q",
				ref.CRISandboxID, pod.e2bSandboxID, ref.SandboxID)
		}
		return pod, nil
	case ref.SandboxID != "":
		pod, ok := e.tracker.GetByE2B(ref.SandboxID)
		if !ok || pod.state == stateRemoved {
			return nil, status.Errorf(codes.NotFound, "sandbox %s not found on this node", ref.SandboxID)
		}
		return pod, nil
	default:
		return nil, status.Error(codes.InvalidArgument, "cri_sandbox_id or sandbox_id is required")
	}
}

// adminOperationFrom 用 pod 身份补齐 E2BOperation；Metadata 覆盖 team/template/build，
// 缺失时沿用 pod 上的值，避免调用方必须重复传。
func adminOperationFrom(pod *podInfo, opts engine.AdminOpOptions) E2BOperation {
	pick := func(key, fallback string) string {
		if v := opts.Metadata[key]; v != "" {
			return v
		}
		return fallback
	}
	return E2BOperation{
		OperationID:  opts.OperationID,
		CRISandboxID: pod.sandboxID,
		E2BSandboxID: pod.e2bSandboxID,
		TeamID:       pick(extraKeyTeamID, pod.teamID),
		TemplateID:   pick(metaKeyTemplateID, pod.templateID),
		BuildID:      pick(metaKeyBuildID, pod.buildID),
	}
}

// adminTimeoutSeconds 把 Duration 转成秒；0 表示由引擎默认值决定。
func adminTimeoutSeconds(d time.Duration) uint64 {
	if d <= 0 {
		return 0
	}
	return uint64(d / time.Second)
}

// sandboxConfigFromSpec 是 SandboxSpec → orchestrator.SandboxConfig 的纯映射。
func sandboxConfigFromSpec(spec engine.SandboxSpec) (*orchestrator.SandboxConfig, error) {
	lookup := func(key string) string {
		if v := spec.Extra[key]; v != "" {
			return v
		}
		return spec.Metadata[key]
	}
	teamID := lookup(extraKeyTeamID)
	if teamID == "" {
		return nil, status.Error(codes.InvalidArgument, "extra.team_id (or metadata.team_id) is required")
	}
	cfg := &orchestrator.SandboxConfig{
		SandboxId:           spec.SandboxID,
		TemplateId:          spec.TemplateID,
		BuildId:             spec.BuildID,
		TeamId:              teamID,
		Vcpu:                spec.VCPU,
		RamMb:               spec.RamMB,
		TotalDiskSizeMb:     spec.DiskMB,
		MaxSandboxLength:    spec.MaxSandboxLengthHours,
		AllowInternetAccess: spec.AllowInternet,
		EnvVars:             spec.EnvVars,
		Metadata:            spec.Metadata,
		KernelVersion:       lookup(extraKeyKernelVersion),
		FirecrackerVersion:  lookup(extraKeyFirecrackerVer),
		EnvdVersion:         lookup(extraKeyEnvdVersion),
		BaseTemplateId:      lookup(extraKeyBaseTemplateID),
		ExecutionId:         lookup(extraKeyExecutionID),
		HugePages:           boolValue(lookup(extraKeyHugePages)),
		Snapshot:            boolValue(lookup(extraKeySnapshot)),
		AutoPause:           boolValue(lookup(extraKeyAutoPause)),
	}
	if spec.Alias != "" {
		alias := spec.Alias
		cfg.Alias = &alias
	}
	if token := lookup(extraKeyEnvdAccessToken); token != "" {
		cfg.EnvdAccessToken = &token
	}
	if policy := lookup(extraKeyAutoResume); policy != "" {
		cfg.AutoResume = &orchestrator.SandboxAutoResumeConfig{Policy: policy}
	}
	for _, m := range spec.VolumeMounts {
		cfg.VolumeMounts = append(cfg.VolumeMounts, &orchestrator.SandboxVolumeMount{
			Id:   m.ID,
			Name: m.Name,
			Path: m.Path,
			Type: m.Type,
		})
	}
	if spec.Network != nil {
		cfg.Network = networkConfigFromPolicy(spec.Network)
	}
	return cfg, nil
}

// boolValue 解析 extra 里的布尔字段；非法值按未设置处理。
func boolValue(raw string) bool {
	if raw == "" {
		return false
	}
	v, err := strconv.ParseBool(raw)
	return err == nil && v
}

// networkConfigFromPolicy 把中性 NetworkPolicy 映射成 orchestrator 网络配置。
func networkConfigFromPolicy(p *engine.NetworkPolicy) *orchestrator.SandboxNetworkConfig {
	cfg := &orchestrator.SandboxNetworkConfig{
		Egress: &orchestrator.SandboxNetworkEgressConfig{
			AllowedCidrs:   append([]string(nil), p.AllowedCIDRs...),
			DeniedCidrs:    append([]string(nil), p.DeniedCIDRs...),
			AllowedDomains: append([]string(nil), p.AllowedDomains...),
		},
	}
	if p.TrafficAccessToken != "" || p.MaskRequestHost != nil {
		ingress := &orchestrator.SandboxNetworkIngressConfig{}
		if p.TrafficAccessToken != "" {
			token := p.TrafficAccessToken
			ingress.TrafficAccessToken = &token
		}
		if p.MaskRequestHost != nil {
			mask := *p.MaskRequestHost
			ingress.MaskRequestHost = &mask
		}
		cfg.Ingress = ingress
	}
	return cfg
}

// egressConfigFromPolicy 只映射出网部分，用于 Update。
func egressConfigFromPolicy(p *engine.NetworkPolicy) *orchestrator.SandboxNetworkEgressConfig {
	if p == nil {
		return nil
	}
	return &orchestrator.SandboxNetworkEgressConfig{
		AllowedCidrs:   append([]string(nil), p.AllowedCIDRs...),
		DeniedCidrs:    append([]string(nil), p.DeniedCIDRs...),
		AllowedDomains: append([]string(nil), p.AllowedDomains...),
	}
}
