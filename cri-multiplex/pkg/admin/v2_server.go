package admin

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/klog/v2"

	adminv2 "github.com/cri-multiplex/pkg/admin/v2"
	"github.com/cri-multiplex/pkg/engine"
)

const (
	// defaultV2OpTimeout 与 v1 的 pause/checkpoint 默认值（60s）对齐。
	defaultV2OpTimeout = 60 * time.Second
	// maxV2OpTimeout 是 timeout_seconds 的上限。
	maxV2OpTimeout = 300 * time.Second
)

// V2Server 实现 v2 两个 admin service：只做协议转换、backend 解析、target 校验与
// 能力分发，不持有后端状态。
type V2Server struct {
	adminv2.UnimplementedSandboxServiceServer
	adminv2.UnimplementedSandboxAdminServiceServer

	reg    *Registry
	routes *engine.RouteTable
}

// NewV2Server 构建 v2 分发器；routes 可为 nil（不做 CRI ID 归属校验）。
func NewV2Server(reg *Registry, routes *engine.RouteTable) *V2Server {
	return &V2Server{reg: reg, routes: routes}
}

// ---- 分发与校验 ----

// moduleFor 按名字（空 = e2b）解析后端；未注册返回 NotFound。
func (s *V2Server) moduleFor(backend string) (*engine.BackendModule, error) {
	mod, ok := s.reg.Resolve(backend)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "backend %q is not registered", backend)
	}
	return mod, nil
}

func (s *V2Server) lifecycleFor(mod *engine.BackendModule) (engine.AdminLifecycle, error) {
	cap, ok := mod.Engine.(engine.AdminLifecycle)
	if !ok {
		return nil, status.Errorf(codes.Unimplemented, "backend %q does not implement admin lifecycle", mod.Descriptor.Type)
	}
	return cap, nil
}

func (s *V2Server) pauserFor(mod *engine.BackendModule) (engine.AdminPauser, error) {
	cap, ok := mod.Engine.(engine.AdminPauser)
	if !ok {
		return nil, status.Errorf(codes.Unimplemented, "backend %q does not support pause", mod.Descriptor.Type)
	}
	return cap, nil
}

func (s *V2Server) checkpointerFor(mod *engine.BackendModule) (engine.AdminCheckpointer, error) {
	cap, ok := mod.Engine.(engine.AdminCheckpointer)
	if !ok {
		return nil, status.Errorf(codes.Unimplemented, "backend %q does not support checkpoint", mod.Descriptor.Type)
	}
	return cap, nil
}

func (s *V2Server) describerFor(mod *engine.BackendModule) (engine.AdminRuntimeDescriber, error) {
	cap, ok := mod.Engine.(engine.AdminRuntimeDescriber)
	if !ok {
		return nil, status.Errorf(codes.Unimplemented, "backend %q does not describe runtime facts", mod.Descriptor.Type)
	}
	return cap, nil
}

// checkCRIOwner 在给了 CRI ID 且路由存在时校验归属：路由命中别的后端返回
// FailedPrecondition；路由未命中（agent-direct 沙箱）不报错，交给引擎判断。
func (s *V2Server) checkCRIOwner(mod *engine.BackendModule, criSandboxID string) error {
	if criSandboxID == "" || s.routes == nil {
		return nil
	}
	owner, ok := s.routes.PodBackend(criSandboxID)
	if !ok {
		return nil
	}
	if owner != mod.Descriptor.Type {
		return status.Errorf(codes.FailedPrecondition,
			"sandbox %s is owned by backend %q, not %q", criSandboxID, owner, mod.Descriptor.Type)
	}
	return nil
}

// clampTimeout 把 timeout_seconds 收敛到 [60s, 300s]。
func clampTimeout(seconds uint64) time.Duration {
	switch {
	case seconds == 0:
		return defaultV2OpTimeout
	case time.Duration(seconds)*time.Second > maxV2OpTimeout:
		return maxV2OpTimeout
	default:
		return time.Duration(seconds) * time.Second
	}
}

// ---- SandboxService ----

func (s *V2Server) Create(ctx context.Context, req *adminv2.SandboxCreateRequest) (*adminv2.SandboxCreateResponse, error) {
	mod, err := s.moduleFor(req.GetBackend())
	if err != nil {
		return nil, err
	}
	lifecycle, err := s.lifecycleFor(mod)
	if err != nil {
		return nil, err
	}
	spec := sandboxSpecFromProto(req.GetSandbox())
	if spec.SandboxID == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox.sandbox_id is required")
	}
	if err := s.checkCRIOwner(mod, spec.SandboxID); err != nil {
		return nil, err
	}
	if t := req.GetStartTime(); t != nil {
		ts := t.AsTime()
		spec.StartTime = &ts
	}
	if t := req.GetEndTime(); t != nil {
		ts := t.AsTime()
		spec.EndTime = &ts
	}
	result, err := lifecycle.AdminCreateSandbox(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &adminv2.SandboxCreateResponse{
		Backend:   string(mod.Descriptor.Type),
		SandboxId: result.SandboxID,
		ClientId:  result.ClientID,
		HostIp:    result.HostIP,
	}, nil
}

func (s *V2Server) Update(ctx context.Context, req *adminv2.SandboxUpdateRequest) (*emptypb.Empty, error) {
	mod, err := s.moduleFor(req.GetBackend())
	if err != nil {
		return nil, err
	}
	lifecycle, err := s.lifecycleFor(mod)
	if err != nil {
		return nil, err
	}
	if req.GetSandboxId() == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox_id is required")
	}
	upd := engine.SandboxUpdate{}
	if t := req.GetEndTime(); t != nil {
		ts := t.AsTime()
		upd.EndTime = &ts
	}
	if egress := req.GetEgress(); egress != nil {
		upd.Egress = networkPolicyFromProto(egress)
	}
	if err := lifecycle.AdminUpdateSandbox(ctx, req.GetSandboxId(), upd); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (s *V2Server) List(ctx context.Context, req *adminv2.SandboxListRequest) (*adminv2.SandboxListResponse, error) {
	mod, err := s.moduleFor(req.GetBackend())
	if err != nil {
		return nil, err
	}
	lifecycle, err := s.lifecycleFor(mod)
	if err != nil {
		return nil, err
	}
	runtimes, err := lifecycle.AdminListSandboxes(ctx)
	if err != nil {
		return nil, err
	}
	resp := &adminv2.SandboxListResponse{}
	for _, rt := range runtimes {
		resp.Sandboxes = append(resp.Sandboxes, sandboxToProto(rt))
	}
	return resp, nil
}

func (s *V2Server) Delete(ctx context.Context, req *adminv2.SandboxDeleteRequest) (*emptypb.Empty, error) {
	mod, err := s.moduleFor(req.GetBackend())
	if err != nil {
		return nil, err
	}
	lifecycle, err := s.lifecycleFor(mod)
	if err != nil {
		return nil, err
	}
	if req.GetSandboxId() == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox_id is required")
	}
	if err := s.checkCRIOwner(mod, req.GetSandboxId()); err != nil {
		return nil, err
	}
	if err := lifecycle.AdminDeleteSandbox(ctx, req.GetSandboxId()); err != nil {
		// 幂等：不存在视为成功。
		if status.Code(err) == codes.NotFound {
			return &emptypb.Empty{}, nil
		}
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (s *V2Server) ListCachedBuilds(ctx context.Context, req *adminv2.SandboxListCachedBuildsRequest) (*adminv2.SandboxListCachedBuildsResponse, error) {
	mod, err := s.moduleFor(req.GetBackend())
	if err != nil {
		return nil, err
	}
	lifecycle, err := s.lifecycleFor(mod)
	if err != nil {
		return nil, err
	}
	builds, err := lifecycle.AdminListCachedBuilds(ctx)
	if err != nil {
		return nil, err
	}
	resp := &adminv2.SandboxListCachedBuildsResponse{}
	for _, b := range builds {
		resp.Builds = append(resp.Builds, &adminv2.CachedBuild{
			BuildId:        b.BuildID,
			ExpirationTime: timestamppb.New(b.ExpirationTime),
		})
	}
	return resp, nil
}

// ---- SandboxAdminService ----

func (s *V2Server) PauseSandbox(ctx context.Context, req *adminv2.PauseSandboxRequest) (*adminv2.PauseSandboxResponse, error) {
	started := time.Now()
	mod, err := s.moduleFor(req.GetBackend())
	if err != nil {
		return nil, err
	}
	pauser, err := s.pauserFor(mod)
	if err != nil {
		return nil, err
	}
	ref, err := sandboxRefFromTarget(req.GetCriSandboxId(), req.GetSandboxId())
	if err != nil {
		s.logAdminOp(mod, "pause", req.GetOperationId(), ref, err, started)
		return nil, err
	}
	if err := s.checkCRIOwner(mod, ref.CRISandboxID); err != nil {
		s.logAdminOp(mod, "pause", req.GetOperationId(), ref, err, started)
		return nil, err
	}
	result, err := pauser.AdminPauseSandbox(ctx, ref, engine.AdminOpOptions{
		OperationID: req.GetOperationId(),
		Timeout:     clampTimeout(req.GetTimeoutSeconds()),
		Metadata:    req.GetMetadata(),
	})
	s.logAdminOp(mod, "pause", req.GetOperationId(), ref, err, started)
	if err != nil {
		return nil, err
	}
	return &adminv2.PauseSandboxResponse{
		SnapshotPersisted: result.SnapshotPersisted,
		StartedAtUnix:     result.StartedAt.Unix(),
		FinishedAtUnix:    result.FinishedAt.Unix(),
	}, nil
}

func (s *V2Server) CheckpointSandbox(ctx context.Context, req *adminv2.CheckpointSandboxRequest) (*adminv2.CheckpointSandboxResponse, error) {
	started := time.Now()
	mod, err := s.moduleFor(req.GetBackend())
	if err != nil {
		return nil, err
	}
	checkpointer, err := s.checkpointerFor(mod)
	if err != nil {
		return nil, err
	}
	ref, err := sandboxRefFromTarget(req.GetCriSandboxId(), req.GetSandboxId())
	if err != nil {
		s.logAdminOp(mod, "checkpoint", req.GetOperationId(), ref, err, started)
		return nil, err
	}
	if err := s.checkCRIOwner(mod, ref.CRISandboxID); err != nil {
		s.logAdminOp(mod, "checkpoint", req.GetOperationId(), ref, err, started)
		return nil, err
	}
	result, err := checkpointer.AdminCheckpointSandbox(ctx, ref, engine.AdminOpOptions{
		OperationID: req.GetOperationId(),
		Timeout:     clampTimeout(req.GetTimeoutSeconds()),
		Metadata:    req.GetMetadata(),
	})
	s.logAdminOp(mod, "checkpoint", req.GetOperationId(), ref, err, started)
	if err != nil {
		return nil, err
	}
	return &adminv2.CheckpointSandboxResponse{
		SnapshotPersisted: result.SnapshotPersisted,
		VmResumed:         result.VMResumed,
		StartedAtUnix:     result.StartedAt.Unix(),
		FinishedAtUnix:    result.FinishedAt.Unix(),
	}, nil
}

func (s *V2Server) GetSandboxRuntime(ctx context.Context, req *adminv2.GetSandboxRuntimeRequest) (*adminv2.GetSandboxRuntimeResponse, error) {
	mod, err := s.moduleFor(req.GetBackend())
	if err != nil {
		return nil, err
	}
	describer, err := s.describerFor(mod)
	if err != nil {
		return nil, err
	}
	ref, err := sandboxRefFromTarget(req.GetCriSandboxId(), req.GetSandboxId())
	if err != nil {
		return nil, err
	}
	if err := s.checkCRIOwner(mod, ref.CRISandboxID); err != nil {
		return nil, err
	}
	rt, err := describer.AdminGetSandboxRuntime(ctx, ref)
	if err != nil {
		return nil, err
	}
	return &adminv2.GetSandboxRuntimeResponse{Runtime: runtimeToProto(rt)}, nil
}

// logAdminOp 输出审计日志：不记录 metadata（可能含 token）。
func (s *V2Server) logAdminOp(mod *engine.BackendModule, action, operationID string, ref engine.SandboxRef, err error, started time.Time) {
	result := "ok"
	if err != nil {
		result = status.Code(err).String()
	}
	target := ref.CRISandboxID
	if target == "" {
		target = ref.SandboxID
	}
	backend := ""
	if mod != nil {
		backend = string(mod.Descriptor.Type)
	}
	klog.InfoS("admin v2 operation",
		"backend", backend,
		"action", action,
		"operation_id", operationID,
		"target", target,
		"result", result,
		"duration_ms", time.Since(started).Milliseconds(),
	)
}

// ---- proto <-> DTO 映射 ----

func sandboxRefFromTarget(criSandboxID, sandboxID string) (engine.SandboxRef, error) {
	if criSandboxID == "" && sandboxID == "" {
		return engine.SandboxRef{}, status.Error(codes.InvalidArgument, "cri_sandbox_id or sandbox_id is required")
	}
	return engine.SandboxRef{CRISandboxID: criSandboxID, SandboxID: sandboxID}, nil
}

func sandboxSpecFromProto(p *adminv2.SandboxSpec) engine.SandboxSpec {
	if p == nil {
		return engine.SandboxSpec{}
	}
	spec := engine.SandboxSpec{
		SandboxID:             p.GetSandboxId(),
		TemplateID:            p.GetTemplateId(),
		BuildID:               p.GetBuildId(),
		Image:                 p.GetImage(),
		Alias:                 p.GetAlias(),
		VCPU:                  p.GetVcpu(),
		RamMB:                 p.GetRamMb(),
		DiskMB:                p.GetDiskMb(),
		MaxSandboxLengthHours: p.GetMaxSandboxLengthHours(),
		EnvVars:               p.GetEnvVars(),
		Metadata:              p.GetMetadata(),
		Extra:                 p.GetExtra(),
		Network:               networkPolicyFromProto(p.GetNetwork()),
	}
	if p.AllowInternet != nil {
		allow := p.GetAllowInternet()
		spec.AllowInternet = &allow
	}
	for _, m := range p.GetVolumeMounts() {
		spec.VolumeMounts = append(spec.VolumeMounts, engine.VolumeMount{
			ID:   m.GetId(),
			Name: m.GetName(),
			Path: m.GetPath(),
			Type: m.GetType(),
		})
	}
	return spec
}

func sandboxSpecToProto(spec engine.SandboxSpec) *adminv2.SandboxSpec {
	p := &adminv2.SandboxSpec{
		SandboxId:             spec.SandboxID,
		TemplateId:            spec.TemplateID,
		BuildId:               spec.BuildID,
		Image:                 spec.Image,
		Alias:                 spec.Alias,
		Vcpu:                  spec.VCPU,
		RamMb:                 spec.RamMB,
		DiskMb:                spec.DiskMB,
		MaxSandboxLengthHours: spec.MaxSandboxLengthHours,
		AllowInternet:         spec.AllowInternet,
		EnvVars:               spec.EnvVars,
		Metadata:              spec.Metadata,
		Extra:                 spec.Extra,
		Network:               networkPolicyToProto(spec.Network),
	}
	for _, m := range spec.VolumeMounts {
		p.VolumeMounts = append(p.VolumeMounts, &adminv2.VolumeMount{
			Id:   m.ID,
			Name: m.Name,
			Path: m.Path,
			Type: m.Type,
		})
	}
	return p
}

func sandboxToProto(rt *engine.SandboxRuntime) *adminv2.Sandbox {
	p := &adminv2.Sandbox{
		Backend:      rt.Backend,
		RuntimeState: rt.State,
		PodIp:        rt.PodIP,
		HostIp:       rt.HostIP,
		Spec: sandboxSpecToProto(engine.SandboxSpec{
			SandboxID:  rt.SandboxID,
			TemplateID: rt.TemplateID,
			BuildID:    rt.BuildID,
			StartTime:  rt.StartTime,
			EndTime:    rt.EndTime,
		}),
	}
	if rt.StartTime != nil {
		p.StartTime = timestamppb.New(*rt.StartTime)
	}
	if rt.EndTime != nil {
		p.EndTime = timestamppb.New(*rt.EndTime)
	}
	return p
}

func networkPolicyFromProto(p *adminv2.NetworkPolicy) *engine.NetworkPolicy {
	if p == nil {
		return nil
	}
	return &engine.NetworkPolicy{
		AllowedCIDRs:       p.GetAllowedCidrs(),
		DeniedCIDRs:        p.GetDeniedCidrs(),
		AllowedDomains:     p.GetAllowedDomains(),
		TrafficAccessToken: p.GetTrafficAccessToken(),
		MaskRequestHost:    p.MaskRequestHost,
	}
}

func networkPolicyToProto(p *engine.NetworkPolicy) *adminv2.NetworkPolicy {
	if p == nil {
		return nil
	}
	return &adminv2.NetworkPolicy{
		AllowedCidrs:       p.AllowedCIDRs,
		DeniedCidrs:        p.DeniedCIDRs,
		AllowedDomains:     p.AllowedDomains,
		TrafficAccessToken: p.TrafficAccessToken,
		MaskRequestHost:    p.MaskRequestHost,
	}
}

func runtimeToProto(rt *engine.SandboxRuntime) *adminv2.Runtime {
	if rt == nil {
		return nil
	}
	p := &adminv2.Runtime{
		Backend:      rt.Backend,
		CriSandboxId: rt.CRISandboxID,
		SandboxId:    rt.SandboxID,
		RuntimeState: rt.State,
		PodName:      rt.PodName,
		PodNamespace: rt.PodNamespace,
		PodUid:       rt.PodUID,
		NodeName:     rt.NodeName,
		PodIp:        rt.PodIP,
		HostIp:       rt.HostIP,
		NetnsPath:    rt.NetNSPath,
		Extra:        rt.Extra,
		TemplateId:   rt.TemplateID,
		BuildId:      rt.BuildID,
	}
	for _, hp := range rt.HostPorts {
		p.Hostports = append(p.Hostports, &adminv2.HostPort{
			HostPort:    uint32(hp.HostPort),
			SandboxPort: uint32(hp.SandboxPort),
		})
	}
	for _, op := range rt.ActiveOps {
		p.ActiveOperations = append(p.ActiveOperations, &adminv2.Operation{
			OperationId:    op.OperationID,
			Action:         op.Action,
			State:          op.State,
			Error:          op.Error,
			StartedAtUnix:  op.StartedAt.Unix(),
			FinishedAtUnix: op.FinishedAt.Unix(),
		})
	}
	return p
}
