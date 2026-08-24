package objectstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

type FileStore struct {
	root string
}

func NewFileStore(root string) (*FileStore, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve object store root: %w", err)
	}
	return &FileStore{root: filepath.Clean(abs)}, nil
}

func (s *FileStore) Kind() string {
	return "file"
}

func (s *FileStore) Stat(_ context.Context, key string) (Info, error) {
	filename, err := s.filename(key)
	if err != nil {
		return Info{}, err
	}
	stat, err := os.Stat(filename)
	if errors.Is(err, os.ErrNotExist) {
		return Info{}, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if err != nil {
		return Info{}, fmt.Errorf("stat object %q: %w", key, err)
	}
	return localFileInfo(key, stat), nil
}

func (s *FileStore) Open(_ context.Context, key string, versionID string) (io.ReadCloser, Info, error) {
	filename, err := s.filename(key)
	if err != nil {
		return nil, Info{}, err
	}
	input, err := os.Open(filename)
	if errors.Is(err, os.ErrNotExist) {
		return nil, Info{}, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if err != nil {
		return nil, Info{}, fmt.Errorf("open object %q: %w", key, err)
	}

	// 先打开再读取句柄元数据，避免路径在 Stat 与 Open 之间被替换后，
	// 返回的版本信息与真正读到的文件不是同一个对象。
	stat, err := input.Stat()
	if err != nil {
		_ = input.Close()
		return nil, Info{}, fmt.Errorf("stat opened object %q: %w", key, err)
	}
	info := localFileInfo(key, stat)
	if versionID != "" && versionID != info.ETag {
		_ = input.Close()
		return nil, Info{}, fmt.Errorf("object %q version changed: requested %s, current %s", key, versionID, info.ETag)
	}

	return input, info, nil
}

func (s *FileStore) Put(_ context.Context, key string, input io.Reader, size int64, expectedSHA256 string) (Info, error) {
	filename, err := s.filename(key)
	if err != nil {
		return Info{}, err
	}
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		return Info{}, fmt.Errorf("create object directory for %q: %w", key, err)
	}

	temp, err := os.CreateTemp(filepath.Dir(filename), ".object-*.tmp")
	if err != nil {
		return Info{}, fmt.Errorf("create temporary object for %q: %w", key, err)
	}
	tempPath := temp.Name()
	committed := false
	defer func() {
		_ = temp.Close()
		if !committed {
			_ = os.Remove(tempPath)
		}
	}()

	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(temp, hash), input)
	if err != nil {
		return Info{}, fmt.Errorf("write object %q: %w", key, err)
	}
	actualSHA256 := hex.EncodeToString(hash.Sum(nil))
	if size >= 0 && written != size {
		return Info{}, fmt.Errorf("object %q size mismatch: wrote %d, expected %d", key, written, size)
	}
	if expectedSHA256 != "" && actualSHA256 != expectedSHA256 {
		return Info{}, fmt.Errorf("object %q SHA-256 mismatch: wrote %s, expected %s", key, actualSHA256, expectedSHA256)
	}
	if err := temp.Sync(); err != nil {
		return Info{}, fmt.Errorf("sync object %q: %w", key, err)
	}
	if err := temp.Close(); err != nil {
		return Info{}, fmt.Errorf("close object %q: %w", key, err)
	}
	// 临时文件与目标位于同一目录。创建硬链接会在目标已存在时原子失败，
	// 从而与 S3 的 If-None-Match:* 保持一致，不覆盖计划阶段之后的并发写入。
	if err := os.Link(tempPath, filename); err != nil {
		return Info{}, fmt.Errorf("publish object %q without overwrite: %w", key, err)
	}
	committed = true
	_ = os.Remove(tempPath)
	// 硬链接创建和临时链接删除会改变 Linux ctime，因此发布完成后读取一次
	// 最终元数据。这里只做 stat，不重新扫描对象内容。
	stat, err := os.Stat(filename)
	if err != nil {
		return Info{}, fmt.Errorf("stat published object %q: %w", key, err)
	}

	return localFileInfo(key, stat), nil
}

func localFileInfo(key string, stat os.FileInfo) Info {
	modified := stat.ModTime().UTC()
	// 本地文件系统没有 S3 VersionID。版本标识只负责发现迁移期间的并发改写；
	// 对象内容摘要仍在真正复制数据的那一次流中计算。
	return Info{
		Key:          key,
		Size:         stat.Size(),
		ETag:         localFileVersion(stat),
		LastModified: modified,
	}
}

// 工具只支持 Linux(与运行时一致)。inode/device 能识别同路径替换,
// ctime 能识别保留 mtime 的原地改写。
func localFileVersion(stat os.FileInfo) string {
	raw, ok := stat.Sys().(*syscall.Stat_t)
	if !ok {
		return metadataFileVersion(stat)
	}
	return fmt.Sprintf(
		"file-%x-%x-%x-%x-%x-%x-%x",
		raw.Dev,
		raw.Ino,
		stat.Size(),
		stat.ModTime().UTC().UnixNano(),
		raw.Ctim.Sec,
		raw.Ctim.Nsec,
		raw.Nlink,
	)
}

func metadataFileVersion(stat os.FileInfo) string {
	return fmt.Sprintf("file-%x-%x", stat.Size(), stat.ModTime().UTC().UnixNano())
}

func (s *FileStore) filename(key string) (string, error) {
	if key == "" || strings.ContainsRune(key, '\\') {
		return "", fmt.Errorf("invalid object key %q", key)
	}
	clean := path.Clean(key)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return "", fmt.Errorf("object key escapes store root: %q", key)
	}
	filename := filepath.Join(s.root, filepath.FromSlash(clean))
	relative, err := filepath.Rel(s.root, filename)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("object key escapes store root: %q", key)
	}
	return filename, nil
}
