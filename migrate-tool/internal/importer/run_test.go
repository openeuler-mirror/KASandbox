package importer

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/bundle"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/model"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/targetstore"
)

const (
	testTargetTeamID    = "22222222-2222-4222-8222-222222222222"
	testTargetClusterID = "33333333-3333-4333-8333-333333333333"
)

// callLog 记录 fake 之间的调用顺序,用来断言“对象发布与复核先于 Catalog 提交”。
type callLog struct{ entries []string }

func (l *callLog) add(entry string) { l.entries = append(l.entries, entry) }

type fakeCatalog struct {
	log       *callLog
	data      *model.CatalogData
	committed *model.CatalogData
	commitErr error
}

func (f *fakeCatalog) Kind() string { return "fake" }

func (f *fakeCatalog) Snapshot(context.Context) (*model.CatalogData, error) { return f.data, nil }

func (f *fakeCatalog) Commit(_ context.Context, data *model.CatalogData) error {
	if f.commitErr != nil {
		return f.commitErr
	}
	f.log.add("commit")
	f.committed = data
	return nil
}

type fakeStore struct {
	log          *callLog
	observations map[string]targetstore.Observation
	publishErr   map[string]error
	recheckErr   map[string]error
}

func (f *fakeStore) Inspect(_ context.Context, object bundle.ObjectRecord) (targetstore.Observation, error) {
	return f.observations[object.LogicalKey], nil
}

func (f *fakeStore) Publish(_ context.Context, object bundle.ObjectRecord, _ string) (targetstore.Observation, error) {
	if err := f.publishErr[object.LogicalKey]; err != nil {
		return targetstore.Observation{}, err
	}
	f.log.add("publish:" + object.LogicalKey)
	return targetstore.Observation{Exists: true, Identical: true, Digest: object.SHA256}, nil
}

func (f *fakeStore) Recheck(_ context.Context, object bundle.ObjectRecord, _ targetstore.Observation) error {
	if err := f.recheckErr[object.LogicalKey]; err != nil {
		return err
	}
	f.log.add("recheck:" + object.LogicalKey)
	return nil
}

func (f *fakeStore) Close() {}

func testVerified(t *testing.T) *bundle.Verified {
	t.Helper()
	// 带纳秒分量:PostgreSQL 只存微秒,比较逻辑必须容忍亚微秒差异。
	base := time.Date(2026, 8, 3, 7, 0, 0, 1500, time.UTC)
	finished := base.Add(time.Minute)
	records := bundle.Records{
		Templates: []bundle.TemplateRecord{
			{ID: "tpl-a", CreatedAt: base, UpdatedAt: base, Public: true, Source: model.SourceTemplate, BuildCount: 1},
			{ID: "tpl-snap", CreatedAt: base, UpdatedAt: base, Source: model.SourceSnapshotTemplate, BuildCount: 1},
		},
		Aliases: []bundle.AliasRecord{
			{SourceID: "alias-1", TemplateID: "tpl-a", Alias: "python", NamespaceKind: bundle.NamespaceTeam, SourceNamespace: stringPointer("builder"), IsRenamable: true},
			{SourceID: "alias-2", TemplateID: "tpl-a", Alias: "python-global", NamespaceKind: bundle.NamespaceGlobal},
			{SourceID: "alias-3", TemplateID: "tpl-a", Alias: "python-legacy", NamespaceKind: bundle.NamespaceLiteral, SourceNamespace: stringPointer("legacy")},
		},
		Builds: []bundle.BuildRecord{
			testBuildRecord("build-1", base, finished),
			testBuildRecord("build-2", base, finished),
		},
		Assignments: []bundle.AssignmentRecord{
			{SourceID: "assign-1", TemplateID: "tpl-a", BuildID: "build-1", Tag: model.DefaultTag, Source: "app", CreatedAt: base},
			{SourceID: "assign-2", TemplateID: "tpl-snap", BuildID: "build-2", Tag: model.DefaultTag, Source: "app", CreatedAt: base},
		},
		SnapshotTemplates: []bundle.SnapshotTemplateRecord{
			{TemplateID: "tpl-snap", SandboxID: "sandbox-1", CreatedAt: base},
		},
	}
	manifest := bundle.Manifest{Objects: []bundle.ObjectRecord{
		{LogicalKey: "build-1/memfile.header", Type: "memfile-header", ReferencingBuildIDs: []string{"build-1"}, Size: 64,
			SHA256: strings.Repeat("a", 64), BundlePath: "objects/sha256/aa/" + strings.Repeat("a", 64), Required: true},
		{LogicalKey: "build-1/rootfs.ext4", Type: "rootfs", ReferencingBuildIDs: []string{"build-1"}, Size: 8,
			SHA256: strings.Repeat("b", 64), BundlePath: "objects/sha256/bb/" + strings.Repeat("b", 64), Required: true},
	}}
	return &bundle.Verified{Inspected: &bundle.Inspected{Root: t.TempDir(), Manifest: manifest, Records: records}}
}

func testBuildRecord(id string, created, finished time.Time) bundle.BuildRecord {
	return bundle.BuildRecord{ID: id, CreatedAt: created, UpdatedAt: finished, FinishedAt: &finished,
		Status: "uploaded", StatusGroup: model.StatusGroupReady, VCPU: 2, RAMMB: 512, FreeDiskSizeMB: 64,
		KernelVersion: "vmlinux-demo", FirecrackerVersion: "v1-demo", Reason: json.RawMessage(`{}`)}
}

func testTargetCatalog() *model.CatalogData {
	return &model.CatalogData{
		SchemaVersion: "20260218120000",
		Teams: []model.Team{
			{ID: testTargetTeamID, Slug: "runtime", Name: "Runtime Team", ClusterID: stringPointer(testTargetClusterID)},
		},
	}
}

func testOptions() Options {
	return Options{TargetTeam: "slug:runtime", LiteralNamespaceMap: map[string]string{"legacy": "archive"}}
}

func newImporter(data *model.CatalogData, observations map[string]targetstore.Observation) (*Importer, *fakeCatalog, *fakeStore, *callLog) {
	log := &callLog{}
	catalog := &fakeCatalog{log: log, data: data}
	store := &fakeStore{log: log, observations: observations}
	return &Importer{Catalog: catalog, Target: store}, catalog, store, log
}

func TestRunDryRunPlansCreatesWithoutWriting(t *testing.T) {
	importer, catalog, _, log := newImporter(testTargetCatalog(), nil)
	result, err := importer.Run(context.Background(), testVerified(t), testOptions(), false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied {
		t.Fatal("dry-run reported applied=true")
	}
	want := Counts{Templates: 2, Aliases: 2, Builds: 2, Assignments: 2, SnapshotTemplates: 1, Objects: 2}
	if result.Plan.Create != want {
		t.Fatalf("create counts = %+v, want %+v", result.Plan.Create, want)
	}
	if result.Plan.Skip.Aliases != 1 {
		t.Fatalf("skip aliases = %d, want 1 global alias", result.Plan.Skip.Aliases)
	}
	if len(result.Plan.Conflicts) != 0 {
		t.Fatalf("unexpected conflicts: %+v", result.Plan.Conflicts)
	}
	if len(log.entries) != 0 {
		t.Fatalf("dry-run performed writes: %v", log.entries)
	}
	if catalog.committed != nil {
		t.Fatal("dry-run committed the catalog")
	}
}

func TestRunApplyPublishesAndRechecksBeforeCommit(t *testing.T) {
	importer, catalog, _, log := newImporter(testTargetCatalog(), nil)
	result, err := importer.Run(context.Background(), testVerified(t), testOptions(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Applied {
		t.Fatal("apply reported applied=false")
	}
	wantOrder := []string{
		"publish:build-1/memfile.header", "publish:build-1/rootfs.ext4",
		"recheck:build-1/memfile.header", "recheck:build-1/rootfs.ext4",
		"commit",
	}
	if len(log.entries) != len(wantOrder) {
		t.Fatalf("call sequence = %v, want %v", log.entries, wantOrder)
	}
	for index, entry := range wantOrder {
		if log.entries[index] != entry {
			t.Fatalf("call sequence = %v, want %v", log.entries, wantOrder)
		}
	}
	if catalog.committed == nil {
		t.Fatal("apply did not commit the catalog")
	}
	assertRebound(t, catalog.committed)
}

// assertRebound 核对导入转换语义:主键保留,归属重绑,来源环境字段清除。
func assertRebound(t *testing.T, committed *model.CatalogData) {
	t.Helper()
	templates := make(map[string]model.Template)
	for _, template := range committed.Templates {
		templates[template.ID] = template
	}
	template, ok := templates["tpl-a"]
	if !ok {
		t.Fatal("template tpl-a was not committed")
	}
	if template.TeamID != testTargetTeamID || template.CreatedBy != nil {
		t.Fatalf("template ownership not rebound: %+v", template)
	}
	if template.ClusterID == nil || *template.ClusterID != testTargetClusterID {
		t.Fatalf("template cluster = %v, want target cluster", template.ClusterID)
	}

	builds := make(map[string]model.Build)
	for _, build := range committed.Builds {
		builds[build.ID] = build
	}
	build, ok := builds["build-1"]
	if !ok {
		t.Fatal("build build-1 was not committed")
	}
	if build.TeamID != testTargetTeamID || build.ClusterNodeID != nil {
		t.Fatalf("build ownership not rebound: %+v", build)
	}
	if build.LegacyTemplateID == nil || *build.LegacyTemplateID != "tpl-a" {
		t.Fatalf("legacy template = %v, want tpl-a", build.LegacyTemplateID)
	}

	aliasNamespaces := make(map[string]*string)
	for _, alias := range committed.Aliases {
		aliasNamespaces[alias.Alias] = alias.Namespace
		if len(alias.ID) != 36 || alias.ID == "alias-1" || alias.ID == "alias-3" {
			t.Fatalf("alias %q kept a source id: %q", alias.Alias, alias.ID)
		}
	}
	if namespace := aliasNamespaces["python"]; namespace == nil || *namespace != "runtime" {
		t.Fatalf("team alias namespace = %v, want runtime", namespace)
	}
	if namespace := aliasNamespaces["python-legacy"]; namespace == nil || *namespace != "archive" {
		t.Fatalf("literal alias namespace = %v, want archive", namespace)
	}
	if _, exists := aliasNamespaces["python-global"]; exists {
		t.Fatal("global alias was published without --include-global-aliases")
	}

	for _, assignment := range committed.Assignments {
		if assignment.Source != "app" {
			t.Fatalf("assignment source = %q, want app", assignment.Source)
		}
		if assignment.ID == "assign-1" || assignment.ID == "assign-2" {
			t.Fatalf("assignment kept a source id: %q", assignment.ID)
		}
	}
	if len(committed.SnapshotTemplates) != 1 || committed.SnapshotTemplates[0].TemplateID != "tpl-snap" {
		t.Fatalf("snapshot templates = %+v", committed.SnapshotTemplates)
	}
}

func TestRunSkipIdenticalReusesRepeatImport(t *testing.T) {
	importer, catalog, _, _ := newImporter(testTargetCatalog(), nil)
	if _, err := importer.Run(context.Background(), testVerified(t), testOptions(), true); err != nil {
		t.Fatal(err)
	}

	identical := targetstore.Observation{Exists: true, Identical: true}
	repeat, _, _, _ := newImporter(catalog.committed.Clone(), map[string]targetstore.Observation{
		"build-1/memfile.header": identical,
		"build-1/rootfs.ext4":    identical,
	})
	options := testOptions()
	options.ConflictPolicy = ConflictSkipIdentical
	result, err := repeat.Run(context.Background(), testVerified(t), options, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Plan.Conflicts) != 0 {
		t.Fatalf("identical repeat import reported conflicts: %+v", result.Plan.Conflicts)
	}
	wantReuse := Counts{Templates: 2, Aliases: 2, Builds: 2, Assignments: 2, SnapshotTemplates: 1, Objects: 2}
	if result.Plan.Reuse != wantReuse {
		t.Fatalf("reuse counts = %+v, want %+v", result.Plan.Reuse, wantReuse)
	}
	if result.Plan.Create != (Counts{}) {
		t.Fatalf("repeat import still creates: %+v", result.Plan.Create)
	}
}

func TestRunSkipIdenticalToleratesBackendRepresentations(t *testing.T) {
	importer, catalog, _, _ := newImporter(testTargetCatalog(), nil)
	if _, err := importer.Run(context.Background(), testVerified(t), testOptions(), true); err != nil {
		t.Fatal(err)
	}
	// 模拟 PostgreSQL 快照的表示差异:本地时区、微秒截断、空切片非 nil、
	// Reason 文本形态不同。语义相同的目标行必须判定为可复用。
	target := catalog.committed.Clone()
	zone := time.FixedZone("UTC+8", 8*3600)
	pg := func(value time.Time) time.Time { return value.In(zone).Truncate(time.Microsecond) }
	for i := range target.Templates {
		target.Templates[i].CreatedAt = pg(target.Templates[i].CreatedAt)
		target.Templates[i].UpdatedAt = pg(target.Templates[i].UpdatedAt)
	}
	for i := range target.Builds {
		build := &target.Builds[i]
		build.CreatedAt, build.UpdatedAt = pg(build.CreatedAt), pg(build.UpdatedAt)
		if build.FinishedAt != nil {
			finished := pg(*build.FinishedAt)
			build.FinishedAt = &finished
		}
		build.CPUFlags = []string{}
		build.Reason = json.RawMessage("{ }")
	}
	for i := range target.Assignments {
		target.Assignments[i].CreatedAt = pg(target.Assignments[i].CreatedAt)
	}
	for i := range target.SnapshotTemplates {
		target.SnapshotTemplates[i].CreatedAt = pg(target.SnapshotTemplates[i].CreatedAt)
	}

	identical := targetstore.Observation{Exists: true, Identical: true}
	repeat, _, _, _ := newImporter(target, map[string]targetstore.Observation{
		"build-1/memfile.header": identical,
		"build-1/rootfs.ext4":    identical,
	})
	options := testOptions()
	options.ConflictPolicy = ConflictSkipIdentical
	result, err := repeat.Run(context.Background(), testVerified(t), options, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Plan.Conflicts) != 0 {
		t.Fatalf("backend representation differences caused conflicts: %+v", result.Plan.Conflicts)
	}
	wantReuse := Counts{Templates: 2, Aliases: 2, Builds: 2, Assignments: 2, SnapshotTemplates: 1, Objects: 2}
	if result.Plan.Reuse != wantReuse {
		t.Fatalf("reuse counts = %+v, want %+v", result.Plan.Reuse, wantReuse)
	}
}

func TestRunCommitFailureSurfaces(t *testing.T) {
	importer, catalog, _, _ := newImporter(testTargetCatalog(), nil)
	catalog.commitErr = errors.New("serialization failure")
	_, err := importer.Run(context.Background(), testVerified(t), testOptions(), true)
	if err == nil || !strings.Contains(err.Error(), "commit target catalog") {
		t.Fatalf("err = %v, want commit failure", err)
	}
}

func TestRunFailPolicyReportsEveryConflictKind(t *testing.T) {
	importer, catalog, _, _ := newImporter(testTargetCatalog(), nil)
	if _, err := importer.Run(context.Background(), testVerified(t), testOptions(), true); err != nil {
		t.Fatal(err)
	}

	exists := targetstore.Observation{Exists: true, Identical: true}
	repeat, repeatCatalog, _, log := newImporter(catalog.committed.Clone(), map[string]targetstore.Observation{
		"build-1/memfile.header": exists,
		"build-1/rootfs.ext4":    exists,
	})
	result, err := repeat.Run(context.Background(), testVerified(t), testOptions(), true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied {
		t.Fatal("apply proceeded despite conflicts")
	}
	kinds := make(map[string]int)
	for _, conflict := range result.Plan.Conflicts {
		kinds[conflict.Kind]++
	}
	for _, kind := range []string{"template", "build", "alias", "assignment", "snapshot-template", "object"} {
		if kinds[kind] == 0 {
			t.Fatalf("missing %q conflict, got %v", kind, kinds)
		}
	}
	for index := 1; index < len(result.Plan.Conflicts); index++ {
		previous, current := result.Plan.Conflicts[index-1], result.Plan.Conflicts[index]
		if previous.Kind > current.Kind || (previous.Kind == current.Kind && previous.Key > current.Key) {
			t.Fatalf("conflicts are not sorted: %+v", result.Plan.Conflicts)
		}
	}
	if len(log.entries) != 0 || repeatCatalog.committed != nil {
		t.Fatalf("conflicting import performed writes: %v", log.entries)
	}
}

func TestRunObjectConflictIncludesForeignDigest(t *testing.T) {
	importer, _, _, _ := newImporter(testTargetCatalog(), map[string]targetstore.Observation{
		"build-1/memfile.header": {Exists: true, Identical: false, Digest: "deadbeef"},
	})
	result, err := importer.Run(context.Background(), testVerified(t), testOptions(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Plan.Conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want exactly one", result.Plan.Conflicts)
	}
	conflict := result.Plan.Conflicts[0]
	if conflict.Kind != "object" || conflict.Key != "build-1/memfile.header" || !strings.Contains(conflict.Reason, "deadbeef") {
		t.Fatalf("object conflict = %+v", conflict)
	}
	if result.Plan.Create.Objects != 1 {
		t.Fatalf("create objects = %d, want the remaining object", result.Plan.Create.Objects)
	}
}

func TestRunDuplicateTargetAssignmentsConflictEvenWhenIdentical(t *testing.T) {
	// 目标行取微秒精度(fixture 时间 1500ns 截断后的形态,模拟 PostgreSQL 存储);
	// 归一化匹配必须仍能识别出重复。
	base := time.Date(2026, 8, 3, 7, 0, 0, 1000, time.UTC)
	target := testTargetCatalog()
	for _, id := range []string{"cccccccc-0000-4000-8000-000000000001", "cccccccc-0000-4000-8000-000000000002"} {
		target.Assignments = append(target.Assignments, model.BuildAssignment{
			ID: id, TemplateID: "tpl-a", BuildID: "build-1", Tag: model.DefaultTag, Source: "app", CreatedAt: base,
		})
	}
	importer, _, _, _ := newImporter(target, nil)
	options := testOptions()
	options.ConflictPolicy = ConflictSkipIdentical
	result, err := importer.Run(context.Background(), testVerified(t), options, false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, conflict := range result.Plan.Conflicts {
		if conflict.Kind == "assignment" && strings.Contains(conflict.Reason, "duplicate") {
			found = true
		}
	}
	if !found {
		t.Fatalf("duplicate assignment rows were not reported: %+v", result.Plan.Conflicts)
	}
}

func TestRunLiteralNamespaceWithoutMappingFails(t *testing.T) {
	importer, _, _, _ := newImporter(testTargetCatalog(), nil)
	_, err := importer.Run(context.Background(), testVerified(t), Options{TargetTeam: "slug:runtime"}, false)
	if err == nil || !strings.Contains(err.Error(), "literal namespace") {
		t.Fatalf("err = %v, want literal namespace mapping error", err)
	}
}

func TestRunRecheckFailureAbortsBeforeCommit(t *testing.T) {
	importer, catalog, store, log := newImporter(testTargetCatalog(), nil)
	store.recheckErr = map[string]error{"build-1/rootfs.ext4": errors.New("object changed")}
	_, err := importer.Run(context.Background(), testVerified(t), testOptions(), true)
	if err == nil || !strings.Contains(err.Error(), "object changed") {
		t.Fatalf("err = %v, want recheck failure", err)
	}
	if catalog.committed != nil {
		t.Fatal("catalog was committed after a failed recheck")
	}
	for _, entry := range log.entries {
		if entry == "commit" {
			t.Fatalf("commit appears in call sequence: %v", log.entries)
		}
	}
}

func TestRunPublishFailureAbortsBeforeCommit(t *testing.T) {
	importer, catalog, store, _ := newImporter(testTargetCatalog(), nil)
	store.publishErr = map[string]error{"build-1/memfile.header": errors.New("write refused")}
	_, err := importer.Run(context.Background(), testVerified(t), testOptions(), true)
	if err == nil || !strings.Contains(err.Error(), "write refused") {
		t.Fatalf("err = %v, want publish failure", err)
	}
	if catalog.committed != nil {
		t.Fatal("catalog was committed after a failed publish")
	}
}

func TestRunValidatesPolicyAndDependencies(t *testing.T) {
	importer, _, _, _ := newImporter(testTargetCatalog(), nil)
	options := testOptions()
	options.ConflictPolicy = "sometimes"
	if _, err := importer.Run(context.Background(), testVerified(t), options, false); err == nil ||
		!strings.Contains(err.Error(), "conflict policy") {
		t.Fatalf("err = %v, want conflict policy error", err)
	}
	if _, err := (&Importer{}).Run(context.Background(), testVerified(t), testOptions(), false); err == nil ||
		!strings.Contains(err.Error(), "required") {
		t.Fatalf("err = %v, want missing dependency error", err)
	}
}

func TestResolveTeamReferenceForms(t *testing.T) {
	teams := testTargetCatalog().Teams
	tests := []struct {
		name      string
		reference string
		wantID    string
		wantErr   bool
	}{
		{name: "by slug", reference: "slug:runtime", wantID: testTargetTeamID},
		{name: "by id", reference: "id:" + testTargetTeamID, wantID: testTargetTeamID},
		{name: "missing prefix", reference: "runtime", wantErr: true},
		{name: "empty value", reference: "slug:", wantErr: true},
		{name: "unknown prefix", reference: "name:runtime", wantErr: true},
		{name: "not found", reference: "slug:absent", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			team, err := resolveTeam(teams, test.reference)
			if test.wantErr {
				if err == nil {
					t.Fatalf("resolveTeam(%q) succeeded, want error", test.reference)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if team.ID != test.wantID {
				t.Fatalf("team id = %q, want %q", team.ID, test.wantID)
			}
		})
	}
}

func TestEqualBuildIgnoresOnlyLegacyTemplateID(t *testing.T) {
	base := time.Date(2026, 8, 3, 7, 0, 0, 0, time.UTC)
	left := model.Build{ID: "build-1", CreatedAt: base, VCPU: 2, LegacyTemplateID: stringPointer("tpl-a")}
	right := left
	right.LegacyTemplateID = stringPointer("tpl-z")
	if !equalBuild(left, right) {
		t.Fatal("builds differing only in LegacyTemplateID were not equal")
	}
	right.VCPU = 4
	if equalBuild(left, right) {
		t.Fatal("builds differing in VCPU compared as equal")
	}
}
