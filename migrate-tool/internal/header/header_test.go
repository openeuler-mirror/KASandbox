package header_test

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/header"
	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/testfixture"
)

// goldenHeader 在运行时生成确定性的 v3 Header 字节,不依赖任何提交的二进制
// fixture;生成器(testfixture)与解析器(header)互相独立实现同一布局。
func goldenHeader(t *testing.T) []byte {
	t.Helper()
	raw, err := testfixture.GoldenHeader()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestGoldenHeader(t *testing.T) {
	raw := goldenHeader(t)
	// SHA-256 锚定生成器与解析器共同遵守的 v3 字节布局,任何一侧偏离都会失败。
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
	raw := goldenHeader(t)
	binary.LittleEndian.PutUint64(raw[:8], 4)
	if _, err := header.Parse(raw); err == nil {
		t.Fatal("expected unsupported header version error")
	}
}

func TestHeaderWithoutStoredMappingsUsesImplicitMapping(t *testing.T) {
	raw := goldenHeader(t)
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
