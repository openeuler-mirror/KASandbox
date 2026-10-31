package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/e2b/e2b-webhook/provider"
	"github.com/e2b/e2b-webhook/providers"
)

// sampleAnnotations 是测试中使用的示例 e2b 注解集合。
var sampleAnnotations = map[string]string{
	"e2b.dev/template-id": "tpl-123",
	"e2b.dev/vcpu":        "2",
	"e2b.dev/ram-mb":      "512",
	"e2b.dev/build-id":    "build-abc",
}

// =============================================================================
// 辅助构造函数
// =============================================================================

// newAdmissionReview 构造一个 AdmissionReview, 用给定 kind / 操作 / 原始对象 JSON。
func newAdmissionReview(kind string, op admissionv1.Operation, raw []byte) *admissionv1.AdmissionReview {
	return &admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID:       "test-uid-1234",
			Kind:      metav1.GroupVersionKind{Kind: kind},
			Operation: op,
			Object:    runtime.RawExtension{Raw: raw},
		},
	}
}

// newPodReview 用 marshaled Pod 构造 AdmissionReview。
func newPodReview(op admissionv1.Operation, pod *corev1.Pod) *admissionv1.AdmissionReview {
	raw, _ := json.Marshal(pod)
	return newAdmissionReview("Pod", op, raw)
}

// newBatchSandboxObj 构造一个 BatchSandbox 的 unstructured 对象。
// env 为 containers[0].env 列表 (每个元素是 map[string]interface{})。
func newBatchSandboxObj(name, ns string, env []interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "e2b.dev/v1",
			"kind":       "BatchSandbox",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": ns,
				"uid":       "bs-uid-1",
			},
			"spec": map[string]interface{}{
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name": "sandbox",
								"env":  env,
							},
						},
					},
				},
			},
		},
	}
}

// envVar 构造一个 env 条目 map。
func envVar(name, value string) map[string]interface{} {
	return map[string]interface{}{"name": name, "value": value}
}

// fakeProvider 是测试用的 Provider：handler 决定选中的后端，默认产出
// e2b.dev/template-id 注解；android 不产出注解。
type fakeProvider struct {
	handler      string
	annotations  map[string]string
	runtimeClass string
	err          error
	nilResult    bool
	calls        int
}

func (f *fakeProvider) Name() string    { return f.handler }
func (f *fakeProvider) Handler() string { return f.handler }

func (f *fakeProvider) Resolve(_ context.Context, _ provider.Request) (*provider.Result, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.nilResult {
		return nil, nil
	}
	annotations := map[string]string{}
	if f.handler != "android" {
		annotations["e2b.dev/template-id"] = "provider-template"
	}
	for key, value := range f.annotations {
		annotations[key] = value
	}
	runtimeClass := f.runtimeClass
	if runtimeClass == "" {
		runtimeClass = f.handler
	}
	return &provider.Result{Annotations: annotations, RuntimeClass: runtimeClass}, nil
}

// newFakeMatcher 为每个 handler 建一个 fakeProvider 并构造 Matcher。
func newFakeMatcher(t *testing.T, handlers ...string) *provider.Matcher {
	t.Helper()
	ps := make([]provider.Provider, 0, len(handlers))
	for _, handler := range handlers {
		ps = append(ps, &fakeProvider{handler: handler})
	}
	return newMatcherFromProviders(t, ps...)
}

func newMatcherFromProviders(t *testing.T, ps ...provider.Provider) *provider.Matcher {
	t.Helper()
	matcher, err := provider.NewMatcher(ps...)
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}
	return matcher
}

// decodePatches 把 AdmissionResponse.Patch 解成 patchOperation 列表。
func decodePatches(t *testing.T, resp *admissionv1.AdmissionResponse) []patchOperation {
	t.Helper()
	if len(resp.Patch) == 0 {
		return nil
	}
	var patches []patchOperation
	if err := json.Unmarshal(resp.Patch, &patches); err != nil {
		t.Fatalf("unmarshal patches: %v", err)
	}
	return patches
}

// patchedAnnotations 按 patch 语义把增量合并到已有注解上（用于断言）。
func patchedAnnotations(t *testing.T, basePath string, patches []patchOperation) map[string]string {
	t.Helper()
	prefix := basePath + "/"
	out := map[string]string{}
	for _, p := range patches {
		switch {
		case p.Path == basePath:
			m, ok := p.Value.(map[string]interface{})
			if !ok {
				t.Fatalf("patch %s value = %T", p.Path, p.Value)
			}
			for k, v := range m {
				out[k] = v.(string)
			}
		case strings.HasPrefix(p.Path, prefix):
			key := strings.ReplaceAll(strings.TrimPrefix(p.Path, prefix), "~1", "/")
			value, ok := p.Value.(string)
			if !ok {
				t.Fatalf("patch %s value = %T", p.Path, p.Value)
			}
			out[key] = value
		}
	}
	return out
}

// patchExists 判断是否存在指定路径的 patch。
func patchExists(patches []patchOperation, path string) bool {
	for _, p := range patches {
		if p.Path == path {
			return true
		}
	}
	return false
}

// setBatchSandboxRuntimeClass 设置 spec.template.spec.runtimeClassName。
func setBatchSandboxRuntimeClass(obj *unstructured.Unstructured, runtimeClass string) {
	spec, _ := obj.Object["spec"].(map[string]interface{})
	template, _ := spec["template"].(map[string]interface{})
	templateSpec, _ := template["spec"].(map[string]interface{})
	templateSpec["runtimeClassName"] = runtimeClass
}

// setPodRuntimeClass 设置 spec.runtimeClassName。
func setPodRuntimeClass(pod *corev1.Pod, runtimeClass string) {
	pod.Spec.RuntimeClassName = &runtimeClass
}

// =============================================================================
// escapeJSONPointer
// =============================================================================

func TestEscapeJSONPointer(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"e2b.dev/vcpu", "e2b.dev~1vcpu"},
		{"e2b.dev/template-id", "e2b.dev~1template-id"},
		{"plain", "plain"},
		{"a~b/c", "a~0b~1c"},
		{"e2b.dev/auto-resume", "e2b.dev~1auto-resume"},
		{"", ""},
	}
	for _, c := range cases {
		got := escapeJSONPointer(c.in)
		if got != c.want {
			t.Errorf("escapeJSONPointer(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// =============================================================================
// buildPatch
// =============================================================================

func TestBuildPatch_NoExistingAnnotations(t *testing.T) {
	patches := buildPatch(nil, sampleAnnotations, "/metadata/annotations")
	if len(patches) != 1 {
		t.Fatalf("expected 1 patch, got %d", len(patches))
	}
	p := patches[0]
	if p.Op != "add" {
		t.Errorf("expected op=add, got %s", p.Op)
	}
	if p.Path != "/metadata/annotations" {
		t.Errorf("expected path=/metadata/annotations, got %s", p.Path)
	}
	m, ok := p.Value.(map[string]string)
	if !ok {
		t.Fatalf("expected value to be map[string]string, got %T", p.Value)
	}
	if len(m) != len(sampleAnnotations) {
		t.Errorf("expected %d annotations, got %d", len(sampleAnnotations), len(m))
	}
	for k, v := range sampleAnnotations {
		if m[k] != v {
			t.Errorf("annotation %s = %q, want %q", k, m[k], v)
		}
	}
}

func TestBuildPatch_EmptyAnnotationsMap(t *testing.T) {
	patches := buildPatch(map[string]string{}, sampleAnnotations, "/metadata/annotations")
	if len(patches) != 1 {
		t.Fatalf("expected 1 patch for empty map, got %d", len(patches))
	}
	if patches[0].Path != "/metadata/annotations" {
		t.Errorf("expected path=/metadata/annotations, got %s", patches[0].Path)
	}
}

func TestBuildPatch_WithExistingAnnotations(t *testing.T) {
	existing := map[string]string{
		"e2b.dev/vcpu":           "8", // 已存在，不覆盖
		"app.kubernetes.io/name": "my-app",
	}
	patches := buildPatch(existing, sampleAnnotations, "/metadata/annotations")

	wantCount := len(sampleAnnotations) - 1
	if len(patches) != wantCount {
		t.Fatalf("expected %d patches, got %d", wantCount, len(patches))
	}

	seen := map[string]bool{}
	for _, p := range patches {
		if p.Op != "add" {
			t.Errorf("expected op=add, got %s for path %s", p.Op, p.Path)
		}
		if !strings.HasPrefix(p.Path, "/metadata/annotations/") {
			t.Errorf("path should start with /metadata/annotations/, got %s", p.Path)
		}
		seen[p.Path] = true
		if _, ok := p.Value.(string); !ok {
			t.Errorf("expected string value for %s, got %T", p.Path, p.Value)
		}
	}

	vcpuPath := "/metadata/annotations/" + escapeJSONPointer("e2b.dev/vcpu")
	if seen[vcpuPath] {
		t.Errorf("vcpu should not be patched because it already exists")
	}
}

func TestBuildPatch_AllAnnotationsExist(t *testing.T) {
	existing := make(map[string]string, len(sampleAnnotations))
	for k, v := range sampleAnnotations {
		existing[k] = v
	}
	patches := buildPatch(existing, sampleAnnotations, "/metadata/annotations")
	if len(patches) != 0 {
		t.Errorf("expected 0 patches when all annotations exist, got %d: %+v", len(patches), patches)
	}
}

func TestBuildPatch_CustomBasePath(t *testing.T) {
	patches := buildPatch(nil, sampleAnnotations, "/spec/template/metadata/annotations")
	if len(patches) != 1 {
		t.Fatalf("expected 1 patch, got %d", len(patches))
	}
	if patches[0].Path != "/spec/template/metadata/annotations" {
		t.Errorf("expected path=/spec/template/metadata/annotations, got %s", patches[0].Path)
	}
}

func TestBuildPatch_JSONRoundTrip(t *testing.T) {
	patches := buildPatch(nil, sampleAnnotations, "/metadata/annotations")
	data, err := json.Marshal(patches)
	if err != nil {
		t.Fatalf("failed to marshal patches: %v", err)
	}

	var decoded []patchOperation
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal patches: %v", err)
	}
	if len(decoded) != 1 {
		t.Fatalf("expected 1 patch after round-trip, got %d", len(decoded))
	}

	m, ok := decoded[0].Value.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map, got %T", decoded[0].Value)
	}
	if m["e2b.dev/vcpu"] != "2" {
		t.Errorf("e2b.dev/vcpu = %v, want 2", m["e2b.dev/vcpu"])
	}
}

// =============================================================================
// admitPod / admitBatchSandbox —— Provider 选择与 patch
// =============================================================================

func TestAdmitPod_AndroidEnvDefaultsToAndroid(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "default"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Env: []corev1.EnvVar{
			{Name: "TEMPLATE_NAME", Value: "tmpl"},
			{Name: "ANDROID_SANDBOX", Value: "true"},
		}}}},
	}
	resp := admitPod(newPodReview(admissionv1.Create, pod), newFakeMatcher(t, "e2b", "android"))
	patches := decodePatches(t, resp)
	if len(patches) != 1 || patches[0].Path != podRuntimeClassNamePath || patches[0].Value != "android" {
		t.Fatalf("patches = %+v, want only runtimeClassName=android", patches)
	}
}

func TestAdmitPod_NoSignalDefaultsToE2B(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "default"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Env: []corev1.EnvVar{
			{Name: "TEMPLATE_NAME", Value: "tmpl"},
		}}}},
	}
	resp := admitPod(newPodReview(admissionv1.Create, pod), newFakeMatcher(t, "e2b", "android"))
	patches := decodePatches(t, resp)
	if !patchExists(patches, podRuntimeClassNamePath) {
		t.Fatalf("runtimeClass patch missing: %+v", patches)
	}
	for _, p := range patches {
		if p.Path == podRuntimeClassNamePath && p.Value != "e2b" {
			t.Fatalf("runtimeClass = %v, want e2b", p.Value)
		}
	}
	if got := patchedAnnotations(t, podAnnotationsPath, patches)["e2b.dev/template-id"]; got != "provider-template" {
		t.Fatalf("e2b.dev/template-id = %q, want default e2b provider annotations", got)
	}
}

func TestAdmitPod_NoSignalNoTemplateSetsRuntimeClassOnly(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "bare-pod", Namespace: "default"}}
	matcher := newMatcherFromProviders(t, &fakeProvider{handler: "e2b", nilResult: true})
	resp := admitPod(newPodReview(admissionv1.Create, pod), matcher)
	patches := decodePatches(t, resp)
	if len(patches) != 1 || patches[0].Path != podRuntimeClassNamePath || patches[0].Value != "e2b" {
		t.Fatalf("patches = %+v, want only runtimeClassName=e2b", patches)
	}
}

func TestAdmitPod_AndroidEnvIgnoredWhenRuntimeClassSet(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "default"},
		Spec: corev1.PodSpec{
			RuntimeClassName: strPtr("e2b"),
			Containers: []corev1.Container{{Name: "main", Env: []corev1.EnvVar{
				{Name: "ANDROID_SANDBOX", Value: "true"},
			}}},
		},
	}
	resp := admitPod(newPodReview(admissionv1.Create, pod), newFakeMatcher(t, "e2b", "android"))
	patches := decodePatches(t, resp)
	if patchExists(patches, podRuntimeClassNamePath) {
		t.Fatalf("explicit runtimeClassName must not be overridden: %+v", patches)
	}
	if got := patchedAnnotations(t, podAnnotationsPath, patches)["e2b.dev/template-id"]; got != "provider-template" {
		t.Fatalf("runtimeClass should win over ANDROID_SANDBOX, got template-id %q", got)
	}
}

func TestAdmitPod_RuntimeClassSelectsProvider(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "default"},
		Spec: corev1.PodSpec{
			RuntimeClassName: strPtr("e2b"),
			Containers:       []corev1.Container{{Name: "main", Env: []corev1.EnvVar{{Name: "TEMPLATE_NAME", Value: "tmpl"}}}},
		},
	}
	resp := admitPod(newPodReview(admissionv1.Create, pod), newFakeMatcher(t, "e2b", "android"))
	if !resp.Allowed {
		t.Fatal("Allowed = false")
	}
	patches := decodePatches(t, resp)
	annotations := patchedAnnotations(t, podAnnotationsPath, patches)
	if annotations["e2b.dev/template-id"] != "provider-template" {
		t.Fatalf("annotations = %v", annotations)
	}
	if patchExists(patches, podRuntimeClassNamePath) {
		t.Fatal("runtimeClass patch must not be added when already set")
	}
}

func TestAdmitPod_AndroidRuntimeClassInjectsNothing(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "default"},
		Spec:       corev1.PodSpec{RuntimeClassName: strPtr("android")},
	}
	resp := admitPod(newPodReview(admissionv1.Create, pod), newFakeMatcher(t, "e2b", "android"))
	if !resp.Allowed {
		t.Fatal("Allowed = false")
	}
	if patches := decodePatches(t, resp); len(patches) != 0 {
		t.Fatalf("patches = %+v, want none", patches)
	}
}

func TestAdmitPod_AnnotationsNeverSelectProvider(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod-1", Namespace: "default",
			Annotations: map[string]string{"example.com/backend": "cube"},
		},
	}
	resp := admitPod(newPodReview(admissionv1.Create, pod), newFakeMatcher(t, "e2b", "cube"))
	patches := decodePatches(t, resp)
	// 注解不参与选路：没有 runtimeClass 时回落到默认 e2b。
	if !patchExists(patches, podRuntimeClassNamePath) {
		t.Fatalf("runtimeClass patch missing: %+v", patches)
	}
	for _, p := range patches {
		if p.Path == podRuntimeClassNamePath && p.Value != "e2b" {
			t.Fatalf("runtimeClass value = %v, want default e2b", p.Value)
		}
	}
	annotations := patchedAnnotations(t, podAnnotationsPath, patches)
	if annotations["e2b.dev/template-id"] != "provider-template" {
		t.Fatalf("annotations = %v, want e2b provider template injected", annotations)
	}
}

func TestAdmitPod_UnregisteredRuntimeClassPassesThrough(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "default"},
		Spec:       corev1.PodSpec{RuntimeClassName: strPtr("runc")},
	}
	resp := admitPod(newPodReview(admissionv1.Create, pod), newFakeMatcher(t, "e2b"))
	if !resp.Allowed || len(resp.Patch) != 0 {
		t.Fatalf("Allowed=%v patch=%s, want pass-through", resp.Allowed, resp.Patch)
	}
}

func TestAdmitPod_ResolveErrorOnlySetsRuntimeClass(t *testing.T) {
	matcher := newMatcherFromProviders(t, &fakeProvider{handler: "e2b", err: errors.New("boom")})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "default"},
	}
	resp := admitPod(newPodReview(admissionv1.Create, pod), matcher)
	patches := decodePatches(t, resp)
	if len(patches) != 1 || patches[0].Path != podRuntimeClassNamePath || patches[0].Value != "e2b" {
		t.Fatalf("patches = %+v, want only runtimeClassName", patches)
	}
}

func TestAdmitPod_UserAnnotationsWin(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod-1", Namespace: "default",
			Annotations: map[string]string{
				"e2b.dev/template-id": "user",
				"e2b.dev/team-id":     "user-team",
			},
		},
	}
	resp := admitPod(newPodReview(admissionv1.Create, pod), newFakeMatcher(t, "e2b"))
	patches := decodePatches(t, resp)
	for _, p := range patches {
		if strings.HasSuffix(p.Path, escapeJSONPointer("e2b.dev/template-id")) {
			t.Fatalf("user annotation overwritten: %+v", p)
		}
	}
	if !patchExists(patches, podRuntimeClassNamePath) {
		t.Fatalf("runtimeClass patch missing: %+v", patches)
	}
}

func TestAdmitPod_OversizedAnnotationsDropped(t *testing.T) {
	big := map[string]string{"e2b.dev/big": strings.Repeat("x", 201*1024)}
	matcher := newMatcherFromProviders(t, &fakeProvider{handler: "e2b", annotations: big})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod-1", Namespace: "default",
			Annotations: map[string]string{"e2b.dev/team-id": "team"},
		},
	}
	resp := admitPod(newPodReview(admissionv1.Create, pod), matcher)
	patches := decodePatches(t, resp)
	if len(patches) != 1 || patches[0].Path != podRuntimeClassNamePath {
		t.Fatalf("patches = %+v, want only runtimeClassName", patches)
	}
}

func TestAdmitBatchSandbox_RuntimeClassFromTemplate(t *testing.T) {
	obj := newBatchSandboxObj("bs-1", "default", []interface{}{envVar("TEMPLATE_NAME", "tmpl")})
	setBatchSandboxRuntimeClass(obj, "cube")
	raw, _ := json.Marshal(obj)
	resp := admitBatchSandbox(newAdmissionReview("BatchSandbox", admissionv1.Create, raw), newFakeMatcher(t, "cube"))
	patches := decodePatches(t, resp)
	if len(patches) != 1 || patches[0].Path != bsAnnotationsPath {
		t.Fatalf("patches = %+v, want annotations at %s", patches, bsAnnotationsPath)
	}
	if patchExists(patches, bsRuntimeClassNamePath) {
		t.Fatal("runtimeClass already set in template, must not be patched")
	}
}

func TestAdmitBatchSandbox_NoSignalDefaultsToE2B(t *testing.T) {
	obj := newBatchSandboxObj("bs-1", "default", []interface{}{envVar("TEMPLATE_NAME", "tmpl")})
	raw, _ := json.Marshal(obj)
	resp := admitBatchSandbox(newAdmissionReview("BatchSandbox", admissionv1.Create, raw), newFakeMatcher(t, "e2b"))
	patches := decodePatches(t, resp)
	if !patchExists(patches, bsRuntimeClassNamePath) {
		t.Fatalf("runtimeClass patch missing: %+v", patches)
	}
	if len(patchedAnnotations(t, bsAnnotationsPath, patches)) == 0 {
		t.Fatalf("annotations patch missing: %+v", patches)
	}
}

// testRequestFromPodCoversEnvAndIdentity 补齐归一化断言。
func TestRequestFromPodCoversEnvAndIdentity(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod-1", Namespace: "ns", UID: "uid-1",
			Labels:      map[string]string{"app": "x"},
			Annotations: map[string]string{"e2b.dev/sandbox-id": "legacy-id"},
		},
		Spec: corev1.PodSpec{
			RuntimeClassName: strPtr("e2b"),
			Containers: []corev1.Container{{Name: "main", Env: []corev1.EnvVar{
				{Name: "TEMPLATE_NAME", Value: "tmpl"},
				{Name: "K", Value: "V"},
			}}},
		},
	}
	req := requestFromPod(pod)
	if req.Kind != "Pod" || req.SandboxID != "legacy-id" || req.Template != "tmpl" || req.RuntimeClass != "e2b" || req.Env["K"] != "V" {
		t.Fatalf("request = %+v", req)
	}
}

func strPtr(s string) *string { return &s }

// =============================================================================
// serveMutate - 分发逻辑
// =============================================================================

func TestServeMutate_CreatePod(t *testing.T) {
	oldMatcher := admissionMatcher
	defer func() { admissionMatcher = oldMatcher }()
	admissionMatcher = newFakeMatcher(t, "e2b")

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "http-pod", Namespace: "default"},
		Spec:       corev1.PodSpec{RuntimeClassName: strPtr("e2b")},
	}
	ar := newPodReview(admissionv1.Create, pod)
	body, _ := json.Marshal(ar)

	req := httptest.NewRequest(http.MethodPost, "/mutate", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	serveMutate(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp admissionv1.AdmissionReview
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Response == nil {
		t.Fatalf("expected non-nil response")
	}
	if !resp.Response.Allowed {
		t.Errorf("expected allowed=true")
	}
	if resp.Response.UID != "test-uid-1234" {
		t.Errorf("expected uid=test-uid-1234, got %s", resp.Response.UID)
	}
	if resp.Response.PatchType == nil || *resp.Response.PatchType != admissionv1.PatchTypeJSONPatch {
		t.Errorf("expected PatchType=JSONPatch")
	}
	if len(resp.Response.Patch) == 0 {
		t.Errorf("expected non-empty patch")
	}
}

func TestServeMutate_CreateBatchSandbox(t *testing.T) {
	oldMatcher := admissionMatcher
	defer func() { admissionMatcher = oldMatcher }()
	admissionMatcher = newFakeMatcher(t, "e2b")

	obj := newBatchSandboxObj("bs-http", "default", []interface{}{envVar("TEMPLATE_NAME", "tpl")})
	setBatchSandboxRuntimeClass(obj, "e2b")
	raw, _ := json.Marshal(obj)
	ar := newAdmissionReview("BatchSandbox", admissionv1.Create, raw)
	body, _ := json.Marshal(ar)

	req := httptest.NewRequest(http.MethodPost, "/mutate", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	serveMutate(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp admissionv1.AdmissionReview
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Response == nil || !resp.Response.Allowed {
		t.Fatalf("expected allowed=true")
	}
	if len(resp.Response.Patch) == 0 {
		t.Errorf("expected non-empty patch")
	}

	var patches []patchOperation
	if err := json.Unmarshal(resp.Response.Patch, &patches); err != nil {
		t.Fatalf("failed to unmarshal patch: %v", err)
	}
	if len(patches) != 1 || patches[0].Path != "/spec/template/metadata/annotations" {
		t.Errorf("expected patch to template annotations, got %+v", patches)
	}
}

func TestServeMutate_NonPodKind(t *testing.T) {
	oldMatcher := admissionMatcher
	defer func() { admissionMatcher = oldMatcher }()
	admissionMatcher = newFakeMatcher(t, "e2b")

	raw, _ := json.Marshal(map[string]string{"key": "val"})
	ar := newAdmissionReview("ConfigMap", admissionv1.Create, raw)
	body, _ := json.Marshal(ar)

	req := httptest.NewRequest(http.MethodPost, "/mutate", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	serveMutate(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp admissionv1.AdmissionReview
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if !resp.Response.Allowed {
		t.Errorf("expected allowed=true for non-pod kind")
	}
	if len(resp.Response.Patch) != 0 {
		t.Errorf("expected empty patch for non-pod kind")
	}
}

func TestServeMutate_WrongMethod(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/mutate", nil)
	rec := httptest.NewRecorder()
	serveMutate(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

func TestServeMutate_WrongContentType(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/mutate", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	serveMutate(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("expected 415, got %d", rec.Code)
	}
}

func TestServeMutate_InvalidBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/mutate", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	serveMutate(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestServeMutate_NilRequest(t *testing.T) {
	body := `{"apiVersion":"admission.k8s.io/v1","kind":"AdmissionReview"}`
	req := httptest.NewRequest(http.MethodPost, "/mutate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	serveMutate(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for nil request, got %d", rec.Code)
	}
}

// =============================================================================
// /healthz 端点
// =============================================================================

func TestServeHealth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	serveHealth(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("expected body 'ok', got %q", rec.Body.String())
	}
}

// =============================================================================
// envOr / envDurationOr
// =============================================================================

func TestEnvOr(t *testing.T) {
	t.Setenv("TEST_ENV_OR", "custom")
	if got := envOr("TEST_ENV_OR", "default"); got != "custom" {
		t.Errorf("expected custom, got %s", got)
	}
	if got := envOr("NONEXISTENT_VAR_XYZ", "default"); got != "default" {
		t.Errorf("expected default, got %s", got)
	}
}

func TestEnvDurationOr(t *testing.T) {
	t.Setenv("TEST_DUR", "15s")
	if got := envDurationOr("TEST_DUR", 5*time.Second); got != 15*time.Second {
		t.Errorf("expected 15s, got %v", got)
	}
	if got := envDurationOr("NONEXISTENT_DUR", 5*time.Second); got != 5*time.Second {
		t.Errorf("expected default 5s, got %v", got)
	}
	t.Setenv("TEST_DUR_BAD", "not-a-duration")
	if got := envDurationOr("TEST_DUR_BAD", 5*time.Second); got != 5*time.Second {
		t.Errorf("expected default for invalid, got %v", got)
	}
}

// =============================================================================
// extractTemplateNameFromBatchSandbox
// =============================================================================

func TestExtractTemplateNameFromBatchSandbox_Success(t *testing.T) {
	obj := newBatchSandboxObj("bs-1", "default", []interface{}{
		envVar("OTHER_VAR", "x"),
		envVar("TEMPLATE_NAME", "my-template-id"),
		envVar("ANOTHER", "y"),
	})
	got, err := extractTemplateNameFromBatchSandbox(obj)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if got != "my-template-id" {
		t.Errorf("expected my-template-id, got %s", got)
	}
}

func TestExtractTemplateNameFromBatchSandbox_FirstEnv(t *testing.T) {
	obj := newBatchSandboxObj("bs-1", "default", []interface{}{
		envVar("TEMPLATE_NAME", "first-match"),
	})
	got, err := extractTemplateNameFromBatchSandbox(obj)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if got != "first-match" {
		t.Errorf("expected first-match, got %s", got)
	}
}

func TestExtractTemplateNameFromBatchSandbox_EmptyValue(t *testing.T) {
	obj := newBatchSandboxObj("bs-1", "default", []interface{}{
		envVar("TEMPLATE_NAME", ""),
	})
	if _, err := extractTemplateNameFromBatchSandbox(obj); err == nil {
		t.Errorf("expected error for empty TEMPLATE_NAME value, got nil")
	}
}

func TestExtractTemplateNameFromBatchSandbox_NotFound(t *testing.T) {
	obj := newBatchSandboxObj("bs-1", "default", []interface{}{
		envVar("OTHER", "x"),
	})
	if _, err := extractTemplateNameFromBatchSandbox(obj); err == nil {
		t.Errorf("expected error when TEMPLATE_NAME missing, got nil")
	}
}

func TestExtractTemplateNameFromBatchSandbox_NoContainers(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{},
				},
			},
		},
	}}
	if _, err := extractTemplateNameFromBatchSandbox(obj); err == nil {
		t.Errorf("expected error for empty containers, got nil")
	}
}

func TestExtractTemplateNameFromBatchSandbox_NoEnv(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{"name": "sandbox"},
					},
				},
			},
		},
	}}
	if _, err := extractTemplateNameFromBatchSandbox(obj); err == nil {
		t.Errorf("expected error for missing env, got nil")
	}
}

// =============================================================================
// getNestedStringMap
// =============================================================================

func TestGetNestedStringMap(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{
					"annotations": map[string]interface{}{
						"e2b.dev/vcpu": "4",
					},
				},
			},
		},
	}}
	got := getNestedStringMap(obj, "spec", "template", "metadata", "annotations")
	if got == nil {
		t.Fatalf("expected non-nil map")
	}
	if got["e2b.dev/vcpu"] != "4" {
		t.Errorf("expected vcpu=4, got %q", got["e2b.dev/vcpu"])
	}
}

func TestGetNestedStringMap_MissingPath(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{}}
	if got := getNestedStringMap(obj, "spec", "template", "metadata", "annotations"); got != nil {
		t.Errorf("expected nil for missing path, got %v", got)
	}
}

// =============================================================================
// copyMap
// =============================================================================

func TestCopyMap(t *testing.T) {
	orig := map[string]string{"a": "1", "b": "2"}
	cp := copyMap(orig)
	cp["a"] = "modified"
	delete(cp, "b")
	cp["c"] = "new"

	if orig["a"] != "1" {
		t.Errorf("original modified by copy: a=%q", orig["a"])
	}
	if _, ok := orig["b"]; !ok {
		t.Errorf("original lost key b")
	}
	if _, ok := orig["c"]; ok {
		t.Errorf("original gained key c")
	}
}

func TestCopyMap_Nil(t *testing.T) {
	cp := copyMap(nil)
	if cp == nil {
		t.Fatalf("expected non-nil map")
	}
	if len(cp) != 0 {
		t.Errorf("expected empty map, got %d", len(cp))
	}
}

// =============================================================================
// 端到端: 应用 patch 并检查最终注解
// =============================================================================

func applyPatch(t *testing.T, annotations map[string]string, patches []patchOperation, basePath string) map[string]string {
	t.Helper()
	result := make(map[string]string)
	for k, v := range annotations {
		result[k] = v
	}
	prefix := basePath + "/"
	for _, p := range patches {
		switch {
		case p.Path == basePath:
			m, ok := p.Value.(map[string]string)
			if !ok {
				t.Fatalf("expected map[string]string value, got %T", p.Value)
			}
			for k, v := range m {
				result[k] = v
			}
		case strings.HasPrefix(p.Path, prefix):
			key := p.Path[len(prefix):]
			key = strings.ReplaceAll(key, "~1", "/")
			key = strings.ReplaceAll(key, "~0", "~")
			v, ok := p.Value.(string)
			if !ok {
				t.Fatalf("expected string value, got %T", p.Value)
			}
			result[key] = v
		default:
			t.Fatalf("unexpected patch path: %s", p.Path)
		}
	}
	return result
}

func TestEndToEnd_PatchProducesCorrectAnnotations(t *testing.T) {
	cases := []struct {
		name     string
		existing map[string]string
		basePath string
	}{
		{"pod no annotations", nil, "/metadata/annotations"},
		{"pod empty annotations", map[string]string{}, "/metadata/annotations"},
		{"pod unrelated annotations", map[string]string{"foo": "bar", "app": "x"}, "/metadata/annotations"},
		{"pod partial e2b annotations", map[string]string{
			"e2b.dev/vcpu":   "8",
			"e2b.dev/ram-mb": "4096",
		}, "/metadata/annotations"},
		{"batchsandbox no annotations", nil, "/spec/template/metadata/annotations"},
		{"batchsandbox partial", map[string]string{"e2b.dev/vcpu": "8"}, "/spec/template/metadata/annotations"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			patches := buildPatch(c.existing, sampleAnnotations, c.basePath)
			result := applyPatch(t, c.existing, patches, c.basePath)

			for k, wantV := range sampleAnnotations {
				gotV, ok := result[k]
				if !ok {
					t.Errorf("missing annotation %s after patch", k)
					continue
				}
				if exV, ex := c.existing[k]; ex {
					if gotV != exV {
						t.Errorf("annotation %s = %q, want existing %q", k, gotV, exV)
					}
				} else {
					if gotV != wantV {
						t.Errorf("annotation %s = %q, want %q", k, gotV, wantV)
					}
				}
			}

			for k, v := range c.existing {
				if gotV, ok := result[k]; !ok || gotV != v {
					t.Errorf("existing annotation %s lost: got %q, want %q", k, gotV, v)
				}
			}
		})
	}
}

// =============================================================================
// 基准测试
// =============================================================================

func BenchmarkBuildPatch_Empty(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = buildPatch(nil, sampleAnnotations, "/metadata/annotations")
	}
}

func BenchmarkBuildPatch_WithExisting(b *testing.B) {
	existing := map[string]string{"foo": "bar", "e2b.dev/vcpu": "4"}
	for i := 0; i < b.N; i++ {
		_ = buildPatch(existing, sampleAnnotations, "/metadata/annotations")
	}
}

func BenchmarkAdmitPod(b *testing.B) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "bench-pod", Namespace: "default"},
		Spec:       corev1.PodSpec{RuntimeClassName: strPtr("e2b")},
	}
	ar := newPodReview(admissionv1.Create, pod)
	matcher, err := provider.NewMatcher(&fakeProvider{handler: "e2b"})
	if err != nil {
		b.Fatalf("NewMatcher: %v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		admitPod(ar, matcher)
	}
}

// =============================================================================
// Provider 装配：启动时注册全部已实现 provider
// =============================================================================

func TestBuildMatcherRegistersAllImplementedProviders(t *testing.T) {
	matcher, err := buildMatcher(providers.NewSandboxTransformer("http://example.invalid", "", time.Second))
	if err != nil {
		t.Fatalf("buildMatcher: %v", err)
	}
	for _, handler := range []string{"e2b", "android"} {
		if _, ok := matcher.Resolve(handler); !ok {
			t.Fatalf("provider %q is not registered", handler)
		}
	}
}

// Android 场景：ANDROID_SANDBOX=true 的对象必须走 android，不能被改判成 e2b。
func TestAdmitPod_AndroidEnvSelectsAndroid(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "default"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Env: []corev1.EnvVar{
			{Name: "TEMPLATE_NAME", Value: "android-tpl-001"},
			{Name: "ANDROID_SANDBOX", Value: "true"},
		}}}},
	}
	matcher, err := buildMatcher(providers.NewSandboxTransformer("http://127.0.0.1:1", "", 200*time.Millisecond))
	if err != nil {
		t.Fatalf("buildMatcher: %v", err)
	}
	resp := admitPod(newPodReview(admissionv1.Create, pod), matcher)
	patches := decodePatches(t, resp)
	if len(patches) != 1 || patches[0].Path != podRuntimeClassNamePath || patches[0].Value != "android" {
		t.Fatalf("patches = %+v, want only runtimeClassName=android", patches)
	}
}
