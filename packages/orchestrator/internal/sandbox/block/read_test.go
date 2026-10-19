package block

import (
	"context"
	"testing"
)

// sliceOnlySource mimics sources like build.File that only return a single
// block per Slice call, regardless of the requested length.
type sliceOnlySource struct {
	data       []byte
	blockSize  int64
	sliceCalls int
}

func (s *sliceOnlySource) BlockSize() int64 {
	return s.blockSize
}

func (s *sliceOnlySource) Slice(_ context.Context, off, length int64) ([]byte, error) {
	s.sliceCalls++

	end := off + min(length, s.blockSize)
	if end > int64(len(s.data)) {
		end = int64(len(s.data))
	}

	return s.data[off:end], nil
}

// fakeReadAtSource adds a working ReadAt on top of the single-block Slice.
type fakeReadAtSource struct {
	sliceOnlySource
	readAtCalls int
}

func (s *fakeReadAtSource) ReadAt(_ context.Context, p []byte, off int64) (int, error) {
	s.readAtCalls++

	return copy(p, s.data[off:]), nil
}

func TestReadSlice(t *testing.T) {
	const blockSize = 4096

	data := make([]byte, 4*blockSize)
	for i := range data {
		data[i] = byte(i)
	}

	ctx := context.Background()

	t.Run("single block uses Slice", func(t *testing.T) {
		src := &sliceOnlySource{data: data, blockSize: blockSize}

		got, err := ReadSlice(ctx, src, 0, blockSize)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(got) != blockSize {
			t.Fatalf("len = %d, want %d", len(got), blockSize)
		}

		if src.sliceCalls != 1 {
			t.Fatalf("sliceCalls = %d, want 1", src.sliceCalls)
		}
	})

	t.Run("multi block prefers ReadAt", func(t *testing.T) {
		src := &fakeReadAtSource{sliceOnlySource: sliceOnlySource{data: data, blockSize: blockSize}}

		got, err := ReadSlice(ctx, src, 0, 4*blockSize)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(got) != 4*blockSize {
			t.Fatalf("len = %d, want %d", len(got), 4*blockSize)
		}

		if src.readAtCalls != 1 {
			t.Fatalf("readAtCalls = %d, want 1", src.readAtCalls)
		}

		if src.sliceCalls != 0 {
			t.Fatalf("sliceCalls = %d, want 0", src.sliceCalls)
		}
	})

	t.Run("multi block falls back to assembling slices", func(t *testing.T) {
		src := &sliceOnlySource{data: data, blockSize: blockSize}

		got, err := ReadSlice(ctx, src, 0, 4*blockSize)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(got) != 4*blockSize {
			t.Fatalf("len = %d, want %d", len(got), 4*blockSize)
		}

		if src.sliceCalls != 4 {
			t.Fatalf("sliceCalls = %d, want 4", src.sliceCalls)
		}

		for i := range data {
			if got[i] != data[i] {
				t.Fatalf("byte %d = %d, want %d", i, got[i], data[i])
			}
		}
	})
}

// oversizedZeroSource mimics build.File returning a shared zero page that is
// larger than the file's own block size (e.g. the 2 MiB header.EmptyHugePage
// for a 4 KiB block when hugepages are disabled). ReadSlice must still return
// exactly `length` bytes, otherwise the UFFDIO_COPY length validation fails.
type oversizedZeroSource struct {
	blockSize int64
}

func (s *oversizedZeroSource) BlockSize() int64 { return s.blockSize }

func (s *oversizedZeroSource) Slice(_ context.Context, _, _ int64) ([]byte, error) {
	return make([]byte, 2*1024*1024), nil
}

func TestReadSliceTrimsOversizedZeroBlock(t *testing.T) {
	const blockSize = 4096

	src := &oversizedZeroSource{blockSize: blockSize}

	got, err := ReadSlice(context.Background(), src, 0, blockSize)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(got) != blockSize {
		t.Fatalf("len = %d, want %d", len(got), blockSize)
	}

	for i, b := range got {
		if b != 0 {
			t.Fatalf("byte %d = %d, want 0", i, b)
		}
	}
}
