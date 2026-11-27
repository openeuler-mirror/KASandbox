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
