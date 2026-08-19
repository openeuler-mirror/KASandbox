package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func TestValidateAnnotations(t *testing.T) {
	e := &grpcE2BEngine{}
	anns := map[string]string{
		annTemplateID: "tmpl",
		annBuildID:    "build",
		annTeamID:     "team",
	}
	template, build, team, err := e.validateAnnotations(anns)
	if err != nil {
		t.Fatalf("validateAnnotations: %v", err)
	}
	if template != "tmpl" || build != "build" || team != "team" {
		t.Fatalf("validateAnnotations = (%q, %q, %q)", template, build, team)
	}

	delete(anns, annTeamID)
	if _, _, _, err := e.validateAnnotations(anns); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing annotation error code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestAnnotationsToSandboxConfig(t *testing.T) {
	e := &grpcE2BEngine{}
	envdToken := "token-a"
	cfg, err := e.annotationsToSandboxConfig(map[string]string{
		annTemplateID:         "tmpl",
		annBuildID:            "build",
		annTeamID:             "team",
		annVCPU:               "4",
		annRAMMB:              "8192",
		annEnvdVersion:        "0.5.3",
		annMaxSandboxLength:   "12",
		annAllowInternet:      "true",
		annKernelVersion:      "kernel-a",
		annFirecrackerVersion: "fc-a",
		annTotalDiskSizeMB:    "1024",
		annHugePages:          "true",
		annAutoPause:          "true",
		annSnapshot:           "true",
		annBaseTemplateID:     "base",
		annExecutionID:        "exec",
		annEnvdAccessToken:    envdToken,
		annEnvVars:            `{"A":"B"}`,
	}, "sandbox-a", "alias-a", map[string]string{"l": "v"})
	if err != nil {
		t.Fatalf("annotationsToSandboxConfig: %v", err)
	}

	if cfg.SandboxId != "sandbox-a" || strDeref(cfg.Alias) != "alias-a" {
		t.Fatalf("identity mismatch: %+v", cfg)
	}
	if cfg.TemplateId != "tmpl" || cfg.BuildId != "build" || cfg.TeamId != "team" {
		t.Fatalf("template/build/team mismatch: %+v", cfg)
	}
	if cfg.Vcpu != 4 || cfg.RamMb != 8192 || cfg.MaxSandboxLength != 12 || cfg.TotalDiskSizeMb != 1024 {
		t.Fatalf("numeric fields mismatch: %+v", cfg)
	}
	if cfg.AllowInternetAccess == nil || !*cfg.AllowInternetAccess || !cfg.HugePages || !cfg.AutoPause || !cfg.Snapshot {
		t.Fatalf("bool fields mismatch: %+v", cfg)
	}
	if cfg.EnvdAccessToken == nil || *cfg.EnvdAccessToken != envdToken {
		t.Fatalf("envd token mismatch: %+v", cfg.EnvdAccessToken)
	}
	if cfg.EnvVars["A"] != "B" || cfg.Metadata["l"] != "v" {
		t.Fatalf("maps mismatch: env=%v metadata=%v", cfg.EnvVars, cfg.Metadata)
	}
}

func TestAnnotationsToSandboxConfigUsesDefaultsWhenOptionalAnnotationsOmitted(t *testing.T) {
	e := &grpcE2BEngine{}
	cfg, err := e.annotationsToSandboxConfig(map[string]string{
		annTemplateID: "tmpl",
		annBuildID:    "build",
		annTeamID:     "team",
	}, "sandbox-a", "alias-a", map[string]string{"app": "e2b"})
	if err != nil {
		t.Fatalf("annotationsToSandboxConfig: %v", err)
	}

	if cfg.TemplateId != "tmpl" || cfg.BuildId != "build" || cfg.TeamId != "team" {
		t.Fatalf("required fields mismatch: %+v", cfg)
	}
	if cfg.Vcpu != defaultSandboxConfig.VCPU ||
		cfg.RamMb != defaultSandboxConfig.RAMMB ||
		cfg.EnvdVersion != defaultSandboxConfig.EnvdVersion ||
		cfg.MaxSandboxLength != defaultSandboxConfig.MaxSandboxLength {
		t.Fatalf("optional numeric/string defaults mismatch: %+v", cfg)
	}
	if cfg.AllowInternetAccess == nil || !*cfg.AllowInternetAccess {
		t.Fatalf("allow internet should default to true when annotation is omitted: %+v", cfg.AllowInternetAccess)
	}
	if cfg.HugePages || cfg.AutoPause || cfg.Snapshot || cfg.TotalDiskSizeMb != 0 ||
		cfg.BaseTemplateId != "" || cfg.ExecutionId != "" || cfg.EnvdAccessToken != nil {
		t.Fatalf("optional zero-value defaults mismatch: %+v", cfg)
	}
	if cfg.KernelVersion != "vmlinux-6.1.158" || cfg.FirecrackerVersion != "v1.13.1" {
		t.Fatalf("runtime version defaults mismatch: %+v", cfg)
	}
	if cfg.Metadata["app"] != "e2b" || strDeref(cfg.Alias) != "alias-a" || cfg.SandboxId != "sandbox-a" {
		t.Fatalf("identity metadata mismatch: %+v", cfg)
	}
}

func TestAnnotationsToSandboxConfigAllowsExplicitFalseBool(t *testing.T) {
	e := &grpcE2BEngine{}
	cfg, err := e.annotationsToSandboxConfig(map[string]string{
		annAllowInternet: "false",
	}, "sandbox-a", "alias-a", nil)
	if err != nil {
		t.Fatalf("annotationsToSandboxConfig: %v", err)
	}

	if cfg.AllowInternetAccess == nil || *cfg.AllowInternetAccess {
		t.Fatalf("allow internet false annotation should set explicit false: %+v", cfg.AllowInternetAccess)
	}
}

func TestAnnotationsToSandboxConfigRejectsInvalidValues(t *testing.T) {
	e := &grpcE2BEngine{}
	tests := []struct {
		name string
		anns map[string]string
	}{
		{name: "vcpu integer", anns: map[string]string{annVCPU: "bad"}},
		{name: "ram integer", anns: map[string]string{annRAMMB: "bad"}},
		{name: "max sandbox length integer", anns: map[string]string{annMaxSandboxLength: "bad"}},
		{name: "total disk size integer", anns: map[string]string{annTotalDiskSizeMB: "bad"}},
		{name: "vcpu zero", anns: map[string]string{annVCPU: "0"}},
		{name: "ram negative", anns: map[string]string{annRAMMB: "-1"}},
		{name: "allow internet boolean", anns: map[string]string{annAllowInternet: "invalid"}},
		{name: "allow internet numeric boolean", anns: map[string]string{annAllowInternet: "1"}},
		{name: "huge pages boolean", anns: map[string]string{annHugePages: "invalid"}},
		{name: "auto pause boolean", anns: map[string]string{annAutoPause: "invalid"}},
		{name: "snapshot boolean", anns: map[string]string{annSnapshot: "invalid"}},
		{name: "auto resume json", anns: map[string]string{annAutoResume: "{bad"}},
		{name: "auto resume empty policy", anns: map[string]string{annAutoResume: `{}`}},
		{name: "auto resume invalid policy", anns: map[string]string{annAutoResume: `{"policy":"garbage"}`}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := e.annotationsToSandboxConfig(tt.anns, "sandbox-a", "alias-a", nil)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("invalid annotation error code = %v, want InvalidArgument (err=%v)", status.Code(err), err)
			}
		})
	}
}

func TestAnnotationsToSandboxConfigDoesNotValidateDeferredJSONAnnotations(t *testing.T) {
	e := &grpcE2BEngine{}
	cfg, err := e.annotationsToSandboxConfig(map[string]string{
		annEnvVars:      "{bad",
		annNetwork:      "{bad",
		annVolumeMounts: "{bad",
	}, "sandbox-a", "alias-a", nil)
	if err != nil {
		t.Fatalf("annotationsToSandboxConfig: %v", err)
	}
	if cfg.EnvVars != nil || cfg.Network != nil || cfg.VolumeMounts != nil {
		t.Fatalf("invalid deferred JSON annotations should be ignored: env=%v network=%v mounts=%v", cfg.EnvVars, cfg.Network, cfg.VolumeMounts)
	}
}

func TestAnnotationsToSandboxConfigValidatesAutoResumePolicy(t *testing.T) {
	e := &grpcE2BEngine{}
	for _, policy := range []string{"any", "off"} {
		t.Run(policy, func(t *testing.T) {
			cfg, err := e.annotationsToSandboxConfig(map[string]string{
				annAutoResume: `{"policy":"` + policy + `"}`,
			}, "sandbox-a", "alias-a", nil)
			if err != nil {
				t.Fatalf("annotationsToSandboxConfig: %v", err)
			}
			if cfg.AutoResume == nil || cfg.AutoResume.Policy != policy {
				t.Fatalf("auto resume policy = %+v, want %q", cfg.AutoResume, policy)
			}
		})
	}
}

func TestE2BIDAndStateHelpers(t *testing.T) {
	if got := stripContainerSuffix("sandbox-c"); got != "sandbox" {
		t.Fatalf("stripContainerSuffix = %q", got)
	}
	if got := stripContainerSuffix("sandbox"); got != "sandbox" {
		t.Fatalf("stripContainerSuffix without suffix = %q", got)
	}
	if got := e2bSandboxIDFromCRI("ABC-123_xyz"); got != "abc123xyz" {
		t.Fatalf("e2bSandboxIDFromCRI cleaned = %q", got)
	}
	if got := e2bSandboxIDFromCRI("short"); len(got) != 32 {
		t.Fatalf("short id should be hashed to 32 chars, got %q", got)
	}
	if inferPodSandboxState(stateRunning) != runtime.PodSandboxState_SANDBOX_READY {
		t.Fatal("running pod should map to SANDBOX_READY")
	}
	if inferContainerState(containerStateRunning) != runtime.ContainerState_CONTAINER_RUNNING {
		t.Fatal("running container should map to CONTAINER_RUNNING")
	}
}

func TestContainerTimeHelpers(t *testing.T) {
	created := time.Unix(1, 2)
	started := time.Unix(3, 4)
	finished := time.Unix(5, 6)
	pod := &podInfo{
		createdAt:           created,
		containerStartedAt:  started,
		containerFinishedAt: finished,
	}
	if got := containerCreatedAt(pod); got != created.UnixNano() {
		t.Fatalf("containerCreatedAt fallback = %d", got)
	}
	pod.containerCreatedAt = time.Unix(7, 8)
	if got := containerCreatedAt(pod); got != pod.containerCreatedAt.UnixNano() {
		t.Fatalf("containerCreatedAt explicit = %d", got)
	}
	if got := containerStartedAt(pod); got != started.UnixNano() {
		t.Fatalf("containerStartedAt = %d", got)
	}
	if got := containerFinishedAt(pod); got != finished.UnixNano() {
		t.Fatalf("containerFinishedAt = %d", got)
	}
}

func TestParseE2BImageRef(t *testing.T) {
	template, build, err := parseE2BImageRef("e2b.dev/tmpl:build")
	if err != nil {
		t.Fatalf("parseE2BImageRef: %v", err)
	}
	if template != "tmpl" || build != "build" {
		t.Fatalf("parseE2BImageRef = (%q, %q)", template, build)
	}
	if _, _, err := parseE2BImageRef("busybox"); err == nil {
		t.Fatal("expected non-e2b image error")
	}
	if _, _, err := parseE2BImageRef("e2b.dev/tmpl"); err == nil {
		t.Fatal("expected invalid e2b image format error")
	}
}

func TestFilterPodSandboxAndContainers(t *testing.T) {
	pods := []*runtime.PodSandbox{
		{Id: "a", State: runtime.PodSandboxState_SANDBOX_READY, Labels: map[string]string{"app": "a"}},
		{Id: "b", State: runtime.PodSandboxState_SANDBOX_NOTREADY, Labels: map[string]string{"app": "b"}},
	}
	filteredPods := filterPodSandbox(pods, &runtime.PodSandboxFilter{
		State:         &runtime.PodSandboxStateValue{State: runtime.PodSandboxState_SANDBOX_READY},
		LabelSelector: map[string]string{"app": "a"},
	})
	if len(filteredPods) != 1 || filteredPods[0].Id != "a" {
		t.Fatalf("filtered pods = %+v", filteredPods)
	}

	containers := []*runtime.Container{
		{Id: "c1", PodSandboxId: "a", State: runtime.ContainerState_CONTAINER_RUNNING, Labels: map[string]string{"app": "a"}},
		{Id: "c2", PodSandboxId: "b", State: runtime.ContainerState_CONTAINER_EXITED, Labels: map[string]string{"app": "b"}},
	}
	filteredContainers := filterContainers(containers, &runtime.ContainerFilter{
		PodSandboxId:  "a",
		State:         &runtime.ContainerStateValue{State: runtime.ContainerState_CONTAINER_RUNNING},
		LabelSelector: map[string]string{"app": "a"},
	})
	if len(filteredContainers) != 1 || filteredContainers[0].Id != "c1" {
		t.Fatalf("filtered containers = %+v", filteredContainers)
	}
}

func TestMapE2BErrorAndNotFound(t *testing.T) {
	notFound := status.Error(codes.NotFound, "missing")
	if !isNotFound(notFound) {
		t.Fatal("isNotFound should return true for NotFound status")
	}
	if got := status.Code(mapE2BError(notFound)); got != codes.NotFound {
		t.Fatalf("mapE2BError NotFound code = %v", got)
	}
	if got := status.Code(mapE2BError(errors.New("plain"))); got != codes.Internal {
		t.Fatalf("mapE2BError plain code = %v, want Internal", got)
	}
}

func TestConnectBinaryEnvelopeRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := writeConnectBinaryEnvelope(&buf, 1, []byte("payload")); err != nil {
		t.Fatalf("writeConnectBinaryEnvelope: %v", err)
	}
	flags, payload, err := readConnectBinaryEnvelope(&buf)
	if err != nil {
		t.Fatalf("readConnectBinaryEnvelope: %v", err)
	}
	if flags != 1 || string(payload) != "payload" {
		t.Fatalf("envelope = (%d, %q)", flags, payload)
	}
}

func TestConnectBinaryEnvelopeErrors(t *testing.T) {
	if _, _, err := readConnectBinaryEnvelope(strings.NewReader("")); err != io.EOF {
		t.Fatalf("empty envelope err = %v, want EOF", err)
	}

	var header [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(header[:], uint64(16*1024*1024+1)<<2)
	if _, _, err := readConnectBinaryEnvelope(bytes.NewReader(header[:n])); err == nil {
		t.Fatal("expected too large envelope error")
	}
}

func TestGRPCFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := writeGRPCFrame(&buf, []byte("hello")); err != nil {
		t.Fatalf("writeGRPCFrame: %v", err)
	}
	compressed, payload, err := readGRPCFrame(&buf)
	if err != nil {
		t.Fatalf("readGRPCFrame: %v", err)
	}
	if compressed || string(payload) != "hello" {
		t.Fatalf("frame = (compressed=%v, payload=%q)", compressed, payload)
	}
}

func TestEnvdRequestAndAccessToken(t *testing.T) {
	if got := envdHost("sandbox-a"); got == "" || !strings.Contains(got, "sandbox-a") {
		t.Fatalf("envdHost = %q", got)
	}
	req, err := newEnvdRequest(context.Background(), "POST", "sandbox-a", "/process.Process/List", nil)
	if err != nil {
		t.Fatalf("newEnvdRequest: %v", err)
	}
	if req.Method != "POST" || req.URL.Path != "/process.Process/List" || req.Host == "" {
		t.Fatalf("request mismatch: method=%s path=%s host=%s", req.Method, req.URL.Path, req.Host)
	}

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-access-token", "token-a"))
	if got := getAccessTokenFromMetadata(ctx); got != "token-a" {
		t.Fatalf("getAccessTokenFromMetadata = %q", got)
	}
	if got := getAccessTokenFromMetadata(context.Background()); got != "" {
		t.Fatalf("missing token = %q, want empty", got)
	}
}

func TestCNIPodIP(t *testing.T) {
	if got := cniPodIP(nil); got != "" {
		t.Fatalf("cniPodIP nil = %q", got)
	}
	if got := cniPodIP(&CNIRecord{PodIP: "10.0.0.10"}); got != "10.0.0.10" {
		t.Fatalf("cniPodIP = %q", got)
	}
}

func TestHiddenFromCRIList(t *testing.T) {
	// 未配置 hide label：一切可见（修改前行为）
	e := &grpcE2BEngine{}
	pod := &podInfo{labels: map[string]string{"flux-sandbox.io/direct": "true"}}
	if e.hiddenFromCRIList(pod) {
		t.Fatal("no hide label configured, pod must be visible")
	}

	// 配置 hide label：匹配的隐藏，不匹配/无该 label 的可见
	e2 := &grpcE2BEngine{hideLabelKey: "flux-sandbox.io/direct", hideLabelValue: "true"}
	if !e2.hiddenFromCRIList(pod) {
		t.Fatal("pod with matching label must be hidden")
	}
	if e2.hiddenFromCRIList(&podInfo{labels: map[string]string{"flux-sandbox.io/direct": "false"}}) {
		t.Fatal("pod with different label value must stay visible")
	}
	if e2.hiddenFromCRIList(&podInfo{labels: map[string]string{"app": "x"}}) {
		t.Fatal("pod without the hide label must stay visible")
	}
	if e2.hiddenFromCRIList(nil) {
		t.Fatal("nil pod must stay visible")
	}
}

func TestRevealsHiddenFromCRIList(t *testing.T) {
	e := &grpcE2BEngine{}
	if e.revealsHiddenFromCRIList(map[string]string{"flux-sandbox.io/direct": "true"}) {
		t.Fatal("no hide label configured, selector must not reveal hidden pods")
	}

	e = &grpcE2BEngine{hideLabelKey: "flux-sandbox.io/direct", hideLabelValue: "true"}
	if !e.revealsHiddenFromCRIList(map[string]string{"flux-sandbox.io/direct": "true"}) {
		t.Fatal("matching selector must reveal hidden pods")
	}
	if e.revealsHiddenFromCRIList(map[string]string{"flux-sandbox.io/direct": "false"}) {
		t.Fatal("different selector value must not reveal hidden pods")
	}
	if e.revealsHiddenFromCRIList(map[string]string{"flux-sandbox.io/runtime": "e2b"}) {
		t.Fatal("runtime-only selector must not reveal hidden pods")
	}
	if e.revealsHiddenFromCRIList(nil) {
		t.Fatal("nil selector must not reveal hidden pods")
	}
}

func TestNewGRPCE2BEngineHideLabelParsing(t *testing.T) {
	e := newGRPCE2BEngine("", "", "", "", CNIConfig{}, nil, "flux-sandbox.io/direct=true")
	if e.hideLabelKey != "flux-sandbox.io/direct" || e.hideLabelValue != "true" {
		t.Fatalf("hide label parsed wrong: %q=%q", e.hideLabelKey, e.hideLabelValue)
	}
	// 非法格式：告警并禁用
	e2 := newGRPCE2BEngine("", "", "", "", CNIConfig{}, nil, "no-equals-sign")
	if e2.hideLabelKey != "" {
		t.Fatalf("invalid hide label should be disabled, got key=%q", e2.hideLabelKey)
	}
}
