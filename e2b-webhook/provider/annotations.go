package provider

// sandbox.cri/* 是 Webhook 与 CRI/Engine 之间的统一注解契约。
const (
	AnnBackend     = "sandbox.cri/backend"
	AnnSandboxID   = "sandbox.cri/sandbox-id"
	AnnTemplate    = "sandbox.cri/template"
	AnnBuild       = "sandbox.cri/build"
	AnnState       = "sandbox.cri/state"
	AnnIP          = "sandbox.cri/ip"
	AnnEndpoint    = "sandbox.cri/endpoint"
	AnnVCPU        = "sandbox.cri/vcpu"
	AnnMemoryMB    = "sandbox.cri/memory-mb"
	AnnDiskMB      = "sandbox.cri/disk-mb"
	AnnEnvdVersion = "sandbox.cri/envd-version"
	AnnEnvVars     = "sandbox.cri/env-vars"
	AnnMetadata    = "sandbox.cri/metadata"
)

// Unified 生成 sandbox.cri/* 基础键；空值跳过，不写空注解。
func Unified(backend, sandboxID, templateID string) map[string]string {
	out := make(map[string]string, 3)
	if backend != "" {
		out[AnnBackend] = backend
	}
	if sandboxID != "" {
		out[AnnSandboxID] = sandboxID
	}
	if templateID != "" {
		out[AnnTemplate] = templateID
	}
	return out
}

// Merge 把 src 合并进 dst，已存在的键不覆盖（用户已有键优先）。
func Merge(dst, src map[string]string) {
	for key, value := range src {
		if _, exists := dst[key]; exists {
			continue
		}
		dst[key] = value
	}
}
