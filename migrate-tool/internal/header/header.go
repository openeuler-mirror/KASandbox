package header

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
)

const (
	metadataSize = 64
	mappingSize  = 40
	// 当前 E2B 对象使用 v3 Header。未来版本必须先确认二进制布局和语义，
	// 不能按 v3 偏移静默解析。
	supportedVersion = 3
)

type Metadata struct {
	Version     uint64
	BlockSize   uint64
	Size        uint64
	Generation  uint64
	BuildID     string
	BaseBuildID string
}

type Mapping struct {
	Offset             uint64
	Length             uint64
	BuildID            string
	BuildStorageOffset uint64
}

type Header struct {
	Metadata Metadata
	Mappings []Mapping
}

func Parse(data []byte) (Header, error) {
	// Header 使用 little-endian：前 64 字节是 Metadata，随后每 40 字节
	// 描述一个逻辑区间及其实际来源 Build。
	if len(data) < metadataSize {
		return Header{}, fmt.Errorf("header is %d bytes, need at least %d", len(data), metadataSize)
	}
	if (len(data)-metadataSize)%mappingSize != 0 {
		return Header{}, fmt.Errorf("header mapping payload is not a multiple of %d bytes", mappingSize)
	}

	metadata := Metadata{
		Version:     binary.LittleEndian.Uint64(data[0:8]),
		BlockSize:   binary.LittleEndian.Uint64(data[8:16]),
		Size:        binary.LittleEndian.Uint64(data[16:24]),
		Generation:  binary.LittleEndian.Uint64(data[24:32]),
		BuildID:     formatUUID(data[32:48]),
		BaseBuildID: formatUUID(data[48:64]),
	}
	if metadata.Version != supportedVersion {
		return Header{}, fmt.Errorf("unsupported header version %d, want %d", metadata.Version, supportedVersion)
	}
	if metadata.BlockSize == 0 {
		return Header{}, fmt.Errorf("header block size is zero")
	}

	mappings := make([]Mapping, 0, (len(data)-metadataSize)/mappingSize)
	for offset := metadataSize; offset < len(data); offset += mappingSize {
		mappings = append(mappings, Mapping{
			Offset:             binary.LittleEndian.Uint64(data[offset : offset+8]),
			Length:             binary.LittleEndian.Uint64(data[offset+8 : offset+16]),
			BuildID:            formatUUID(data[offset+16 : offset+32]),
			BuildStorageOffset: binary.LittleEndian.Uint64(data[offset+32 : offset+40]),
		})
	}
	if len(mappings) == 0 {
		// 没有显式 Mapping 的完整层等价于：整个逻辑空间来自当前 Build，
		// BuildStorageOffset 从 0 开始。
		mappings = append(mappings, Mapping{
			Offset:  0,
			Length:  metadata.Size,
			BuildID: metadata.BuildID,
		})
	}

	return Header{Metadata: metadata, Mappings: mappings}, nil
}

func Validate(h Header) error {
	if h.Metadata.BlockSize == 0 {
		return fmt.Errorf("header block size is zero")
	}
	if len(h.Mappings) == 0 {
		return fmt.Errorf("header has no effective mappings")
	}
	var current uint64
	for index, mapping := range h.Mappings {
		if mapping.Offset != current {
			return fmt.Errorf("mapping %d starts at %d, expected %d", index, mapping.Offset, current)
		}
		if mapping.Length == 0 || mapping.Length%h.Metadata.BlockSize != 0 {
			return fmt.Errorf("mapping %d length %d is not a positive multiple of block size %d", index, mapping.Length, h.Metadata.BlockSize)
		}
		if mapping.Length > math.MaxUint64-current || current+mapping.Length > h.Metadata.Size {
			return fmt.Errorf("mapping %d exceeds header size %d", index, h.Metadata.Size)
		}
		if mapping.Length > math.MaxUint64-mapping.BuildStorageOffset {
			return fmt.Errorf("mapping %d source range overflows", index)
		}
		current += mapping.Length
	}
	if current != h.Metadata.Size {
		return fmt.Errorf("mappings cover %d bytes, expected %d", current, h.Metadata.Size)
	}
	return nil
}

// NilUUID 表示零填充区间，不引用任何 Build 对象。
const NilUUID = "00000000-0000-0000-0000-000000000000"

func formatUUID(raw []byte) string {
	encoded := make([]byte, 36)
	hex.Encode(encoded[0:8], raw[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], raw[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], raw[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], raw[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], raw[10:16])
	return string(encoded)
}
