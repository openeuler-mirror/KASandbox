package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/klog/v2"
)

// ────────────────────────────────────────────
// API 请求 / 响应结构体
// ────────────────────────────────────────────

// sandboxTransformRequest 是 POST /sandboxes/transform 的请求体。
// API 仅需要 templateName (template ID 或 alias), sandboxID 由服务端生成。
type sandboxTransformRequest struct {
	TemplateName string `json:"templateName"`
}

// sandboxTransformResponse 是 POST /sandboxes/transform 的响应体。
type sandboxTransformResponse struct {
	Sandbox   sandboxConfig `json:"sandbox"`
	StartTime string        `json:"startTime,omitempty"`
	EndTime   string        `json:"endTime,omitempty"`
}

// sandboxConfig 是 API 返回的沙箱配置。
// JSON tag 为驼峰, 与 e2b API 实际返回字段对齐。
type sandboxConfig struct {
	Alias               string          `json:"alias,omitempty"`
	BaseTemplateID      string          `json:"baseTemplateId"`
	TemplateID          string          `json:"templateId"`
	BuildID             string          `json:"buildId"`
	TeamID              string          `json:"teamId"`
	SandboxID           string          `json:"sandboxId,omitempty"`
	Vcpu                int             `json:"vcpu"`
	RAMMB               int             `json:"ramMb"`
	TotalDiskSizeMB     int             `json:"totalDiskSizeMb"`
	MaxSandboxLength    int             `json:"maxSandboxLength"`
	HugePages           bool            `json:"hugePages"`
	AutoPause           bool            `json:"autoPause"`
	Snapshot            bool            `json:"snapshot"`
	AllowInternetAccess *bool           `json:"allowInternetAccess,omitempty"`
	EnvdVersion         string          `json:"envdVersion"`
	KernelVersion       string          `json:"kernelVersion"`
	FirecrackerVersion  string          `json:"firecrackerVersion"`
	ExecutionID         string          `json:"executionId"`
	EnvdAccessToken     *string         `json:"envdAccessToken,omitempty"`
	Network             json.RawMessage `json:"network"`
}

// toE2BAnnotations 将 API 返回的沙箱配置转换为 e2b 注解 map。
func (cfg *sandboxConfig) toE2BAnnotations() map[string]string {
	annos := map[string]string{
		"e2b.dev/base_template_id":    cfg.BaseTemplateID,
		"e2b.dev/template-id":         cfg.TemplateID,
		"e2b.dev/build-id":            cfg.BuildID,
		"e2b.dev/team-id":             cfg.TeamID,
		"e2b.dev/vcpu":                strconv.Itoa(cfg.Vcpu),
		"e2b.dev/ram-mb":              strconv.Itoa(cfg.RAMMB),
		"e2b.dev/total-disk-size-mb":  strconv.Itoa(cfg.TotalDiskSizeMB),
		"e2b.dev/max-sandbox-length":  strconv.Itoa(cfg.MaxSandboxLength),
		"e2b.dev/huge-pages":          strconv.FormatBool(cfg.HugePages),
		"e2b.dev/auto-pause":          strconv.FormatBool(cfg.AutoPause),
		"e2b.dev/snapshot":            strconv.FormatBool(cfg.Snapshot),
		"e2b.dev/envd-version":        cfg.EnvdVersion,
		"e2b.dev/kernel-version":      cfg.KernelVersion,
		"e2b.dev/firecracker-version": cfg.FirecrackerVersion,
		"e2b.dev/execution-id":        cfg.ExecutionID,
	}

	// allow_internet_access: nil → "false"
	if cfg.AllowInternetAccess != nil {
		annos["e2b.dev/allow-internet"] = strconv.FormatBool(*cfg.AllowInternetAccess)
	} else {
		annos["e2b.dev/allow-internet"] = "false"
	}

	// envd_access_token: nil → ""
	if cfg.EnvdAccessToken != nil {
		annos["e2b.dev/envd-access-token"] = *cfg.EnvdAccessToken
	} else {
		annos["e2b.dev/envd-access-token"] = ""
	}

	// network: JSON marshal
	if len(cfg.Network) > 0 {
		annos["e2b.dev/network"] = string(cfg.Network)
	} else {
		annos["e2b.dev/network"] = `{"egress":{},"ingress":{}}`
	}

	// 不在 API 响应中的字段, 使用默认值
	annos["e2b.dev/env-vars"] = "{}"
	annos["e2b.dev/volume-mounts"] = "[]"
	annos["e2b.dev/auto-resume"] = `{"policy":"off"}`

	return annos
}

// ────────────────────────────────────────────
// SandboxTransformer
// ────────────────────────────────────────────

// SandboxTransformer 调用 e2b API 将沙箱配置转换为注解。
// 每次调用都会向 API 发起新请求, 不缓存结果。
type SandboxTransformer struct {
	apiURL     string
	apiKey     string
	httpClient *http.Client
}

// NewSandboxTransformer 创建新的沙箱转换器。
func NewSandboxTransformer(apiURL, apiKey string, timeout time.Duration) *SandboxTransformer {
	return &SandboxTransformer{
		apiURL: strings.TrimRight(apiURL, "/"),
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

// FetchForPod 从 pod 的 containers[0].env 中提取 TEMPLATE_NAME, 调用 API 获取沙箱配置, 映射为 e2b 注解。
// 无 TEMPLATE_NAME 时返回 (nil, nil), 表示放行不注入 (注解可能已由 BatchSandbox webhook 注入并继承)。
func (s *SandboxTransformer) FetchForPod(pod *corev1.Pod) (map[string]string, error) {
	templateID := extractTemplateNameFromPodEnv(pod)

	if templateID == "" {
		klog.V(3).Infof("pod %s/%s has no TEMPLATE_NAME env, skipping",
			pod.Namespace, pod.Name)
		return nil, nil
	}

	sandboxID := pod.Name

	annos, err := s.callTransformAPI(templateID, sandboxID)
	if err != nil {
		klog.Errorf("transform API failed for templateID=%s sandboxID=%s: %v",
			templateID, sandboxID, err)
		return nil, fmt.Errorf("transform API: %w", err)
	}

	return annos, nil
}

// FetchForBatchSandbox 从 BatchSandbox 资源的
// spec.template.spec.containers[0].env 中提取 TEMPLATE_NAME 的值作为 templateID,
// 调用 API 获取沙箱配置, 映射为 e2b 注解。
func (s *SandboxTransformer) FetchForBatchSandbox(obj *unstructured.Unstructured) (map[string]string, error) {
	templateID, err := extractTemplateNameFromBatchSandbox(obj)
	if err != nil {
		klog.Errorf("batchsandbox %s/%s: %v", obj.GetNamespace(), obj.GetName(), err)
		return nil, err
	}

	// sandboxID: 优先使用 BatchSandbox 名称, 否则回退到 UID
	sandboxID := obj.GetName()
	if sandboxID == "" {
		sandboxID = string(obj.GetUID())
		klog.V(3).Infof("batchsandbox has no name, using UID as sandboxID: %s", sandboxID)
	}

	annos, err := s.callTransformAPI(templateID, sandboxID)
	if err != nil {
		klog.Errorf("transform API failed for templateID=%s sandboxID=%s: %v",
			templateID, sandboxID, err)
		return nil, fmt.Errorf("transform API: %w", err)
	}

	return annos, nil
}

// extractTemplateNameFromPodEnv 从 pod 的 containers[0].env 中提取 TEMPLATE_NAME 的值。
// 不存在时返回空字符串。
func extractTemplateNameFromPodEnv(pod *corev1.Pod) string {
	if len(pod.Spec.Containers) == 0 {
		return ""
	}
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == "TEMPLATE_NAME" && env.Value != "" {
			return env.Value
		}
	}
	return ""
}

// hasAndroidSandboxEnv 检查 pod 的 containers[0].env 中是否存在 ANDROID_SANDBOX=true。
func hasAndroidSandboxEnv(pod *corev1.Pod) bool {
	if len(pod.Spec.Containers) == 0 {
		return false
	}
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == "ANDROID_SANDBOX" && env.Value == "true" {
			return true
		}
	}
	return false
}

func extractTemplateNameFromBatchSandbox(obj *unstructured.Unstructured) (string, error) {
	containers, found, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	if err != nil {
		return "", fmt.Errorf("read spec.template.spec.containers: %w", err)
	}
	if !found || len(containers) == 0 {
		return "", fmt.Errorf("spec.template.spec.containers not found or empty")
	}

	firstContainer, ok := containers[0].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("containers[0] is not an object")
	}

	envList, found, err := unstructured.NestedSlice(firstContainer, "env")
	if err != nil {
		return "", fmt.Errorf("read containers[0].env: %w", err)
	}
	if !found {
		return "", fmt.Errorf("containers[0].env not found")
	}

	for _, e := range envList {
		envVar, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		name, found, err := unstructured.NestedString(envVar, "name")
		if err != nil || !found {
			continue
		}
		if name == "TEMPLATE_NAME" {
			value, found, err := unstructured.NestedString(envVar, "value")
			if err != nil || !found || value == "" {
				return "", fmt.Errorf("TEMPLATE_NAME env var is empty")
			}
			return value, nil
		}
	}

	return "", fmt.Errorf("TEMPLATE_NAME env var not found in containers[0].env")
}

// callTransformAPI 调用 POST /sandboxes/transform。
// templateID 作为 templateName 发送给 API; sandboxID 仅用于日志 (API 服务端自行生成)。
func (s *SandboxTransformer) callTransformAPI(templateID, sandboxID string) (map[string]string, error) {
	url := s.apiURL + "/sandboxes/transform"
	klog.V(2).Infof("calling %s (templateName=%s sandboxID=%s)", url, templateID, sandboxID)

	reqBody := sandboxTransformRequest{
		TemplateName: templateID,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if s.apiKey != "" {
		req.Header.Set("X-API-Key", s.apiKey)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("api request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("api returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var apiResp sandboxTransformResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	annos := apiResp.Sandbox.toE2BAnnotations()

	klog.V(2).Infof("transform returned %d annotations for templateID=%s",
		len(annos), templateID)
	return annos, nil
}

// copyMap 返回 map 的浅拷贝, 避免调用方修改原始数据。
func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
