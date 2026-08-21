package testfixture

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/artifact"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/model"
)

const (
	SourceTeamID    = "11111111-1111-4111-8111-111111111111"
	TargetTeamID    = "22222222-2222-4222-8222-222222222222"
	TargetClusterID = "33333333-3333-4333-8333-333333333333"

	TemplateID         = "tpl-python"
	SnapshotTemplateID = "tpl-checkpoint"
	BuildID            = "00112233-4455-6677-8899-aabbccddeeff"
	SnapshotBuildID    = "11112233-4455-6677-8899-aabbccddeeff"
	PendingBuildID     = "22222233-4455-6677-8899-aabbccddeeff"
	AncestorBuildID    = "ffeeddcc-bbaa-4988-8766-554433221100"
)

type Mapping struct {
	Offset             uint64
	Length             uint64
	BuildID            string
	BuildStorageOffset uint64
}

func Create(root string) error {
	base := time.Date(2026, 8, 3, 7, 0, 0, 0, time.UTC)
	source := model.CatalogData{
		SchemaVersion: "20260218120000",
		Teams:         []model.Team{{ID: SourceTeamID, Slug: "builder", Name: "Builder Team"}},
		Templates: []model.Template{
			{ID: TemplateID, CreatedAt: base, UpdatedAt: base.Add(time.Minute), Public: true, TeamID: SourceTeamID, Source: model.SourceTemplate, BuildCount: 2, SpawnCount: 5},
			{ID: SnapshotTemplateID, CreatedAt: base.Add(2 * time.Minute), UpdatedAt: base.Add(3 * time.Minute), TeamID: SourceTeamID, Source: model.SourceSnapshotTemplate, BuildCount: 1},
		},
		Aliases: []model.Alias{
			{ID: "aaaaaaaa-0000-4000-8000-000000000001", TemplateID: TemplateID, Namespace: ptr("builder"), Alias: "python", IsRenamable: true},
			{ID: "aaaaaaaa-0000-4000-8000-000000000002", TemplateID: TemplateID, Namespace: nil, Alias: "python-global"},
			{ID: "aaaaaaaa-0000-4000-8000-000000000003", TemplateID: TemplateID, Namespace: ptr("legacy"), Alias: "python-legacy"},
			{ID: "aaaaaaaa-0000-4000-8000-000000000004", TemplateID: SnapshotTemplateID, Namespace: ptr("builder"), Alias: "checkpoint"},
		},
		Builds: []model.Build{
			readyBuild(BuildID, SourceTeamID, base.Add(10*time.Minute)),
			readyBuild(SnapshotBuildID, SourceTeamID, base.Add(20*time.Minute)),
			pendingBuild(PendingBuildID, SourceTeamID, base.Add(30*time.Minute)),
		},
		Assignments: []model.BuildAssignment{
			{ID: "bbbbbbbb-0000-4000-8000-000000000001", TemplateID: TemplateID, BuildID: BuildID, Tag: model.DefaultTag, Source: "app", CreatedAt: base.Add(10 * time.Minute)},
			{ID: "bbbbbbbb-0000-4000-8000-000000000002", TemplateID: TemplateID, BuildID: PendingBuildID, Tag: model.DefaultTag, Source: "app", CreatedAt: base.Add(30 * time.Minute)},
			{ID: "bbbbbbbb-0000-4000-8000-000000000003", TemplateID: SnapshotTemplateID, BuildID: SnapshotBuildID, Tag: model.DefaultTag, Source: "app", CreatedAt: base.Add(20 * time.Minute)},
		},
		SnapshotTemplates: []model.SnapshotTemplate{{TemplateID: SnapshotTemplateID, SandboxID: "sandbox-source", CreatedAt: base.Add(2 * time.Minute)}},
	}
	target := model.CatalogData{SchemaVersion: source.SchemaVersion, Teams: []model.Team{{ID: TargetTeamID, Slug: "runtime", Name: "Runtime Team", ClusterID: ptr(TargetClusterID)}}}

	if err := writeJSON(filepath.Join(root, "source", "catalog.json"), source); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(root, "target", "catalog.json"), target); err != nil {
		return err
	}
	objects := filepath.Join(root, "source", "objects")
	if err := writeBuildObjects(objects, BuildID, []Mapping{{0, 4, AncestorBuildID, 0}, {4, 4, BuildID, 0}}, []Mapping{{0, 8, BuildID, 0}}); err != nil {
		return err
	}
	if err := writeBuildObjects(objects, SnapshotBuildID, []Mapping{{0, 4, BuildID, 0}, {4, 4, SnapshotBuildID, 0}}, []Mapping{{0, 8, AncestorBuildID, 0}}); err != nil {
		return err
	}
	data := map[string][]byte{
		artifact.Memfile(AncestorBuildID): []byte("AMEM"), artifact.Memfile(BuildID): []byte("BMEM"),
		artifact.Memfile(SnapshotBuildID): []byte("SMEM"), artifact.Rootfs(BuildID): []byte("BROOTFS!"),
		artifact.Rootfs(AncestorBuildID): []byte("AROOTFS!"),
	}
	for key, value := range data {
		if err := writeFile(filepath.Join(objects, filepath.FromSlash(key)), value); err != nil {
			return err
		}
	}
	golden, err := GoldenHeader()
	if err != nil {
		return err
	}
	if err := writeFile(filepath.Join(root, "golden", "header-v3.bin"), golden); err != nil {
		return err
	}
	return nil
}

func GoldenHeader() ([]byte, error) {
	return encodeHeader("00112233-4455-6677-8899-aabbccddeeff", "ffeeddcc-bbaa-9988-7766-554433221100", 3, 2097152, 6291456, 2,
		[]Mapping{{0, 2097152, "ffeeddcc-bbaa-9988-7766-554433221100", 0}, {2097152, 2097152, "00000000-0000-0000-0000-000000000000", 0}, {4194304, 2097152, "00112233-4455-6677-8899-aabbccddeeff", 0}})
}

func writeBuildObjects(root, buildID string, memMappings, rootMappings []Mapping) error {
	mem, err := encodeHeader(buildID, buildID, 3, 4, 8, 1, memMappings)
	if err != nil {
		return err
	}
	rootfs, err := encodeHeader(buildID, buildID, 3, 4, 8, 1, rootMappings)
	if err != nil {
		return err
	}
	metadata, err := json.Marshal(map[string]any{"version": 2, "template": map[string]any{"build_id": buildID, "kernel_version": "vmlinux-demo", "firecracker_version": "v1-demo"}, "context": map[string]any{}})
	if err != nil {
		return err
	}
	values := map[string][]byte{artifact.MemfileHeader(buildID): mem, artifact.RootfsHeader(buildID): rootfs,
		artifact.Snapfile(buildID): []byte("SNAP-" + buildID), artifact.Metadata(buildID): append(metadata, '\n')}
	for key, value := range values {
		if err := writeFile(filepath.Join(root, filepath.FromSlash(key)), value); err != nil {
			return err
		}
	}
	return nil
}

func encodeHeader(buildID, baseBuildID string, version, blockSize, size, generation uint64, mappings []Mapping) ([]byte, error) {
	result := make([]byte, 64+40*len(mappings))
	binary.LittleEndian.PutUint64(result[0:8], version)
	binary.LittleEndian.PutUint64(result[8:16], blockSize)
	binary.LittleEndian.PutUint64(result[16:24], size)
	binary.LittleEndian.PutUint64(result[24:32], generation)
	buildRaw, err := parseUUID(buildID)
	if err != nil {
		return nil, err
	}
	copy(result[32:48], buildRaw)
	baseRaw, err := parseUUID(baseBuildID)
	if err != nil {
		return nil, err
	}
	copy(result[48:64], baseRaw)
	for index, mapping := range mappings {
		offset := 64 + 40*index
		binary.LittleEndian.PutUint64(result[offset:offset+8], mapping.Offset)
		binary.LittleEndian.PutUint64(result[offset+8:offset+16], mapping.Length)
		raw, err := parseUUID(mapping.BuildID)
		if err != nil {
			return nil, err
		}
		copy(result[offset+16:offset+32], raw)
		binary.LittleEndian.PutUint64(result[offset+32:offset+40], mapping.BuildStorageOffset)
	}
	return result, nil
}

func parseUUID(value string) ([]byte, error) {
	raw, err := hex.DecodeString(strings.ReplaceAll(value, "-", ""))
	if err != nil || len(raw) != 16 {
		return nil, fmt.Errorf("invalid UUID %q", value)
	}
	return raw, nil
}

func readyBuild(id, teamID string, created time.Time) model.Build {
	finished := created.Add(time.Minute)
	version := "v2"
	envd := "0.1.0"
	total := int64(128)
	arch := "x86_64"
	return model.Build{ID: id, CreatedAt: created, UpdatedAt: finished, FinishedAt: &finished, Status: "uploaded", StatusGroup: model.StatusGroupReady,
		VCPU: 2, RAMMB: 512, FreeDiskSizeMB: 64, TotalDiskSizeMB: &total, KernelVersion: "vmlinux-demo", FirecrackerVersion: "v1-demo",
		EnvdVersion: &envd, Version: &version, CPUArchitecture: &arch, TeamID: teamID, Reason: json.RawMessage(`{}`)}
}

func pendingBuild(id, teamID string, created time.Time) model.Build {
	value := readyBuild(id, teamID, created)
	value.Status = "building"
	value.StatusGroup = "in_progress"
	value.FinishedAt = nil
	return value
}

func writeJSON(filename string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(filename, append(raw, '\n'))
}
func writeFile(filename string, value []byte) error {
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filename, value, 0o644)
}
func ptr(value string) *string { return &value }
