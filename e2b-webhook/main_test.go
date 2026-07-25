package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
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

// mockTransformer 实现 annotationTransformer 接口, 用于测试 admitPod / admitBatchSandbox。
type mockTransformer struct {
	mu             sync.Mutex
	podAnnotations map[string]string
	podErr         error
	podCalls       int
	bsAnnotations  map[string]string
	bsErr          error
	bsCalls        int
}

func (m *mockTransformer) FetchForPod(pod *corev1.Pod) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.podCalls++
	return copyMap(m.podAnnotations), m.podErr
}

func (m *mockTransformer) FetchForBatchSandbox(obj *unstructured.Unstructured) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bsCalls++
	return copyMap(m.bsAnnotations), m.bsErr
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
// admitPod
// =============================================================================

func TestAdmitPod_Create(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
	}
	ar := newPodReview(admissionv1.Create, pod)
	tr := &mockTransformer{podAnnotations: sampleAnnotations}

	resp := admitPod(ar, tr)
	if !resp.Allowed {
		t.Fatalf("expected allowed=true")
	}
	if resp.UID != "test-uid-1234" {
		t.Errorf("expected uid=test-uid-1234, got %s", resp.UID)
	}
	if resp.PatchType == nil || *resp.PatchType != admissionv1.PatchTypeJSONPatch {
		t.Fatalf("expected PatchType=JSONPatch")
	}
	if len(resp.Patch) == 0 {
		t.Fatalf("expected non-empty patch")
	}

	var patches []patchOperation
	if err := json.Unmarshal(resp.Patch, &patches); err != nil {
		t.Fatalf("failed to unmarshal patch: %v", err)
	}
	if len(patches) != 2 {
		t.Fatalf("expected 2 patches (annotations + runtimeClassName), got %d", len(patches))
	}
	if patches[0].Path != "/metadata/annotations" {
		t.Errorf("expected path=/metadata/annotations, got %s", patches[0].Path)
	}
	if patches[1].Path != "/spec/runtimeClassName" || patches[1].Value != "e2b" {
		t.Errorf("expected path=/spec/runtimeClassName value=e2b, got path=%s value=%v",
			patches[1].Path, patches[1].Value)
	}
	if tr.podCalls != 1 {
		t.Errorf("expected FetchForPod called once, got %d", tr.podCalls)
	}
}

func TestAdmitPod_AndroidSandbox(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "android-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "sandbox",
					Env: []corev1.EnvVar{
						{Name: "TEMPLATE_NAME", Value: "tpl-android"},
						{Name: "ANDROID_SANDBOX", Value: "true"},
					},
				},
			},
		},
	}
	ar := newPodReview(admissionv1.Create, pod)
	tr := &mockTransformer{podAnnotations: sampleAnnotations}

	resp := admitPod(ar, tr)
	if !resp.Allowed {
		t.Fatalf("expected allowed=true")
	}

	var patches []patchOperation
	if err := json.Unmarshal(resp.Patch, &patches); err != nil {
		t.Fatalf("failed to unmarshal patch: %v", err)
	}

	// 期望: 仅 1 个 runtimeClassName patch (跳过 e2b 注解和 API 调用)
	if len(patches) != 1 {
		t.Fatalf("expected 1 patch, got %d", len(patches))
	}

	var rcPatch *patchOperation
	for i := range patches {
		if patches[i].Path == "/spec/runtimeClassName" {
			rcPatch = &patches[i]
			break
		}
	}
	if rcPatch == nil {
		t.Fatalf("expected runtimeClassName patch, got none")
	}
	if rcPatch.Value != "android" {
		t.Errorf("expected runtimeClassName=android, got %v", rcPatch.Value)
	}
}

func TestAdmitPod_NonCreate(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
	}
	tr := &mockTransformer{podAnnotations: sampleAnnotations}
	for _, op := range []admissionv1.Operation{
		admissionv1.Update,
		admissionv1.Delete,
		admissionv1.Connect,
	} {
		ar := newPodReview(op, pod)
		resp := admitPod(ar, tr)
		if !resp.Allowed {
			t.Errorf("op=%s: expected allowed=true", op)
		}
		if len(resp.Patch) != 0 {
			t.Errorf("op=%s: expected empty patch, got %d bytes", op, len(resp.Patch))
		}
		if resp.PatchType != nil {
			t.Errorf("op=%s: expected nil PatchType", op)
		}
	}
	if tr.podCalls != 0 {
		t.Errorf("expected FetchForPod not called for non-create, got %d", tr.podCalls)
	}
}

func TestAdmitPod_MalformedJSON(t *testing.T) {
	ar := &admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID:       "bad-uid",
			Kind:      metav1.GroupVersionKind{Kind: "Pod"},
			Operation: admissionv1.Create,
			Object:    runtime.RawExtension{Raw: []byte(`{not valid json`)},
		},
	}
	tr := &mockTransformer{podAnnotations: sampleAnnotations}
	resp := admitPod(ar, tr)
	if resp.Allowed {
		t.Errorf("expected allowed=false for malformed JSON")
	}
	if resp.Result == nil {
		t.Errorf("expected non-nil Result")
	}
}

func TestAdmitPod_PartialE2BAnnotations(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "partial-pod",
			Namespace: "default",
			Annotations: map[string]string{
				"e2b.dev/vcpu":        "4", // 自定义值，不应被覆盖
				"e2b.dev/template-id": "custom-template",
			},
		},
	}
	ar := newPodReview(admissionv1.Create, pod)
	tr := &mockTransformer{podAnnotations: sampleAnnotations}
	resp := admitPod(ar, tr)
	if !resp.Allowed {
		t.Fatalf("expected allowed=true")
	}

	var patches []patchOperation
	if err := json.Unmarshal(resp.Patch, &patches); err != nil {
		t.Fatalf("failed to unmarshal patch: %v", err)
	}

	// sampleAnnotations 中有 4 个, pod 已有 2 个 (vcpu, template-id), 应注入 2 个注解 + 1 个 runtimeClassName
	want := len(sampleAnnotations) - 2 + 1
	if len(patches) != want {
		t.Fatalf("expected %d patches, got %d", want, len(patches))
	}

	for _, p := range patches {
		if strings.HasSuffix(p.Path, escapeJSONPointer("e2b.dev/vcpu")) {
			t.Errorf("vcpu should not be patched")
		}
		if strings.HasSuffix(p.Path, escapeJSONPointer("e2b.dev/template-id")) {
			t.Errorf("template-id should not be patched")
		}
	}
}

// =============================================================================
// admitBatchSandbox
// =============================================================================

func TestAdmitBatchSandbox_Create(t *testing.T) {
	obj := newBatchSandboxObj("bs-1", "default", []interface{}{
		envVar("TEMPLATE_NAME", "tpl-from-env"),
		envVar("OTHER", "x"),
	})
	raw, _ := json.Marshal(obj)
	ar := newAdmissionReview("BatchSandbox", admissionv1.Create, raw)
	tr := &mockTransformer{bsAnnotations: sampleAnnotations}

	resp := admitBatchSandbox(ar, tr)
	if !resp.Allowed {
		t.Fatalf("expected allowed=true")
	}
	if resp.UID != "test-uid-1234" {
		t.Errorf("expected uid=test-uid-1234, got %s", resp.UID)
	}
	if resp.PatchType == nil || *resp.PatchType != admissionv1.PatchTypeJSONPatch {
		t.Fatalf("expected PatchType=JSONPatch")
	}
	if len(resp.Patch) == 0 {
		t.Fatalf("expected non-empty patch")
	}

	var patches []patchOperation
	if err := json.Unmarshal(resp.Patch, &patches); err != nil {
		t.Fatalf("failed to unmarshal patch: %v", err)
	}
	if len(patches) != 1 {
		t.Fatalf("expected 1 patch, got %d", len(patches))
	}
	if patches[0].Path != "/spec/template/metadata/annotations" {
		t.Errorf("expected path=/spec/template/metadata/annotations, got %s", patches[0].Path)
	}
	if tr.bsCalls != 1 {
		t.Errorf("expected FetchForBatchSandbox called once, got %d", tr.bsCalls)
	}
}

func TestAdmitBatchSandbox_NonCreate(t *testing.T) {
	obj := newBatchSandboxObj("bs-1", "default", []interface{}{envVar("TEMPLATE_NAME", "tpl")})
	raw, _ := json.Marshal(obj)
	tr := &mockTransformer{bsAnnotations: sampleAnnotations}
	for _, op := range []admissionv1.Operation{
		admissionv1.Update,
		admissionv1.Delete,
		admissionv1.Connect,
	} {
		ar := newAdmissionReview("BatchSandbox", op, raw)
		resp := admitBatchSandbox(ar, tr)
		if !resp.Allowed {
			t.Errorf("op=%s: expected allowed=true", op)
		}
		if len(resp.Patch) != 0 {
			t.Errorf("op=%s: expected empty patch, got %d bytes", op, len(resp.Patch))
		}
	}
	if tr.bsCalls != 0 {
		t.Errorf("expected FetchForBatchSandbox not called for non-create, got %d", tr.bsCalls)
	}
}

func TestAdmitBatchSandbox_MalformedJSON(t *testing.T) {
	ar := &admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID:       "bad-uid",
			Kind:      metav1.GroupVersionKind{Kind: "BatchSandbox"},
			Operation: admissionv1.Create,
			Object:    runtime.RawExtension{Raw: []byte(`{not valid json`)},
		},
	}
	tr := &mockTransformer{bsAnnotations: sampleAnnotations}
	resp := admitBatchSandbox(ar, tr)
	if resp.Allowed {
		t.Errorf("expected allowed=false for malformed JSON")
	}
	if resp.Result == nil {
		t.Errorf("expected non-nil Result")
	}
}

func TestAdmitBatchSandbox_PartialAnnotations(t *testing.T) {
	obj := newBatchSandboxObj("bs-1", "default", []interface{}{envVar("TEMPLATE_NAME", "tpl")})
	// 在 spec.template.metadata.annotations 设置已有的 e2b 注解
	_ = unstructured.SetNestedField(obj.Object, map[string]interface{}{
		"e2b.dev/vcpu": "8", // 已存在, 不应被覆盖
	}, "spec", "template", "metadata", "annotations")

	raw, _ := json.Marshal(obj)
	ar := newAdmissionReview("BatchSandbox", admissionv1.Create, raw)
	tr := &mockTransformer{bsAnnotations: sampleAnnotations}
	resp := admitBatchSandbox(ar, tr)
	if !resp.Allowed {
		t.Fatalf("expected allowed=true")
	}

	var patches []patchOperation
	if err := json.Unmarshal(resp.Patch, &patches); err != nil {
		t.Fatalf("failed to unmarshal patch: %v", err)
	}
	// vcpu 已存在, 其余 3 个应被注入
	if len(patches) != len(sampleAnnotations)-1 {
		t.Fatalf("expected %d patches, got %d", len(sampleAnnotations)-1, len(patches))
	}
	for _, p := range patches {
		if !strings.HasPrefix(p.Path, "/spec/template/metadata/annotations/") {
			t.Errorf("patch path should target template annotations, got %s", p.Path)
		}
		if strings.HasSuffix(p.Path, escapeJSONPointer("e2b.dev/vcpu")) {
			t.Errorf("vcpu should not be patched")
		}
	}
}

// =============================================================================
// serveMutate - 分发逻辑
// =============================================================================

func TestServeMutate_CreatePod(t *testing.T) {
	oldFetcher := annotationFetcher
	defer func() { annotationFetcher = oldFetcher }()
	annotationFetcher = &mockTransformer{podAnnotations: sampleAnnotations}

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "http-pod", Namespace: "default"}}
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
	oldFetcher := annotationFetcher
	defer func() { annotationFetcher = oldFetcher }()
	annotationFetcher = &mockTransformer{bsAnnotations: sampleAnnotations}

	obj := newBatchSandboxObj("bs-http", "default", []interface{}{envVar("TEMPLATE_NAME", "tpl")})
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
	oldFetcher := annotationFetcher
	defer func() { annotationFetcher = oldFetcher }()
	annotationFetcher = &mockTransformer{podAnnotations: sampleAnnotations}

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
// sandboxConfig.toE2BAnnotations
// =============================================================================

func TestSandboxConfig_ToE2BAnnotations(t *testing.T) {
	internetTrue := true
	token := "secret-token"
	cfg := sandboxConfig{
		BaseTemplateID:      "base-1",
		TemplateID:          "tpl-xyz",
		BuildID:             "build-1",
		TeamID:              "team-1",
		Vcpu:                4,
		RAMMB:               2048,
		TotalDiskSizeMB:     10240,
		MaxSandboxLength:    600,
		HugePages:           true,
		AutoPause:           true,
		Snapshot:            true,
		AllowInternetAccess: &internetTrue,
		EnvdVersion:         "0.1.0",
		KernelVersion:       "5.10",
		FirecrackerVersion:  "1.0",
		ExecutionID:         "exec-1",
		EnvdAccessToken:     &token,
		Network:             json.RawMessage(`{"egress":{"a":1}}`),
	}

	annos := cfg.toE2BAnnotations()

	cases := map[string]string{
		"e2b.dev/base_template_id":    "base-1",
		"e2b.dev/template-id":         "tpl-xyz",
		"e2b.dev/build-id":            "build-1",
		"e2b.dev/team-id":             "team-1",
		"e2b.dev/vcpu":                "4",
		"e2b.dev/ram-mb":              "2048",
		"e2b.dev/total-disk-size-mb":  "10240",
		"e2b.dev/max-sandbox-length":  "600",
		"e2b.dev/huge-pages":          "true",
		"e2b.dev/auto-pause":          "true",
		"e2b.dev/snapshot":            "true",
		"e2b.dev/envd-version":        "0.1.0",
		"e2b.dev/kernel-version":      "5.10",
		"e2b.dev/firecracker-version": "1.0",
		"e2b.dev/execution-id":        "exec-1",
		"e2b.dev/allow-internet":      "true",
		"e2b.dev/envd-access-token":   "secret-token",
		"e2b.dev/network":             `{"egress":{"a":1}}`,
		"e2b.dev/env-vars":            "{}",
		"e2b.dev/volume-mounts":       "[]",
		"e2b.dev/auto-resume":         `{"policy":"off"}`,
	}
	for k, want := range cases {
		if got := annos[k]; got != want {
			t.Errorf("annotation %s = %q, want %q", k, got, want)
		}
	}
}

func TestSandboxConfig_ToE2BAnnotations_NilOptionals(t *testing.T) {
	cfg := sandboxConfig{
		TemplateID: "tpl-xyz",
		Vcpu:       1,
		RAMMB:      512,
	}
	annos := cfg.toE2BAnnotations()

	if annos["e2b.dev/allow-internet"] != "false" {
		t.Errorf("nil allow_internet_access should default to false, got %q", annos["e2b.dev/allow-internet"])
	}
	if annos["e2b.dev/envd-access-token"] != "" {
		t.Errorf("nil envd_access_token should default to empty, got %q", annos["e2b.dev/envd-access-token"])
	}
	if annos["e2b.dev/network"] != `{"egress":{},"ingress":{}}` {
		t.Errorf("empty network should default to empty object, got %q", annos["e2b.dev/network"])
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
// SandboxTransformer - mock API server
// =============================================================================

// transformAPIServer 启动一个模拟 /sandboxes/transform 的 httptest.Server。
// cfg 为返回的沙箱配置; status 为 HTTP 状态码。
// captured 记录最后一次收到的请求体 (可选)。
func transformAPIServer(t *testing.T, cfg sandboxConfig, status int, captured *sandboxTransformRequest) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sandboxes/transform" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if captured != nil {
			_ = json.NewDecoder(r.Body).Decode(captured)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(sandboxTransformResponse{Sandbox: cfg})
	}))
}

func sampleConfig(templateID string) sandboxConfig {
	return sandboxConfig{
		TemplateID: templateID,
		BuildID:    "build-1",
		TeamID:     "team-1",
		Vcpu:       2,
		RAMMB:      512,
	}
}

// =============================================================================
// SandboxTransformer.FetchForPod
// =============================================================================

func TestSandboxTransformer_FetchForPod_Success(t *testing.T) {
	var captured sandboxTransformRequest
	server := transformAPIServer(t, sampleConfig("tpl-1"), http.StatusOK, &captured)
	defer server.Close()

	tr := NewSandboxTransformer(server.URL, "key-1", 2*time.Second)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p1",
			Namespace: "ns",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "sandbox",
					Image: "test:latest",
					Env: []corev1.EnvVar{
						{Name: "TEMPLATE_NAME", Value: "tpl-1"},
					},
				},
			},
		},
	}
	annos, err := tr.FetchForPod(pod)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if annos["e2b.dev/template-id"] != "tpl-1" {
		t.Errorf("expected template-id=tpl-1, got %q", annos["e2b.dev/template-id"])
	}
	if captured.TemplateName != "tpl-1" {
		t.Errorf("expected API called with templateName=tpl-1, got %q", captured.TemplateName)
	}
}

func TestSandboxTransformer_FetchForPod_FallbackSandboxID(t *testing.T) {
	var captured sandboxTransformRequest
	server := transformAPIServer(t, sampleConfig("tpl-1"), http.StatusOK, &captured)
	defer server.Close()

	tr := NewSandboxTransformer(server.URL, "", 2*time.Second)
	// sandboxID 始终使用 pod.Name
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-name-1",
			Namespace: "ns",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "sandbox",
					Image: "test:latest",
					Env: []corev1.EnvVar{
						{Name: "TEMPLATE_NAME", Value: "tpl-1"},
					},
				},
			},
		},
	}
	_, err := tr.FetchForPod(pod)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if captured.TemplateName != "tpl-1" {
		t.Errorf("expected API called with templateName=tpl-1, got %q", captured.TemplateName)
	}
}

func TestSandboxTransformer_FetchForPod_MissingLabel(t *testing.T) {
	server := transformAPIServer(t, sampleConfig("tpl-1"), http.StatusOK, nil)
	defer server.Close()

	tr := NewSandboxTransformer(server.URL, "", 2*time.Second)
	// 无 TEMPLATE_NAME env: 应返回 (nil, nil) 放行
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "ns", Labels: map[string]string{}},
	}
	annos, err := tr.FetchForPod(pod)
	if err != nil {
		t.Fatalf("expected no error for missing label/env, got %v", err)
	}
	if annos != nil {
		t.Errorf("expected nil annotations when no templateID source, got %v", annos)
	}
}

func TestSandboxTransformer_FetchForPod_EnvTemplateName(t *testing.T) {
	var captured sandboxTransformRequest
	server := transformAPIServer(t, sampleConfig("tpl-env"), http.StatusOK, &captured)
	defer server.Close()

	tr := NewSandboxTransformer(server.URL, "", 2*time.Second)
	// containers[0].env 有 TEMPLATE_NAME: 应正常调用 API
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "spotbox-abc-0", Namespace: "ns", Labels: map[string]string{}},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "sandbox",
					Image: "test:latest",
					Env: []corev1.EnvVar{
						{Name: "ENVD_PORT", Value: "3999"},
						{Name: "TEMPLATE_NAME", Value: "tpl-env"},
					},
				},
			},
		},
	}
	annos, err := tr.FetchForPod(pod)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if annos["e2b.dev/template-id"] != "tpl-env" {
		t.Errorf("expected template-id=tpl-env, got %q", annos["e2b.dev/template-id"])
	}
	if captured.TemplateName != "tpl-env" {
		t.Errorf("expected API called with templateName=tpl-env, got %q", captured.TemplateName)
	}
}

func TestSandboxTransformer_FetchForPod_NoCache(t *testing.T) {
	var mu sync.Mutex
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sandboxTransformResponse{Sandbox: sampleConfig("tpl-1")})
	}))
	defer server.Close()

	tr := NewSandboxTransformer(server.URL, "", 2*time.Second)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "p1", Namespace: "ns",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "sandbox",
					Image: "test:latest",
					Env: []corev1.EnvVar{
						{Name: "TEMPLATE_NAME", Value: "tpl-1"},
					},
				},
			},
		},
	}

	a1, err := tr.FetchForPod(pod)
	if err != nil {
		t.Fatalf("first fetch failed: %v", err)
	}
	a2, err := tr.FetchForPod(pod)
	if err != nil {
		t.Fatalf("second fetch failed: %v", err)
	}
	if a1["e2b.dev/template-id"] != a2["e2b.dev/template-id"] {
		t.Errorf("two fetches returned different template-id")
	}

	mu.Lock()
	defer mu.Unlock()
	if callCount != 2 {
		t.Errorf("expected API called twice (no cache), got %d", callCount)
	}
}

func TestSandboxTransformer_FetchForPod_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer server.Close()

	tr := NewSandboxTransformer(server.URL, "", 2*time.Second)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "p1", Namespace: "ns",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "sandbox",
					Image: "test:latest",
					Env: []corev1.EnvVar{
						{Name: "TEMPLATE_NAME", Value: "tpl-1"},
					},
				},
			},
		},
	}
	if _, err := tr.FetchForPod(pod); err == nil {
		t.Errorf("expected error on API failure, got nil")
	}
}

// =============================================================================
// SandboxTransformer.FetchForBatchSandbox
// =============================================================================

func TestSandboxTransformer_FetchForBatchSandbox_Success(t *testing.T) {
	var captured sandboxTransformRequest
	server := transformAPIServer(t, sampleConfig("tpl-from-env"), http.StatusOK, &captured)
	defer server.Close()

	tr := NewSandboxTransformer(server.URL, "key-1", 2*time.Second)
	obj := newBatchSandboxObj("bs-1", "default", []interface{}{
		envVar("TEMPLATE_NAME", "tpl-from-env"),
	})
	annos, err := tr.FetchForBatchSandbox(obj)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if annos["e2b.dev/template-id"] != "tpl-from-env" {
		t.Errorf("expected template-id=tpl-from-env, got %q", annos["e2b.dev/template-id"])
	}
	if captured.TemplateName != "tpl-from-env" {
		t.Errorf("expected API called with templateName=tpl-from-env, got %q", captured.TemplateName)
	}
}

func TestSandboxTransformer_FetchForBatchSandbox_NoCache(t *testing.T) {
	var mu sync.Mutex
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sandboxTransformResponse{Sandbox: sampleConfig("tpl-x")})
	}))
	defer server.Close()

	tr := NewSandboxTransformer(server.URL, "", 2*time.Second)
	obj := newBatchSandboxObj("bs-1", "default", []interface{}{
		envVar("TEMPLATE_NAME", "tpl-x"),
	})

	_, _ = tr.FetchForBatchSandbox(obj)
	_, _ = tr.FetchForBatchSandbox(obj)

	mu.Lock()
	defer mu.Unlock()
	if callCount != 2 {
		t.Errorf("expected API called twice (no cache), got %d", callCount)
	}
}

func TestSandboxTransformer_FetchForBatchSandbox_NoTemplateName(t *testing.T) {
	server := transformAPIServer(t, sampleConfig("tpl-x"), http.StatusOK, nil)
	defer server.Close()

	tr := NewSandboxTransformer(server.URL, "", 2*time.Second)
	obj := newBatchSandboxObj("bs-1", "default", []interface{}{
		envVar("OTHER", "x"),
	})
	if _, err := tr.FetchForBatchSandbox(obj); err == nil {
		t.Errorf("expected error for missing TEMPLATE_NAME, got nil")
	}
}

func TestSandboxTransformer_FetchForBatchSandbox_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad", http.StatusBadRequest)
	}))
	defer server.Close()

	tr := NewSandboxTransformer(server.URL, "", 2*time.Second)
	obj := newBatchSandboxObj("bs-1", "default", []interface{}{
		envVar("TEMPLATE_NAME", "tpl-x"),
	})
	if _, err := tr.FetchForBatchSandbox(obj); err == nil {
		t.Errorf("expected error on API failure, got nil")
	}
}

// =============================================================================
// 无缓存: Pod 与 BatchSandbox 各自独立调用 API
// =============================================================================

func TestSandboxTransformer_NoSharedCache_PodAndBatchSandbox(t *testing.T) {
	var mu sync.Mutex
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sandboxTransformResponse{Sandbox: sampleConfig("shared-tpl")})
	}))
	defer server.Close()

	tr := NewSandboxTransformer(server.URL, "", 2*time.Second)

	// Pod 用 TEMPLATE_NAME=shared-tpl
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "p1", Namespace: "ns",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "sandbox",
					Image: "test:latest",
					Env: []corev1.EnvVar{
						{Name: "TEMPLATE_NAME", Value: "shared-tpl"},
					},
				},
			},
		},
	}
	if _, err := tr.FetchForPod(pod); err != nil {
		t.Fatalf("pod fetch failed: %v", err)
	}

	// BatchSandbox 用 TEMPLATE_NAME=shared-tpl, 无缓存, 应再次调用 API
	bs := newBatchSandboxObj("bs-1", "ns", []interface{}{
		envVar("TEMPLATE_NAME", "shared-tpl"),
	})
	if _, err := tr.FetchForBatchSandbox(bs); err != nil {
		t.Fatalf("batchsandbox fetch failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if callCount != 2 {
		t.Errorf("expected API called twice (no shared cache), got %d", callCount)
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
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "bench-pod", Namespace: "default"}}
	ar := newPodReview(admissionv1.Create, pod)
	tr := &mockTransformer{podAnnotations: sampleAnnotations}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		admitPod(ar, tr)
	}
}
