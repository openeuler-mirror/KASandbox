package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/fc"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/rootfs"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
)

// CheckpointToFiles pauses the VM, has Firecracker write a snapshot of it to
// snapfilePath and memfilePath, and resumes it.
//
// This is not Pause: nothing is exported into a template and the sandbox keeps
// running afterwards. The files are Firecracker's own snapshot output, so they
// can be handed straight back to it later.
//
// With diff set, the memory file only holds the pages dirtied since the
// previous snapshot, written at their original offsets. The caller passes a
// file that already holds the previous full view, so the overwrite yields the
// full view of this moment. Dirty page tracking has to have been armed when
// the VM was started or loaded.
//
// epochAdvanced reports whether Firecracker wrote the snapshot: doing so
// clears its dirty-page bitmap, so from that point on the memory file is the
// only valid base for the next Diff and must be preserved even if a later
// step fails.
func (s *Sandbox) CheckpointToFiles(ctx context.Context, snapfilePath string, memfilePath string, dirtyBitmapPath string, diff bool, sealedLayerPath string, timings PhaseTimings) (epochAdvanced bool, layer *rootfs.SealedLayer, e error) {
	ctx, span := tracer.Start(ctx, "sandbox-checkpoint-to-files")
	defer span.End()

	provider := s.Rootfs()

	sealer, ok := provider.(rootfs.LayerSealer)
	if !ok {
		return false, nil, fmt.Errorf("rootfs provider %T cannot seal write layers", provider)
	}

	process := s.process

	// How long the sandbox is frozen is the cost that matters here — not how
	// long the whole operation takes — so it is measured on its own.
	pausedAt := time.Now()

	if err := timings.Timed("pause", func() error { return process.Pause(ctx) }); err != nil {
		return false, nil, fmt.Errorf("failed to pause VM: %w", err)
	}

	// Resume even if the snapshot fails, and even if the caller gave up on us
	// meanwhile — a paused VM everyone believes is running is worse than a
	// missing snapshot.
	defer func() {
		if err := timings.Timed("resume", func() error { return process.ResumeVM(context.WithoutCancel(ctx)) }); err != nil {
			e = errors.Join(e, fmt.Errorf("failed to resume VM after snapshot: %w", err))
		}

		timings.Mark("frozen", pausedAt)
		checkpointPausedHistogram.Record(ctx, time.Since(pausedAt).Milliseconds())
	}()

	if err := timings.Timed("snapshot", func() error {
		return process.CreateSnapshotWithMemFile(ctx, snapfilePath, memfilePath, dirtyBitmapPath, diff)
	}); err != nil {
		return false, nil, fmt.Errorf("failed to create snapshot: %w", err)
	}

	// The disk is sealed inside the same pause window, so the layer and the
	// memory file are images of the same instant — anything the guest had not
	// flushed to disk yet is in the memory image, exactly as it was.
	newCachePath := filepath.Join(
		filepath.Dir(s.files.SandboxCacheRootfsPath(s.config.StorageConfig)),
		fmt.Sprintf("rootfs-%s-%s.cow", s.Runtime.SandboxID, uuid.NewString()),
	)

	sealStart := time.Now()
	layer, err := sealer.SealLayer(ctx, newCachePath, sealedLayerPath)
	timings.Mark("seal", sealStart)
	if err != nil {
		// The snapshot exists, so the epoch has advanced regardless.
		return true, nil, fmt.Errorf("failed to seal rootfs write layer: %w", err)
	}

	return true, layer, nil
}

// RootfsHeader returns the header describing the sandbox's template rootfs —
// the base of its layer stack.
func (s *Sandbox) RootfsHeader() (*header.Header, error) {
	device, err := s.Template.Rootfs()
	if err != nil {
		return nil, fmt.Errorf("failed to get template rootfs: %w", err)
	}

	return device.Header(), nil
}

// assembleView builds the read path of a checkpoint's disk view: the template
// base with the checkpoint's sealed layers stacked on top, each reopened from
// its raw layer file with the block knowledge from its sidecar.
func (s *Sandbox) assembleView(ctx context.Context, layers []RestoreLayer) (block.ReadonlyDevice, error) {
	base, err := s.Template.Rootfs()
	if err != nil {
		return nil, fmt.Errorf("failed to get template rootfs: %w", err)
	}

	size, err := base.Size(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get rootfs size: %w", err)
	}
	blockSize := base.BlockSize()

	var device block.ReadonlyDevice = base
	for _, layer := range layers {
		cache, err := block.NewCache(size, blockSize, layer.Path, false)
		if err != nil {
			return nil, fmt.Errorf("failed to reopen sealed layer %s: %w", layer.Path, err)
		}

		cache.MarkCached(layer.DirtyOffsets)
		device = block.NewSealedView(device, cache)
	}

	return device, nil
}

// newCowPath returns a fresh, unique path for a write layer file.
func (s *Sandbox) newCowPath() string {
	return filepath.Join(
		filepath.Dir(s.files.SandboxCacheRootfsPath(s.config.StorageConfig)),
		fmt.Sprintf("rootfs-%s-%s.cow", s.Runtime.SandboxID, uuid.NewString()),
	)
}

// RevertMaterializer builds the two files an in-place rollback needs, given
// the live dirty bitmap Firecracker just exported: the revert bitmap and a
// sparse memory file holding the target-moment content of exactly those
// pages. cleanup removes them and runs however the rollback ends.
type RevertMaterializer func(liveBitmapPath string) (memPath string, revertBitmapPath string, cleanup func(), err error)

// RollbackTornError marks a rollback that failed after guest state had
// already moved: memory is (partly) at the target but the disk view or the
// resume is not. The sandbox cannot continue and must be recreated.
type RollbackTornError struct{ Err error }

func (e RollbackTornError) Error() string {
	return fmt.Sprintf("rollback left the sandbox torn between two moments: %s", e.Err)
}

func (e RollbackTornError) Unwrap() error { return e.Err }

// RollbackInPlace rewinds the sandbox to a checkpoint without replacing the
// Firecracker process: FC writes back the revert set onto its live objects
// (cost proportional to the set), and the disk view is swapped underneath
// the live NBD mount inside the same pause window. All host resources —
// process, KVM fds, tap, network slot, NBD device — stay.
//
// The live dirty bitmap is exported and the revert files are materialized
// only after the pause: paused, the live set cannot grow, so the
// materialized content covers everything Firecracker will write back.
//
// Failures before the rollback's commit point resume the VM: the sandbox
// keeps running at its current state. Failures past it come back as
// RollbackTornError and only recreating the sandbox helps.
func (s *Sandbox) RollbackInPlace(
	ctx context.Context,
	snapfilePath string,
	materialize RevertMaterializer,
	layers []RestoreLayer,
	timings PhaseTimings,
) (e error) {
	ctx, span := tracer.Start(ctx, "sandbox-rollback-in-place")
	defer span.End()

	resetter, ok := s.Rootfs().(rootfs.ViewResetter)
	if !ok {
		return fmt.Errorf("rootfs provider %T cannot reset its view", s.Rootfs())
	}

	process := s.process

	rollbackPausedAt := time.Now()

	if err := timings.Timed("pause", func() error { return process.Pause(ctx) }); err != nil {
		return fmt.Errorf("failed to pause VM: %w", err)
	}

	// resumeOnError puts the VM back as it was, for failures that have not
	// touched guest state yet.
	resumeOnError := func(err error) error {
		if resumeErr := process.ResumeVM(context.WithoutCancel(ctx)); resumeErr != nil {
			err = errors.Join(err, fmt.Errorf("failed to resume VM: %w", resumeErr))
		}

		return err
	}

	liveBitmapPath := filepath.Join(filepath.Dir(snapfilePath), "live_bitmap.tmp")
	if err := timings.Timed("save_live_bitmap", func() error { return process.SaveDirtyBitmap(ctx, liveBitmapPath) }); err != nil {
		return resumeOnError(fmt.Errorf("failed to export live dirty bitmap: %w", err))
	}
	defer os.Remove(liveBitmapPath)

	// Where the restore's inputs came from — page cache or the disk. Both
	// halves read files with ordinary buffered reads: this process resolves
	// the revert set out of the ancestor chain's diff files, and Firecracker
	// then reads the materialized file back into guest memory. A warm cache
	// makes both nearly free, which is the normal case and also the reason
	// the numbers need a witness: a restore that quietly went to disk looks
	// exactly like one that did not, only slower. Without this counter
	// "recovery cost does not grow with chain depth" cannot be told apart
	// from "the chain happened to still be cached".
	selfBefore, selfOK := diskReadBytes(os.Getpid())

	materializeStart := time.Now()
	memfilePath, revertBitmapPath, cleanup, err := materialize(liveBitmapPath)
	timings.Mark("materialize", materializeStart)

	if mb, ok := diskReadSince(os.Getpid(), selfBefore, selfOK); ok {
		timings.SetMB("materialize_disk_read_mb", mb)
	}

	if err != nil {
		return resumeOnError(fmt.Errorf("failed to materialize revert files: %w", err))
	}
	defer cleanup()

	// Firecracker is a separate process, so its reads of the materialized
	// file are charged to its own pid, not ours. This file was written
	// moments ago and is never fsynced — it is removed again by cleanup — so
	// a non-zero reading here means writeback beat the rollback to it.
	fcPid, fcPidErr := process.Pid()

	var (
		fcBefore uint64
		fcOK     bool
	)

	if fcPidErr == nil {
		fcBefore, fcOK = diskReadBytes(fcPid)
	}

	// The rollback endpoint distinguishes failures before its commit point
	// (VM untouched, resumable) from after (VM faulted).
	fcStart := time.Now()
	result, err := process.RollbackSnapshot(ctx, snapfilePath, memfilePath, revertBitmapPath)
	timings.Mark("fc_rollback", fcStart)

	if mb, ok := diskReadSince(fcPid, fcBefore, fcOK); ok {
		timings.SetMB("fc_rollback_disk_read_mb", mb)
	}

	if err != nil {
		var faulted fc.RollbackFaultedError
		if errors.As(err, &faulted) {
			return RollbackTornError{Err: fmt.Errorf("in-place rollback failed: %w", err)}
		}

		return resumeOnError(fmt.Errorf("in-place rollback failed: %w", err))
	}

	// Firecracker measures its own half of the rollback and reports it. That
	// half is the bulk of the downtime, so surface the breakdown instead of
	// leaving the biggest phase as one opaque number.
	timings.SetUs("fc_validate", result.TimingsUs.Validate)
	timings.SetUs("fc_quiesce", result.TimingsUs.Quiesce)
	timings.SetUs("fc_memory", result.TimingsUs.Memory)
	timings.SetUs("fc_vcpus", result.TimingsUs.Vcpus)
	timings.SetUs("fc_gic", result.TimingsUs.Gic)
	timings.SetUs("fc_devices", result.TimingsUs.Devices)
	timings.SetUs("fc_total", result.TimingsUs.Total)

	telemetry.ReportEvent(ctx, fmt.Sprintf(
		"rolled back memory in place: %d pages, fc total %dus",
		result.RestoredPages, result.TimingsUs.Total,
	))

	// Disk view switch, inside the same pause window so disk and memory are
	// of the same instant. A failure here is past the memory rollback: the
	// guest is already at the target moment while the disk is not, so the
	// sandbox is torn and no resume happens.
	assembleStart := time.Now()
	device, err := s.assembleView(ctx, layers)
	timings.Mark("assemble_view", assembleStart)
	if err != nil {
		return RollbackTornError{Err: fmt.Errorf("failed to assemble checkpoint disk view: %w", err)}
	}

	if err := timings.Timed("reset_view", func() error { return resetter.ResetView(ctx, device, s.newCowPath()) }); err != nil {
		return RollbackTornError{Err: fmt.Errorf("failed to reset rootfs view: %w", err)}
	}

	// The quiescent moment for connection tracking: VM paused, no traffic
	// can re-create an entry the rollback invalidates.
	if err := timings.Timed("conntrack", func() error { return s.Slot.FlushConntrack(ctx) }); err != nil {
		logger.L().Warn(ctx, "failed to flush conntrack during in-place rollback",
			logger.WithSandboxID(s.Runtime.SandboxID),
			zap.Error(err))
	}

	if err := timings.Timed("resume", func() error { return process.ResumeVM(ctx) }); err != nil {
		return RollbackTornError{Err: fmt.Errorf("failed to resume VM after rollback: %w", err)}
	}

	timings.Mark("frozen", rollbackPausedAt)

	return nil
}

// RestoreLayer is one sealed rootfs layer of the disk view a restore brings
// back, bottom to top: the file and the block offsets it holds.
type RestoreLayer struct {
	Path         string
	DirtyOffsets []int64
}
