//go:build !mooncake || !linux || !cgo

package targetstore

import (
	"context"
	"fmt"
)

// NewMooncake 在普通制品中显式报错，避免配置看似成功、直到导入时才出现 nil
// provider。带 Mooncake 的 Linux CGO 制品由 mooncake_native.go 提供同名构造函数。
func NewMooncake(context.Context, string) (Store, error) {
	return nil, fmt.Errorf("Mooncake support is not included in this binary")
}
