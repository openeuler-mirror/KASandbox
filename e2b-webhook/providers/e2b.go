package providers

import (
	"context"

	"github.com/e2b/e2b-webhook/provider"
)

// E2BProvider 把 e2b API 的 transform 结果翻译成注解：
// e2b.dev/*（与历史逐字一致）+ sandbox.cri/* 统一键子集。
type E2BProvider struct {
	transformer *SandboxTransformer
}

// NewE2BProvider 创建 e2b Provider；transformer 不能为 nil。
func NewE2BProvider(t *SandboxTransformer) *E2BProvider {
	return &E2BProvider{transformer: t}
}

func (p *E2BProvider) Name() string    { return "e2b" }
func (p *E2BProvider) Handler() string { return "e2b" }

// Resolve 调用 transform API 并产出注解。
// Template 为空时返回 (nil, nil)：webhook 走"只补 runtimeClass"的降级路径。
func (p *E2BProvider) Resolve(ctx context.Context, req provider.Request) (*provider.Result, error) {
	cfg, err := p.transformer.Transform(ctx, req.Template)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, nil
	}
	sandboxID := req.SandboxID
	if sandboxID == "" {
		sandboxID = cfg.SandboxID
	}
	annotations := cfg.toE2BAnnotations()
	provider.Merge(annotations, cfg.toUnifiedAnnotations(p.Handler(), sandboxID))
	return &provider.Result{Annotations: annotations, RuntimeClass: p.Handler()}, nil
}
