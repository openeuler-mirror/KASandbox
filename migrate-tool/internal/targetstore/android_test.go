package targetstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/artifact"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/bundle"
)

func TestMooncakeAndroidDisksUseSeekableLayout(t *testing.T) {
	for _, disk := range artifact.Layers(artifact.OSAndroid)[2:] {
		for _, size := range []int{7, int(mooncakeChunkSize), int(mooncakeChunkSize) + 17} {
			t.Run(fmt.Sprintf("%s/%d", disk.Name, size), func(t *testing.T) {
				client := newFakeMooncakeClient()
				target := newMooncakeStore(client, "android-test")
				content := make([]byte, size)
				for i := range content {
					content[i] = byte(i % 251)
				}
				filename := filepath.Join(t.TempDir(), disk.Name)
				if err := os.WriteFile(filename, content, 0o600); err != nil {
					t.Fatal(err)
				}
				record := bundle.ObjectRecord{LogicalKey: disk.DataKey("build"), Type: disk.DataType, Size: int64(size), SHA256: testDigest(content)}
				if _, err := target.Publish(context.Background(), record, filename); err != nil {
					t.Fatal(err)
				}
				key := "android-test/" + record.LogicalKey
				var meta mooncakeObjectMetadata
				if err := json.Unmarshal(client.objects[key], &meta); err != nil {
					t.Fatalf("disk was not stored as Seekable: %v", err)
				}
				if meta.Size != int64(size) || meta.ChunkSize != mooncakeChunkSize {
					t.Fatalf("metadata = %+v", meta)
				}
				var restored []byte
				for offset := int64(0); offset < meta.Size; offset += meta.ChunkSize {
					restored = append(restored, client.objects[fmt.Sprintf("%s#c#%d", key, offset)]...)
				}
				if !bytes.Equal(restored, content) {
					t.Fatal("Mooncake chunks do not reconstruct the disk")
				}
				if client.puts[len(client.puts)-1] != key {
					t.Fatalf("completion key not last: %v", client.puts)
				}
				client = newFakeMooncakeClient()
				client.failPut = key + "#c#0"
				target = newMooncakeStore(client, "android-test")
				if _, err := target.Publish(context.Background(), record, filename); err == nil {
					t.Fatal("chunk failure ignored")
				}
				if _, ok := client.objects[key]; ok {
					t.Fatal("failed disk published its completion key")
				}
			})
		}
	}
}

func TestMooncakeAndroidHeadersRemainBlobs(t *testing.T) {
	for _, disk := range artifact.Layers(artifact.OSAndroid)[2:] {
		client := newFakeMooncakeClient()
		target := newMooncakeStore(client, "test")
		content := []byte("opaque-header-bytes")
		filename := filepath.Join(t.TempDir(), disk.Name+".header")
		if err := os.WriteFile(filename, content, 0o600); err != nil {
			t.Fatal(err)
		}
		record := bundle.ObjectRecord{LogicalKey: disk.HeaderKey("build"), Type: disk.HeaderType, Size: int64(len(content)), SHA256: testDigest(content)}
		if _, err := target.Publish(context.Background(), record, filename); err != nil {
			t.Fatal(err)
		}
		if len(client.puts) != 1 || !bytes.Equal(client.objects["test/"+record.LogicalKey], content) {
			t.Fatal("header was not published as original Blob bytes")
		}
	}
}
