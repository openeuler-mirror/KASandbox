package main

import (
	"context"
	"errors"
	"strings"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	"github.com/e2b/e2b-webhook/provider"
)

var (
	errNoContainers     = errors.New("spec.template.spec.containers not found or empty")
	errInvalidContainer = errors.New("containers[0] is not an object")
	errNoEnv            = errors.New("containers[0].env not found")
	errEmptyTemplate    = errors.New("TEMPLATE_NAME env var is empty")
	errNoTemplate       = errors.New("TEMPLATE_NAME env var not found in containers[0].env")
)

// 旧版 Webhook 的默认选路：没有 runtimeClass 时，ANDROID_SANDBOX=true 视为 android，
// 否则默认 e2b（保留升级前的注入行为，避免存量负载掉到 containerd）。
const (
	legacyDefaultBackend = "e2b"
	legacyAndroidEnvName = "ANDROID_SANDBOX"
	legacyAndroidBackend = "android"
)

const (
	// resolveTimeout 是调用 Provider 的上限；超时按"只补 runtimeClass"降级。
	resolveTimeout = 2 * time.Second
	// annotationLimitBytes 是合并后注解总量的上限（k8s 对象大小约束）。
	annotationLimitBytes = 200 * 1024

	podAnnotationsPath      = "/metadata/annotations"
	podRuntimeClassNamePath = "/spec/runtimeClassName"
	bsAnnotationsPath       = "/spec/template/metadata/annotations"
	bsRuntimeClassNamePath  = "/spec/template/spec/runtimeClassName"
)

// requestFromPod 把 Pod 归一化成 Provider 视角的 Request。
func requestFromPod(pod *corev1.Pod) provider.Request {
	req := provider.Request{
		Kind:        "Pod",
		Namespace:   pod.Namespace,
		Name:        pod.Name,
		UID:         string(pod.UID),
		Labels:      pod.Labels,
		Annotations: pod.Annotations,
		Env:         podEnv(pod),
	}
	if pod.Spec.RuntimeClassName != nil {
		req.RuntimeClass = *pod.Spec.RuntimeClassName
	}
	req.SandboxID = sandboxIDFromAnnotations(req.Annotations)
	req.Template = req.Env["TEMPLATE_NAME"]
	return req
}

// requestFromBatchSandbox 把 BatchSandbox（CRD）归一化成 Provider 视角的 Request，
// runtimeClass / annotations / labels 读取模板字段（spec.template.*）。
func requestFromBatchSandbox(obj *unstructured.Unstructured) (provider.Request, error) {
	req := provider.Request{
		Kind:      "BatchSandbox",
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
		UID:       string(obj.GetUID()),
	}
	runtimeClass, found, err := unstructured.NestedString(obj.Object, "spec", "template", "spec", "runtimeClassName")
	if err != nil {
		return req, err
	}
	if found {
		req.RuntimeClass = runtimeClass
	}
	req.Annotations = getNestedStringMap(obj, "spec", "template", "metadata", "annotations")
	req.Labels = getNestedStringMap(obj, "spec", "template", "metadata", "labels")
	req.Env = batchSandboxEnv(obj)
	if template, err := extractTemplateNameFromBatchSandbox(obj); err == nil {
		req.Template = template
	}
	req.SandboxID = sandboxIDFromAnnotations(req.Annotations)
	return req, nil
}

// sandboxIDFromAnnotations 优先 sandbox.cri/sandbox-id，回退历史的 e2b.dev/sandbox-id。
func sandboxIDFromAnnotations(annotations map[string]string) string {
	if id := annotations[provider.AnnSandboxID]; id != "" {
		return id
	}
	return annotations["e2b.dev/sandbox-id"]
}

func podEnv(pod *corev1.Pod) map[string]string {
	if len(pod.Spec.Containers) == 0 {
		return nil
	}
	out := make(map[string]string, len(pod.Spec.Containers[0].Env))
	for _, env := range pod.Spec.Containers[0].Env {
		out[env.Name] = env.Value
	}
	return out
}

func batchSandboxEnv(obj *unstructured.Unstructured) map[string]string {
	containers, found, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	if err != nil || !found || len(containers) == 0 {
		return nil
	}
	first, ok := containers[0].(map[string]interface{})
	if !ok {
		return nil
	}
	envList, found, err := unstructured.NestedSlice(first, "env")
	if err != nil || !found {
		return nil
	}
	out := make(map[string]string, len(envList))
	for _, item := range envList {
		env, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := env["name"].(string)
		value, _ := env["value"].(string)
		out[name] = value
	}
	return out
}

// admitPod 处理 Pod 的 AdmissionReview。
func admitPod(ar *admissionv1.AdmissionReview, matcher *provider.Matcher) *admissionv1.AdmissionResponse {
	klog.V(2).Infof("admitPod: kind=%s operation=%s uid=%s", ar.Request.Kind, ar.Request.Operation, ar.Request.UID)
	if ar.Request.Operation != admissionv1.Create {
		klog.V(3).Infof("skip non-create operation: %s", ar.Request.Operation)
		return allowResponse(ar.Request.UID)
	}

	var pod corev1.Pod
	if _, _, err := deserializer.Decode(ar.Request.Object.Raw, nil, &pod); err != nil {
		klog.Errorf("failed to decode pod object: %v", err)
		return errorResponse(ar.Request.UID, "failed to decode pod: "+err.Error())
	}
	return admitObject(ar, matcher, requestFromPod(&pod), pod.Annotations, pod.Spec.RuntimeClassName != nil)
}

// admitBatchSandbox 处理 BatchSandbox 的 AdmissionReview。
func admitBatchSandbox(ar *admissionv1.AdmissionReview, matcher *provider.Matcher) *admissionv1.AdmissionResponse {
	klog.V(2).Infof("admitBatchSandbox: kind=%s operation=%s uid=%s", ar.Request.Kind, ar.Request.Operation, ar.Request.UID)
	if ar.Request.Operation != admissionv1.Create {
		klog.V(3).Infof("skip non-create operation: %s", ar.Request.Operation)
		return allowResponse(ar.Request.UID)
	}

	var obj unstructured.Unstructured
	if _, _, err := deserializer.Decode(ar.Request.Object.Raw, nil, &obj); err != nil {
		klog.Errorf("failed to decode batchsandbox object: %v", err)
		return errorResponse(ar.Request.UID, "failed to decode batchsandbox: "+err.Error())
	}
	req, err := requestFromBatchSandbox(&obj)
	if err != nil {
		klog.Errorf("failed to normalize batchsandbox %s/%s: %v", obj.GetNamespace(), obj.GetName(), err)
		return errorResponse(ar.Request.UID, "failed to normalize batchsandbox: "+err.Error())
	}
	runtimeClassSet := req.RuntimeClass != ""
	return admitObject(ar, matcher, req, req.Annotations, runtimeClassSet)
}

// admitObject 是两种 Kind 共用的选择/合并/降级逻辑。
func admitObject(ar *admissionv1.AdmissionReview, matcher *provider.Matcher, req provider.Request, existing map[string]string, runtimeClassSet bool) *admissionv1.AdmissionResponse {
	if matcher == nil {
		klog.Error("admission matcher is not initialized; allowing request without mutation")
		return allowResponse(ar.Request.UID)
	}

	selection, ok := matcher.Select(req)
	if !ok {
		explicit := strings.TrimSpace(req.RuntimeClass)
		if explicit == "" {
			explicit = strings.TrimSpace(req.Annotations[provider.AnnBackend])
		}
		if explicit != "" {
			klog.Warningf("%s %s/%s requests backend %q but no provider is registered; passing through without mutation",
				req.Kind, req.Namespace, req.Name, explicit)
			return allowResponse(ar.Request.UID)
		}
		// 没有任何显式信号时才走旧版默认选路：ANDROID_SANDBOX=true → android，否则 e2b。
		fallback, found := defaultProvider(matcher, req)
		if !found {
			return allowResponse(ar.Request.UID)
		}
		klog.V(2).Infof("%s %s/%s has no backend signal; defaulting to %s",
			req.Kind, req.Namespace, req.Name, fallback.Handler())
		selection = &provider.Selection{Provider: fallback, Source: "default"}
	}

	handler := selection.Provider.Handler()
	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()
	result, err := selection.Provider.Resolve(ctx, req)
	if err != nil {
		klog.Warningf("provider %s failed to resolve %s %s/%s: %v (only runtimeClass will be set)",
			handler, req.Kind, req.Namespace, req.Name, err)
		result = nil
	}

	patches := buildMergedPatch(req.Kind, req.Namespace, req.Name, existing, result, handler, runtimeClassSet)
	if len(patches) == 0 {
		klog.V(3).Infof("no patches needed for %s %s/%s", req.Kind, req.Namespace, req.Name)
		return allowResponse(ar.Request.UID)
	}
	klog.V(2).Infof("injecting %d patches into %s %s/%s", len(patches), req.Kind, req.Namespace, req.Name)
	return patchResponse(ar.Request.UID, patches)
}

// defaultProvider 复刻旧版入口逻辑：只带 ANDROID_SANDBOX=true 的 Pod 走 android，
// 其余没有 runtimeClass/backend 注解的对象默认 e2b。对应 Provider 未注册时返回 false（放行）。
func defaultProvider(matcher *provider.Matcher, req provider.Request) (provider.Provider, bool) {
	if strings.EqualFold(strings.TrimSpace(req.Env[legacyAndroidEnvName]), "true") {
		if p, ok := matcher.Resolve(legacyAndroidBackend); ok {
			return p, true
		}
	}
	p, ok := matcher.Resolve(legacyDefaultBackend)
	return p, ok
}

// buildMergedPatch 生成合并后的 patch：runtimeClass（未设置时）+ 注解增量。
// 用户已有注解不覆盖；合并后总大小超限时丢弃 Provider 的注解增量。
func buildMergedPatch(kind, namespace, name string, existing map[string]string, result *provider.Result, handler string, runtimeClassSet bool) []patchOperation {
	annotationsPath, runtimeClassPath := podAnnotationsPath, podRuntimeClassNamePath
	if kind == "BatchSandbox" {
		annotationsPath, runtimeClassPath = bsAnnotationsPath, bsRuntimeClassNamePath
	}

	var patches []patchOperation
	runtimeClass := ""
	var providerAnnotations map[string]string
	if result != nil {
		runtimeClass = result.RuntimeClass
		providerAnnotations = result.Annotations
	}
	if runtimeClass == "" {
		runtimeClass = handler
	}
	if !runtimeClassSet && runtimeClass != "" {
		patches = append(patches, patchOperation{Op: "add", Path: runtimeClassPath, Value: runtimeClass})
	}

	deltas := make(map[string]string, len(providerAnnotations))
	merged := copyMap(existing)
	for key, value := range providerAnnotations {
		if _, exists := existing[key]; exists {
			continue
		}
		deltas[key] = value
		merged[key] = value
	}
	if len(deltas) > 0 {
		if annotationBytes(merged) > annotationLimitBytes {
			klog.Warningf("%s %s/%s: merged annotations exceed %d bytes, dropping provider annotations",
				kind, namespace, name, annotationLimitBytes)
			return patches
		}
		patches = append(patches, buildPatch(existing, deltas, annotationsPath)...)
	}
	return patches
}

func annotationBytes(annotations map[string]string) int {
	total := 0
	for key, value := range annotations {
		total += len(key) + len(value)
	}
	return total
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

// copyMap 返回 m 的浅拷贝; 若 m 为 nil 则返回空 map。
func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for key, value := range m {
		out[key] = value
	}
	return out
}

// extractTemplateNameFromBatchSandbox 从 spec.template.spec.containers[0].env
// 提取 TEMPLATE_NAME；缺失或为空时返回错误。
func extractTemplateNameFromBatchSandbox(obj *unstructured.Unstructured) (string, error) {
	containers, found, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	if err != nil {
		return "", err
	}
	if !found || len(containers) == 0 {
		return "", errNoContainers
	}
	first, ok := containers[0].(map[string]interface{})
	if !ok {
		return "", errInvalidContainer
	}
	envList, found, err := unstructured.NestedSlice(first, "env")
	if err != nil || !found {
		return "", errNoEnv
	}
	for _, item := range envList {
		env, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := env["name"].(string)
		if name != "TEMPLATE_NAME" {
			continue
		}
		value, _ := env["value"].(string)
		if value == "" {
			return "", errEmptyTemplate
		}
		return value, nil
	}
	return "", errNoTemplate
}

// allowResponse 构造放行响应（不带 patch）。
func allowResponse(uid types.UID) *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{Allowed: true, UID: uid}
}
