package objectstore_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/objectstore"
)

func TestFileStoreRejectsTraversal(t *testing.T) {
	store, err := objectstore.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), "../outside", strings.NewReader("x"), 1, ""); err == nil {
		t.Fatal("expected traversal to be rejected")
	}
}

func TestFileStoreDetectsChangeWithoutContentHashInStat(t *testing.T) {
	root := t.TempDir()
	store, err := objectstore.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), "object", strings.NewReader("aaaa"), 4, ""); err != nil {
		t.Fatal(err)
	}
	before, err := store.Stat(context.Background(), "object")
	if err != nil {
		t.Fatal(err)
	}
	newTime := before.LastModified.Add(2 * time.Second)
	filename := filepath.Join(root, "object")
	if err := os.WriteFile(filename, []byte("bbbb"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filename, newTime, newTime); err != nil {
		t.Fatal(err)
	}
	after, err := store.Stat(context.Background(), "object")
	if err != nil {
		t.Fatal(err)
	}
	if before.SameObjectVersion(after) {
		t.Fatalf("SameObjectVersion(%#v, %#v) = true", before, after)
	}
}

func TestFileStoreUsesMetadataAsLocalVersion(t *testing.T) {
	store, err := objectstore.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := "template object"
	digest := sha256.Sum256([]byte(content))
	written, err := store.Put(
		context.Background(),
		"objects/template",
		strings.NewReader(content),
		int64(len(content)),
		hex.EncodeToString(digest[:]),
	)
	if err != nil {
		t.Fatal(err)
	}
	if written.ETag == hex.EncodeToString(digest[:]) {
		t.Fatal("local ETag unexpectedly contains the content SHA-256")
	}

	current, err := store.Stat(context.Background(), "objects/template")
	if err != nil {
		t.Fatal(err)
	}
	if !written.SameObjectVersion(current) {
		t.Fatalf("Put returned %#v, Stat returned %#v", written, current)
	}
}

func TestFileStoreOpenRejectsStaleVersion(t *testing.T) {
	root := t.TempDir()
	store, err := objectstore.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.Put(context.Background(), "object", strings.NewReader("aaaa"), 4, "")
	if err != nil {
		t.Fatal(err)
	}

	filename := filepath.Join(root, "object")
	if err := os.WriteFile(filename, []byte("bbbb"), 0o644); err != nil {
		t.Fatal(err)
	}
	newTime := before.LastModified.Add(2 * time.Second)
	if err := os.Chtimes(filename, newTime, newTime); err != nil {
		t.Fatal(err)
	}
	reader, _, err := store.Open(context.Background(), "object", before.ETag)
	if err == nil {
		reader.Close()
		t.Fatal("expected stale local version to be rejected")
	}
	if reader != nil {
		reader.Close()
		t.Fatal("Open returned a reader for a stale version")
	}

	current, err := store.Stat(context.Background(), "object")
	if err != nil {
		t.Fatal(err)
	}
	reader, opened, err := store.Open(context.Background(), "object", current.ETag)
	if err != nil {
		t.Fatal(err)
	}
	content, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		t.Fatal(err)
	}
	if string(content) != "bbbb" {
		t.Fatalf("Open read %q, want %q", content, "bbbb")
	}
	if !current.SameObjectVersion(opened) {
		t.Fatalf("Stat returned %#v, Open returned %#v", current, opened)
	}
}

func TestFileStorePutFailureKeepsExistingObject(t *testing.T) {
	root := t.TempDir()
	store, err := objectstore.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), "object", strings.NewReader("original"), 8, ""); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name           string
		size           int64
		expectedSHA256 string
	}{
		{name: "size mismatch", size: 99},
		{name: "digest mismatch", size: 7, expectedSHA256: strings.Repeat("0", sha256.Size*2)},
		{name: "existing target", size: 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := store.Put(
				context.Background(),
				"object",
				strings.NewReader("changed"),
				tt.size,
				tt.expectedSHA256,
			)
			if err == nil {
				t.Fatal("expected Put validation to fail")
			}
			content, err := os.ReadFile(filepath.Join(root, "object"))
			if err != nil {
				t.Fatal(err)
			}
			if string(content) != "original" {
				t.Fatalf("existing object changed to %q", content)
			}
		})
	}

	temporary, err := filepath.Glob(filepath.Join(root, ".object-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temporary) != 0 {
		t.Fatalf("temporary files were not cleaned up: %v", temporary)
	}
}
