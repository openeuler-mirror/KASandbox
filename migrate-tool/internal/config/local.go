package config

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/catalog"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/objectstore"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/targetstore"
)

func OpenCatalog(endpoint string) (catalog.Catalog, error) {
	if strings.HasPrefix(endpoint, "postgres://") || strings.HasPrefix(endpoint, "postgresql://") {
		return catalog.NewPostgres(endpoint)
	}
	path, err := localPath(endpoint)
	if err != nil {
		return nil, fmt.Errorf("catalog endpoint: %w", err)
	}
	return catalog.NewFile(path)
}

func OpenStore(ctx context.Context, endpoint string) (objectstore.Store, error) {
	if strings.HasPrefix(endpoint, "s3://") {
		options, err := parseS3(endpoint)
		if err != nil {
			return nil, err
		}
		return objectstore.NewS3Store(ctx, options)
	}
	path, err := localPath(endpoint)
	if err != nil {
		return nil, fmt.Errorf("object store endpoint: %w", err)
	}
	return objectstore.NewFileStore(path)
}

// OpenTargetStore 只用于 import。File/S3 继续复用成熟的 objectstore 实现；
// Mooncake 只暴露目标端发布能力，因此 export 不会意外获得不完整的读取支持。
func OpenTargetStore(ctx context.Context, endpoint string) (targetstore.Store, error) {
	if strings.HasPrefix(endpoint, "mooncake://") {
		namespace, err := parseMooncake(endpoint)
		if err != nil {
			return nil, err
		}
		return targetstore.NewMooncake(ctx, namespace)
	}
	store, err := OpenStore(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	return targetstore.FromObjectStore(store), nil
}

func parseMooncake(endpoint string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse Mooncake endpoint: %w", err)
	}
	if parsed.Scheme != "mooncake" || parsed.Host == "" {
		return "", fmt.Errorf("Mooncake URI must use mooncake://NAMESPACE")
	}
	// namespace 不是连接地址。拒绝 host:port,否则 master 地址被误当 namespace,
	// 对象全部发布到错误前缀下,E2B 运行时将读不到任何迁移对象。
	if strings.ContainsRune(parsed.Host, ':') {
		return "", fmt.Errorf("Mooncake namespace must not contain a port; connection settings come from MOONCAKE_* environment variables")
	}
	// URI 只承载 namespace；连接地址、协议和设备仍使用 E2B Job 的
	// MOONCAKE_* 环境变量，避免同一配置出现两个来源。
	if parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", fmt.Errorf("Mooncake URI must contain only a namespace")
	}
	return parsed.Host, nil
}

func parseS3(endpoint string) (objectstore.S3Config, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return objectstore.S3Config{}, fmt.Errorf("parse S3 endpoint: %w", err)
	}
	if parsed.Scheme != "s3" || parsed.Opaque != "" {
		return objectstore.S3Config{}, fmt.Errorf("S3 URI must use s3://")
	}
	if parsed.User != nil {
		return objectstore.S3Config{}, fmt.Errorf("S3 URI must not contain credentials")
	}
	if parsed.Host == "" {
		return objectstore.S3Config{}, fmt.Errorf("S3 URI requires a bucket")
	}
	if parsed.Port() != "" || strings.ContainsRune(parsed.Host, ':') {
		return objectstore.S3Config{}, fmt.Errorf("S3 URI bucket must not contain a port")
	}
	if strings.ContainsRune(endpoint, '#') {
		return objectstore.S3Config{}, fmt.Errorf("S3 URI must not contain a fragment")
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return objectstore.S3Config{}, fmt.Errorf("parse S3 URI query: %w", err)
	}
	for key, values := range query {
		switch key {
		case "endpoint", "path_style", "region":
		default:
			return objectstore.S3Config{}, fmt.Errorf("unsupported S3 URI query parameter %q", key)
		}
		if len(values) != 1 {
			return objectstore.S3Config{}, fmt.Errorf("S3 URI query parameter %q must appear once", key)
		}
	}

	// 显式 URI 参数优先，其次采用 AWS SDK 常用环境变量，最后使用 S3 的
	// 通用默认 Region。自定义 endpoint 通常是 MinIO，默认启用 path-style。
	region, hasRegion := query["region"]
	regionValue := ""
	if hasRegion {
		regionValue = region[0]
		if regionValue == "" {
			return objectstore.S3Config{}, fmt.Errorf("S3 region must not be empty")
		}
	} else {
		regionValue = os.Getenv("AWS_REGION")
	}
	if regionValue == "" {
		regionValue = os.Getenv("AWS_DEFAULT_REGION")
	}
	if regionValue == "" {
		regionValue = "us-east-1"
	}

	baseEndpoint := ""
	if values, ok := query["endpoint"]; ok {
		baseEndpoint = values[0]
		if baseEndpoint == "" {
			return objectstore.S3Config{}, fmt.Errorf("S3 endpoint must not be empty")
		}
	} else {
		baseEndpoint = os.Getenv("TM_S3_ENDPOINT")
	}
	pathStyle := baseEndpoint != ""
	if values, ok := query["path_style"]; ok {
		value, err := strconv.ParseBool(values[0])
		if err != nil {
			return objectstore.S3Config{}, fmt.Errorf("parse path_style: %w", err)
		}
		pathStyle = value
	}

	prefix := strings.TrimPrefix(parsed.Path, "/")
	options := objectstore.S3Config{
		Bucket:    parsed.Host,
		Prefix:    prefix,
		Region:    regionValue,
		Endpoint:  baseEndpoint,
		PathStyle: pathStyle,
	}
	if err := options.Validate(); err != nil {
		return objectstore.S3Config{}, fmt.Errorf("S3 URI: %w", err)
	}

	return options, nil
}

func localPath(endpoint string) (string, error) {
	if endpoint == "" {
		return "", fmt.Errorf("endpoint is required")
	}
	if !strings.Contains(endpoint, "://") {
		// 单斜杠笔误(mooncake:/ns、s3:/bucket)不能静默当成本地路径,否则
		// 对象会写进名为 "mooncake:" 的本地目录而命令仍报告成功。单字母
		// 前缀是 Windows 盘符,不受影响。
		if scheme, _, ok := strings.Cut(endpoint, ":"); ok && len(scheme) > 1 && isSchemeLike(scheme) {
			return "", fmt.Errorf("endpoint %q looks like a URI with a malformed scheme; use SCHEME:// or a plain path", endpoint)
		}
		return endpoint, nil
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse %q: %w", endpoint, err)
	}
	if parsed.Scheme != "file" || (parsed.Host != "" && parsed.Host != "localhost") {
		return "", fmt.Errorf("Demo supports only local paths and file:// endpoints, got %q", endpoint)
	}
	// url.Parse 已经对 Path 做过一次转义解码；再次 PathUnescape 会把文件名中
	// 字面的 "%2F" 错误地变成目录分隔符。
	value := parsed.Path
	if runtime.GOOS == "windows" && len(value) >= 3 && value[0] == '/' && value[2] == ':' {
		value = value[1:]
	}
	return filepath.FromSlash(value), nil
}

func isSchemeLike(value string) bool {
	for index, char := range value {
		switch {
		case char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z':
		case index > 0 && (char >= '0' && char <= '9' || char == '+' || char == '-' || char == '.'):
		default:
			return false
		}
	}
	return true
}
