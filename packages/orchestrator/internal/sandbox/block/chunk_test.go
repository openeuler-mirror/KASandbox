package block

import (
	"context"
	"crypto/rand"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block/metrics"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

func newTestFullFetchChunker(t *testing.T, size int64) (*FullFetchChunker, []byte) {
	t.Helper()

	data := make([]byte, size)
	_, err := rand.Read(data)
	require.NoError(t, err)

	m, err := metrics.NewMetrics(noop.NewMeterProvider())
	require.NoError(t, err)

	c, err := NewFullFetchChunker(size, testBlockSize, &slowUpstream{data: data, blockSize: testBlockSize},
		filepath.Join(t.TempDir(), "cache"), m)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	return c, data
}

// 一次 64 KiB 的宿主页读取，其在存储里的起点不在 4 MiB 块边界上、跨进了下一块：
// 预热模板的页缓存页是 4 KiB 零散落盘的，这种跨块读在模板 24a1cbb3 上出现了 46 次，
// 线上 15a1df77 也有 18 处。修复前 fetchToCache 只按长度算块数、只拉起始块，
// 后半截没进缓存，Slice 报 BytesNotAvailableError，沙箱被杀。
func TestFullFetchChunker_ReadAcrossChunkBoundary(t *testing.T) {
	t.Parallel()

	size := int64(3 * storage.MemoryChunkSize)
	c, data := newTestFullFetchChunker(t, size)

	for _, tc := range []struct {
		name   string
		off    int64
		length int64
	}{
		{"64K page straddling the first chunk boundary", storage.MemoryChunkSize - testBlockSize, 16 * testBlockSize},
		{"64K page straddling the second chunk boundary", 2*storage.MemoryChunkSize - 15*testBlockSize, 16 * testBlockSize},
		{"read spanning three chunks", storage.MemoryChunkSize - testBlockSize, storage.MemoryChunkSize + 2*testBlockSize},
		{"aligned single block", storage.MemoryChunkSize, testBlockSize},
		{"last block of the file", size - testBlockSize, testBlockSize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := make([]byte, tc.length)
			n, err := c.ReadAt(context.Background(), buf, tc.off)
			require.NoError(t, err)
			require.Equal(t, int(tc.length), n)
			require.Equal(t, data[tc.off:tc.off+tc.length], buf)
		})
	}
}
