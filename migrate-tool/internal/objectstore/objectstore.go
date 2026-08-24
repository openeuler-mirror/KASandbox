package objectstore

import (
	"context"
	"errors"
	"io"
	"time"
)

var ErrNotFound = errors.New("object not found")

// Info 描述一次读取绑定的对象版本。ETag 只参与版本比较，不当作内容摘要；
// Bundle 的内容完整性始终使用复制流计算出的 SHA-256。
type Info struct {
	Key          string    `json:"key"`
	Size         int64     `json:"size"`
	ETag         string    `json:"etag,omitempty"`
	VersionID    string    `json:"version_id,omitempty"`
	LastModified time.Time `json:"last_modified,omitempty"`
}

// SameObjectVersion 判断两次 Stat/Open 观察到的是否为对象存储里同一版本的
// 同一对象——"版本"指 S3 对象版本控制意义上的存储版本,与工具或数据格式的
// 跨版本兼容无关(格式层面由 Bundle manifest version、Header supportedVersion
// 与 PostgreSQL schema preflight 分别把关,均只接受同版本)。
//
// 用途:导出复制一个大对象期间,源对象可能被并发覆盖。复制完成后再次
// Stat,只有对象版本与开读时一致,这份字节才发布进 Bundle;否则整对象
// 重试并重新计算摘要。
func (i Info) SameObjectVersion(other Info) bool {
	if i.Key != other.Key || i.Size != other.Size {
		return false
	}
	// 真正的 VersionID 可以唯一指定不可变对象；未开启版本控制时退回到
	// Size + ETag + LastModified，三者必须同时一致。
	if hasImmutableVersionID(i.VersionID) || hasImmutableVersionID(other.VersionID) {
		return hasImmutableVersionID(i.VersionID) && i.VersionID == other.VersionID
	}
	if i.ETag == "" || other.ETag == "" || i.ETag != other.ETag {
		return false
	}
	return !i.LastModified.IsZero() && !other.LastModified.IsZero() && i.LastModified.Equal(other.LastModified)
}

func hasImmutableVersionID(versionID string) bool {
	// S3 用字面量 "null" 表示未版本化对象。它仍可被原地覆盖，不能按不可变
	// VersionID 处理，必须继续比较 ETag 和修改时间。
	return versionID != "" && versionID != "null"
}

type Store interface {
	Kind() string
	Stat(context.Context, string) (Info, error)
	// Open 的 versionID 为空时读取当前版本；非空时必须精确读取该版本。
	Open(context.Context, string, string) (io.ReadCloser, Info, error)
	// Put 只创建不存在的逻辑 key，并核对调用方给出的大小和 SHA-256。
	Put(context.Context, string, io.Reader, int64, string) (Info, error)
}
