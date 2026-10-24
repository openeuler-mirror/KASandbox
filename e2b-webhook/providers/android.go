package providers

import (
	"context"

	"github.com/e2b/e2b-webhook/provider"
)

// AndroidProvider 只负责把 runtimeClass 指向 android；不调用任何后端 API，
// 也不产出注解。
type AndroidProvider struct{}

// NewAndroidProvider 创建 android Provider。
func NewAndroidProvider() *AndroidProvider { return &AndroidProvider{} }

func (p *AndroidProvider) Name() string    { return "android" }
func (p *AndroidProvider) Handler() string { return "android" }

// Resolve 返回只带 runtimeClass 的结果。
func (p *AndroidProvider) Resolve(context.Context, provider.Request) (*provider.Result, error) {
	return &provider.Result{RuntimeClass: p.Handler()}, nil
}
