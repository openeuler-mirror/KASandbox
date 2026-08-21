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

const mooncakeChunkSize int64 = 4 * 1024 * 1024

type mooncakeObjectMetadata struct {
	Size      int64 `json:"size"`
	ChunkSize int64 `json:"chunk_size"`
}

// mooncakeClient 只保留导入所需的 native API。Mooncake 的 Get、range reader、
// NUMA buffer 等读取能力属于 E2B 运行时，不属于这个目标端迁移工具。
type mooncakeClient interface {
	Put(string, []byte) error
	Exists(string) (bool, error)
	Close()
}

type mooncakeStore struct {
	client    mooncakeClient
	namespace string
}

func newMooncakeStore(client mooncakeClient, namespace string) Store {
	return &mooncakeStore{client: client, namespace: namespace}
}

func (s *mooncakeStore) Close() {
	s.client.Close()
}

func (s *mooncakeStore) Inspect(_ context.Context, object bundle.ObjectRecord) (Observation, error) {
	exists, err := s.client.Exists(s.key(object.LogicalKey))
	if err != nil {
		return Observation{}, fmt.Errorf("inspect Mooncake object %q: %w", object.LogicalKey, err)
	}
	// 首版不读取 Mooncake 已有对象，也不伪造 VersionID。logical key 存在就由
	// Importer 按冲突处理，因此 Identical 固定为 false。
	return Observation{Exists: exists}, nil
}

func (s *mooncakeStore) Publish(_ context.Context, object bundle.ObjectRecord, filename string) (Observation, error) {
	// E2B 把 memfile/rootfs 当作可随机读取的大对象，其余 header、snapfile、metadata
	// 都是单 key Blob。Bundle.Verify 已校验 type 与 logical key，这里不再匹配文件名。
	switch object.Type {
	case artifact.TypeMemfile, artifact.TypeRootfs:
		return s.publishSeekable(object, filename)
	default:
		return s.publishBlob(object, filename)
	}
}

func (s *mooncakeStore) Recheck(_ context.Context, object bundle.ObjectRecord, _ Observation) error {
	exists, err := s.client.Exists(s.key(object.LogicalKey))
	if err != nil {
		return fmt.Errorf("recheck Mooncake object %q before catalog commit: %w", object.LogicalKey, err)
	}
	if !exists {
		return fmt.Errorf("Mooncake object %q disappeared before catalog commit", object.LogicalKey)
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
