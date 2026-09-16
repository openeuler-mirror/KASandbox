package targetstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/bundle"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/objectstore"
)

func TestObjectStoreTargetInspectsMissingObject(t *testing.T) {
	store, err := objectstore.NewFileStore(filepath.Join(t.TempDir(), "objects"))
	if err != nil {
		t.Fatal(err)
	}
	target := FromObjectStore(store)

	observed, err := target.Inspect(context.Background(), bundle.ObjectRecord{
		LogicalKey: "build/rootfs.ext4",
		SHA256:     "unused-for-missing-object",
	})
	if err != nil {
		t.Fatal(err)
	}
	if observed.Exists || observed.Identical || observed.Digest != "" {
		t.Fatalf("Inspect() = %#v, want a missing observation", observed)
	}
}

func TestObjectStoreTargetDistinguishesIdenticalContent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "objects")
	store, err := objectstore.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("known object content")
	digest := testDigest(content)
	if _, err := store.Put(context.Background(), "build/memfile", strings.NewReader(string(content)), int64(len(content)), digest); err != nil {
		t.Fatal(err)
	}
	target := FromObjectStore(store)

	identical, err := target.Inspect(context.Background(), bundle.ObjectRecord{LogicalKey: "build/memfile", SHA256: digest})
	if err != nil {
		t.Fatal(err)
	}
	if !identical.Exists || !identical.Identical || identical.Digest != digest {
		t.Fatalf("identical Inspect() = %#v", identical)
	}

	different, err := target.Inspect(context.Background(), bundle.ObjectRecord{LogicalKey: "build/memfile", SHA256: strings.Repeat("0", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if !different.Exists || different.Identical || different.Digest != digest {
		t.Fatalf("different Inspect() = %#v", different)
	}
}

func TestObjectStoreTargetPublishesAndRechecksTheObservedVersion(t *testing.T) {
	root := filepath.Join(t.TempDir(), "objects")
	store, err := objectstore.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("publish me")
	source := filepath.Join(t.TempDir(), "object")
	if err := os.WriteFile(source, content, 0o600); err != nil {
		t.Fatal(err)
	}
	record := bundle.ObjectRecord{
		LogicalKey: "build/metadata.json",
		Size:       int64(len(content)),
		SHA256:     testDigest(content),
	}
	target := FromObjectStore(store)

	observed, err := target.Publish(context.Background(), record, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := target.Recheck(context.Background(), record, observed); err != nil {
		t.Fatalf("Recheck() after Publish() = %v", err)
	}

	if err := os.WriteFile(filepath.Join(root, "build", "metadata.json"), []byte("changed content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := target.Recheck(context.Background(), record, observed); err == nil || !strings.Contains(err.Error(), "changed before catalog commit") {
		t.Fatalf("Recheck() after replacement = %v", err)
	}
}

func TestObjectStoreTargetRejectsVersionDriftWhileHashing(t *testing.T) {
	target := FromObjectStore(changingStore{})
	_, err := target.Inspect(context.Background(), bundle.ObjectRecord{LogicalKey: "object"})
	if err == nil || !strings.Contains(err.Error(), "changed while hashing") {
		t.Fatalf("Inspect() error = %v, want version drift error", err)
	}
}

type changingStore struct{}

func (changingStore) Kind() string { return "changing" }

func (changingStore) Open(context.Context, string, string) (io.ReadCloser, objectstore.Info, error) {
	modified := time.Unix(1, 0).UTC()
	return io.NopCloser(strings.NewReader("data")), objectstore.Info{
		Key: "object", Size: 4, ETag: "before", LastModified: modified,
	}, nil
}

func (changingStore) Stat(context.Context, string) (objectstore.Info, error) {
	return objectstore.Info{
		Key: "object", Size: 4, ETag: "after", LastModified: time.Unix(2, 0).UTC(),
	}, nil
}

func (changingStore) Put(context.Context, string, io.Reader, int64, string) (objectstore.Info, error) {
	panic("unexpected Put")
}

func testDigest(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}
