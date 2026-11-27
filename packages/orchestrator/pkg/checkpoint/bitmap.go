package checkpoint

import (
	"encoding/binary"
	"fmt"
	"os"
)

// The FCDB sidecar format, shared with the forked Firecracker (see
// serialize_dirty_bitmap there): magic "FCDB", version u32 LE, page_size u64
// LE, num_pages u64 LE, then ceil(num_pages/64) u64 LE words, bit i of word w
// covering the page at file offset (w*64+i)*page_size of the memory file.
const (
	fcdbMagic   = "FCDB"
	fcdbVersion = 1
)

// dirtyBitmap is one parsed sidecar, or the running union of several.
type dirtyBitmap struct {
	pageSize uint64
	numPages uint64
	words    []uint64
}

func readDirtyBitmap(path string) (*dirtyBitmap, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	if len(data) < 24 || string(data[0:4]) != fcdbMagic {
		return nil, fmt.Errorf("malformed dirty bitmap file %s", path)
	}
	if v := binary.LittleEndian.Uint32(data[4:8]); v != fcdbVersion {
		return nil, fmt.Errorf("unsupported dirty bitmap version %d", v)
	}

	b := &dirtyBitmap{
		pageSize: binary.LittleEndian.Uint64(data[8:16]),
		numPages: binary.LittleEndian.Uint64(data[16:24]),
	}

	numWords := (b.numPages + 63) / 64
	if uint64(len(data)) != 24+numWords*8 {
		return nil, fmt.Errorf("truncated dirty bitmap file %s", path)
	}

	b.words = make([]uint64, numWords)
	for i := range b.words {
		b.words[i] = binary.LittleEndian.Uint64(data[24+i*8:])
	}

	return b, nil
}

// merge unions another bitmap into this one. Geometry must match — both
// describe the same guest memory.
func (b *dirtyBitmap) merge(other *dirtyBitmap) error {
	if b.pageSize != other.pageSize || b.numPages != other.numPages {
		return fmt.Errorf(
			"bitmap geometry mismatch: %d pages of %d vs %d pages of %d",
			b.numPages, b.pageSize, other.numPages, other.pageSize,
		)
	}

	for i := range b.words {
		b.words[i] |= other.words[i]
	}

	return nil
}

func (b *dirtyBitmap) clone() *dirtyBitmap {
	words := make([]uint64, len(b.words))
	copy(words, b.words)

	return &dirtyBitmap{pageSize: b.pageSize, numPages: b.numPages, words: words}
}

// writeTo serializes the bitmap into the sidecar format at path, fsynced.
func (b *dirtyBitmap) writeTo(path string) error {
	data := make([]byte, 24+len(b.words)*8)
	copy(data[0:4], fcdbMagic)
	binary.LittleEndian.PutUint32(data[4:8], fcdbVersion)
	binary.LittleEndian.PutUint64(data[8:16], b.pageSize)
	binary.LittleEndian.PutUint64(data[16:24], b.numPages)
	for i, w := range b.words {
		binary.LittleEndian.PutUint64(data[24+i*8:], w)
	}

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}

	return syncFile(path)
}

// contains reports whether the page's bit is set.
func (b *dirtyBitmap) contains(page uint64) bool {
	return b.words[page/64]&(1<<(page%64)) != 0
}

// allOnesBitmap is the implicit epoch bitmap of a full capture: every page.
// Bits past numPages stay clear so unions with real sidecars agree.
func allOnesBitmap(pageSize uint64, numPages uint64) *dirtyBitmap {
	words := make([]uint64, (numPages+63)/64)
	for i := range words {
		words[i] = ^uint64(0)
	}
	if r := numPages % 64; r != 0 {
		words[len(words)-1] = (uint64(1) << r) - 1
	}

	return &dirtyBitmap{pageSize: pageSize, numPages: numPages, words: words}
}
