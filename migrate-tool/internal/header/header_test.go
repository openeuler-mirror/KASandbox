package header_test

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/header"
)

func TestGoldenHeader(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "demo", "golden", "header-v3.bin"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if got, want := hex.EncodeToString(sum[:]), "6231337edf23a6833c135456359dcb5400310408e1cfb5c76dd7fbaff870b9f8"; got != want {
		t.Fatalf("golden SHA-256 = %s, want %s", got, want)
	}
	parsed, err := header.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := header.Validate(parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Metadata.BuildID != "00112233-4455-6677-8899-aabbccddeeff" {
		t.Fatalf("build id = %s", parsed.Metadata.BuildID)
	}
	if len(parsed.Mappings) != 3 || parsed.Mappings[0].BuildID != "ffeeddcc-bbaa-9988-7766-554433221100" || parsed.Mappings[2].BuildID != "00112233-4455-6677-8899-aabbccddeeff" {
		t.Fatalf("mappings = %#v", parsed.Mappings)
	}
}

func TestRejectsUnsupportedHeaderVersion(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "demo", "golden", "header-v3.bin"))
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(raw[:8], 4)
	if _, err := header.Parse(raw); err == nil {
		t.Fatal("expected unsupported header version error")
	}
}

func TestHeaderWithoutStoredMappingsUsesImplicitMapping(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "demo", "golden", "header-v3.bin"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := header.Parse(raw[:64])
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Mappings) != 1 || parsed.Mappings[0].BuildID != parsed.Metadata.BuildID || parsed.Mappings[0].Length != parsed.Metadata.Size {
		t.Fatalf("implicit mapping = %#v", parsed.Mappings)
	}
}

func TestRejectsPartialMapping(t *testing.T) {
	if _, err := header.Parse(make([]byte, 65)); err == nil {
		t.Fatal("expected partial mapping error")
	}
}
