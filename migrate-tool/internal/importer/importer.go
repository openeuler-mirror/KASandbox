package importer

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/bundle"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/catalog"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/model"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/targetstore"
)

const (
	ConflictFail          = "fail"
	ConflictSkipIdentical = "skip-identical"
)

type Options struct {
	TargetTeam           string            `json:"target_team"`
	IncludeGlobalAliases bool              `json:"include_global_aliases"`
	LiteralNamespaceMap  map[string]string `json:"literal_namespace_map,omitempty"`
	ConflictPolicy       string            `json:"conflict_policy"`
}

type Conflict struct {
	Kind   string `json:"kind"`
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

type Plan struct {
	TargetTeam model.Team `json:"target_team"`
	Create     Counts     `json:"create"`
	Reuse      Counts     `json:"reuse"`
	Skip       Counts     `json:"skip"`
	Conflicts  []Conflict `json:"conflicts"`
}

type Counts struct {
	Templates         int `json:"templates"`
	Aliases           int `json:"aliases"`
	Builds            int `json:"builds"`
	Assignments       int `json:"assignments"`
	SnapshotTemplates int `json:"snapshot_templates"`
	Objects           int `json:"objects"`
}

type Result struct {
	Plan    Plan `json:"plan"`
	Applied bool `json:"applied"`
}

type Importer struct {
	Catalog catalog.Catalog
	Target  targetstore.Store
}

type prepared struct {
	plan        Plan
	target      *model.CatalogData
	objectWrite []bundle.ObjectRecord
	objectReuse []observedObject
}

type observedObject struct {
	record      bundle.ObjectRecord
	observation targetstore.Observation
}

type aliasIdentity struct {
	namespace    string
	hasNamespace bool
	alias        string
}

func (i *Importer) Run(ctx context.Context, verified *bundle.Verified, options Options, apply bool) (*Result, error) {
	if i.Catalog == nil || i.Target == nil {
		return nil, fmt.Errorf("target catalog and object store are required")
	}
	if options.ConflictPolicy == "" {
		options.ConflictPolicy = ConflictFail
	}
	if options.ConflictPolicy != ConflictFail && options.ConflictPolicy != ConflictSkipIdentical {
		return nil, fmt.Errorf("conflict policy must be fail or skip-identical")
	}
	current, err := i.Catalog.Snapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("read target catalog: %w", err)
	}
	work := current.Clone()
	prepared, err := i.prepare(ctx, verified, work, options)
	if err != nil {
		return nil, err
	}
	if len(prepared.plan.Conflicts) > 0 {
		return &Result{Plan: prepared.plan}, nil
	}
	if !apply {
		return &Result{Plan: prepared.plan}, nil
	}

	observed := slices.Clone(prepared.objectReuse)
	for _, object := range prepared.objectWrite {
		filename := filepath.Join(verified.Root, filepath.FromSlash(object.BundlePath))
		observation, err := i.Target.Publish(ctx, object, filename)
		if err != nil {
			return nil, err
		}
		observed = append(observed, observedObject{record: object, observation: observation})
	}
	// 对象写入和数据库事务无法组成跨存储原子提交。至少在提交 Catalog 前
	// 再确认本次新建和计划复用的对象仍是刚刚校验过的版本。
	for _, object := range observed {
		if err := i.Target.Recheck(ctx, object.record, object.observation); err != nil {
			return nil, err
		}
	}
	if err := i.Catalog.Commit(ctx, prepared.target); err != nil {
		return nil, fmt.Errorf("commit target catalog: %w", err)
	}
	return &Result{Plan: prepared.plan, Applied: true}, nil
}

func (i *Importer) prepare(ctx context.Context, verified *bundle.Verified, target *model.CatalogData, options Options) (*prepared, error) {
	team, err := resolveTeam(target.Teams, options.TargetTeam)
	if err != nil {
		return nil, err
	}
	plan := Plan{TargetTeam: team}
	policy := options.ConflictPolicy

	templateByID := make(map[string]model.Template, len(target.Templates))
	for _, value := range target.Templates {
		templateByID[value.ID] = value
	}
	// Template/Build 的主键在迁移中保持不变，但环境归属字段必须重新绑定到
	// 目标 Team；源端创建者和调度节点没有跨环境语义，因此不复制。
	for _, record := range verified.Records.Templates {
		value := model.Template{ID: record.ID, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt, Public: record.Public,
			TeamID: team.ID, CreatedBy: nil, ClusterID: team.ClusterID, Source: record.Source,
			BuildCount: record.BuildCount, SpawnCount: record.SpawnCount, LastSpawnedAt: record.LastSpawnedAt}
		if existing, ok := templateByID[value.ID]; ok {
			if policy == ConflictSkipIdentical && equalTemplate(existing, value) {
				plan.Reuse.Templates++
			} else {
				addConflict(&plan, "template", value.ID, "target template id already exists")
			}
			continue
		}
		target.Templates = append(target.Templates, value)
		templateByID[value.ID] = value
		plan.Create.Templates++
	}

	buildByID := make(map[string]model.Build, len(target.Builds))
	for _, value := range target.Builds {
		buildByID[value.ID] = value
	}
	// 旧 schema 仍要求 env_builds.env_id。一个 Build 可以通过 Assignment 属于
	// 多个 Template，因此这里只选稳定的最小 Template ID 填充兼容列；真实关系
	// 始终由 Assignments 表达。
	legacyTemplateByBuild := deterministicLegacyTemplates(verified.Records.Assignments)
	for _, record := range verified.Records.Builds {
		legacy := legacyTemplateByBuild[record.ID]
		value := model.Build{ID: record.ID, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt, FinishedAt: record.FinishedAt,
			Status: record.Status, StatusGroup: record.StatusGroup, Dockerfile: record.Dockerfile, StartCommand: record.StartCommand,
			ReadyCommand: record.ReadyCommand, VCPU: record.VCPU, RAMMB: record.RAMMB, FreeDiskSizeMB: record.FreeDiskSizeMB,
			TotalDiskSizeMB: record.TotalDiskSizeMB, KernelVersion: record.KernelVersion, FirecrackerVersion: record.FirecrackerVersion,
			LegacyTemplateID: &legacy, EnvdVersion: record.EnvdVersion, ClusterNodeID: nil, Reason: record.Reason,
			Version: record.Version, CPUArchitecture: record.CPUArchitecture, CPUFamily: record.CPUFamily,
			CPUModel: record.CPUModel, CPUModelName: record.CPUModelName, CPUFlags: record.CPUFlags, TeamID: team.ID}
		if existing, ok := buildByID[value.ID]; ok {
			if policy == ConflictSkipIdentical && equalBuild(existing, value) {
				plan.Reuse.Builds++
			} else {
				addConflict(&plan, "build", value.ID, "target build id already exists with different metadata")
			}
			continue
		}
		target.Builds = append(target.Builds, value)
		buildByID[value.ID] = value
		plan.Create.Builds++
	}

	aliasByName := make(map[aliasIdentity]model.Alias, len(target.Aliases))
	for _, value := range target.Aliases {
		aliasByName[identifyAlias(value)] = value
	}
	// Alias 的 Namespace 表示名称作用域，而不是源 Team 的永久身份。Team-scoped
	// Alias 跟随目标 Team；Global/Literal 必须由调用方显式决定，避免抢占名称。
	for _, record := range verified.Records.Aliases {
		namespace, include, err := targetNamespace(record, team.Slug, options)
		if err != nil {
			return nil, err
		}
		if !include {
			plan.Skip.Aliases++
			continue
		}
		id, err := newUUID()
		if err != nil {
			return nil, fmt.Errorf("generate target alias id: %w", err)
		}
		value := model.Alias{
			ID: id, TemplateID: record.TemplateID, Namespace: namespace,
			Alias: record.Alias, IsRenamable: record.IsRenamable,
		}
		identity := identifyAlias(value)
		key := value.QualifiedName()
		if existing, ok := aliasByName[identity]; ok {
			if policy == ConflictSkipIdentical && existing.TemplateID == value.TemplateID && existing.IsRenamable == value.IsRenamable {
				plan.Reuse.Aliases++
			} else {
				addConflict(&plan, "alias", key, "target alias already exists")
			}
			continue
		}
		target.Aliases = append(target.Aliases, value)
		aliasByName[identity] = value
		plan.Create.Aliases++
	}

	for _, record := range verified.Records.Assignments {
		matches := 0
		identical := false
		for _, existing := range target.Assignments {
			if existing.TemplateID == record.TemplateID &&
				existing.BuildID == record.BuildID &&
				existing.Tag == record.Tag &&
				normalizeCatalogTime(existing.CreatedAt).Equal(normalizeCatalogTime(record.CreatedAt)) {
				matches++
				identical = existing.Source == "app"
			}
		}
		key := fmt.Sprintf(
			"%s:%s:%s:%s",
			record.TemplateID,
			record.Tag,
			record.BuildID,
			record.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z"),
		)
		if matches > 0 {
			if policy == ConflictSkipIdentical && matches == 1 && identical {
				plan.Reuse.Assignments++
			} else {
				addConflict(&plan, "assignment", key, "target assignment already exists with different source or duplicate rows")
			}
			continue
		}
		id, err := newUUID()
		if err != nil {
			return nil, fmt.Errorf("generate target assignment id: %w", err)
		}
		target.Assignments = append(target.Assignments, model.BuildAssignment{
			ID: id, TemplateID: record.TemplateID, BuildID: record.BuildID,
			Tag: record.Tag, Source: "app", CreatedAt: record.CreatedAt,
		})
		plan.Create.Assignments++
	}

	snapshotByTemplate := make(map[string]model.SnapshotTemplate, len(target.SnapshotTemplates))
	for _, value := range target.SnapshotTemplates {
		snapshotByTemplate[value.TemplateID] = value
	}
	for _, record := range verified.Records.SnapshotTemplates {
		value := model.SnapshotTemplate(record)
		if existing, ok := snapshotByTemplate[value.TemplateID]; ok {
			if policy == ConflictSkipIdentical && equalSnapshotTemplate(existing, value) {
				plan.Reuse.SnapshotTemplates++
			} else {
				addConflict(&plan, "snapshot-template", value.TemplateID, "target snapshot template row already exists")
			}
			continue
		}
		target.SnapshotTemplates = append(target.SnapshotTemplates, value)
		snapshotByTemplate[value.TemplateID] = value
		plan.Create.SnapshotTemplates++
	}

	objectWrite := make([]bundle.ObjectRecord, 0)
	objectReuse := make([]observedObject, 0)
	// 数据库只能在对象落盘后提交，否则目标 Catalog 可能先引用不存在的文件。
	// 反过来，提交失败最多留下无引用对象，后续可按 digest 复用或清理。
	for _, object := range verified.Manifest.Objects {
		observed, err := i.Target.Inspect(ctx, object)
		if err != nil {
			return nil, err
		}
		if observed.Exists {
			if policy == ConflictSkipIdentical && observed.Identical {
				plan.Reuse.Objects++
				objectReuse = append(objectReuse, observedObject{record: object, observation: observed})
			} else {
				reason := "target object already exists"
				if observed.Digest != "" {
					reason += " with digest " + observed.Digest
				}
				addConflict(&plan, "object", object.LogicalKey, reason)
			}
			continue
		}
		objectWrite = append(objectWrite, object)
		plan.Create.Objects++
	}

	slices.SortFunc(plan.Conflicts, func(a, b Conflict) int {
		if a.Kind != b.Kind {
			return strings.Compare(a.Kind, b.Kind)
		}
		return strings.Compare(a.Key, b.Key)
	})
	return &prepared{plan: plan, target: target, objectWrite: objectWrite, objectReuse: objectReuse}, nil
}

func resolveTeam(teams []model.Team, reference string) (model.Team, error) {
	prefix, value, ok := strings.Cut(reference, ":")
	if !ok || value == "" || (prefix != "id" && prefix != "slug") {
		return model.Team{}, fmt.Errorf("target team must use id:<UUID> or slug:<SLUG>")
	}
	for _, team := range teams {
		if (prefix == "id" && team.ID == value) || (prefix == "slug" && team.Slug == value) {
			return team, nil
		}
	}
	return model.Team{}, fmt.Errorf("target team %q was not found", reference)
}

func targetNamespace(record bundle.AliasRecord, targetSlug string, options Options) (*string, bool, error) {
	switch record.NamespaceKind {
	case bundle.NamespaceGlobal:
		return nil, options.IncludeGlobalAliases, nil
	case bundle.NamespaceTeam:
		value := targetSlug
		return &value, true, nil
	case bundle.NamespaceLiteral:
		if record.SourceNamespace == nil {
			return nil, false, fmt.Errorf("literal alias %q has no source namespace", record.Alias)
		}
		value, ok := options.LiteralNamespaceMap[*record.SourceNamespace]
		if !ok || value == "" {
			return nil, false, fmt.Errorf("literal namespace %q requires an explicit SOURCE=TARGET mapping", *record.SourceNamespace)
		}
		return &value, true, nil
	default:
		return nil, false, fmt.Errorf("unsupported namespace kind %q", record.NamespaceKind)
	}
}

func deterministicLegacyTemplates(assignments []bundle.AssignmentRecord) map[string]string {
	result := make(map[string]string)
	for _, assignment := range assignments {
		current, ok := result[assignment.BuildID]
		if !ok || assignment.TemplateID < current {
			result[assignment.BuildID] = assignment.TemplateID
		}
	}
	return result
}

// normalizeCatalogTime 折叠 Catalog 后端的时间表示差异:pgx 解码 timestamptz
// 得到本地时区、且数据库只存微秒精度,而 Bundle JSON 保留 UTC 和纳秒。
// skip-identical 的相同判定统一按 UTC 微秒比较。
func normalizeCatalogTime(value time.Time) time.Time {
	return value.UTC().Truncate(time.Microsecond)
}

func normalizeCatalogTimePtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	normalized := normalizeCatalogTime(*value)
	return &normalized
}

func equalTemplate(left, right model.Template) bool {
	left.CreatedAt, right.CreatedAt = normalizeCatalogTime(left.CreatedAt), normalizeCatalogTime(right.CreatedAt)
	left.UpdatedAt, right.UpdatedAt = normalizeCatalogTime(left.UpdatedAt), normalizeCatalogTime(right.UpdatedAt)
	left.LastSpawnedAt, right.LastSpawnedAt = normalizeCatalogTimePtr(left.LastSpawnedAt), normalizeCatalogTimePtr(right.LastSpawnedAt)
	return reflect.DeepEqual(left, right)
}

// equalBuild 忽略 LegacyTemplateID(导入时由 Assignment 重新构造),并折叠
// 后端表示差异:时间时区与精度、空 CPUFlags 与 nil、Reason 的 JSON 文本形态。
func equalBuild(left, right model.Build) bool {
	if !equalReason(left.Reason, right.Reason) {
		return false
	}
	left.Reason, right.Reason = nil, nil
	left.LegacyTemplateID, right.LegacyTemplateID = nil, nil
	left.CreatedAt, right.CreatedAt = normalizeCatalogTime(left.CreatedAt), normalizeCatalogTime(right.CreatedAt)
	left.UpdatedAt, right.UpdatedAt = normalizeCatalogTime(left.UpdatedAt), normalizeCatalogTime(right.UpdatedAt)
	left.FinishedAt, right.FinishedAt = normalizeCatalogTimePtr(left.FinishedAt), normalizeCatalogTimePtr(right.FinishedAt)
	if len(left.CPUFlags) == 0 {
		left.CPUFlags = nil
	}
	if len(right.CPUFlags) == 0 {
		right.CPUFlags = nil
	}
	return reflect.DeepEqual(left, right)
}

func equalSnapshotTemplate(left, right model.SnapshotTemplate) bool {
	return left.TemplateID == right.TemplateID && left.SandboxID == right.SandboxID &&
		normalizeCatalogTime(left.CreatedAt).Equal(normalizeCatalogTime(right.CreatedAt))
}

// equalReason 按 JSON 语义比较:jsonb 往返会重排键序和空白,字节比较会误报。
// 空值与 "{}" 等价,与 insertMissing 写入 "{}" 的约定一致。
func equalReason(left, right json.RawMessage) bool {
	if len(left) == 0 {
		left = json.RawMessage("{}")
	}
	if len(right) == 0 {
		right = json.RawMessage("{}")
	}
	var leftValue, rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return bytes.Equal(left, right)
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

func addConflict(plan *Plan, kind, key, reason string) {
	plan.Conflicts = append(plan.Conflicts, Conflict{Kind: kind, Key: key, Reason: reason})
}

func identifyAlias(alias model.Alias) aliasIdentity {
	identity := aliasIdentity{alias: alias.Alias}
	if alias.Namespace != nil {
		identity.namespace = *alias.Namespace
		identity.hasNamespace = true
	}
	return identity
}

func newUUID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	raw := hex.EncodeToString(value[:])
	return raw[0:8] + "-" + raw[8:12] + "-" + raw[12:16] + "-" + raw[16:20] + "-" + raw[20:32], nil
}
