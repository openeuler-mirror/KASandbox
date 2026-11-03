// Package provider 定义 webhook 的后端选择与注解产出契约。
// Webhook 主流程只依赖这里的接口，不认识任何后端前缀。
package provider

import "context"

// Request 是归一化后的准入对象视图（Pod 与 BatchSandbox 共用）。
type Request struct {
	Kind         string
	Namespace    string
	Name         string
	UID          string
	RuntimeClass string
	SandboxID    string
	Template     string
	Labels       map[string]string
	Annotations  map[string]string
	Env          map[string]string
}

// Result 是 Provider 的产出：要注入的注解与建议的 runtimeClass。
type Result struct {
	Annotations  map[string]string
	RuntimeClass string
}

// Provider 是后端在 webhook 侧的适配器。
type Provider interface {
	Name() string
	Handler() string
	Resolve(ctx context.Context, req Request) (*Result, error)
}

// Selection 记录命中哪个 Provider 以及命中的信号来源。
type Selection struct {
	Provider Provider
	Source   string
}
