//go:build linux

package block

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// A normal upstream snapshot after local checkpoints must retain every sealed
// write and honour newer explicit zeroes, for all three 2026.30 export paths.
func TestCheckpointLayersSurviveUpstreamExports(t *testing.T) {
	for _, mode := range []string{"eject", "background", "in-place"} {
		t.Run(mode, func(t *testing.T) {
			bs := int64(header.PageSize)
			dir := t.TempDir()
			newCache := func(name string) *Cache {
				c, err := NewCache(3*bs, bs, filepath.Join(dir, name), false)
				require.NoError(t, err)
				return c
			}
			base, err := NewEmpty(3*bs, bs, uuid.New())
			require.NoError(t, err)
			o := NewOverlay(base, newCache("c0"))
			t.Cleanup(func() { require.NoError(t, o.Close()) })
			_, err = o.WriteAt(fill(bs, 0xAA), 0)
			require.NoError(t, err)
			_, err = o.WriteAt(fill(bs, 0xBB), bs)
			require.NoError(t, err)
			first, err := o.Seal(newCache("c1"))
			require.NoError(t, err)
			_, err = o.WriteZeroesAt(0, bs)
			require.NoError(t, err)
			_, err = o.WriteAt(fill(bs, 0xCC), 2*bs)
			require.NoError(t, err)
			second, err := o.Seal(newCache("c2"))
			require.NoError(t, err)
			require.Equal(t, []int64{0, 2 * bs}, second.DirtyOffsets(), "zero blocks must be in sealed sidecars")
			_, err = o.WriteAt(fill(bs, 0xDD), bs)
			require.NoError(t, err)
			out, err := os.Create(filepath.Join(dir, "diff"))
			require.NoError(t, err)
			defer out.Close()
			var meta *header.DiffMetadata
			switch mode {
			case "eject":
				cache, e := o.EjectCache()
				require.NoError(t, e)
				defer cache.Close()
				meta, err = cache.ExportToDiff(t.Context(), out)
			case "background":
				cache, e := o.SwapCache(newCache("background"))
				require.NoError(t, e)
				meta, err = cache.ExportToDiff(t.Context(), out)
			case "in-place":
				meta, err = o.ExportDiffInPlace(t.Context(), out)
			}
			require.NoError(t, err)
			require.Equal(t, []uint32{1, 2}, meta.Dirty.ToArray())
			require.Equal(t, []uint32{0}, meta.Empty.ToArray())
			data, err := os.ReadFile(out.Name())
			require.NoError(t, err)
			require.Equal(t, append(fill(bs, 0xDD), fill(bs, 0xCC)...), data)
			// Close must keep the store-owned layer files for future restores.
			require.FileExists(t, first.filePath)
			require.FileExists(t, second.filePath)
		})
	}
}

func TestCheckpointReopenAndResetRetainsZeroes(t *testing.T) {
	bs := int64(header.PageSize)
	dir := t.TempDir()
	base, err := NewEmpty(2*bs, bs, uuid.New())
	require.NoError(t, err)
	fresh := func(name string) *Cache {
		c, e := NewCache(2*bs, bs, filepath.Join(dir, name), false)
		require.NoError(t, e)
		return c
	}
	o := NewOverlay(base, fresh("first"))
	_, err = o.WriteAt(fill(bs, 0xAA), 0)
	require.NoError(t, err)
	_, err = o.WriteZeroesAt(bs, bs)
	require.NoError(t, err)
	sealed, err := o.Seal(fresh("second"))
	require.NoError(t, err)
	require.NoError(t, sealed.MoveFile(filepath.Join(dir, "layer")))
	reopened, err := NewCache(2*bs, bs, sealed.filePath, false)
	require.NoError(t, err)
	reopened.MarkCached(sealed.DirtyOffsets())
	_, err = o.WriteAt(fill(bs, 0xBB), 0)
	require.NoError(t, err)
	require.NoError(t, o.ResetView(NewSealedView(base, reopened), fresh("restored")))
	buf := make([]byte, 2*bs)
	_, err = o.ReadAt(t.Context(), buf, 0)
	require.NoError(t, err)
	require.Equal(t, append(fill(bs, 0xAA), fill(bs, 0)...), buf)
	require.NoError(t, o.Close())
	require.FileExists(t, reopened.filePath)
}
