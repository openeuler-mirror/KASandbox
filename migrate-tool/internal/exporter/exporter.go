package exporter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/artifact"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/bundle"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/catalog"
	artifactheader "gitcode.com/openeuler/KASandbox/migrate-tool/internal/header"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/model"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/objectstore"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/selection"
)

type Exporter struct {
	Catalog     catalog.Catalog
	Store       objectstore.Store
	ToolVersion string
	MaxRetries  int
	Now         func() time.Time
}

type Result struct {
	Path     string
	Manifest bundle.Manifest
}

type copiedObject struct {
	record bundle.ObjectRecord
	path   string
}

func (e *Exporter) Export(ctx context.Context, options selection.Options, outputPath string) (*Result, error) {
	if e.Catalog == nil || e.Store == nil {
		return nil, fmt.Errorf("catalog and object store are required")
	}
	if e.MaxRetries <= 0 {
		e.MaxRetries = 3
	}
	if e.Now == nil {
		e.Now = time.Now
	}

	output, err := filepath.Abs(outputPath)
	if err != nil {
		return nil, fmt.Errorf("resolve output path: %w", err)
	}
	if _, err := os.Stat(output); err == nil {
		return nil, fmt.Errorf("output bundle %q already exists", output)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("check output bundle: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return nil, fmt.Errorf("create output parent: %w", err)
	}
	stage, err := os.MkdirTemp(filepath.Dir(output), "."+filepath.Base(output)+".partial-*")
	if err != nil {
		return nil, fmt.Errorf("create bundle staging directory: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(stage)
		}
	}()

	source, err := e.Catalog.Snapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("read source catalog snapshot: %w", err)
	}
	selected, err := selection.Select(source, options)
	if err != nil {
		return nil, fmt.Errorf("select export records: %w", err)
	}
	records, err := bundle.RecordsFromSelection(selected)
	if err != nil {
		return nil, fmt.Errorf("normalize export records: %w", err)
	}
	descriptors, err := bundle.WriteRecords(stage, records)
	if err != nil {
		return nil, err
	}

	objects, layouts, retries, err := e.copyBuildObjects(ctx, stage, records.Builds)
	if err != nil {
		return nil, err
	}
	// 复制期间的临时目录不属于 Bundle v1 布局,发布前必须移除。
	if err := os.RemoveAll(filepath.Join(stage, ".object-tmp")); err != nil {
		return nil, fmt.Errorf("remove object staging directory: %w", err)
	}
	if err := e.recheckSelection(ctx, selected); err != nil {
		return nil, err
	}

	var objectBytes int64
	for _, object := range objects {
		objectBytes += object.Size
	}
	createdAt := e.Now().UTC()
	manifest := bundle.Manifest{
		Format: bundle.Format, FormatVersion: bundle.FormatVersion, ToolVersion: e.ToolVersion, CreatedAt: createdAt,
		Source:    bundle.Source{SchemaVersion: source.SchemaVersion, CatalogProvider: e.Catalog.Kind(), StorageProvider: e.Store.Kind()},
		Selection: bundle.Selection{Options: selected.Options, Fingerprint: selected.Fingerprint},
		Counts: bundle.Counts{
			Templates:         len(records.Templates),
			Aliases:           len(records.Aliases),
			Builds:            len(records.Builds),
			Assignments:       len(records.Assignments),
			SnapshotTemplates: len(records.SnapshotTemplates),
			Objects:           len(objects),
			ObjectBytes:       objectBytes,
		},
		Records: descriptors, Objects: objects,
	}
	for _, layout := range layouts {
		if layout.OSType == artifact.OSAndroid {
			manifest.FormatVersion = bundle.MultiDiskFormatVersion
			manifest.BuildLayouts = layouts
			break
		}
	}
	if err := os.MkdirAll(filepath.Join(stage, "reports"), 0o755); err != nil {
		return nil, fmt.Errorf("create reports directory: %w", err)
	}
	report := bundle.ExportReport{
		CreatedAt:            createdAt,
		SelectionFingerprint: selected.Fingerprint,
		ObjectsCopied:        len(objects),
		ObjectBytes:          objectBytes,
		ObjectRetries:        retries,
	}
	if err := writeJSON(filepath.Join(stage, "reports", "export-report.json"), report); err != nil {
		return nil, err
	}
	if err := bundle.WriteManifest(stage, &manifest); err != nil {
		return nil, err
	}
	if _, err := bundle.Verify(stage); err != nil {
		return nil, fmt.Errorf("verify staged bundle: %w", err)
	}
	if err := os.Rename(stage, output); err != nil {
		return nil, fmt.Errorf("publish bundle: %w", err)
	}
	committed = true

	return &Result{Path: output, Manifest: manifest}, nil
}

func (e *Exporter) copyBuildObjects(ctx context.Context, stage string, builds []bundle.BuildRecord) ([]bundle.ObjectRecord, []bundle.BuildLayout, int, error) {
	copied := make(map[string]*copiedObject)
	dataBounds := make(map[string]uint64)
	dataRefs := make(map[string][]string)
	dataTypes := make(map[string]string)
	layouts := make([]bundle.BuildLayout, 0, len(builds))
	retries := 0

	for _, build := range builds {
		meta, n, err := e.copyObject(ctx, stage, artifact.Metadata(build.ID), artifact.TypeMetadata, []string{build.ID})
		retries += n
		if err != nil {
			return nil, nil, retries, err
		}
		raw, err := os.ReadFile(meta.path)
		if err != nil {
			return nil, nil, retries, err
		}
		osType, err := artifact.MetadataOS(raw, build.ID)
		if err != nil {
			return nil, nil, retries, err
		}
		layouts = append(layouts, bundle.BuildLayout{BuildID: build.ID, OSType: osType, Disks: artifact.DiskNames(osType)})
		copied[meta.record.LogicalKey] = meta

		snap, n, err := e.copyObject(ctx, stage, artifact.Snapfile(build.ID), artifact.TypeSnapfile, []string{build.ID})
		retries += n
		if err != nil {
			return nil, nil, retries, err
		}
		copied[snap.record.LogicalKey] = snap

		for _, layer := range artifact.Layers(osType) {
			key := layer.HeaderKey(build.ID)
			object, n, err := e.copyObject(ctx, stage, key, layer.HeaderType, []string{build.ID})
			retries += n
			if err != nil {
				if errors.Is(err, objectstore.ErrNotFound) {
					if osType == artifact.OSLinux {
						return nil, nil, retries, fmt.Errorf("legacy_headerless_build %s: missing %s", build.ID, key)
					}
					return nil, nil, retries, fmt.Errorf("missing required header for %s build %s: %s (legacy_headerless_build or incomplete multi-disk build): %w", osType, build.ID, key, err)
				}
				return nil, nil, retries, err
			}
			copied[key] = object
			raw, err := os.ReadFile(object.path)
			if err != nil {
				return nil, nil, retries, fmt.Errorf("read copied header %q: %w", key, err)
			}
			h, err := artifactheader.Parse(raw)
			if err != nil {
				return nil, nil, retries, fmt.Errorf("parse header %q: %w", key, err)
			}
			if err := artifactheader.Validate(h); err != nil {
				return nil, nil, retries, fmt.Errorf("validate header %q: %w", key, err)
			}
			if h.Metadata.BuildID != build.ID {
				return nil, nil, retries, fmt.Errorf("header %q build id is %q, expected %q", key, h.Metadata.BuildID, build.ID)
			}
			// Flattened mappings name final data Builds, always for this same layer.
			for _, mapping := range h.Mappings {
				if mapping.BuildID == artifactheader.NilUUID {
					continue
				}
				if mapping.Length > math.MaxUint64-mapping.BuildStorageOffset {
					return nil, nil, retries, fmt.Errorf("header %q mapping source range overflows", key)
				}
				dataKey := layer.DataKey(mapping.BuildID)
				end := mapping.BuildStorageOffset + mapping.Length
				if end > dataBounds[dataKey] {
					dataBounds[dataKey] = end
				}
				dataRefs[dataKey] = append(dataRefs[dataKey], build.ID)
				dataTypes[dataKey] = layer.DataType
			}
		}
	}

	keys := make([]string, 0, len(dataBounds))
	for key := range dataBounds {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		object, n, err := e.copyObject(ctx, stage, key, dataTypes[key], uniqueSorted(dataRefs[key]))
		retries += n
		if err != nil {
			return nil, nil, retries, err
		}
		if uint64(object.record.Size) < dataBounds[key] {
			return nil, nil, retries, fmt.Errorf("object %q is %d bytes, header mappings require at least %d", key, object.record.Size, dataBounds[key])
		}
		copied[key] = object
	}
	result := make([]bundle.ObjectRecord, 0, len(copied))
	for _, object := range copied {
		result = append(result, object.record)
	}
	bundle.SortObjects(result)
	slices.SortFunc(layouts, func(a, b bundle.BuildLayout) int { return strings.Compare(a.BuildID, b.BuildID) })
	return result, layouts, retries, nil
}

func (e *Exporter) copyObject(ctx context.Context, stage, key, objectType string, refs []string) (*copiedObject, int, error) {
	tempDir := filepath.Join(stage, ".object-tmp")
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		return nil, 0, fmt.Errorf("create object temp directory: %w", err)
	}
	var lastErr error
	for attempt := 0; attempt < e.MaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, attempt, err
		}
		// Open 返回本次读取绑定的源对象身份。复制完成后再次 Stat，只有同一
		// 版本才发布到 Bundle；不稳定对象会从头重试并重新计算摘要。
		reader, before, err := e.Store.Open(ctx, key, "")
		if err != nil {
			return nil, attempt, fmt.Errorf("open source object %q: %w", key, err)
		}
		temp, err := os.CreateTemp(tempDir, ".copy-*")
		if err != nil {
			reader.Close()
			return nil, attempt, fmt.Errorf("create object temporary file: %w", err)
		}
		hash := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(temp, hash), reader)
		readerErr := reader.Close()
		syncErr := temp.Sync()
		closeErr := temp.Close()
		if copyErr != nil || readerErr != nil || syncErr != nil || closeErr != nil {
			_ = os.Remove(temp.Name())
			lastErr = errors.Join(copyErr, readerErr, syncErr, closeErr)
			continue
		}
		after, err := e.Store.Stat(ctx, key)
		if err != nil {
			_ = os.Remove(temp.Name())
			lastErr = err
			continue
		}
		if written != before.Size || !before.SameObjectVersion(after) {
			_ = os.Remove(temp.Name())
			lastErr = fmt.Errorf("source object changed while being read")
			continue
		}
		digest := hex.EncodeToString(hash.Sum(nil))
		relative, err := bundle.ObjectBundlePath(digest)
		if err != nil {
			_ = os.Remove(temp.Name())
			return nil, attempt, err
		}
		finalPath := filepath.Join(stage, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
			_ = os.Remove(temp.Name())
			return nil, attempt, err
		}
		if _, err := os.Stat(finalPath); errors.Is(err, os.ErrNotExist) {
			if err := os.Rename(temp.Name(), finalPath); err != nil {
				_ = os.Remove(temp.Name())
				return nil, attempt, fmt.Errorf("store bundle object %q: %w", key, err)
			}
		} else if err == nil {
			_ = os.Remove(temp.Name())
		} else {
			_ = os.Remove(temp.Name())
			return nil, attempt, err
		}
		return &copiedObject{record: bundle.ObjectRecord{LogicalKey: key, Type: objectType, ReferencingBuildIDs: uniqueSorted(refs), Size: written,
			SHA256: digest, BundlePath: relative, Required: true, SourceIdentity: before}, path: finalPath}, attempt, nil
	}
	return nil, e.MaxRetries - 1, fmt.Errorf("source object %q did not stabilize after %d attempts: %w", key, e.MaxRetries, lastErr)
}

func (e *Exporter) recheckSelection(ctx context.Context, selected *selection.Result) error {
	// 大对象复制期间不持有 PostgreSQL 事务。结束时只检查会使这次快照失效的
	// 删除和 ready 状态漂移；期间新增的 Tag/Alias 留给下一次导出。
	current, err := e.Catalog.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("recheck source catalog: %w", err)
	}
	templates := make(map[string]struct{}, len(current.Templates))
	for _, template := range current.Templates {
		templates[template.ID] = struct{}{}
	}
	builds := make(map[string]string, len(current.Builds))
	for _, build := range current.Builds {
		builds[build.ID] = build.StatusGroup
	}
	for _, template := range selected.Data.Templates {
		if _, ok := templates[template.ID]; !ok {
			return fmt.Errorf("selected template %q was deleted during export", template.ID)
		}
	}
	for _, build := range selected.Data.Builds {
		status, ok := builds[build.ID]
		if !ok {
			return fmt.Errorf("selected build %q was deleted during export", build.ID)
		}
		if status != model.StatusGroupReady {
			return fmt.Errorf("selected build %q left ready status during export: %s", build.ID, status)
		}
	}
	return nil
}

func writeJSON(filename string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %q: %w", filename, err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(filename, raw, 0o644); err != nil {
		return fmt.Errorf("write %q: %w", filename, err)
	}
	return nil
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; !ok {
			seen[value] = struct{}{}
			result = append(result, value)
		}
	}
	slices.Sort(result)
	return result
}
