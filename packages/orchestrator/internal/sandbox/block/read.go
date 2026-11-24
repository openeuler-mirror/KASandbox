package block

import (
	"context"
	"fmt"
)

// contextReaderAt is implemented by sources that support arbitrary-length
// reads (e.g. the template memfile, which is backed by build.File.ReadAt).
type contextReaderAt interface {
	ReadAt(ctx context.Context, p []byte, off int64) (int, error)
}

// ReadSlice returns exactly length bytes starting at offset from the source.
//
// A Slicer is only guaranteed to support slices of its own block size (see
// build.File.Slice), so a range larger than the block size must be read either
// through ReadAt or by assembling the buffer block by block.
func ReadSlice(ctx context.Context, source Slicer, offset, length int64) ([]byte, error) {
	if length == source.BlockSize() {
		block, err := source.Slice(ctx, offset, length)
		if err != nil {
			return nil, err
		}

		// Some sources return a full shared page that can be larger than the
		// requested block size (e.g. build.File returns the shared 2 MiB zero
		// page for zero blocks regardless of the file's block size). Trim it so
		// the returned slice always has exactly `length` bytes.
		if int64(len(block)) > length {
			block = block[:length]
		}

		return block, nil
	}

	if reader, ok := source.(contextReaderAt); ok {
		buf := make([]byte, length)

		n, err := reader.ReadAt(ctx, buf, offset)
		if err != nil {
			return nil, fmt.Errorf("failed to read %d bytes at offset %d: %w", length, offset, err)
		}

		if int64(n) != length {
			return nil, fmt.Errorf("read %d bytes at offset %d, expected %d", n, offset, length)
		}

		return buf, nil
	}

	// Fallback for sources that only support single-block slices.
	blockSize := source.BlockSize()
	buf := make([]byte, 0, length)

	for int64(len(buf)) < length {
		block, err := source.Slice(ctx, offset+int64(len(buf)), blockSize)
		if err != nil {
			return nil, err
		}

		if len(block) == 0 {
			return nil, fmt.Errorf("source returned an empty slice at offset %d", offset+int64(len(buf)))
		}

		buf = append(buf, block[:min(int64(len(block)), length-int64(len(buf)))]...)
	}

	return buf, nil
}
