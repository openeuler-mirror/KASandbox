package model

import (
	"encoding/json"
	"slices"
	"time"
)

const (
	SourceTemplate         = "template"
	SourceSnapshotTemplate = "snapshot_template"
	StatusGroupReady       = "ready"
	DefaultTag             = "default"
)

// StatusGroupForBuildStatus 与数据库 trg_compute_status_group 的映射保持一致。
// Bundle 保留原始 status，同时用归一化分组决定 Build 是否可迁移。
func StatusGroupForBuildStatus(status string) string {
	switch status {
	case "pending", "waiting":
		return "pending"
	case "in_progress", "building", "snapshotting":
		return "in_progress"
	case "ready", "uploaded", "success":
		return StatusGroupReady
	default:
		return "failed"
	}
}

type CatalogData struct {
	SchemaVersion     string             `json:"schema_version"`
	Teams             []Team             `json:"teams"`
	Templates         []Template         `json:"templates"`
	Aliases           []Alias            `json:"aliases"`
	Builds            []Build            `json:"builds"`
	Assignments       []BuildAssignment  `json:"assignments"`
	SnapshotTemplates []SnapshotTemplate `json:"snapshot_templates,omitempty"`
}

type Team struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
	// ClusterID 决定目标 Template 可在哪个 Cluster 启动；迁移只读取现有 Team，
	// 不创建 Team/Cluster，也不复制源 Team 的 Cluster。
	ClusterID *string `json:"cluster_id,omitempty"`
}

type Template struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Public    bool      `json:"public"`
	// TeamID 和 ClusterID 是环境归属字段；导入时分别绑定 Target Team 及其 Cluster。
	TeamID        string     `json:"team_id"`
	CreatedBy     *string    `json:"created_by,omitempty"`
	ClusterID     *string    `json:"cluster_id,omitempty"`
	Source        string     `json:"source"`
	BuildCount    int64      `json:"build_count,omitempty"`
	SpawnCount    int64      `json:"spawn_count,omitempty"`
	LastSpawnedAt *time.Time `json:"last_spawned_at,omitempty"`
}

type Alias struct {
	ID         string `json:"id"`
	TemplateID string `json:"template_id"`
	// Namespace=nil 表示环境级 Global Alias；非空值通常是 Team slug，也可能
	// 是需要调用方显式映射的历史命名空间。
	Namespace   *string `json:"namespace,omitempty"`
	Alias       string  `json:"alias"`
	IsRenamable bool    `json:"is_renamable"`
}

// QualifiedName 返回用户可见的完整 Alias。Global Alias 没有 Namespace。
func (a Alias) QualifiedName() string {
	if a.Namespace == nil {
		return a.Alias
	}
	return *a.Namespace + "/" + a.Alias
}

type Build struct {
	ID                 string     `json:"id"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	FinishedAt         *time.Time `json:"finished_at,omitempty"`
	Status             string     `json:"status"`
	StatusGroup        string     `json:"status_group"`
	Dockerfile         *string    `json:"dockerfile,omitempty"`
	StartCommand       *string    `json:"start_command,omitempty"`
	ReadyCommand       *string    `json:"ready_command,omitempty"`
	VCPU               int64      `json:"vcpu"`
	RAMMB              int64      `json:"ram_mb"`
	FreeDiskSizeMB     int64      `json:"free_disk_size_mb"`
	TotalDiskSizeMB    *int64     `json:"total_disk_size_mb,omitempty"`
	KernelVersion      string     `json:"kernel_version"`
	FirecrackerVersion string     `json:"firecracker_version"`
	// LegacyTemplateID 对应历史 env_builds.env_id；真实 M:N 关系以 Assignments 为准。
	LegacyTemplateID *string `json:"legacy_template_id,omitempty"`
	EnvdVersion      *string `json:"envd_version,omitempty"`
	// ClusterNodeID 只记录源 Build Node，导入时必须清空，不能成为目标调度约束。
	ClusterNodeID   *string         `json:"cluster_node_id,omitempty"`
	Reason          json.RawMessage `json:"reason,omitempty"`
	Version         *string         `json:"version,omitempty"`
	CPUArchitecture *string         `json:"cpu_architecture,omitempty"`
	CPUFamily       *string         `json:"cpu_family,omitempty"`
	CPUModel        *string         `json:"cpu_model,omitempty"`
	CPUModelName    *string         `json:"cpu_model_name,omitempty"`
	CPUFlags        []string        `json:"cpu_flags,omitempty"`
	TeamID          string          `json:"team_id"`
}

type BuildAssignment struct {
	// Assignment 是 Template、Tag 与 Build 的带时间关系。latest 依据 CreatedAt
	// 选择，而不是 Build 自身的创建时间。
	ID         string    `json:"id"`
	TemplateID string    `json:"template_id"`
	BuildID    string    `json:"build_id"`
	Tag        string    `json:"tag"`
	Source     string    `json:"source"`
	CreatedAt  time.Time `json:"created_at"`
}

type SnapshotTemplate struct {
	// SnapshotTemplate 是 Template 的一对一来源记录，不等同于运行态 snapshots。
	TemplateID string    `json:"template_id"`
	SandboxID  string    `json:"sandbox_id"`
	CreatedAt  time.Time `json:"created_at"`
}

// Clone 为导入计划创建可追加的工作副本。元素中的字符串和时间指针按只读值
// 使用；真正可变的字节/字符串切片会单独复制。
func (d *CatalogData) Clone() *CatalogData {
	if d == nil {
		return nil
	}
	cloned := *d
	cloned.Teams = slices.Clone(d.Teams)
	cloned.Templates = slices.Clone(d.Templates)
	cloned.Aliases = slices.Clone(d.Aliases)
	cloned.Builds = slices.Clone(d.Builds)
	for index := range cloned.Builds {
		cloned.Builds[index].Reason = slices.Clone(d.Builds[index].Reason)
		cloned.Builds[index].CPUFlags = slices.Clone(d.Builds[index].CPUFlags)
	}
	cloned.Assignments = slices.Clone(d.Assignments)
	cloned.SnapshotTemplates = slices.Clone(d.SnapshotTemplates)
	return &cloned
}
