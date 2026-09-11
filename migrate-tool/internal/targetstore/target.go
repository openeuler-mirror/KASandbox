// Package targetstore 定义 Importer 写入目标对象存储所需的最小能力。
// Exporter 仍直接依赖 objectstore.Store，因为源端读取与目标端发布是两套不同契约。
package targetstore

import (
	"context"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/bundle"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/objectstore"
)

// Observation 是 Importer 规划一次对象写入所需的全部信息。
// identity 只由 File/S3 adapter 用于提交 Catalog 前的版本复核，Importer 不需要
// 理解 ETag、VersionID 等后端细节。
type Observation struct {
	Exists    bool
	Identical bool
	Digest    string
	Problem   string
	identity  objectstore.Info
}

// ClosedWriterVerifier closes the publishing client before verifying the
// complete set through a new client. Catalog commit must wait for this check.
type ClosedWriterVerifier interface {
	VerifyAfterClose(context.Context, []bundle.ObjectRecord) error
}

// Store 由创建它的调用方持有；Close 在 dry-run、apply 和提前返回路径上都必须调用。
type Store interface {
	Inspect(context.Context, bundle.ObjectRecord) (Observation, error)
	Publish(context.Context, bundle.ObjectRecord, string) (Observation, error)
	Recheck(context.Context, bundle.ObjectRecord, Observation) error
	Close()
}
