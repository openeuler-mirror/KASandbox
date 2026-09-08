package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/fc"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

// Port is the in-sandbox port the checkpoint API is addressed on. Requests to
// it are answered by this service on the host instead of being proxied into
// the sandbox: a checkpoint pauses the VM and drives Firecracker's snapshot
// API, neither of which anything inside the VM can do.
const Port uint64 = 49984

const (
	rpcPrefix   = "/checkpoint.Checkpoint/"
	healthRoute = "/health"

	createMethod  = "CreateCheckpoint"
	restoreMethod = "RestoreCheckpoint"
	listMethod    = "ListCheckpoints"
	deleteMethod  = "DeleteCheckpoint"

	// trafficAccessTokenHeader mirrors the constant in internal/proxy. The
	// proxy's own token check sits below the point where checkpoint requests
	// are intercepted, so this service enforces it itself.
	trafficAccessTokenHeader = "e2b-traffic-access-token"

	// envdRestoreTimeout bounds waiting for the guest's envd to answer after
	// a restore, same role as the resume path's envd timeout.
	//
	// It has to stay well under the SDK's default request timeout, which is
	// also 60s. At equal budgets the client always gives up first, so the
	// caller sees a bare "ReadTimeout: timed out" and the one line that says
	// what actually happened -- the rollback succeeded, the guest did not come
	// back -- never reaches them. That is exactly the case where the operator
	// has the least to go on, and on 950 there is no one to read the logs.
	//
	// A healthy restore answers in ~2ms, so 45s is not a real constraint on
	// the guest; it only decides who reports the failure.
	envdRestoreTimeout = 45 * time.Second
)

type Service struct {
	store     *Store
	sandboxes *sandbox.Map

	dropConnections func(connectionKey string) error
}

func NewService(store *Store, sandboxes *sandbox.Map) *Service {
	return &Service{
		store:     store,
		sandboxes: sandboxes,
	}
}

// SetConnectionDropper hands the service a way to throw away pooled
// connections to a sandbox. A restore rolls the guest's TCP state back, so
// connections held open across it are talking to a peer that has forgotten
// them and have to go.
func (s *Service) SetConnectionDropper(drop func(connectionKey string) error) {
	s.dropConnections = drop
}

// OnInsert implements sandbox.MapSubscriber. Nothing happens at insert time.
func (s *Service) OnInsert(_ *sandbox.Sandbox) {}

// OnRemove implements sandbox.MapSubscriber. Checkpoints are host-local state
// with the sandbox's lifetime, so they go with it.
func (s *Service) OnRemove(sandboxID string) {
	if err := s.store.RemoveSandbox(sandboxID); err != nil {
		logger.L().Warn(context.Background(), "failed to remove sandbox checkpoints",
			logger.WithSandboxID(sandboxID),
			zap.Error(err))
	}
}

// Handles reports whether this service answers the request instead of the
// sandbox behind it: the checkpoint RPCs and the daemon health probe the SDK
// uses for is_running.
func Handles(port uint64, path string) bool {
	return port == Port && (path == healthRoute || strings.HasPrefix(path, rpcPrefix))
}

func (s *Service) ServeCheckpoint(w http.ResponseWriter, r *http.Request, sandboxID string) {
	sbx, ok := s.sandboxes.Get(sandboxID)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("sandbox %s not found", sandboxID))

		return
	}

	// The proxy validates the traffic access token below the point where this
	// service intercepts, so the same check happens here.
	if err := authorize(r, sbx); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated", err.Error())

		return
	}

	if r.URL.Path == healthRoute {
		writeJSON(w, map[string]any{"status": "ok"})

		return
	}

	var (
		err       error
		stats     = &callStats{}
		operation = r.URL.Path[len(rpcPrefix):]
		start     = time.Now()
	)

	switch operation {
	case createMethod:
		operation = "create"
		err = s.create(w, r, sbx, stats)
	case restoreMethod:
		operation = "restore"
		err = s.restore(w, r, sbx, stats)
	case listMethod:
		operation = "list"
		err = s.list(w, sbx)
	case deleteMethod:
		operation = "delete"
		err = s.delete(w, r, sbx)
	default:
		writeError(w, http.StatusNotFound, "unimplemented", fmt.Sprintf("unknown method %s", r.URL.Path))

		return
	}

	// The request context is gone by the time a create or restore finishes
	// its own work, so the operation's own context carries the measurement.
	record(context.WithoutCancel(r.Context()), operation, start, err, stats)
}

func authorize(r *http.Request, sbx *sandbox.Sandbox) error {
	net := sbx.Config.Network
	if net == nil || net.GetIngress() == nil || net.GetIngress().TrafficAccessToken == nil {
		return nil
	}

	raw := r.Header.Get(trafficAccessTokenHeader)
	if raw == "" {
		return fmt.Errorf("missing %s header", trafficAccessTokenHeader)
	}
	if raw != *net.GetIngress().TrafficAccessToken {
		return fmt.Errorf("invalid traffic access token")
	}

	return nil
}

// create takes a checkpoint: memory as a sparse diff holding only the pages
// this epoch dirtied (Firecracker writes them straight into a fresh file —
// no base is cloned, so the cost is O(dirty pages) on any filesystem), disk
// as a sealed write layer, both under one pause of the VM.
//
// The checkpoint that roots a sandbox's tree captures full memory instead,
// so the tree resolves every page from its own files. See fullRootEnabled.
// fullRootEnabled reports whether the checkpoint that roots a sandbox's tree
// captures full memory (the default). A full root makes the tree resolve
// every page from its own files, at the cost of one full-memory write and
// guest-memory-sized storage on the sandbox's first checkpoint.
//
// Turning it off (CHECKPOINT_FULL_ROOT=false) makes that first checkpoint as
// cheap as every other one and saves the storage, but roots the tree on a
// diff against the sandbox's start memory source: restores then resolve
// pages that predate every checkpoint from the template memfile, which on a
// cluster means reaching the object store during a rollback.
func fullRootEnabled() bool {
	switch os.Getenv("CHECKPOINT_FULL_ROOT") {
	case "", "true", "1":
		return true
	default:
		return false
	}
}

func (s *Service) create(w http.ResponseWriter, r *http.Request, sbx *sandbox.Sandbox, stats *callStats) error {
	// The VM is paused partway through. A client that gives up on the request
	// must not leave it that way, so the work is not tied to the request.
	ctx := context.WithoutCancel(r.Context())

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return writeError(w, http.StatusBadRequest, "invalid_argument", fmt.Sprintf("failed to read request: %s", err))
	}

	var req struct {
		Name string `json:"name"`
	}
	// An empty body is a valid request with no name set.
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return writeError(w, http.StatusBadRequest, "invalid_argument", fmt.Sprintf("failed to parse request: %s", err))
		}
	}

	sandboxID := sbx.Runtime.SandboxID

	timings := sandbox.PhaseTimings{}
	opStart := time.Now()

	unlock := s.store.LockSandbox(sandboxID)
	defer unlock()

	// The diff is relative to the tree parent: the previous checkpoint, or
	// the restore target after a rollback. A checkpoint with no parent roots
	// a tree and captures full memory instead — as does any checkpoint taken
	// without dirty tracking or on a broken chain, which roots a fresh
	// subtree because nothing links it to what came before.
	parentID, broken := s.store.BaseState(sandboxID)
	rooting := parentID == "" && fullRootEnabled()
	diff := fc.TrackDirtyPagesEnabled() && !broken && !rooting

	if !fc.TrackDirtyPagesEnabled() {
		logger.L().Warn(ctx, "dirty page tracking is off; every checkpoint captures full memory",
			logger.WithSandboxID(sandboxID))
	} else if broken {
		logger.L().Warn(ctx, "incremental chain is broken; capturing full memory as a new root",
			logger.WithSandboxID(sandboxID))
	} else if rooting {
		logger.L().Info(ctx, "rooting the checkpoint tree with a full memory capture",
			logger.WithSandboxID(sandboxID))
	}

	memMode := MemModeIncremental
	if !diff {
		memMode = MemModeFull
		parentID = ""
	}
	stats.set("mem_mode", memMode)

	prepareStart := time.Now()
	entry, err := s.store.Prepare(sandboxID, req.Name, parentID)
	timings.Mark("prepare", prepareStart)
	if err != nil {
		return writeError(w, http.StatusInternalServerError, "internal", fmt.Sprintf("failed to prepare checkpoint: %s", err))
	}

	layersDir, err := s.store.LayersDir(sandboxID)
	if err != nil {
		s.store.Discard(entry)
		return writeError(w, http.StatusInternalServerError, "internal", fmt.Sprintf("failed to prepare layers dir: %s", err))
	}

	// The base header seeds the disk bookkeeping on the sandbox's first
	// checkpoint; later ones ignore it.
	headerStart := time.Now()
	baseHeader, err := sbx.RootfsHeader()
	timings.Mark("rootfs_header", headerStart)
	if err != nil {
		s.store.Discard(entry)
		return writeError(w, http.StatusInternalServerError, "internal", fmt.Sprintf("failed to get rootfs header: %s", err))
	}

	layerID := uuid.New()
	sealedLayerPath := filepath.Join(layersDir, "layer-"+layerID.String())

	snapStart := time.Now()
	epochAdvanced, sealed, err := sbx.CheckpointToFiles(ctx, entry.TempSnapfile(), entry.TempMemDiff(), entry.TempMemBitmap(), diff, sealedLayerPath, timings)
	timings.Mark("checkpoint_to_files", snapStart)
	if err != nil {
		s.failCreate(ctx, entry, memMode, epochAdvanced)
		return writeError(w, http.StatusInternalServerError, "internal", fmt.Sprintf("failed to snapshot sandbox: %s", err))
	}

	appendStart := time.Now()
	rootfsView, err := s.store.AppendLayer(sandboxID, baseHeader, layerID, sealed, entry.RootfsHeaderPath())
	timings.Mark("append_layer", appendStart)
	if err != nil {
		s.failCreate(ctx, entry, memMode, epochAdvanced)
		return writeError(w, http.StatusInternalServerError, "internal", fmt.Sprintf("failed to record rootfs layer: %s", err))
	}

	commitStart := time.Now()
	if err := s.store.Commit(entry, memMode, rootfsView); err != nil {
		s.failCreate(ctx, entry, memMode, epochAdvanced)
		return writeError(w, http.StatusInternalServerError, "internal", fmt.Sprintf("failed to record checkpoint: %s", err))
	}
	timings.Mark("commit", commitStart)

	timings.Mark("total", opStart)
	writeTimings(filepath.Join(entry.Dir, "timings.json"), timings)

	logger.L().Info(ctx, "created checkpoint",
		logger.WithSandboxID(sandboxID),
		zap.String("checkpoint_id", entry.ID),
		zap.String("mem_mode", memMode),
		zap.Any("timings_ms", map[string]float64(timings)),
		zap.String("parent_id", entry.ParentID))

	// memMode is reported so a caller can tell an incremental checkpoint from
	// a full one. A full checkpoint on anything but a sandbox's first is what
	// happens when the host is not tracking dirty pages: it costs one guest
	// memory copy every time and is otherwise completely silent.
	writeJSON(w, map[string]any{"checkpointId": entry.ID, "memMode": memMode})

	return nil
}

// failCreate cleans up a failed checkpoint. When the Firecracker snapshot
// itself succeeded the epoch has advanced: the temp diff holds the only copy
// of that epoch's pages, so the entry is committed hidden and the tree stays
// sound. If even that fails, the chain is marked broken — the next
// checkpoint captures full memory as a new root, and restores are refused
// until it does, because an incomplete revert would be silent corruption.
func (s *Service) failCreate(ctx context.Context, entry *Entry, memMode string, epochAdvanced bool) {
	if epochAdvanced {
		if err := s.store.CommitHidden(entry, memMode); err != nil {
			s.store.InvalidateBase(entry.SandboxID)

			logger.L().Error(ctx, "failed to keep the advanced epoch; chain broken until a full checkpoint",
				logger.WithSandboxID(entry.SandboxID),
				zap.Error(err))

			s.store.Discard(entry)
		}

		return
	}

	s.store.Discard(entry)
}

// restore rolls the sandbox back onto a checkpoint in place: Firecracker
// writes the revert set back onto the live VM and the disk view is switched
// under the live NBD mount, all in one pause. This is the only route — the
// scenarios a rebuild would serve (a dead process, a torn VM) are explicitly
// outside what checkpoint restore promises and belong to e2b's own snapshot
// machinery.
func (s *Service) restore(w http.ResponseWriter, r *http.Request, sbx *sandbox.Sandbox, stats *callStats) error {
	// Same as create: a restore must not be abandoned mid-flight because the
	// client gave up.
	ctx := context.WithoutCancel(r.Context())

	id, err := checkpointIDFrom(r)
	if err != nil {
		return writeError(w, http.StatusBadRequest, "invalid_argument", err.Error())
	}

	sandboxID := sbx.Runtime.SandboxID

	timings := sandbox.PhaseTimings{}
	opStart := time.Now()

	unlock := s.store.LockSandbox(sandboxID)
	defer unlock()

	entry, ok := s.store.Get(sandboxID, id)
	if !ok {
		return writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("checkpoint %s not found", id))
	}

	layers, err := s.store.DiskViewForEntry(entry)
	if err != nil {
		return writeError(w, http.StatusInternalServerError, "internal", fmt.Sprintf("failed to load checkpoint disk view: %s", err))
	}
	if layers == nil {
		return writeError(w, http.StatusInternalServerError, "failed_precondition", fmt.Sprintf("checkpoint %s has no disk view and cannot be restored", id))
	}

	// Pages older than every checkpoint on the target's ancestor chain
	// resolve to the memory the sandbox started from.
	baseDev, err := sbx.Template.Memfile(ctx)
	if err != nil {
		return writeError(w, http.StatusInternalServerError, "internal", fmt.Sprintf("failed to open start memory source: %s", err))
	}

	rollbackStart := time.Now()
	err = sbx.RollbackInPlace(ctx, entry.Snapfile, func(liveBitmapPath string) (string, string, func(), error) {
		return s.store.MaterializeRevert(ctx, sandboxID, entry.ID, liveBitmapPath, baseDev)
	}, layers, timings)
	timings.Mark("rollback_in_place", rollbackStart)
	if err != nil {
		var torn sandbox.RollbackTornError
		if errors.As(err, &torn) {
			// Past the point of no return the guest is between two moments in
			// time; only recreating the sandbox helps.
			return writeError(w, http.StatusInternalServerError, "data_loss", fmt.Sprintf("restore failed past the commit point; the sandbox must be recreated: %s", err))
		}

		// Before that point the VM was resumed untouched: the sandbox keeps
		// running at its pre-restore state.
		return writeError(w, http.StatusInternalServerError, "internal", fmt.Sprintf("failed to restore sandbox (it keeps running at its current state): %s", err))
	}

	// The guest woke up with the clock of checkpoint time; envd init brings
	// the wall clock back to now, same as the pause/resume path. Monotonic
	// time staying rolled back is part of restore semantics.
	envdStart := time.Now()
	if err := sbx.WaitForEnvd(ctx, envdRestoreTimeout); err != nil {
		// The hypervisor half succeeded and the guest is the part that is
		// wedged, which is not visible from the error the caller gets. Say so
		// here, with the timings, so a rare recurrence leaves a trail even if
		// the client already walked away.
		logger.L().Error(ctx, "rollback succeeded but the guest's envd never answered",
			logger.WithSandboxID(sandboxID),
			zap.String("checkpoint_id", id),
			zap.Duration("waited", envdRestoreTimeout),
			zap.Any("timings_ms", timings),
			zap.Error(err))
		return writeError(w, http.StatusInternalServerError, "internal", fmt.Sprintf("sandbox restored but envd did not come back after %s: %s", envdRestoreTimeout, err))
	}
	timings.Mark("wait_envd", envdStart)

	// Dirty tracking restarted from zero at rollback, so the next diff is
	// relative to exactly this entry: the tree grows a branch from here, and
	// the checkpoints newer than the target stay restorable on theirs.
	s.store.SetBaseToEntry(entry)

	// The disk view was switched to the checkpoint's header; the next sealed
	// layer stacks on top of exactly what the sandbox now reads.
	if err := s.store.SetRootfsToEntry(entry); err != nil {
		logger.L().Error(ctx, "failed to reset rootfs bookkeeping after restore",
			logger.WithSandboxID(sandboxID),
			zap.Error(err))
	}

	if s.dropConnections != nil {
		if err := s.dropConnections(sbx.LifecycleID); err != nil {
			logger.L().Warn(ctx, "failed to drop connections to restored sandbox",
				logger.WithSandboxID(sandboxID),
				zap.Error(err))
		}
	}

	timings.Mark("total", opStart)
	writeTimings(filepath.Join(s.store.Root(), sandboxID, lastRestoreTimingsName), timings)

	logger.L().Info(ctx, "restored checkpoint",
		logger.WithSandboxID(sandboxID),
		zap.String("checkpoint_id", entry.ID),
		zap.Any("timings_ms", map[string]float64(timings)))

	writeJSON(w, map[string]any{"success": true})

	return nil
}

func (s *Service) list(w http.ResponseWriter, sbx *sandbox.Sandbox) error {
	entries := s.store.List(sbx.Runtime.SandboxID)

	infos := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		info := map[string]any{
			"checkpointId": e.ID,
			// protojson encodes int64 as a string.
			"createdAt": fmt.Sprintf("%d", e.CreatedAt.Unix()),
		}
		if e.Name != "" {
			info["name"] = e.Name
		}
		if e.MemMode != "" {
			info["memMode"] = e.MemMode
		}

		infos = append(infos, info)
	}

	writeJSON(w, map[string]any{"checkpoints": infos})

	return nil
}

func (s *Service) delete(w http.ResponseWriter, r *http.Request, sbx *sandbox.Sandbox) error {
	id, err := checkpointIDFrom(r)
	if err != nil {
		return writeError(w, http.StatusBadRequest, "invalid_argument", err.Error())
	}

	sandboxID := sbx.Runtime.SandboxID

	unlock := s.store.LockSandbox(sandboxID)
	defer unlock()

	if err := s.store.Delete(sandboxID, id); err != nil {
		return writeError(w, http.StatusNotFound, "not_found", err.Error())
	}

	writeJSON(w, map[string]any{"success": true})

	return nil
}

func checkpointIDFrom(r *http.Request) (string, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read request: %w", err)
	}

	// protojson emits lowerCamelCase, but accepts the proto field name too, so
	// both spellings have to be understood here.
	var req struct {
		CheckpointID      string `json:"checkpointId"`
		CheckpointIDSnake string `json:"checkpoint_id"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return "", fmt.Errorf("failed to parse request: %w", err)
	}

	id := req.CheckpointID
	if id == "" {
		id = req.CheckpointIDSnake
	}

	if id == "" {
		return "", fmt.Errorf("checkpoint_id is required")
	}

	if err := validateID(id); err != nil {
		return "", fmt.Errorf("invalid checkpoint id: %w", err)
	}

	return id, nil
}

func writeJSON(w http.ResponseWriter, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	_ = json.NewEncoder(w).Encode(body)
}

// writeError replies in the shape Connect defines for errors, which is what
// the SDK turns back into an exception. It returns what it reported, so a
// handler can answer the client and hand the outcome to its caller in one
// statement.
func writeError(w http.ResponseWriter, status int, code string, message string) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(map[string]any{
		"code":    code,
		"message": message,
	})

	return errors.New(message)
}

// lastRestoreTimingsName is where a sandbox's most recent restore leaves its
// host-side phase breakdown. Restores repeat and overwrite; a checkpoint's own
// timings.json is written once and stays.
const lastRestoreTimingsName = "last-restore-timings.json"

// writeTimings drops a host-side phase breakdown next to what it describes.
//
// It goes in a file rather than only into the log because that is what a
// benchmark can read: the client wall clock and the guest freeze window are
// both measurable from outside, but where the time went inside the host is
// not, and scraping it back out of log lines is a worse contract than a small
// JSON file. Failure to write it is not worth failing an otherwise good
// checkpoint over.
func writeTimings(path string, t sandbox.PhaseTimings) {
	blob, err := json.Marshal(t)
	if err != nil {
		return
	}

	_ = os.WriteFile(path, blob, 0o644)
}
