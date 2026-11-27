package storage

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestZstdEncoderConcurrency(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		set  int
		want int
	}{
		{"unset", 0, 1},
		{"negative", -1, 1},
		{"one", 1, 1},
		{"explicit", 4, 4},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.want, zstdEncoderConcurrency(CompressConfig{EncoderConcurrency: tc.set}))
		})
	}
}

// TestCompressStreamDefaultEncoderConcurrency round-trips zstd with the
// encoder concurrency left unset, as deployments run it; defaultCfg sets 1.
func TestCompressStreamDefaultEncoderConcurrency(t *testing.T) {
	t.Parallel()

	cfg := defaultCfg(CompressionZstd, 1, 2*megabyte)
	cfg.EncoderConcurrency = 0
	original := generateSemiRandomData(10 * megabyte)

	up := &memPartUploader{}
	ft, _, err := compressStream(t.Context(), bytes.NewReader(original), cfg, up, 4, nil)
	require.NoError(t, err)

	decompressed, err := decompressAll(ft.Table(), up.Assemble())
	require.NoError(t, err)
	require.Equal(t, original, decompressed)
}

func TestSharedCompressorPoolKey(t *testing.T) {
	t.Parallel()

	base := defaultCfg(CompressionZstd, 4, DefaultCompressFrameSize)
	basePool, err := sharedCompressorPool(base)
	require.NoError(t, err)

	tests := []struct {
		name   string
		change func(*CompressConfig)
		shared bool
	}{
		{"same config", func(*CompressConfig) {}, true},
		{"more encode workers", func(c *CompressConfig) { c.FrameEncodeWorkers = 8 }, true},
		{"larger parts", func(c *CompressConfig) { c.MinPartSizeMB = 100 }, true},
		{"compression type", func(c *CompressConfig) { c.Type = CompressionLZ4.String() }, false},
		{"level", func(c *CompressConfig) { c.Level = 3 }, false},
		{"frame size", func(c *CompressConfig) { c.FrameSizeKB = 1024 }, false},
		{"encoder concurrency", func(c *CompressConfig) { c.EncoderConcurrency = 2 }, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := base
			tc.change(&cfg)
			pool, err := sharedCompressorPool(cfg)
			require.NoError(t, err)

			if tc.shared {
				require.Same(t, basePool, pool)
			} else {
				require.NotSame(t, basePool, pool)
			}
		})
	}
}

// TestCompressStreamReusesCompressorPool checks that compressStream takes its
// compressors from the shared pool for its config, so the encoders one upload
// warms up are there for the next.
func TestCompressStreamReusesCompressorPool(t *testing.T) {
	t.Parallel()

	// A config no other test uses, so no other test creates its pool.
	cfg := defaultCfg(CompressionZstd, 4, 512*1024)
	cfg.Level = 1
	key := newCompressorPoolKey(cfg)
	_, existed := compressorPools.Load(key)
	require.False(t, existed, "another test uses this config")
	t.Cleanup(func() { compressorPools.Delete(key) })

	data := generateSemiRandomData(4 * megabyte)
	upload := func() {
		_, _, err := compressStream(t.Context(), bytes.NewReader(data), cfg, &memPartUploader{}, 4, nil)
		require.NoError(t, err)
	}

	upload()
	first, ok := compressorPools.Load(key)
	require.True(t, ok, "compressStream did not use the shared pool")

	upload()
	second, _ := compressorPools.Load(key)
	require.Same(t, first, second)
}

// TestCompressStreamPerUploadCompressors checks the kill switch: the upload
// builds its own pool and leaves the shared ones alone.
func TestCompressStreamPerUploadCompressors(t *testing.T) {
	t.Parallel()

	// A config no other test uses, so no other test creates its pool.
	cfg := defaultCfg(CompressionLZ4, 4, 1024*1024)
	cfg.PerUploadCompressors = true
	key := newCompressorPoolKey(cfg)

	data := generateSemiRandomData(4 * megabyte)
	up := &memPartUploader{}
	ft, _, err := compressStream(t.Context(), bytes.NewReader(data), cfg, up, 4, nil)
	require.NoError(t, err)

	decompressed, err := decompressAll(ft.Table(), up.Assemble())
	require.NoError(t, err)
	require.Equal(t, data, decompressed)

	_, shared := compressorPools.Load(key)
	require.False(t, shared, "the upload used a shared pool")
}
