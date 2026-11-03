// Package providers 实现 webhook 的各后端 Provider。
//
// 本文件承载 e2b API 的 transform 客户端与 sandboxConfig→e2b.dev/* 注解映射。
package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"k8s.io/klog/v2"
)

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
// JSON tag 为驼峰, 与 e2b API 实际返回字段对齐 (api.SandboxConfigGrpc)。
type sandboxConfig struct {
	Alias               string                   `json:"alias,omitempty"`
	AllowInternetAccess *bool                    `json:"allowInternetAccess,omitempty"`
	AutoPause           bool                     `json:"autoPause"`
	AutoResume          *sandboxAutoResumeConfig `json:"autoResume,omitempty"`
	BaseTemplateID      string                   `json:"baseTemplateId"`
	BuildID             string                   `json:"buildId"`
	EnvVars             map[string]string        `json:"envVars,omitempty"`
	EnvdAccessToken     *string                  `json:"envdAccessToken,omitempty"`
	EnvdVersion         string                   `json:"envdVersion"`
	ExecutionID         string                   `json:"executionId"`
	FirecrackerVersion  string                   `json:"firecrackerVersion"`
	HugePages           bool                     `json:"hugePages"`
	KernelVersion       string                   `json:"kernelVersion"`
	MaxSandboxLength    int                      `json:"maxSandboxLength"`
	Metadata            map[string]string        `json:"metadata,omitempty"`
	Network             *sandboxNetworkConfig    `json:"network,omitempty"`
	RAMMB               int                      `json:"ramMb"`
	SandboxID           string                   `json:"sandboxId,omitempty"`
	Snapshot            bool                     `json:"snapshot"`
	TeamID              string                   `json:"teamId"`
	TemplateID          string                   `json:"templateId"`
	TotalDiskSizeMB     int                      `json:"totalDiskSizeMb"`
	Vcpu                int                      `json:"vcpu"`
	VolumeMounts        []sandboxVolumeMount     `json:"volumeMounts,omitempty"`
}

// sandboxAutoResumeConfig 镜像 api.SandboxAutoResumeConfigGrpc。
type sandboxAutoResumeConfig struct {
	Policy string `json:"policy,omitempty"`
}

// sandboxNetworkConfig 镜像 api.SandboxNetworkConfigGrpc。
type sandboxNetworkConfig struct {
	Egress  *sandboxNetworkEgressConfig  `json:"egress,omitempty"`
	Ingress *sandboxNetworkIngressConfig `json:"ingress,omitempty"`
}

type sandboxNetworkEgressConfig struct {
	AllowedCidrs   []string `json:"allowedCidrs,omitempty"`
	AllowedDomains []string `json:"allowedDomains,omitempty"`
	DeniedCidrs    []string `json:"deniedCidrs,omitempty"`
}

type sandboxNetworkIngressConfig struct {
	MaskRequestHost    *string `json:"maskRequestHost,omitempty"`
	TrafficAccessToken *string `json:"trafficAccessToken,omitempty"`
}

// sandboxVolumeMount 镜像 api.SandboxVolumeMountGrpc。
type sandboxVolumeMount struct {
	MountPath *string `json:"mountPath,omitempty"`
	ReadOnly  *bool   `json:"readOnly,omitempty"`
	VolumeID  *string `json:"volumeId,omitempty"`
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

	// allow_internet_access: nil → "true"
	if cfg.AllowInternetAccess != nil {
		annos["e2b.dev/allow-internet"] = strconv.FormatBool(*cfg.AllowInternetAccess)
	} else {
		annos["e2b.dev/allow-internet"] = "true"
	}

	// envd_access_token: nil → ""
	if cfg.EnvdAccessToken != nil {
		annos["e2b.dev/envd-access-token"] = *cfg.EnvdAccessToken
	} else {
		annos["e2b.dev/envd-access-token"] = ""
	}

	// network: 优先使用 API 返回值, 为空时 fallback 到空结构
	if cfg.Network != nil {
		if netBytes, err := json.Marshal(cfg.Network); err == nil {
			annos["e2b.dev/network"] = string(netBytes)
		} else {
			annos["e2b.dev/network"] = `{"egress":{},"ingress":{}}`
		}
	} else {
		annos["e2b.dev/network"] = `{"egress":{},"ingress":{}}`
	}

	// env-vars: 优先使用 API 返回值, 为空时 fallback 到 "{}"
	if len(cfg.EnvVars) > 0 {
		if envBytes, err := json.Marshal(cfg.EnvVars); err == nil {
			annos["e2b.dev/env-vars"] = string(envBytes)
		} else {
			annos["e2b.dev/env-vars"] = "{}"
		}
	} else {
		annos["e2b.dev/env-vars"] = "{}"
	}

	// volume-mounts: 优先使用 API 返回值, 为空时 fallback 到 "[]"
	if len(cfg.VolumeMounts) > 0 {
		if mountBytes, err := json.Marshal(cfg.VolumeMounts); err == nil {
			annos["e2b.dev/volume-mounts"] = string(mountBytes)
		} else {
			annos["e2b.dev/volume-mounts"] = "[]"
		}
	} else {
		annos["e2b.dev/volume-mounts"] = "[]"
	}

	// auto-resume: 优先使用 API 返回值, 为空时 fallback 到 {"policy":"off"}
	if cfg.AutoResume != nil && cfg.AutoResume.Policy != "" {
		annos["e2b.dev/auto-resume"] = fmt.Sprintf(`{"policy":%q}`, cfg.AutoResume.Policy)
	} else {
		annos["e2b.dev/auto-resume"] = `{"policy":"off"}`
	}

	// metadata: 优先使用 API 返回值, 为空时不设置注解
	if len(cfg.Metadata) > 0 {
		if metaBytes, err := json.Marshal(cfg.Metadata); err == nil {
			annos["e2b.dev/metadata"] = string(metaBytes)
		}
	}

	// sandbox-id: API 服务端生成的稳定逻辑 sandbox ID。注入后 cri-multiplex
	// 优先于 Pod UID 派生 ID 使用, 是 Pause/Resume 跨 Pod 重建保持身份的关键;
	// 同时作为 webhook 调 E2B API internal 快照接口 (pause-snapshot 等) 的身份键。
	if cfg.SandboxID != "" {
		annos["e2b.dev/sandbox-id"] = cfg.SandboxID
	}

	return annos
}

// ToE2BAnnotations 是 toE2BAnnotations 的导出别名，供过渡期的 webhook 适配器使用。
func (cfg *sandboxConfig) ToE2BAnnotations() map[string]string {
	return cfg.toE2BAnnotations()
}

// ────────────────────────────────────────────
// SandboxTransformer
// ────────────────────────────────────────────

// SandboxTransformer 调用 e2b API 把模板名转换为沙箱配置。
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

// Transform 调用 POST /sandboxes/transform 获取模板对应的沙箱配置。
// templateName 为空时返回 (nil, nil)：调用方走"只补 runtimeClass"的降级路径。
func (s *SandboxTransformer) Transform(ctx context.Context, templateName string) (*sandboxConfig, error) {
	if templateName == "" {
		klog.V(3).Infof("empty template name, skipping transform")
		return nil, nil
	}
	cfg, err := s.callTransformAPI(ctx, templateName)
	if err != nil {
		klog.Errorf("transform API failed for templateName=%s: %v", templateName, err)
		return nil, fmt.Errorf("transform API: %w", err)
	}
	return cfg, nil
}

// callTransformAPI 调用 POST /sandboxes/transform。
func (s *SandboxTransformer) callTransformAPI(ctx context.Context, templateName string) (*sandboxConfig, error) {
	url := s.apiURL + "/sandboxes/transform"
	klog.V(2).Infof("calling %s (templateName=%s)", url, templateName)

	bodyBytes, err := json.Marshal(sandboxTransformRequest{TemplateName: templateName})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
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
	klog.V(2).Infof("transform returned config for templateName=%s", templateName)
	return &apiResp.Sandbox, nil
}
