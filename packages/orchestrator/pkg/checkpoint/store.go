// Package checkpoint implements the host side of sandbox checkpoint/rollback:
// a per-sandbox tree of diff checkpoints and the service that answers the
// in-sandbox checkpoint port (49984) on the host.
//
// Each checkpoint stores only the pages its epoch dirtied (a sparse diff
// file plus a bitmap sidecar); the tree roots at the sandbox's start memory
// source. A restore never re-materializes a full image on disk — the revert
// set is resolved page-by-page through the target's ancestor chain. This is
// what makes checkpoints O(dirty pages) on any filesystem: nothing here
// depends on reflink.
//
// Restoring rolls the live Firecracker process back in place; there is no
// rebuild route. Rolling back does not prune: checkpoints newer than the
// target stay restorable as branches (the next checkpoint's parent is the
// restore target), and deletion is an explicit API that hides entries still
// referenced by descendants instead of breaking their chains.
//
// Checkpoints are host-local and live only as long as the sandbox does: the
// files sit under the store root, and an orchestrator restart takes every
// sandbox with it anyway. The on-disk index exists so a run can be inspected
// after the fact, not to survive a restart.
package checkpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	indexFileName     = "index.json"
	manifestFileName  = "manifest.json"
	snapfileFileName  = "snapfile"
	memDiffFileName   = "mem_diff"
	memBitmapFileName = "mem_bitmap"
	rootfsHeaderName  = "rootfs.header"

	// Materialized rollback inputs, written next to the target checkpoint
	// for the duration of one restore.
	revertMemFileName    = "revert_mem"
	revertBitmapFileName = "revert_bitmap"

	// layersDirName holds sealed rootfs write layers. They are sandbox-private
	// bookkeeping, not checkpoint artifacts: several checkpoints' views can
	// reference the same layer, so deleting a checkpoint never deletes layers.
	// They are reclaimed with the sandbox.
	layersDirName = "layers"

	tempSuffix = ".tmp"
)

// State of a checkpoint. Only committed checkpoints are visible to Get/List;
// a half-written snapshot must never be restorable.
const (
	StatePrepared  = "prepared"
	StateCommitted = "committed"
)

// MemMode records what the memory file holds.
const (
	// MemModeFull: Firecracker wrote the complete guest memory. A full
	// checkpoint is a self-sufficient tree root: content resolution
	// terminates at it and never consults anything older. Taken when the
	// incremental chain is unavailable (dirty tracking off) or broken (an
	// epoch was lost).
	MemModeFull = "full"
	// MemModeIncremental: the file holds only the pages dirtied since the
	// parent checkpoint (or since the sandbox started, for a first
	// checkpoint), each at its guest-physical offset, holes elsewhere. Same
	// name as on the reflink branch, where the increment is written into a
	// clone of the parent instead of into a sparse file.
	MemModeIncremental = "incremental"
)

// RootfsLayer is one sealed write layer referenced by a checkpoint's rootfs
// view. The path points into the sandbox's shared layers directory.
type RootfsLayer struct {
	BuildID string `json:"build_id"`
	Path    string `json:"path"`
}

// RootfsView describes the disk state at checkpoint time: a serialized merged
// header (template base plus every sealed layer up to this point) and the
// local layer files the header's mappings resolve to.
type RootfsView struct {
	HeaderPath string        `json:"header_path"`
	Layers     []RootfsLayer `json:"layers,omitempty"`
}

// Entry is one checkpoint of one sandbox — one node of the sandbox's
// checkpoint tree. ParentID names the checkpoint its diff is relative to;
// "" means the tree root, the sandbox's start memory source. The tree shape
// follows use: checkpoints chain linearly until a restore, after which the
// next checkpoint's parent is the restore target and the newer checkpoints
// remain as a branch.
type Entry struct {
	ID        string    `json:"id"`
	Name      string    `json:"name,omitempty"`
	SandboxID string    `json:"sandbox_id"`
	CreatedAt time.Time `json:"created_at"`
	State     string    `json:"state"`

	// ParentID is the checkpoint this entry's diff is relative to; "" for a
	// root (diff against the start memory source, or a full capture).
	ParentID string `json:"parent_id,omitempty"`

	// Hidden marks an entry that is part of the tree but not of the API:
	// deleted while descendants still resolve pages through it, or the
	// rescued epoch of a failed create. List never shows it and Get never
	// returns it; content resolution and revert unions use it like any
	// other committed entry.
	Hidden bool `json:"hidden,omitempty"`

	Dir      string `json:"dir"`
	Snapfile string `json:"snapfile"`
	MemDiff  string `json:"mem_diff"`
	MemMode  string `json:"mem_mode,omitempty"`

	// MemBitmap is the sidecar recording which pages this checkpoint's
	// snapshot wrote — one epoch's dirty set. It is both the revert-set
	// ingredient and the page index of MemDiff; a diff checkpoint without
	// one is unusable. Full checkpoints may omit it (implicitly all ones).
	MemBitmap string `json:"mem_bitmap,omitempty"`

	Rootfs *RootfsView `json:"rootfs,omitempty"`
}

// TempSnapfile is where the snapshot is written before commit.
func (e *Entry) TempSnapfile() string { return e.Snapfile + tempSuffix }

// TempMemDiff is where the memory diff is written before commit.
func (e *Entry) TempMemDiff() string { return e.MemDiff + tempSuffix }

// TempMemBitmap is where the dirty bitmap sidecar is written before commit.
func (e *Entry) TempMemBitmap() string { return e.MemBitmap + tempSuffix }

// RootfsHeaderPath is where the merged rootfs header of this checkpoint goes.
func (e *Entry) RootfsHeaderPath() string { return filepath.Join(e.Dir, rootfsHeaderName) }

// baseRef records what the next checkpoint's diff is relative to — its
// parent-to-be in the tree. Absent from the map: the sandbox has no
// checkpoints yet, so the next diff is relative to the start memory source
// (the root). invalid: an epoch was lost and the chain is broken — the next
// checkpoint must capture full memory and become a new self-sufficient root,
// and until it does the sandbox cannot roll back.
type baseRef struct {
	entryID string
	invalid bool
}

// Store holds the checkpoints of every sandbox on this node, plus per-sandbox
// bookkeeping that is not part of any checkpoint: the parent-to-be of the
// next checkpoint and the per-sandbox operation lock.
type Store struct {
	root string

	mu        sync.Mutex
	bySandbox map[string]map[string]*Entry
	bases     map[string]baseRef
	rootfs    map[string]*rootfsState
	opLocks   map[string]*sync.Mutex
}

func NewStore(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create checkpoint store root: %w", err)
	}

	return &Store{
		root:      root,
		bySandbox: make(map[string]map[string]*Entry),
		bases:     make(map[string]baseRef),
		rootfs:    make(map[string]*rootfsState),
		opLocks:   make(map[string]*sync.Mutex),
	}, nil
}

// Root is where the store keeps every sandbox's checkpoints.
func (s *Store) Root() string {
	return s.root
}

// LockSandbox serializes checkpoint operations on one sandbox. Create,
// restore and delete each hold this for their full duration: the memory base
// and the rootfs layer stack are per-sandbox state that two operations must
// not advance concurrently.
func (s *Store) LockSandbox(sandboxID string) func() {
	s.mu.Lock()
	l, ok := s.opLocks[sandboxID]
	if !ok {
		l = &sync.Mutex{}
		s.opLocks[sandboxID] = l
	}
	s.mu.Unlock()

	l.Lock()

	return l.Unlock
}

// LayersDir returns (and creates) the sandbox's sealed-layer directory.
func (s *Store) LayersDir(sandboxID string) (string, error) {
	if err := validateID(sandboxID); err != nil {
		return "", fmt.Errorf("invalid sandbox id: %w", err)
	}

	dir := filepath.Join(s.root, sandboxID, layersDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create layers dir: %w", err)
	}

	return dir, nil
}

// BaseState reports the parent of the next checkpoint: the entry id ("" for
// the start-source root) and whether the chain is broken, in which case the
// next checkpoint must capture full memory and restores are refused until
// it does.
func (s *Store) BaseState(sandboxID string) (parentID string, broken bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	b := s.bases[sandboxID]

	return b.entryID, b.invalid
}

// Prepare reserves an entry, creates its directory and journals it as
// prepared, so the caller can write the snapshot into the temp files. The
// entry is not visible to Get or List until Commit. parentID is the
// checkpoint the diff is taken against — "" for a root capture.
func (s *Store) Prepare(sandboxID string, name string, parentID string) (*Entry, error) {
	if err := validateID(sandboxID); err != nil {
		return nil, fmt.Errorf("invalid sandbox id: %w", err)
	}

	id := fmt.Sprintf("ckpt_%d", time.Now().UnixNano())
	dir := filepath.Join(s.root, sandboxID, id)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create checkpoint dir: %w", err)
	}

	e := &Entry{
		ID:        id,
		Name:      name,
		SandboxID: sandboxID,
		CreatedAt: time.Now().UTC(),
		State:     StatePrepared,
		ParentID:  parentID,
		Dir:       dir,
		Snapfile:  filepath.Join(dir, snapfileFileName),
		MemDiff:   filepath.Join(dir, memDiffFileName),
		MemBitmap: filepath.Join(dir, memBitmapFileName),
	}

	if err := writeManifest(e); err != nil {
		os.RemoveAll(dir)

		return nil, err
	}

	return e, nil
}

// commitFiles moves the temp artifacts into their final names, durably. A
// diff checkpoint must have its sidecar: it is the only record of which
// pages the diff file holds, so without it neither content resolution nor
// revert sets can ever use the entry.
func commitFiles(e *Entry, memMode string, withSnapfile bool) error {
	files := [][2]string{
		{e.TempMemDiff(), e.MemDiff},
	}
	if withSnapfile {
		files = append(files, [2]string{e.TempSnapfile(), e.Snapfile})
	} else {
		// A hidden entry is never a restore target: only its diff and
		// sidecar take part in resolution and revert unions.
		os.Remove(e.TempSnapfile())
		e.Snapfile = ""
	}
	if _, err := os.Stat(e.TempMemBitmap()); err == nil {
		files = append(files, [2]string{e.TempMemBitmap(), e.MemBitmap})
	} else if memMode == MemModeIncremental {
		return fmt.Errorf("firecracker wrote no dirty bitmap sidecar; diff checkpoints need one")
	} else {
		e.MemBitmap = ""
	}

	for _, f := range files {
		if err := syncFile(f[0]); err != nil {
			return fmt.Errorf("failed to sync %s: %w", f[0], err)
		}

		if err := os.Rename(f[0], f[1]); err != nil {
			return fmt.Errorf("failed to move %s into place: %w", f[0], err)
		}
	}

	if err := syncDir(e.Dir); err != nil {
		return fmt.Errorf("failed to sync checkpoint dir: %w", err)
	}

	return nil
}

// Commit publishes a prepared entry: temp files are fsynced and renamed into
// place, the manifest flips to committed, and the next checkpoint's parent
// becomes this entry. The caller must have finished writing the temp files,
// including having resumed the VM.
func (s *Store) Commit(e *Entry, memMode string, rootfs *RootfsView) error {
	e.MemMode = memMode
	e.Rootfs = rootfs

	if err := commitFiles(e, memMode, true); err != nil {
		return err
	}

	e.State = StateCommitted
	if err := writeManifest(e); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.publishLocked(e)
}

// CommitHidden publishes a failed checkpoint's artifacts as a hidden entry.
// Once Firecracker has written the snapshot the epoch has advanced: the diff
// file holds the only copy of that epoch's pages and the sidecar is the only
// record of which pages those are. As a hidden committed entry the epoch
// stays a sound part of the tree — content resolution and revert unions use
// it like any other — while the API never shows it. The create RPC still
// fails; only the epoch survives.
func (s *Store) CommitHidden(e *Entry, memMode string) error {
	e.MemMode = memMode
	e.Hidden = true
	e.Rootfs = nil

	if err := commitFiles(e, memMode, false); err != nil {
		return err
	}

	os.Remove(e.RootfsHeaderPath())

	e.State = StateCommitted
	if err := writeManifest(e); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.publishLocked(e)
}

// publishLocked records a committed entry and makes it the base: Firecracker
// cleared its dirty bitmap when it wrote the snapshot, so from now on the
// live set counts from this moment and the next increment is relative to it.
func (s *Store) publishLocked(e *Entry) error {
	entries, ok := s.bySandbox[e.SandboxID]
	if !ok {
		entries = make(map[string]*Entry)
		s.bySandbox[e.SandboxID] = entries
	}
	entries[e.ID] = e

	s.setBaseLocked(e.SandboxID, baseRef{entryID: e.ID})

	return s.writeIndexLocked(e.SandboxID)
}

// InvalidateBase marks the sandbox's incremental chain broken: the epoch
// advanced (Firecracker cleared its dirty bitmap) but the file it produced
// could not be kept, so no future diff can account for that epoch's pages.
// The next checkpoint must capture full memory and become a new root, and
// until it does restores are refused — an incomplete revert would be a
// silently corrupted guest, which is strictly worse than an error.
func (s *Store) InvalidateBase(sandboxID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.setBaseLocked(sandboxID, baseRef{invalid: true})
}

// SetBaseToEntry makes a committed checkpoint the next checkpoint's parent.
// Called after a restore: dirty tracking restarted from zero at rollback, so
// the next diff is relative to exactly this entry — which is how a branch
// grows from the restore target instead of the abandoned tip.
func (s *Store) SetBaseToEntry(e *Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.setBaseLocked(e.SandboxID, baseRef{entryID: e.ID})
}

// setBaseLocked moves the base. A hidden entry that stops being the base was
// kept only for that role plus whatever hangs off it, so it is pruned once
// nothing resolves through it any more.
func (s *Store) setBaseLocked(sandboxID string, b baseRef) {
	prev := s.bases[sandboxID]
	s.bases[sandboxID] = b

	if prev.entryID == "" || prev.entryID == b.entryID {
		return
	}

	prevEntry, ok := s.bySandbox[sandboxID][prev.entryID]
	if !ok || !prevEntry.Hidden {
		return
	}

	s.pruneLocked(sandboxID, prevEntry)
}

// pruneLocked removes e if it is hidden, has no children and is not the
// base — no ancestor chain and no revert path runs through it any more —
// and then its parent on the same terms, up the tree.
func (s *Store) pruneLocked(sandboxID string, e *Entry) {
	entries := s.bySandbox[sandboxID]
	base := s.bases[sandboxID]

	for cur := e; cur != nil && cur.Hidden && cur.ID != base.entryID && !hasChildLocked(entries, cur.ID); {
		parentID := cur.ParentID

		delete(entries, cur.ID)
		os.RemoveAll(cur.Dir)

		cur = entries[parentID]
	}
}

// ancestorChainLocked returns target and its ancestors, newest first,
// ending at a root (ParentID ""). Every link must exist: the deletion rules
// hide referenced entries instead of removing them, so a missing parent is
// corruption, not a normal state.
func ancestorChainLocked(entries map[string]*Entry, target *Entry) ([]*Entry, error) {
	var chain []*Entry
	for e := target; ; {
		chain = append(chain, e)
		if e.ParentID == "" {
			break
		}

		p, ok := entries[e.ParentID]
		if !ok {
			return nil, fmt.Errorf("checkpoint %s references missing parent %s", e.ID, e.ParentID)
		}
		e = p
	}

	return chain, nil
}

// revertPathLocked returns the checkpoints whose epoch bitmaps cover every
// page that may differ between the current base moment and the target
// moment: both tree branches walked down to (and excluding) their lowest
// common ancestor. A page in neither branch was written on neither side of
// the fork, so both moments inherited it unchanged from the common ancestor
// and it needs no revert. targetChain is target's ancestor chain; baseID ""
// (no checkpoint yet) makes the whole target chain the path.
func revertPathLocked(entries map[string]*Entry, baseID string, targetChain []*Entry) ([]*Entry, error) {
	inTargetChain := map[string]bool{"": true}
	for _, e := range targetChain {
		inTargetChain[e.ID] = true
	}

	var path []*Entry

	lca := baseID
	for !inTargetChain[lca] {
		e, ok := entries[lca]
		if !ok {
			return nil, fmt.Errorf("current base %s is missing from the store", lca)
		}

		path = append(path, e)
		lca = e.ParentID
	}

	for _, e := range targetChain {
		if e.ID == lca {
			break
		}

		path = append(path, e)
	}

	return path, nil
}

// StartMemSource is the sandbox's start memory device — the root of the
// checkpoint tree. It mirrors the relevant part of block.ReadonlyDevice
// without this package importing the sandbox packages. ReadAt only accepts
// BlockSize()-aligned ranges (the template memfile is served by a chunker
// with that contract), so the materializer reads aligned windows and slices.
type StartMemSource interface {
	ReadAt(ctx context.Context, b []byte, off int64) (int, error)
	BlockSize() int64
}

// entryBitmap loads a checkpoint's epoch bitmap: its sidecar, or the
// implicit all-ones bitmap of a full capture written without one. A diff
// checkpoint without a sidecar is unusable — nothing records which pages
// its file holds — and poisons any restore whose path or resolution
// reaches it.
// entryContentBitmap says which pages a checkpoint's memory file can serve
// when resolving content for a restore. For a diff capture that is its epoch
// sidecar; a full capture holds every page whatever its sidecar records, so
// it terminates resolution and nothing below it is ever read.
func entryContentBitmap(e *Entry, pageSize uint64, numPages uint64) (*dirtyBitmap, error) {
	if e.MemMode == MemModeFull {
		return allOnesBitmap(pageSize, numPages), nil
	}

	return entryBitmap(e, pageSize, numPages)
}

func entryBitmap(e *Entry, pageSize uint64, numPages uint64) (*dirtyBitmap, error) {
	if e.MemBitmap == "" {
		if e.MemMode == MemModeFull {
			return allOnesBitmap(pageSize, numPages), nil
		}

		return nil, fmt.Errorf("checkpoint %s has no dirty bitmap sidecar", e.ID)
	}

	bm, err := readDirtyBitmap(e.MemBitmap)
	if err != nil {
		return nil, fmt.Errorf("failed to read sidecar of checkpoint %s: %w", e.ID, err)
	}
	if bm.pageSize != pageSize || bm.numPages != numPages {
		return nil, fmt.Errorf(
			"sidecar of checkpoint %s describes %d pages of %d bytes, guest has %d pages of %d",
			e.ID, bm.numPages, bm.pageSize, numPages, pageSize,
		)
	}

	return bm, nil
}

// MaterializeRevert builds the two files an in-place rollback to target
// needs: the revert bitmap — every page that may differ between the
// sandbox's memory now and the target moment, i.e. the union of the epoch
// bitmaps on the tree path from the current base to the target plus the
// live dirty set Firecracker just exported — and a sparse memory file
// holding the target-moment content of exactly those pages, each at its
// guest-physical offset. Firecracker then writes back precisely these pages
// and re-derives the same union itself, so nothing it touches can fall into
// a file hole.
//
// Content is resolved per page through the target's ancestor chain: the
// newest checkpoint at or before the target whose bitmap holds the page
// provides it from its memory file. A full capture roots the chain and holds
// every page, so resolution stops there; baseDev, the sandbox's start memory
// source, is only reached by trees rooted on a diff (CHECKPOINT_FULL_ROOT
// off, or checkpoints taken before it existed).
//
// The returned cleanup removes both files and is safe to call no matter how
// the rollback went.
func (s *Store) MaterializeRevert(ctx context.Context, sandboxID string, targetID string, liveBitmapPath string, baseDev StartMemSource) (memPath string, bitmapPath string, cleanup func(), err error) {
	s.mu.Lock()

	entries := s.bySandbox[sandboxID]
	target, ok := entries[targetID]
	if !ok || target.State != StateCommitted {
		s.mu.Unlock()

		return "", "", nil, fmt.Errorf("checkpoint %s not found", targetID)
	}

	base := s.bases[sandboxID]
	if base.invalid {
		s.mu.Unlock()

		return "", "", nil, fmt.Errorf("the incremental chain is broken (an epoch was lost); take a new checkpoint before restoring")
	}

	chain, err := ancestorChainLocked(entries, target)
	if err != nil {
		s.mu.Unlock()

		return "", "", nil, err
	}

	pathEntries, err := revertPathLocked(entries, base.entryID, chain)
	if err != nil {
		s.mu.Unlock()

		return "", "", nil, err
	}
	s.mu.Unlock()

	live, err := readDirtyBitmap(liveBitmapPath)
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to read live dirty bitmap: %w", err)
	}

	revert := live.clone()
	for _, e := range pathEntries {
		epoch, err := entryBitmap(e, revert.pageSize, revert.numPages)
		if err != nil {
			return "", "", nil, err
		}

		if err := revert.merge(epoch); err != nil {
			return "", "", nil, err
		}
	}

	chainBitmaps := make([]*dirtyBitmap, len(chain))
	for i, e := range chain {
		bm, err := entryContentBitmap(e, revert.pageSize, revert.numPages)
		if err != nil {
			return "", "", nil, err
		}

		chainBitmaps[i] = bm
	}

	memPath = filepath.Join(target.Dir, revertMemFileName+tempSuffix)
	bitmapPath = filepath.Join(target.Dir, revertBitmapFileName+tempSuffix)
	cleanup = func() {
		os.Remove(memPath)
		os.Remove(bitmapPath)
	}

	if err := writeRevertMem(ctx, memPath, revert, chain, chainBitmaps, baseDev); err != nil {
		cleanup()

		return "", "", nil, err
	}

	if err := revert.writeTo(bitmapPath); err != nil {
		cleanup()

		return "", "", nil, fmt.Errorf("failed to write revert bitmap: %w", err)
	}

	return memPath, bitmapPath, cleanup, nil
}

// revertExtentPages caps how many pages one read/write covers, so a huge
// revert run does not demand a huge buffer.
const revertExtentPages = 1024

// readAlignedFromBase fills b from the start memory source, whose ReadAt
// contract is block-aligned offsets and lengths: the covering aligned window
// is read block by block into a scratch buffer and the requested span copied
// out. memSize clamps the window — the device ends with guest memory.
func readAlignedFromBase(ctx context.Context, dev StartMemSource, b []byte, off int64, memSize int64) (int, error) {
	bs := dev.BlockSize()
	if bs <= 0 || (off%bs == 0 && int64(len(b))%bs == 0) {
		return dev.ReadAt(ctx, b, off)
	}

	lo := off / bs * bs
	hi := (off + int64(len(b)) + bs - 1) / bs * bs
	if hi > memSize {
		hi = memSize
	}

	scratch := make([]byte, hi-lo)
	for blockOff := lo; blockOff < hi; blockOff += bs {
		blockLen := min(bs, hi-blockOff)
		if _, err := dev.ReadAt(ctx, scratch[blockOff-lo:blockOff-lo+blockLen], blockOff); err != nil {
			return 0, err
		}
	}

	return copy(b, scratch[off-lo:]), nil
}

// writeRevertMem writes the target-moment content of every page set in
// revert into a sparse file at path, sized to the full guest memory (the
// rollback endpoint validates the length). Runs of consecutive pages
// resolved from the same source move as single extents.
func writeRevertMem(ctx context.Context, path string, revert *dirtyBitmap, chain []*Entry, chainBitmaps []*dirtyBitmap, baseDev StartMemSource) (e error) {
	out, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("failed to create revert memory file: %w", err)
	}
	defer func() {
		if closeErr := out.Close(); e == nil && closeErr != nil {
			e = fmt.Errorf("failed to close revert memory file: %w", closeErr)
		}
	}()

	pageSize := revert.pageSize
	if err := out.Truncate(int64(revert.numPages * pageSize)); err != nil {
		return fmt.Errorf("failed to size revert memory file: %w", err)
	}

	diffFiles := make(map[int]*os.File)
	defer func() {
		for _, f := range diffFiles {
			f.Close()
		}
	}()

	// resolve returns the chain index providing a page's target-moment
	// content, or -1 for the start memory source.
	resolve := func(page uint64) int {
		for i, bm := range chainBitmaps {
			if bm.contains(page) {
				return i
			}
		}

		return -1
	}

	buf := make([]byte, revertExtentPages*pageSize)

	memSize := int64(revert.numPages * pageSize)

	copyExtent := func(startPage uint64, endPage uint64, src int) error {
		var readAt func(b []byte, off int64) (int, error)
		if src < 0 {
			readAt = func(b []byte, off int64) (int, error) {
				return readAlignedFromBase(ctx, baseDev, b, off, memSize)
			}
		} else {
			f, ok := diffFiles[src]
			if !ok {
				var err error
				f, err = os.Open(chain[src].MemDiff)
				if err != nil {
					return fmt.Errorf("failed to open diff of checkpoint %s: %w", chain[src].ID, err)
				}
				diffFiles[src] = f
			}
			readAt = f.ReadAt
		}

		for page := startPage; page < endPage; page += revertExtentPages {
			pages := min(endPage-page, revertExtentPages)
			b := buf[:pages*pageSize]
			off := int64(page * pageSize)

			// A diff file's data at these offsets is present by construction
			// (the page is in its bitmap). A short read can only mean the
			// file ends early; the bytes past it are zeros, and the buffer
			// is reused, so its tail must be cleared explicitly.
			n, err := readAt(b, off)
			if err != nil && err != io.EOF {
				return fmt.Errorf("failed to read revert content at offset %d: %w", off, err)
			}
			clear(b[n:])

			if _, err := out.WriteAt(b, off); err != nil {
				return fmt.Errorf("failed to write revert content at offset %d: %w", off, err)
			}
		}

		return nil
	}

	var runStart uint64
	runSrc := 0
	inRun := false

	for page := uint64(0); page < revert.numPages; page++ {
		if !revert.contains(page) {
			if inRun {
				if err := copyExtent(runStart, page, runSrc); err != nil {
					return err
				}
				inRun = false
			}

			continue
		}

		src := resolve(page)
		if inRun && src == runSrc {
			continue
		}

		if inRun {
			if err := copyExtent(runStart, page, runSrc); err != nil {
				return err
			}
		}

		runStart, runSrc, inRun = page, src, true
	}

	if inRun {
		if err := copyExtent(runStart, revert.numPages, runSrc); err != nil {
			return err
		}
	}

	return nil
}

// Discard removes a prepared entry's directory. It is for cleaning up after a
// failed snapshot; any epoch rescue (CommitHidden) must happen before this.
func (s *Store) Discard(e *Entry) {
	if e == nil || e.Dir == "" || e.State != StatePrepared {
		return
	}

	os.RemoveAll(e.Dir)
}

// Get returns a committed, visible checkpoint. Hidden entries are tree
// bookkeeping, not API objects: they cannot be restored to or deleted.
func (s *Store) Get(sandboxID string, id string) (*Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.bySandbox[sandboxID][id]
	if !ok || e.Hidden {
		return nil, false
	}

	return e, ok
}

// List returns a sandbox's visible checkpoints, oldest first.
func (s *Store) List(sandboxID string) []*Entry {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries := make([]*Entry, 0, len(s.bySandbox[sandboxID]))
	for _, e := range s.bySandbox[sandboxID] {
		if e.Hidden {
			continue
		}

		entries = append(entries, e)
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].CreatedAt.Before(entries[j].CreatedAt)
	})

	return entries
}

// hasChildLocked reports whether any entry's diff is relative to id.
func hasChildLocked(entries map[string]*Entry, id string) bool {
	for _, e := range entries {
		if e.ParentID == id {
			return true
		}
	}

	return false
}

// Delete removes a checkpoint from the API. An entry that descendants still
// resolve pages through — one with children, or the parent-to-be of the
// next checkpoint — is hidden instead of removed: deleting its files would
// silently break every chain through it (the gsd implementation does exactly
// that). A leaf is removed physically, and hidden ancestors that just lost
// their last child are reclaimed with it.
func (s *Store) Delete(sandboxID string, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, ok := s.bySandbox[sandboxID]
	if !ok {
		return fmt.Errorf("sandbox %s has no checkpoints", sandboxID)
	}

	e, ok := entries[id]
	if !ok || e.Hidden {
		return fmt.Errorf("checkpoint %s not found", id)
	}

	base := s.bases[sandboxID]

	if hasChildLocked(entries, id) || base.entryID == id {
		// Hidden entries are never restore targets, so the snapfile and the
		// rootfs view go; the diff and its sidecar stay, because descendants
		// resolve pages through them.
		e.Hidden = true
		e.Rootfs = nil
		os.Remove(e.Snapfile)
		os.Remove(e.RootfsHeaderPath())
		e.Snapfile = ""

		if err := writeManifest(e); err != nil {
			return err
		}

		return s.writeIndexLocked(sandboxID)
	}

	parentID := e.ParentID
	delete(entries, id)
	if err := os.RemoveAll(e.Dir); err != nil {
		return fmt.Errorf("failed to remove checkpoint dir: %w", err)
	}

	s.pruneLocked(sandboxID, entries[parentID])

	return s.writeIndexLocked(sandboxID)
}

// RemoveSandbox drops every checkpoint, layer and base of a sandbox, for when
// it goes away.
func (s *Store) RemoveSandbox(sandboxID string) error {
	if err := validateID(sandboxID); err != nil {
		return fmt.Errorf("invalid sandbox id: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.bySandbox, sandboxID)
	delete(s.bases, sandboxID)
	delete(s.rootfs, sandboxID)
	delete(s.opLocks, sandboxID)

	if err := os.RemoveAll(filepath.Join(s.root, sandboxID)); err != nil {
		return fmt.Errorf("failed to remove sandbox checkpoints: %w", err)
	}

	return nil
}

func writeManifest(e *Entry) error {
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal manifest: %w", err)
	}

	path := filepath.Join(e.Dir, manifestFileName)
	tmp := path + tempSuffix
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("failed to write manifest: %w", err)
	}

	if err := syncFile(tmp); err != nil {
		return fmt.Errorf("failed to sync manifest: %w", err)
	}

	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("failed to replace manifest: %w", err)
	}

	return syncDir(e.Dir)
}

func (s *Store) writeIndexLocked(sandboxID string) error {
	entries := make([]*Entry, 0, len(s.bySandbox[sandboxID]))
	for _, e := range s.bySandbox[sandboxID] {
		entries = append(entries, e)
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].CreatedAt.Before(entries[j].CreatedAt)
	})

	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal checkpoint index: %w", err)
	}

	path := filepath.Join(s.root, sandboxID, indexFileName)

	tmp := path + tempSuffix
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("failed to write checkpoint index: %w", err)
	}

	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("failed to replace checkpoint index: %w", err)
	}

	return nil
}

func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	return f.Sync()
}

func syncDir(path string) error {
	return syncFile(path)
}

// validateID rejects anything that would let an id escape the store root.
func validateID(id string) error {
	if id == "" {
		return fmt.Errorf("empty id")
	}

	if strings.ContainsAny(id, "/\\") || strings.Contains(id, "..") {
		return fmt.Errorf("id %q contains path separators", id)
	}

	return nil
}
