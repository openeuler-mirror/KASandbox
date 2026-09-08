package checkpoint

import (
	"encoding/binary"
	"fmt"
	"os"

	"github.com/google/uuid"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/rootfs"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// rootfsState is the sandbox's live disk-view bookkeeping: the merged header
// describing "template base + every sealed layer so far" and the layer files
// backing it. It is the disk-side sibling of the memory base — internal,
// per-sandbox, and reset by restores.
type rootfsState struct {
	meta    *header.Metadata
	mapping []*header.BuildMap
	layers  []RootfsLayer

	// poisoned is set when the in-memory stack and the bookkeeping can no
	// longer be proven to agree (a merge failed validation). Further
	// checkpoints must fail loudly rather than record wrong views.
	poisoned bool
}

// AppendLayer folds a sealed write layer into the sandbox's disk view and
// writes the merged header for the checkpoint being created. base seeds the
// bookkeeping on the first layer — it is the template rootfs header the
// sandbox started on.
//
// The in-memory view advances even if writing the header file fails: the
// layer is part of the live stack the moment it was sealed, and bookkeeping
// must follow the stack, not the RPC's fate.
func (s *Store) AppendLayer(sandboxID string, base *header.Header, layerID uuid.UUID, layer *rootfs.SealedLayer, headerOutPath string) (view *RootfsView, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	st, ok := s.rootfs[sandboxID]
	if !ok {
		// The layer is already sealed into the live stack by the time this
		// runs, so a sandbox whose stack cannot be described must be marked
		// as such rather than left looking healthy.
		st = &rootfsState{}
		s.rootfs[sandboxID] = st

		if base == nil {
			st.poisoned = true

			return nil, fmt.Errorf("no rootfs bookkeeping for sandbox %s and no base header to seed it", sandboxID)
		}

		st.meta = base.Metadata
		st.mapping = base.Mapping
	}

	if st.poisoned {
		return nil, fmt.Errorf("rootfs bookkeeping for sandbox %s is poisoned; refusing to record a view that may be wrong", sandboxID)
	}

	// Past this point the stack has advanced and the bookkeeping has to keep
	// up with it. Any failure means it no longer can, and every later view
	// built from it would be missing this layer — silently serving stale
	// blocks. Refuse to be trusted again instead; a restore reseeds the
	// bookkeeping from a checkpoint's own header and clears this.
	defer func() {
		if err != nil {
			st.poisoned = true
		}
	}()

	if int64(st.meta.Size) != layer.Size {
		return nil, fmt.Errorf("sealed layer size %d does not match device size %d", layer.Size, st.meta.Size)
	}

	layerMaps := identityMappings(layerID, layer.DirtyOffsets, layer.BlockSize)
	merged := header.NormalizeMappings(header.MergeMappings(st.mapping, layerMaps))

	if err := header.ValidateMappings(merged, st.meta.Size, st.meta.BlockSize); err != nil {
		return nil, fmt.Errorf("merged rootfs mappings failed validation: %w", err)
	}

	st.meta = st.meta.NextGeneration(layerID)
	st.mapping = merged
	st.layers = append(st.layers, RootfsLayer{BuildID: layerID.String(), Path: layer.Path})

	// The sidecar records which blocks the layer holds, so a restore can
	// reopen the raw layer file with the same knowledge the live cache had.
	if err := writeLayerMeta(layer.Path, layer.DirtyOffsets); err != nil {
		return nil, fmt.Errorf("failed to write layer sidecar: %w", err)
	}

	data, err := header.Serialize(st.meta, st.mapping)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize rootfs header: %w", err)
	}

	if err := os.WriteFile(headerOutPath, data, 0o644); err != nil {
		return nil, fmt.Errorf("failed to write rootfs header: %w", err)
	}

	if err := syncFile(headerOutPath); err != nil {
		return nil, fmt.Errorf("failed to sync rootfs header: %w", err)
	}

	layers := make([]RootfsLayer, len(st.layers))
	copy(layers, st.layers)

	return &RootfsView{
		HeaderPath: headerOutPath,
		Layers:     layers,
	}, nil
}

// SetRootfsToEntry resets the sandbox's disk-view bookkeeping to a restored
// checkpoint's view: the next sealed layer stacks on top of exactly what the
// sandbox now reads.
func (s *Store) SetRootfsToEntry(e *Entry) error {
	if e.Rootfs == nil {
		return fmt.Errorf("checkpoint %s has no rootfs view", e.ID)
	}

	data, err := os.ReadFile(e.Rootfs.HeaderPath)
	if err != nil {
		return fmt.Errorf("failed to read rootfs header: %w", err)
	}

	h, err := header.DeserializeBytes(data)
	if err != nil {
		return fmt.Errorf("failed to deserialize rootfs header: %w", err)
	}

	layers := make([]RootfsLayer, len(e.Rootfs.Layers))
	copy(layers, e.Rootfs.Layers)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.rootfs[e.SandboxID] = &rootfsState{
		meta:    h.Metadata,
		mapping: h.Mapping,
		layers:  layers,
	}

	return nil
}

// DiskViewForEntry loads the restore-side disk view of a checkpoint: its
// sealed layers bottom to top, each with the block offsets it holds (from the
// layer sidecars written at seal time).
func (s *Store) DiskViewForEntry(e *Entry) ([]sandbox.RestoreLayer, error) {
	if e.Rootfs == nil {
		return nil, nil
	}

	layers := make([]sandbox.RestoreLayer, 0, len(e.Rootfs.Layers))
	for _, l := range e.Rootfs.Layers {
		offsets, err := readLayerMeta(l.Path)
		if err != nil {
			return nil, fmt.Errorf("failed to read sidecar of layer %s: %w", l.Path, err)
		}

		layers = append(layers, sandbox.RestoreLayer{
			Path:         l.Path,
			DirtyOffsets: offsets,
		})
	}

	return layers, nil
}

const layerMetaSuffix = ".meta"

// writeLayerMeta persists a layer's block offsets next to it, 8 bytes
// little-endian each, via temp file and atomic rename.
func writeLayerMeta(layerPath string, offsets []int64) error {
	data := make([]byte, 8*len(offsets))
	for i, off := range offsets {
		binary.LittleEndian.PutUint64(data[8*i:], uint64(off))
	}

	path := layerPath + layerMetaSuffix
	tmp := path + tempSuffix
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}

	if err := syncFile(tmp); err != nil {
		return err
	}

	return os.Rename(tmp, path)
}

func readLayerMeta(layerPath string) ([]int64, error) {
	data, err := os.ReadFile(layerPath + layerMetaSuffix)
	if err != nil {
		return nil, err
	}

	if len(data)%8 != 0 {
		return nil, fmt.Errorf("sidecar has truncated content (%d bytes)", len(data))
	}

	offsets := make([]int64, len(data)/8)
	for i := range offsets {
		offsets[i] = int64(binary.LittleEndian.Uint64(data[8*i:]))
	}

	return offsets, nil
}

// identityMappings builds the header mappings a sealed layer contributes:
// runs of consecutive dirty blocks, each mapping a device range to the same
// offsets inside the layer file. Storage offset equals device offset because
// the layer is the raw write-layer file, sealed in place — no compaction.
func identityMappings(layerID uuid.UUID, dirtyOffsets []int64, blockSize int64) []*header.BuildMap {
	var mappings []*header.BuildMap

	var runStart, runLength int64
	flush := func() {
		if runLength == 0 {
			return
		}

		mappings = append(mappings, &header.BuildMap{
			Offset:             uint64(runStart),
			Length:             uint64(runLength),
			BuildId:            layerID,
			BuildStorageOffset: uint64(runStart),
		})
	}

	for _, off := range dirtyOffsets {
		if runLength > 0 && off == runStart+runLength {
			runLength += blockSize

			continue
		}

		flush()
		runStart, runLength = off, blockSize
	}
	flush()

	return mappings
}
