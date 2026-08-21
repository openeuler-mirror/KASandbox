package targetstore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/artifact"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/bundle"
)

func TestMooncakePublishesBlobAsOnePhysicalKey(t *testing.T) {
	client := newFakeMooncakeClient()
	target := newMooncakeStore(client, "templates-test")
	content := []byte(`{"template":{"build_id":"build"}}`)
	filename := filepath.Join(t.TempDir(), "metadata.json")
	if err := os.WriteFile(filename, content, 0o600); err != nil {
		t.Fatal(err)
	}
	record := bundle.ObjectRecord{
		LogicalKey: "build/metadata.json",
		Type:       artifact.TypeMetadata,
		Size:       int64(len(content)),
		SHA256:     testDigest(content),
	}

	observed, err := target.Publish(context.Background(), record, filename)
	if err != nil {
		t.Fatal(err)
	}
	if !observed.Exists || !observed.Identical {
		t.Fatalf("Publish() = %#v", observed)
	}
	wantKey := "templates-test/build/metadata.json"
	if len(client.puts) != 1 || client.puts[0] != wantKey {
		t.Fatalf("Put keys = %v, want [%s]", client.puts, wantKey)
	}
	if !bytes.Equal(client.objects[wantKey], content) {
		t.Fatalf("stored content = %q, want %q", client.objects[wantKey], content)
	}
}

func TestMooncakeDoesNotPublishBlobWithUnexpectedContent(t *testing.T) {
	client := newFakeMooncakeClient()
	target := newMooncakeStore(client, "templates-test")
	content := []byte("metadata")
	filename := filepath.Join(t.TempDir(), "metadata.json")
	if err := os.WriteFile(filename, content, 0o600); err != nil {
		t.Fatal(err)
	}
	record := bundle.ObjectRecord{
		LogicalKey: "build/metadata.json",
		Type:       artifact.TypeMetadata,
		Size:       int64(len(content)),
		SHA256:     testDigest([]byte("different")),
	}

	if _, err := target.Publish(context.Background(), record, filename); err == nil {
		t.Fatal("Publish() succeeded with an unexpected digest")
	}
	if len(client.puts) != 0 {
		t.Fatalf("Put keys = %v, want no writes", client.puts)
	}
}

func TestMooncakePublishesSeekableChunksBeforeMetadata(t *testing.T) {
	client := newFakeMooncakeClient()
	target := newMooncakeStore(client, "templates-test")
	content := append(bytes.Repeat([]byte{0x5a}, 4*1024*1024), 0x01, 0x02, 0x03)
	filename := filepath.Join(t.TempDir(), "memfile")
	if err := os.WriteFile(filename, content, 0o600); err != nil {
		t.Fatal(err)
	}
	record := bundle.ObjectRecord{
		LogicalKey: "build/memfile",
		Type:       artifact.TypeMemfile,
		Size:       int64(len(content)),
		SHA256:     testDigest(content),
	}

	if _, err := target.Publish(context.Background(), record, filename); err != nil {
		t.Fatal(err)
	}
	wantPuts := []string{
		"templates-test/build/memfile#c#0",
		"templates-test/build/memfile#c#4194304",
		"templates-test/build/memfile",
	}
	if len(client.puts) != len(wantPuts) {
		t.Fatalf("Put keys = %v, want %v", client.puts, wantPuts)
	}
	for index := range wantPuts {
		if client.puts[index] != wantPuts[index] {
			t.Fatalf("Put keys = %v, want %v", client.puts, wantPuts)
		}
	}
	if got := client.objects[wantPuts[0]]; !bytes.Equal(got, content[:4*1024*1024]) {
		t.Fatalf("first chunk has %d bytes and unexpected content", len(got))
	}
	if got := client.objects[wantPuts[1]]; !bytes.Equal(got, []byte{0x01, 0x02, 0x03}) {
		t.Fatalf("tail chunk = %v", got)
	}
	wantMetadata := []byte(`{"size":4194307,"chunk_size":4194304}`)
	if got := client.objects[wantPuts[2]]; !bytes.Equal(got, wantMetadata) {
		t.Fatalf("metadata = %s, want %s", got, wantMetadata)
	}
}

func TestMooncakeDoesNotCommitSeekableWithUnexpectedContent(t *testing.T) {
	content := []byte("seekable content")
	tests := []struct {
		name   string
		size   int64
		digest string
	}{
		{name: "size", size: int64(len(content) + 1), digest: testDigest(content)},
		{name: "digest", size: int64(len(content)), digest: testDigest([]byte("different"))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newFakeMooncakeClient()
			target := newMooncakeStore(client, "templates-test")
			filename := filepath.Join(t.TempDir(), "rootfs.ext4")
			if err := os.WriteFile(filename, content, 0o600); err != nil {
				t.Fatal(err)
			}
			record := bundle.ObjectRecord{
				LogicalKey: "build/rootfs.ext4",
				Type:       artifact.TypeRootfs,
				Size:       test.size,
				SHA256:     test.digest,
			}

			if _, err := target.Publish(context.Background(), record, filename); err == nil {
				t.Fatal("Publish() succeeded with unexpected content")
			}
			if _, exists := client.objects["templates-test/build/rootfs.ext4"]; exists {
				t.Fatal("logical metadata key was written")
			}
		})
	}
}

func TestMooncakeDoesNotCommitSeekableAfterChunkFailure(t *testing.T) {
	client := newFakeMooncakeClient()
	client.failPut = "templates-test/build/memfile#c#0"
	target := newMooncakeStore(client, "templates-test")
	content := []byte("seekable content")
	filename := filepath.Join(t.TempDir(), "memfile")
	if err := os.WriteFile(filename, content, 0o600); err != nil {
		t.Fatal(err)
	}
	record := bundle.ObjectRecord{
		LogicalKey: "build/memfile",
		Type:       artifact.TypeMemfile,
		Size:       int64(len(content)),
		SHA256:     testDigest(content),
	}

	if _, err := target.Publish(context.Background(), record, filename); err == nil {
		t.Fatal("Publish() succeeded after a chunk failure")
	}
	if _, exists := client.objects["templates-test/build/memfile"]; exists {
		t.Fatal("logical metadata key was written")
	}
}

func TestMooncakeTreatsExistingCompletionKeyAsConflict(t *testing.T) {
	client := newFakeMooncakeClient()
	client.objects["templates-test/build/memfile"] = []byte(`{"size":1,"chunk_size":4194304}`)
	target := newMooncakeStore(client, "templates-test")
	record := bundle.ObjectRecord{LogicalKey: "build/memfile", Type: artifact.TypeMemfile}

	observed, err := target.Inspect(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	if !observed.Exists || observed.Identical || observed.Digest != "" {
		t.Fatalf("Inspect() = %#v, want an existing non-identical observation", observed)
	}
	delete(client.objects, "templates-test/build/memfile")
	if err := target.Recheck(context.Background(), record, observed); err == nil {
		t.Fatal("Recheck() succeeded after the completion key disappeared")
	}
}

func TestMooncakeCloseReleasesClient(t *testing.T) {
	client := newFakeMooncakeClient()
	target := newMooncakeStore(client, "templates-test")
	target.Close()
	if !client.closed {
		t.Fatal("Close() did not release the Mooncake client")
	}
}

type fakeMooncakeClient struct {
	objects map[string][]byte
	puts    []string
	failPut string
	closed  bool
}

func newFakeMooncakeClient() *fakeMooncakeClient {
	return &fakeMooncakeClient{objects: make(map[string][]byte)}
}

func (c *fakeMooncakeClient) Put(key string, value []byte) error {
	c.puts = append(c.puts, key)
	if key == c.failPut {
		return errors.New("injected Put failure")
	}
	c.objects[key] = bytes.Clone(value)
	return nil
}

func (c *fakeMooncakeClient) Exists(key string) (bool, error) {
	_, ok := c.objects[key]
	return ok, nil
}

func (c *fakeMooncakeClient) Close() {
	c.closed = true
}
