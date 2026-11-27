//go:build linux

package checkpoint

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Restoring between sibling branches must rewrite the union of both paths
// while sourcing every byte from the target's ancestors, including zero pages.
func TestCheckpointMaterializeRevertAcrossBranches(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)
	const pageSize = uint64(4096)
	page := func(value byte) []byte { return bytes.Repeat([]byte{value}, int(pageSize)) }
	bitmap := func(pages ...uint64) *dirtyBitmap {
		b := &dirtyBitmap{pageSize: pageSize, numPages: 3, words: []uint64{0}}
		for _, p := range pages {
			b.words[0] |= 1 << p
		}
		return b
	}
	commit := func(parent, mode string, values map[uint64]byte, dirty *dirtyBitmap) *Entry {
		e, eerr := store.Prepare("lifecycle", "", parent)
		require.NoError(t, eerr)
		require.NoError(t, os.WriteFile(e.TempSnapfile(), []byte("snapshot"), 0600))
		data := make([]byte, 3*pageSize)
		for p, v := range values {
			copy(data[p*pageSize:], page(v))
		}
		require.NoError(t, os.WriteFile(e.TempMemDiff(), data, 0600))
		if dirty != nil {
			require.NoError(t, dirty.writeTo(e.TempMemBitmap()))
		}
		require.NoError(t, store.Commit(e, mode, nil))
		return e
	}
	root := commit("", MemModeFull, map[uint64]byte{0: 'A', 1: 'B', 2: 'C'}, nil)
	left := commit(root.ID, MemModeIncremental, map[uint64]byte{0: 'L'}, bitmap(0))
	store.SetBaseToEntry(root)
	right := commit(root.ID, MemModeIncremental, map[uint64]byte{1: 0}, bitmap(1))
	live := filepath.Join(store.Root(), "live.bitmap")
	require.NoError(t, bitmap(2).writeTo(live))
	mem, bm, cleanup, err := store.MaterializeRevert(t.Context(), "lifecycle", left.ID, live, nil)
	require.NoError(t, err)
	data, err := os.ReadFile(mem)
	require.NoError(t, err)
	require.Equal(t, append(append(page('L'), page('B')...), page('C')...), data)
	revert, err := readDirtyBitmap(bm)
	require.NoError(t, err)
	require.Equal(t, uint64(7), revert.words[0])
	cleanup()
	require.NoFileExists(t, mem)
	require.NoFileExists(t, bm)
	store.SetBaseToEntry(left)
	mem, _, cleanup, err = store.MaterializeRevert(t.Context(), "lifecycle", right.ID, live, nil)
	require.NoError(t, err)
	defer cleanup()
	data, err = os.ReadFile(mem)
	require.NoError(t, err)
	require.Equal(t, append(append(page('A'), page(0)...), page('C')...), data)
	store.InvalidateBase("lifecycle")
	_, _, _, err = store.MaterializeRevert(t.Context(), "lifecycle", right.ID, live, nil)
	require.ErrorContains(t, err, "chain is broken")
}
