//go:build linux

package checkpoint

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/rootfs"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestCheckpointRootfsHeaderRoundTripAndBranch(t *testing.T) {
	for _, version := range []uint64{3, 4, 5} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			bs := int64(header.PageSize)
			dir := t.TempDir()
			store, err := NewStore(dir)
			require.NoError(t, err)
			baseID, firstID, secondID := uuid.New(), uuid.New(), uuid.New()
			meta := header.NewTemplateMetadata(baseID, uint64(bs), uint64(3*bs))
			meta.Version = version
			base, err := header.NewHeader(meta, nil)
			require.NoError(t, err)
			base.Builds = map[uuid.UUID]header.BuildData{baseID: {Size: 3 * bs, Checksum: [32]byte{1}}}
			makeLayer := func(id uuid.UUID, offset int64) *rootfs.SealedLayer {
				path := filepath.Join(dir, id.String())
				require.NoError(t, os.WriteFile(path, make([]byte, 3*bs), 0600))
				return &rootfs.SealedLayer{Path: path, DirtyOffsets: []int64{offset}, Size: 3 * bs, BlockSize: bs}
			}
			headerPath := filepath.Join(dir, "first.header")
			view, err := store.AppendLayer("lifecycle", base, firstID, makeLayer(firstID, bs), headerPath)
			require.NoError(t, err)
			raw, err := os.ReadFile(headerPath)
			require.NoError(t, err)
			decoded, err := header.DeserializeBytes(raw)
			require.NoError(t, err)
			require.Equal(t, version, decoded.Metadata.Version)
			require.Equal(t, firstID, decoded.Mapping.At(1).BuildId)
			if version >= 4 {
				require.Equal(t, base.Builds[baseID].Checksum, decoded.Builds[baseID].Checksum)
			}
			e := &Entry{SandboxID: "lifecycle", Rootfs: view}
			_, err = store.AppendLayer("lifecycle", base, secondID, makeLayer(secondID, 0), filepath.Join(dir, "second.header"))
			require.NoError(t, err)
			require.NoError(t, store.SetRootfsToEntry(e))
			branchID := uuid.New()
			branch, err := store.AppendLayer("lifecycle", base, branchID, makeLayer(branchID, 2*bs), filepath.Join(dir, "branch.header"))
			require.NoError(t, err)
			require.Len(t, branch.Layers, 2)
			require.Equal(t, firstID.String(), branch.Layers[0].BuildID)
			require.Equal(t, branchID.String(), branch.Layers[1].BuildID)
			layers, err := store.DiskViewForEntry(&Entry{Rootfs: branch})
			require.NoError(t, err)
			require.Equal(t, []int64{bs}, layers[0].DirtyOffsets)
			require.Equal(t, []int64{2 * bs}, layers[1].DirtyOffsets)
		})
	}
}
