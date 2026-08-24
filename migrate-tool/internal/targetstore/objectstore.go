package targetstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/bundle"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/objectstore"
)

type objectStoreTarget struct {
	store objectstore.Store
}

func FromObjectStore(store objectstore.Store) Store {
	return &objectStoreTarget{store: store}
}

// FileStore 和 AWS S3 client 没有需要由一次 import 显式释放的连接句柄。
func (t *objectStoreTarget) Close() {}

func (t *objectStoreTarget) Inspect(ctx context.Context, object bundle.ObjectRecord) (Observation, error) {
	// File/S3 可以读取已有对象，因此仍保留原 Importer 的 skip-identical 语义：
	// 在同一次稳定版本读取中计算 SHA-256，而不是把 ETag 当成内容摘要。
	reader, before, err := t.store.Open(ctx, object.LogicalKey, "")
	if errors.Is(err, objectstore.ErrNotFound) {
		return Observation{}, nil
	}
	if err != nil {
		return Observation{}, fmt.Errorf("open target object %q: %w", object.LogicalKey, err)
	}

	hash := sha256.New()
	_, copyErr := io.Copy(hash, reader)
	closeErr := reader.Close()
	if copyErr != nil || closeErr != nil {
		return Observation{}, fmt.Errorf("hash target object %q: %w", object.LogicalKey, errors.Join(copyErr, closeErr))
	}
	after, err := t.store.Stat(ctx, object.LogicalKey)
	if err != nil {
		return Observation{}, fmt.Errorf("recheck target object %q: %w", object.LogicalKey, err)
	}
	if !before.SameObjectVersion(after) {
		return Observation{}, fmt.Errorf("target object %q changed while hashing", object.LogicalKey)
	}

	digest := hex.EncodeToString(hash.Sum(nil))
	return Observation{
		Exists:    true,
		Identical: digest == object.SHA256,
		Digest:    digest,
		identity:  after,
	}, nil
}

func (t *objectStoreTarget) Publish(ctx context.Context, object bundle.ObjectRecord, filename string) (Observation, error) {
	input, err := os.Open(filename)
	if err != nil {
		return Observation{}, fmt.Errorf("open bundle object %q: %w", object.LogicalKey, err)
	}
	// FileStore 的原子硬链接和 S3 的 If-None-Match 仍由原 adapter 负责；这里
	// 只把 Bundle 记录和文件路径转换成统一的目标端调用。
	identity, putErr := t.store.Put(ctx, object.LogicalKey, input, object.Size, object.SHA256)
	closeErr := input.Close()
	if putErr != nil || closeErr != nil {
		return Observation{}, fmt.Errorf("write target object %q: %w", object.LogicalKey, errors.Join(putErr, closeErr))
	}
	return Observation{Exists: true, Identical: true, Digest: object.SHA256, identity: identity}, nil
}

func (t *objectStoreTarget) Recheck(ctx context.Context, object bundle.ObjectRecord, observed Observation) error {
	current, err := t.store.Stat(ctx, object.LogicalKey)
	if err != nil {
		return fmt.Errorf("recheck target object %q before catalog commit: %w", object.LogicalKey, err)
	}
	if !observed.identity.SameObjectVersion(current) {
		return fmt.Errorf("target object %q changed before catalog commit", object.LogicalKey)
	}
	return nil
}
