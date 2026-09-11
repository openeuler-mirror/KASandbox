package bundle

import (
	"encoding/json"
	"time"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/objectstore"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/selection"
)

const (
	Format                 = "e2b-template-migration"
	FormatVersion          = 1
	MultiDiskFormatVersion = 2

	NamespaceTeam    = "team-scoped"
	NamespaceGlobal  = "global"
	NamespaceLiteral = "literal"
)

type Manifest struct {
	Format        string             `json:"format"`
	FormatVersion int                `json:"format_version"`
	ToolVersion   string             `json:"tool_version"`
	CreatedAt     time.Time          `json:"created_at"`
	Source        Source             `json:"source"`
	Selection     Selection          `json:"selection"`
	Counts        Counts             `json:"counts"`
	Records       []RecordDescriptor `json:"records"`
	Objects       []ObjectRecord     `json:"objects"`
	// BundleDigest 覆盖 Manifest（本字段清空后）、全部 record 文件及对象清单，
	// 使导入计划可以稳定绑定到一个 Bundle。
	BundleDigest string `json:"bundle_digest"`
	// Omitted for v1 to preserve the exact legacy manifest digest encoding.
	BuildLayouts []BuildLayout `json:"build_layouts,omitempty"`
}

type BuildLayout struct {
	BuildID string   `json:"build_id"`
	OSType  string   `json:"os_type"`
	Disks   []string `json:"disks"`
}

type Source struct {
	SchemaVersion   string `json:"schema_version"`
	CatalogProvider string `json:"catalog_provider"`
	StorageProvider string `json:"storage_provider"`
}

type Selection struct {
	Options     selection.Options `json:"options"`
	Fingerprint string            `json:"fingerprint"`
}

type Counts struct {
	Templates         int   `json:"templates"`
	Aliases           int   `json:"aliases"`
	Builds            int   `json:"builds"`
	Assignments       int   `json:"assignments"`
	SnapshotTemplates int   `json:"snapshot_templates"`
	Objects           int   `json:"objects"`
	ObjectBytes       int64 `json:"object_bytes"`
}

type RecordDescriptor struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Count  int    `json:"count"`
	SHA256 string `json:"sha256"`
}

// ObjectRecord 同时记录逻辑 key 与内容寻址路径。多个逻辑对象内容相同时可以
// 指向同一 BundlePath，但每个逻辑 key 仍有独立记录和 Build 引用关系。
type ObjectRecord struct {
	LogicalKey          string   `json:"logical_key"`
	Type                string   `json:"type"`
	ReferencingBuildIDs []string `json:"referencing_build_ids"`
	Size                int64    `json:"size"`
	SHA256              string   `json:"sha256"`
	BundlePath          string   `json:"bundle_path"`
	// v1/v2 的对象全部必需；保留 Required 是现有 wire format 的兼容字段。
	Required       bool             `json:"required"`
	SourceIdentity objectstore.Info `json:"source_identity"`
}

type TemplateRecord struct {
	ID            string     `json:"id"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	Public        bool       `json:"public"`
	Source        string     `json:"source"`
	BuildCount    int64      `json:"build_count,omitempty"`
	SpawnCount    int64      `json:"spawn_count,omitempty"`
	LastSpawnedAt *time.Time `json:"last_spawned_at,omitempty"`
}

type AliasRecord struct {
	SourceID   string `json:"source_id"`
	TemplateID string `json:"template_id"`
	Alias      string `json:"alias"`
	// NamespaceKind 把源 Team 名称与 Alias 的业务作用域分开。导入时 Team
	// 作用域跟随目标 Team，Global/Literal 则按显式选项处理。
	NamespaceKind   string  `json:"namespace_kind"`
	SourceNamespace *string `json:"source_namespace,omitempty"`
	IsRenamable     bool    `json:"is_renamable"`
}

type BuildRecord struct {
	// Build ID 与可执行属性跨环境保留；Team、Cluster Node 和旧 env_id 等环境
	// 归属字段由 Importer 重绑或从 Assignment 重新构造，因此不直接写入 record。
	ID                 string          `json:"id"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
	FinishedAt         *time.Time      `json:"finished_at,omitempty"`
	Status             string          `json:"status"`
	StatusGroup        string          `json:"status_group"`
	Dockerfile         *string         `json:"dockerfile,omitempty"`
	StartCommand       *string         `json:"start_command,omitempty"`
	ReadyCommand       *string         `json:"ready_command,omitempty"`
	VCPU               int64           `json:"vcpu"`
	RAMMB              int64           `json:"ram_mb"`
	FreeDiskSizeMB     int64           `json:"free_disk_size_mb"`
	TotalDiskSizeMB    *int64          `json:"total_disk_size_mb,omitempty"`
	KernelVersion      string          `json:"kernel_version"`
	FirecrackerVersion string          `json:"firecracker_version"`
	EnvdVersion        *string         `json:"envd_version,omitempty"`
	Reason             json.RawMessage `json:"reason,omitempty"`
	Version            *string         `json:"version,omitempty"`
	CPUArchitecture    *string         `json:"cpu_architecture,omitempty"`
	CPUFamily          *string         `json:"cpu_family,omitempty"`
	CPUModel           *string         `json:"cpu_model,omitempty"`
	CPUModelName       *string         `json:"cpu_model_name,omitempty"`
	CPUFlags           []string        `json:"cpu_flags,omitempty"`
}

type AssignmentRecord struct {
	SourceID   string    `json:"source_id"`
	TemplateID string    `json:"template_id"`
	BuildID    string    `json:"build_id"`
	Tag        string    `json:"tag"`
	Source     string    `json:"source"`
	CreatedAt  time.Time `json:"created_at"`
}

type SnapshotTemplateRecord struct {
	TemplateID string    `json:"template_id"`
	SandboxID  string    `json:"sandbox_id"`
	CreatedAt  time.Time `json:"created_at"`
}

type Records struct {
	// 这些切片分别写入固定名称的 JSONL 文件。拆开存放便于流式校验，也让
	// Manifest 能为每类记录独立声明数量和摘要。
	Templates         []TemplateRecord
	Aliases           []AliasRecord
	Builds            []BuildRecord
	Assignments       []AssignmentRecord
	SnapshotTemplates []SnapshotTemplateRecord
}

type ExportReport struct {
	CreatedAt            time.Time `json:"created_at"`
	SelectionFingerprint string    `json:"selection_fingerprint"`
	ObjectsCopied        int       `json:"objects_copied"`
	ObjectBytes          int64     `json:"object_bytes"`
	ObjectRetries        int       `json:"object_retries"`
}
