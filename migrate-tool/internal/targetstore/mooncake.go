package targetstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/artifact"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/bundle"
)

// mooncakeChunkSize 必须等于 KASandbox 运行时 packages/shared/pkg/storage
// 的 MemoryChunkSize(storage.go,4 MiB)。Mooncake 本身是纯 KV 存储,
// "大对象按 4 MiB 分片、分片 key 为 <key>#c#<offset>、逻辑 key 存
// size/chunk_size 元数据"是 E2B 在其上自定的应用层布局,编译在运行时读端
// (storage_mooncake.go)里,没有可在线查询的接口,只能照读端源码对齐。
// 读端虽会从元数据读回 chunk_size,但其缓冲池按 MemoryChunkSize 固定分配,
// 4 MiB 是唯一安全值;写端(本工具)偏离读端会导致导入的对象运行时读不出来。
const mooncakeChunkSize int64 = 4 * 1024 * 1024

type mooncakeObjectMetadata struct {
	Size      int64 `json:"size"`
	ChunkSize int64 `json:"chunk_size"`
}

// Get returns a view consumed before the next call; implementations must use
// registered memory where the native transport requires it.
type mooncakeClient interface {
	Put(string, []byte) error
	Exists(string) (bool, error)
	Get(string, int64) ([]byte, error)
	Close()
}

type mooncakeStore struct {
	client    mooncakeClient
	namespace string
	reopen    func(context.Context) (mooncakeClient, error)
}

func newMooncakeStore(client mooncakeClient, namespace string) *mooncakeStore {
	return &mooncakeStore{client: client, namespace: namespace}
}

func (s *mooncakeStore) Close() {
	if s.client != nil {
		s.client.Close()
		s.client = nil
	}
}

func (s *mooncakeStore) Inspect(ctx context.Context, object bundle.ObjectRecord) (Observation, error) {
	if s.client == nil {
		return Observation{}, fmt.Errorf("Mooncake client is closed")
	}
	exists, err := s.client.Exists(s.key(object.LogicalKey))
	if err != nil {
		return Observation{}, fmt.Errorf("inspect Mooncake object %q: %w", object.LogicalKey, err)
	}
	if !exists {
		return Observation{}, nil
	}
	digest, err := s.digest(ctx, object)
	if err != nil {
		return Observation{Exists: true, Problem: err.Error()}, nil
	}
	return Observation{Exists: true, Identical: digest == object.SHA256, Digest: digest}, nil
}

func (s *mooncakeStore) Publish(_ context.Context, object bundle.ObjectRecord, filename string) (Observation, error) {
	// E2B 把内存及各块磁盘当作可随机读取的大对象，其余 header、snapfile、metadata
	// 都是单 key Blob。Bundle.Verify 已校验 type 与 logical key，这里不再匹配文件名。
	if artifact.IsDataType(object.Type) {
		return s.publishSeekable(object, filename)
	}
	return s.publishBlob(object, filename)
}

func (s *mooncakeStore) Recheck(ctx context.Context, object bundle.ObjectRecord, _ Observation) error {
	digest, err := s.digest(ctx, object)
	if err != nil {
		return err
	}
	if digest != object.SHA256 {
		return fmt.Errorf("Mooncake object %q SHA-256 mismatch: got %s, want %s", object.LogicalKey, digest, object.SHA256)
	}
	return nil
}

func (s *mooncakeStore) read(ctx context.Context, key string, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.client == nil {
		return nil, fmt.Errorf("Mooncake client is closed")
	}
	exists, err := s.client.Exists(s.key(key))
	if err != nil {
		return nil, fmt.Errorf("query Mooncake key %q: %w", key, err)
	}
	if !exists {
		return nil, fmt.Errorf("Mooncake key %q is missing: %w", key, os.ErrNotExist)
	}
	data, err := s.client.Get(s.key(key), limit)
	if err != nil {
		return nil, fmt.Errorf("read Mooncake key %q: %w", key, err)
	}
	return data, nil
}

func (s *mooncakeStore) digest(ctx context.Context, object bundle.ObjectRecord) (string, error) {
	hash := sha256.New()
	if artifact.IsDataType(object.Type) {
		data, err := s.read(ctx, object.LogicalKey, 4096)
		if err != nil {
			return "", err
		}
		var metadata mooncakeObjectMetadata
		if err := json.Unmarshal(data, &metadata); err != nil {
			return "", fmt.Errorf("decode Mooncake metadata %q: %w", object.LogicalKey, err)
		}
		if metadata.Size != object.Size || metadata.ChunkSize != mooncakeChunkSize {
			return "", fmt.Errorf("Mooncake metadata %q does not match Bundle size/chunk layout", object.LogicalKey)
		}
		for offset := int64(0); offset < object.Size; offset += mooncakeChunkSize {
			want := min(mooncakeChunkSize, object.Size-offset)
			key := fmt.Sprintf("%s#c#%d", object.LogicalKey, offset)
			data, err := s.read(ctx, key, want)
			if err != nil {
				return "", err
			}
			if int64(len(data)) != want {
				return "", fmt.Errorf("Mooncake chunk %q has %d bytes, want %d", key, len(data), want)
			}
			_, _ = hash.Write(data)
		}
	} else {
		data, err := s.read(ctx, object.LogicalKey, object.Size)
		if err != nil {
			return "", err
		}
		if int64(len(data)) != object.Size {
			return "", fmt.Errorf("Mooncake blob %q has %d bytes, want %d", object.LogicalKey, len(data), object.Size)
		}
		_, _ = hash.Write(data)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (s *mooncakeStore) VerifyAfterClose(ctx context.Context, objects []bundle.ObjectRecord) error {
	s.Close()
	if s.reopen == nil {
		return fmt.Errorf("Mooncake verification requires a fresh client factory")
	}
	client, err := s.reopen(ctx)
	if err != nil {
		return fmt.Errorf("open independent Mooncake reader: %w", err)
	}
	reader := newMooncakeStore(client, s.namespace)
	defer reader.Close()
	for _, object := range objects {
		if err := reader.Recheck(ctx, object, Observation{}); err != nil {
			return fmt.Errorf("verify after closing Mooncake writer: %w", err)
		}
	}
	return nil
}

func (s *mooncakeStore) publishBlob(object bundle.ObjectRecord, filename string) (Observation, error) {
	content, err := os.ReadFile(filename)
	if err != nil {
		return Observation{}, fmt.Errorf("read bundle object %q: %w", object.LogicalKey, err)
	}
	if err := verifyContent(object, content); err != nil {
		return Observation{}, err
	}
	if err := s.client.Put(s.key(object.LogicalKey), content); err != nil {
		return Observation{}, fmt.Errorf("write Mooncake blob %q: %w", object.LogicalKey, err)
	}
	return Observation{Exists: true, Identical: true, Digest: object.SHA256}, nil
}

func (s *mooncakeStore) publishSeekable(object bundle.ObjectRecord, filename string) (Observation, error) {
	input, err := os.Open(filename)
	if err != nil {
		return Observation{}, fmt.Errorf("open bundle object %q: %w", object.LogicalKey, err)
	}
	defer input.Close()

	hash := sha256.New()
	buffer := make([]byte, int(mooncakeChunkSize))
	var size int64
	for {
		read, readErr := io.ReadFull(input, buffer)
		if read > 0 {
			chunk := buffer[:read]
			_, _ = hash.Write(chunk)
			// `#c#OFFSET` 是 E2B Mooncake reader 的固定寻址协议，不是迁移工具
			// 自定义的临时命名。offset 使用原始对象中的绝对字节位置。
			chunkKey := fmt.Sprintf("%s#c#%d", s.key(object.LogicalKey), size)
			if err := s.client.Put(chunkKey, chunk); err != nil {
				return Observation{}, fmt.Errorf("write Mooncake chunk %q at offset %d: %w", object.LogicalKey, size, err)
			}
			size += int64(read)
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
		if readErr != nil {
			return Observation{}, fmt.Errorf("read bundle object %q: %w", object.LogicalKey, readErr)
		}
	}

	actual := hex.EncodeToString(hash.Sum(nil))
	if size != object.Size {
		return Observation{}, fmt.Errorf("bundle object %q size mismatch: got %d, want %d", object.LogicalKey, size, object.Size)
	}
	if actual != object.SHA256 {
		return Observation{}, fmt.Errorf("bundle object %q SHA-256 mismatch: got %s, want %s", object.LogicalKey, actual, object.SHA256)
	}

	metadata, err := json.Marshal(mooncakeObjectMetadata{Size: size, ChunkSize: mooncakeChunkSize})
	if err != nil {
		return Observation{}, fmt.Errorf("encode Mooncake metadata for %q: %w", object.LogicalKey, err)
	}
	// logical key 的元数据是 E2B 判断 Seekable 完整可读的提交点。它必须最后写；
	// 若分片或摘要校验失败，遗留 chunk 会在同一 namespace 的下次重跑中被覆盖。
	if err := s.client.Put(s.key(object.LogicalKey), metadata); err != nil {
		return Observation{}, fmt.Errorf("write Mooncake metadata %q: %w", object.LogicalKey, err)
	}
	return Observation{Exists: true, Identical: true, Digest: object.SHA256}, nil
}

func (s *mooncakeStore) key(logicalKey string) string {
	if s.namespace == "" {
		return logicalKey
	}
	return s.namespace + "/" + logicalKey
}

func verifyContent(object bundle.ObjectRecord, content []byte) error {
	if int64(len(content)) != object.Size {
		return fmt.Errorf("bundle object %q size mismatch: got %d, want %d", object.LogicalKey, len(content), object.Size)
	}
	digest := sha256.Sum256(content)
	actual := hex.EncodeToString(digest[:])
	if actual != object.SHA256 {
		return fmt.Errorf("bundle object %q SHA-256 mismatch: got %s, want %s", object.LogicalKey, actual, object.SHA256)
	}
	return nil
}
