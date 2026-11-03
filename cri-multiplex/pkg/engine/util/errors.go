package util

import (
	"errors"
	"os"
	"strings"
	"syscall"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// isCleanupNotFound 把"资源已经不存在"的各种表达归一为 true，供清理路径做幂等处理。
func isCleanupNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return true
	}
	if status.Code(err) == codes.NotFound {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "no such file") || strings.Contains(msg, "no chain/target/match") ||
		strings.Contains(msg, "bad rule")
}
