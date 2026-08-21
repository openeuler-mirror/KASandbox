package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/model"
)

type File struct {
	path string
}

func NewFile(path string) (*File, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve catalog path: %w", err)
	}
	return &File{path: abs}, nil
}

func (f *File) Kind() string {
	return "file"
}

func (f *File) Snapshot(_ context.Context) (*model.CatalogData, error) {
	input, err := os.Open(f.path)
	if err != nil {
		return nil, fmt.Errorf("open catalog %q: %w", f.path, err)
	}
	defer input.Close()

	var data model.CatalogData
	decoder := json.NewDecoder(input)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&data); err != nil {
		return nil, fmt.Errorf("decode catalog %q: %w", f.path, err)
	}
	// Catalog 是单个 JSON 文档；接受第二个值会让拼接或损坏的文件只加载前半段。
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("unexpected JSON value after catalog")
		}
		return nil, fmt.Errorf("decode catalog %q: %w", f.path, err)
	}
	if err := Validate(&data); err != nil {
		return nil, fmt.Errorf("validate catalog %q: %w", f.path, err)
	}

	return &data, nil
}

func (f *File) Commit(_ context.Context, data *model.CatalogData) error {
	if err := Validate(data); err != nil {
		return fmt.Errorf("validate catalog before commit: %w", err)
	}

	directory := filepath.Dir(f.path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create catalog directory: %w", err)
	}

	// 先在同目录完整写入并 Sync，再 Rename 发布，避免进程中断留下半个 JSON。
	temp, err := os.CreateTemp(directory, ".catalog-*.tmp")
	if err != nil {
		return fmt.Errorf("create catalog temporary file: %w", err)
	}
	tempPath := temp.Name()
	committed := false
	defer func() {
		_ = temp.Close()
		if !committed {
			_ = os.Remove(tempPath)
		}
	}()

	encoder := json.NewEncoder(temp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(data); err != nil {
		return fmt.Errorf("encode catalog: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync catalog: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close catalog: %w", err)
	}
	if err := os.Chmod(tempPath, 0o644); err != nil {
		return fmt.Errorf("set catalog permissions: %w", err)
	}
	if err := os.Rename(tempPath, f.path); err != nil {
		return fmt.Errorf("replace catalog: %w", err)
	}
	committed = true

	return nil
}
