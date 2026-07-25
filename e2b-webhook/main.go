package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

var (
	runtimeScheme = runtime.NewScheme()
	codecs        = serializer.NewCodecFactory(runtimeScheme)
	deserializer  = codecs.UniversalDeserializer()
)

// annotationFetcher 是全局的注解获取器, 在 main 中初始化。
var annotationFetcher annotationTransformer

// patchOperation 描述一个 JSON Patch 操作。
type patchOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

func init() {
	_ = corev1.AddToScheme(runtimeScheme)
	_ = admissionv1.AddToScheme(runtimeScheme)
}

// buildPatch 构造注入 e2b 注解所需的 JSON Patch。
// basePath 指定 annotations 所在的 JSON Pointer 路径, 例如:
//   - Pod:                "/metadata/annotations"
//   - BatchSandbox 模板:  "/spec/template/metadata/annotations"
//
// 若目标当前没有 annotations，则用 add 添加整个 map；
// 否则对每个不存在的 e2b 注解执行 add。
func buildPatch(existingAnnotations, e2bAnnotations map[string]string, basePath string) []patchOperation {
	var patches []patchOperation

	if len(e2bAnnotations) == 0 {
		return nil
	}

	if len(existingAnnotations) == 0 {
		// basePath 不存在，整体添加
		patches = append(patches, patchOperation{
			Op:    "add",
			Path:  basePath,
			Value: e2bAnnotations,
		})
		return patches
	}

	// annotations 已存在，逐个添加缺失的 e2b 注解
	for k, v := range e2bAnnotations {
		if _, ok := existingAnnotations[k]; ok {
			// 已存在同名注解，不覆盖
			continue
		}
		patches = append(patches, patchOperation{
			Op:    "add",
			Path:  basePath + "/" + escapeJSONPointer(k),
			Value: v,
		})
	}
	return patches
}

// escapeJSONPointer 转义 JSON Pointer 中的特殊字符 (RFC 6901)。
func escapeJSONPointer(s string) string {
	// '~' 必须先转义为 ~0，'/' 转义为 ~1
	s = strings.ReplaceAll(s, "~", "~0")
	s = strings.ReplaceAll(s, "/", "~1")
	return s
}

// errorResponse 构造一个拒绝请求的 AdmissionResponse。
func errorResponse(uid types.UID, msg string) *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{
		Allowed: false,
		UID:     uid,
		Result: &metav1.Status{
			Status:  metav1.StatusFailure,
			Message: msg,
		},
	}
}

// patchResponse 构造一个带 JSON Patch 的 AdmissionResponse。
// patches 序列化失败时返回错误响应。
func patchResponse(uid types.UID, patches []patchOperation) *admissionv1.AdmissionResponse {
	patchBytes, err := json.Marshal(patches)
	if err != nil {
		klog.Errorf("failed to marshal patches: %v", err)
		return errorResponse(uid, fmt.Sprintf("failed to marshal patch: %v", err))
	}
	patchType := admissionv1.PatchTypeJSONPatch
	return &admissionv1.AdmissionResponse{
		Allowed:   true,
		UID:       uid,
		Patch:     patchBytes,
		PatchType: &patchType,
	}
}

// annotationTransformer 将 Pod / BatchSandbox 转换为 e2b 注解。
type annotationTransformer interface {
	FetchForPod(pod *corev1.Pod) (map[string]string, error)
	FetchForBatchSandbox(obj *unstructured.Unstructured) (map[string]string, error)
}

// admitPod 处理 Pod 的 AdmissionReview，返回 AdmissionResponse。
func admitPod(ar *admissionv1.AdmissionReview, transformer annotationTransformer) *admissionv1.AdmissionResponse {
	klog.V(2).Infof("admitPod: kind=%s operation=%s uid=%s",
		ar.Request.Kind, ar.Request.Operation, ar.Request.UID)

	if ar.Request.Operation != admissionv1.Create {
		klog.V(3).Infof("skip non-create operation: %s", ar.Request.Operation)
		return &admissionv1.AdmissionResponse{Allowed: true, UID: ar.Request.UID}
	}

	var pod corev1.Pod
	if _, _, err := deserializer.Decode(ar.Request.Object.Raw, nil, &pod); err != nil {
		klog.Errorf("failed to decode pod object: %v", err)
		return errorResponse(ar.Request.UID, fmt.Sprintf("failed to decode pod: %v", err))
	}

	klog.V(2).Infof("handling pod %s/%s", pod.Namespace, pod.Name)

	// ANDROID_SANDBOX=true: 仅注入 runtimeClassName=android 后直接返回, 不调用 API 也不注入 e2b 注解
	if hasAndroidSandboxEnv(&pod) {
		klog.V(2).Infof("injecting runtimeClassName=android for pod %s/%s (skip e2b)", pod.Namespace, pod.Name)
		return patchResponse(ar.Request.UID, []patchOperation{{
			Op: "add", Path: "/spec/runtimeClassName", Value: "android",
		}})
	}

	// 从 e2b API 动态获取注解 (带容错, 失败时返回空注解)
	e2bAnnotations, err := transformer.FetchForPod(&pod)
	if err != nil {
		klog.Warningf("fetch annotations with error (using fallback): %v", err)
	}

	patches := buildPatch(pod.Annotations, e2bAnnotations, "/metadata/annotations")

	// 注入 runtimeClassName: e2b (若未设置)
	if pod.Spec.RuntimeClassName == nil {
		patches = append(patches, patchOperation{
			Op: "add", Path: "/spec/runtimeClassName", Value: "e2b",
		})
		klog.V(2).Infof("injecting runtimeClassName=e2b for pod %s/%s", pod.Namespace, pod.Name)
	}

	if len(patches) == 0 {
		klog.V(3).Infof("no patches needed for pod %s/%s", pod.Namespace, pod.Name)
		return &admissionv1.AdmissionResponse{Allowed: true, UID: ar.Request.UID}
	}

	klog.V(2).Infof("injecting %d patches into pod %s/%s", len(patches), pod.Namespace, pod.Name)
	return patchResponse(ar.Request.UID, patches)
}

// admitBatchSandbox 处理 BatchSandbox 的 AdmissionReview，返回 AdmissionResponse。
// BatchSandbox 是 CRD, 使用 unstructured 解码; 注解注入到
// spec.template.metadata.annotations, 以便传播到其创建的 Pod。
func admitBatchSandbox(ar *admissionv1.AdmissionReview, transformer annotationTransformer) *admissionv1.AdmissionResponse {
	klog.V(2).Infof("admitBatchSandbox: kind=%s operation=%s uid=%s",
		ar.Request.Kind, ar.Request.Operation, ar.Request.UID)

	if ar.Request.Operation != admissionv1.Create {
		klog.V(3).Infof("skip non-create operation: %s", ar.Request.Operation)
		return &admissionv1.AdmissionResponse{Allowed: true, UID: ar.Request.UID}
	}

	// 解析 BatchSandbox 对象 (unstructured, 因为是 CRD 未注册到 scheme)
	var obj unstructured.Unstructured
	if _, _, err := deserializer.Decode(ar.Request.Object.Raw, nil, &obj); err != nil {
		klog.Errorf("failed to decode batchsandbox object: %v", err)
		return errorResponse(ar.Request.UID, fmt.Sprintf("failed to decode batchsandbox: %v", err))
	}

	klog.V(2).Infof("handling batchsandbox %s/%s", obj.GetNamespace(), obj.GetName())

	// 调用 e2b API 获取沙箱配置 (带容错, 失败时返回空注解)
	e2bAnnotations, err := transformer.FetchForBatchSandbox(&obj)
	if err != nil {
		klog.Warningf("fetch annotations with error (using fallback): %v", err)
	}

	// 注解注入到 Pod 模板的 annotations, 以便传播到 BatchSandbox 创建的 Pod
	existingAnnotations := getNestedStringMap(&obj, "spec", "template", "metadata", "annotations")
	patches := buildPatch(existingAnnotations, e2bAnnotations, "/spec/template/metadata/annotations")

	if len(patches) == 0 {
		klog.V(3).Infof("no patches needed for batchsandbox %s/%s", obj.GetNamespace(), obj.GetName())
		return &admissionv1.AdmissionResponse{Allowed: true, UID: ar.Request.UID}
	}

	klog.V(2).Infof("injecting %d patches into batchsandbox %s/%s",
		len(patches), obj.GetNamespace(), obj.GetName())
	return patchResponse(ar.Request.UID, patches)
}

// getNestedStringMap 从 unstructured 对象中按路径获取 map[string]string。
// 路径不存在或类型不匹配时返回 nil。
func getNestedStringMap(obj *unstructured.Unstructured, fields ...string) map[string]string {
	nested, found, err := unstructured.NestedStringMap(obj.Object, fields...)
	if err != nil || !found {
		return nil
	}
	return nested
}

// serveMutate 是 /mutate 端点的 HTTP handler。
func serveMutate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		klog.Errorf("failed to read request body: %v", err)
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		klog.Errorf("invalid content-type: %s", contentType)
		http.Error(w, "invalid content-type", http.StatusUnsupportedMediaType)
		return
	}

	var admissionReview admissionv1.AdmissionReview
	if _, _, err := deserializer.Decode(body, nil, &admissionReview); err != nil {
		klog.Errorf("failed to decode admission review: %v", err)
		http.Error(w, fmt.Sprintf("failed to decode: %v", err), http.StatusBadRequest)
		return
	}

	if admissionReview.Request == nil {
		klog.Error("admission review request is nil")
		http.Error(w, "admission review request is nil", http.StatusBadRequest)
		return
	}

	// 处理 Pod 和 BatchSandbox, 其它类型直接放行
	switch admissionReview.Request.Kind.Kind {
	case "Pod":
		admissionReview.Response = admitPod(&admissionReview, annotationFetcher)
	case "BatchSandbox":
		admissionReview.Response = admitBatchSandbox(&admissionReview, annotationFetcher)
	default:
		klog.V(3).Infof("skip unhandled kind: %s", admissionReview.Request.Kind.Kind)
		admissionReview.Response = &admissionv1.AdmissionResponse{
			Allowed: true,
			UID:     admissionReview.Request.UID,
		}
	}

	respBytes, err := json.Marshal(&admissionReview)
	if err != nil {
		klog.Errorf("failed to marshal response: %v", err)
		http.Error(w, "failed to marshal response", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(respBytes); err != nil {
		klog.Errorf("failed to write response: %v", err)
	}
}

// serveHealth 是健康检查端点。
func serveHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func main() {
	certFile := envOr("TLS_CERT_FILE", "/tls/tls.crt")
	keyFile := envOr("TLS_KEY_FILE", "/tls/tls.key")
	port := envOr("PORT", "8443")

	// e2b API 配置
	apiURL := envOr("E2B_API_URL", "http://api.e2b.svc.cluster.local:3000")
	apiKey := envOr("E2B_API_KEY", "")
	apiTimeout := envDurationOr("E2B_API_TIMEOUT", 5*time.Second)

	// 创建沙箱转换器 (调用 POST /sandboxes/transform, 每次发起新请求)
	transformer := NewSandboxTransformer(apiURL, apiKey, apiTimeout)
	annotationFetcher = transformer

	mux := http.NewServeMux()
	mux.HandleFunc("/mutate", serveMutate)
	mux.HandleFunc("/healthz", serveHealth)

	server := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}

	klog.Infof("starting e2b-webhook server on :%s (cert=%s key=%s)", port, certFile, keyFile)
	klog.Infof("transform API: %s/sandboxes/transform (timeout=%s)",
		apiURL, apiTimeout)

	if err := server.ListenAndServeTLS(certFile, keyFile); err != nil {
		klog.Fatalf("server failed: %v", err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDurationOr(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		klog.Warningf("invalid duration %s=%s, using default %s: %v", key, v, def, err)
		return def
	}
	return d
}
