package cube

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/cri-multiplex/pkg/engine"
)

const (
	annTemplateID   = "cube.dev/template-id"
	annVCPU         = "cube.dev/vcpu"
	annRAMMB        = "cube.dev/ram-mb"
	annDiskMB       = "cube.dev/disk-mb"
	annImage        = "cube.dev/image"
	annStorageMedia = "cube.dev/image-storage-media"
	annContainer    = "cube.dev/container-name"
	annVolumeName   = "cube.dev/volume-name"
	annCommand      = "cube.dev/command"
	annArgs         = "cube.dev/args"

	annMasterTemplateID      = "cube.master.appsnapshot.template.id"
	annMasterTemplateVersion = "cube.master.appsnapshot.template.version"
	annMasterAppSnapshotVer  = "cube.master.appsnapshot.version"
	annUpdateAction          = "cube.master.update_action"
	annPauseSnapshotID       = "cube.master.pause.snapshot.id"
	annRuntimeSnapshotID     = "cube.master.runtime.snapshot.id"
	annInstanceType          = "cube.master.instance.type"
	annStorageBackend        = "cube.master.storage.backend"
	annDesiredSandboxID      = "cube.master.desired.sandbox.id"
	annSnapshotAttachedAt    = "cube.master.runtime.snapshot.attached_at"
	annDeleteTombstone       = "cube.pause.delete_tombstone"

	podRunning = "running"
	podPaused  = "paused"
	// podParked is a paused sandbox whose CRI Stop/Remove already ran.
	// OpenFuyao's agent always removes the pod after pause; the snap id has to
	// stay on this node or the following RunPodSandbox becomes a cold create.
	podParked = "parked"
	// podStopped is a destroyed runtime kept only so List reports NOTREADY.
	// A missing id waits out the scheduler's fresh-row grace plus the orphan
	// confirm window, which misses a 120s idle check. An explicit stopped
	// phase releases the slot on the next reconcile after the fresh-row grace.
	podStopped = "stopped"

	ctrCreated = "created"
	ctrRunning = "running"
	ctrExited  = "exited"

	defaultDiskLimit = "10240Mi"
	labelHash        = "io.kubernetes.container.hash"
	labelRestart     = "io.kubernetes.container.restartCount"
)

// pauseDetachGrace absorbs the Stop+Remove pair the agent issues immediately
// after pause. A later Remove is a real delete and tombstones the snapshot.
var pauseDetachGrace = 5 * time.Second

// stoppedRetain is how long a deleted sandbox stays listable as NOTREADY.
const stoppedRetain = 10 * time.Minute

// Config 是 cube engine 的启动配置。
// Enabled 为 false 时不拨 Cubelet，CRI 入口返回 FailedPrecondition。
type Config struct {
	Deps        engine.Deps
	Enabled     bool
	CubeletAddr string
	NodeName    string
	Timeout     time.Duration
	StateStore  engine.StateStore
}

// Engine 把 CRI 沙箱翻译成本机 Cubelet 调用。
// Pause 目前只作为引擎方法，Admin 分发还没接。
type Engine struct {
	cfg      Config
	client   cubeClient
	mu       sync.Mutex
	inflight map[string]struct{}
	pods     map[string]*CubePodState
	// envdHTTP dials the guest envd directly (SandboxIP:49983). Tests may replace it.
	envdHTTP *http.Client

	// Background persist coalesces whole-map snapshots so Create only does a
	// short in-memory map insert (e2b tracker style). Pause/Destroy/Close wait.
	persistMu    sync.Mutex
	persistCond  *sync.Cond
	persistDirty bool
	persistGen   uint64
	flushedGen   uint64
	persistErr   error
	persistCh    chan struct{}
	persistStop  chan struct{}
	persistDone  chan struct{}
}

var _ engine.RuntimeEngine = (*Engine)(nil)

func New(cfg Config) (*Engine, error) {
	cfg = normalizeConfig(cfg)
	if !cfg.Enabled {
		return newEngine(cfg, nil), nil
	}
	if strings.TrimSpace(cfg.CubeletAddr) == "" {
		return nil, status.Error(codes.InvalidArgument, "cube: cubelet address is required")
	}
	client, err := dialCubelet(cfg.CubeletAddr, cfg.Timeout)
	if err != nil {
		return nil, err
	}
	return newEngine(cfg, client), nil
}

func normalizeConfig(cfg Config) Config {
	if cfg.StateStore == nil {
		cfg.StateStore = cfg.Deps.Store
	}
	if cfg.NodeName == "" {
		cfg.NodeName = cfg.Deps.NodeName
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 120 * time.Second
	}
	return cfg
}

func newEngine(cfg Config, client cubeClient) *Engine {
	cfg = normalizeConfig(cfg)
	e := &Engine{
		cfg:         cfg,
		client:      client,
		inflight:    map[string]struct{}{},
		pods:        map[string]*CubePodState{},
		persistCh:   make(chan struct{}, 1),
		persistStop: make(chan struct{}),
		persistDone: make(chan struct{}),
	}
	e.persistCond = sync.NewCond(&e.persistMu)
	go e.persistLoop()
	return e
}

func (e *Engine) Type() engine.EngineType { return engine.EngineTypeCube }

func (e *Engine) Close() error {
	if e == nil {
		return nil
	}
	e.schedulePersist()
	if err := e.waitPersist(); err != nil {
		log.Printf("[CubeEngine] state sync on close: %v", err)
	}
	select {
	case <-e.persistStop:
	default:
		close(e.persistStop)
	}
	<-e.persistDone
	if syncer, ok := e.cfg.StateStore.(engine.LazyStateStore); ok {
		if err := syncer.Sync(); err != nil {
			log.Printf("[CubeEngine] state store sync on close: %v", err)
		}
	}
	if e.client == nil {
		return nil
	}
	return e.client.Close()
}

func (e *Engine) RestoreState(ctx context.Context) error {
	if e == nil || !e.cfg.Enabled {
		return nil
	}
	if e.cfg.StateStore != nil {
		var loaded map[string]CubePodState
		if _, err := e.cfg.StateStore.LoadBackendState(cubeStatePath, &loaded); err != nil {
			return err
		}
		e.mu.Lock()
		for id, pod := range loaded {
			if pod.CRISandboxID == "" {
				pod.CRISandboxID = id
			}
			if pod.CRISandboxID == "" {
				continue
			}
			cp := clonePod(pod)
			e.pods[pod.CRISandboxID] = &cp
		}
		e.mu.Unlock()
	}
	if e.client == nil {
		return nil
	}
	listCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	listed, err := e.client.List(listCtx)
	if err != nil {
		log.Printf("[CubeEngine] restore list skipped: %v", err)
		return nil
	}
	paused := make(map[string]bool, len(listed))
	for _, item := range listed {
		if item.Paused {
			paused[item.ID] = true
		}
	}
	e.mu.Lock()
	var changed []CubePodState
	for _, pod := range e.pods {
		if pod.State == podRunning && paused[pod.SandboxID] {
			pod.State = podPaused
			changed = append(changed, clonePod(*pod))
		}
	}
	e.mu.Unlock()
	for _, pod := range changed {
		if err := e.persist(pod); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) RunPodSandbox(ctx context.Context, req *runtime.RunPodSandboxRequest) (*runtime.RunPodSandboxResponse, error) {
	totalStart := time.Now()
	meta := req.GetConfig().GetMetadata()
	if meta.GetUid() == "" {
		return nil, status.Error(codes.InvalidArgument, "pod uid is required")
	}
	id := meta.GetUid()
	beginStart := time.Now()
	pod, err := e.begin(id)
	beginMs := time.Since(beginStart).Milliseconds()
	if err != nil {
		return nil, err
	}
	defer e.end(id)

	switch {
	case pod != nil && pod.State == podRunning:
		if err := e.persist(*pod); err != nil {
			return nil, err
		}
		log.Printf("[CubeEngine] RunPodSandbox idempotent cri=%s sandbox=%s", id, pod.SandboxID)
		return &runtime.RunPodSandboxResponse{PodSandboxId: id}, nil
	case pod != nil && (pod.State == podPaused || pod.State == podParked):
		// Catalog ids (csnap-) are not cube templates. Resume always uses the
		// snap-<24 hex> this engine stored at pause, and the original tpl-.
		return e.resume(ctx, pod)
	case pod != nil && pod.State == podStopped:
		if err := e.forget(id); err != nil {
			return nil, err
		}
		return e.coldCreate(ctx, req, id, beginMs, totalStart)
	default:
		return e.coldCreate(ctx, req, id, beginMs, totalStart)
	}
}

func (e *Engine) coldCreate(ctx context.Context, req *runtime.RunPodSandboxRequest, id string, beginMs int64, totalStart time.Time) (*runtime.RunPodSandboxResponse, error) {
	specStart := time.Now()
	spec, err := coldSpecFromAnnotations(req.GetConfig().GetAnnotations())
	specMs := time.Since(specStart).Milliseconds()
	if err != nil {
		return nil, err
	}
	cubeletStart := time.Now()
	result, err := e.client.Create(ctx, createRequest{Cold: &spec})
	cubeletMs := time.Since(cubeletStart).Milliseconds()
	if err != nil {
		return nil, err
	}
	if result.SandboxID == "" || result.SandboxIP == "" {
		if result.SandboxID != "" {
			_ = e.client.Destroy(ctx, result.SandboxID, nil)
		}
		return nil, status.Error(codes.Internal, "cubelet create returned empty sandbox id or ip")
	}
	buildStart := time.Now()
	now := time.Now()
	meta := req.GetConfig().GetMetadata()
	pod := CubePodState{
		CRISandboxID: id,
		SandboxID:    result.SandboxID,
		SandboxUUID:  result.SandboxUUID,
		SandboxIP:    result.SandboxIP,
		OriginNode:   e.cfg.NodeName,
		TemplateID:   spec.TemplateID,
		VCPU:         spec.CPU,
		Memory:       spec.Memory,
		DiskLimit:    spec.DiskLimit,
		State:        podRunning,
		Name:         meta.GetName(),
		Namespace:    meta.GetNamespace(),
		PodUID:       meta.GetUid(),
		Labels:       copyMap(req.GetConfig().GetLabels()),
		Annotations:  copyMap(req.GetConfig().GetAnnotations()),
		CreatedAt:    now,
	}
	buildMs := time.Since(buildStart).Milliseconds()
	commitStart := time.Now()
	if err := e.commit(pod); err != nil {
		return nil, err
	}
	commitMs := time.Since(commitStart).Milliseconds()
	totalMs := time.Since(totalStart).Milliseconds()
	log.Printf("[CubeEngine] created cri=%s sandbox=%s ip=%s", id, result.SandboxID, result.SandboxIP)
	log.Printf("[CreatePerf] cri=%s begin_ms=%d spec_ms=%d cubelet_ms=%d build_ms=%d commit_ms=%d total_ms=%d",
		id, beginMs, specMs, cubeletMs, buildMs, commitMs, totalMs)
	return &runtime.RunPodSandboxResponse{PodSandboxId: id}, nil
}

func (e *Engine) resume(ctx context.Context, pod *CubePodState) (*runtime.RunPodSandboxResponse, error) {
	if e.cfg.NodeName != "" && pod.OriginNode != "" && pod.OriginNode != e.cfg.NodeName {
		return nil, status.Errorf(codes.FailedPrecondition, "resume only allowed on origin node %s", pod.OriginNode)
	}
	if pod.PauseSnapshotID == "" {
		return nil, status.Error(codes.InvalidArgument, "paused sandbox is missing snap id")
	}
	result, err := e.client.Create(ctx, createRequest{Resume: &resumeSpec{
		SandboxID:       pod.SandboxID,
		PauseSnapshotID: pod.PauseSnapshotID,
		AttachedAt:      time.Now(),
		Backend:         pod.Backend,
	}})
	if err != nil {
		return nil, err
	}
	if result.SandboxID != pod.SandboxID {
		return nil, status.Errorf(codes.Internal, "resume sandbox id %q, want %q", result.SandboxID, pod.SandboxID)
	}
	if result.SandboxIP == "" {
		return nil, status.Error(codes.Internal, "cubelet resume returned empty sandbox ip")
	}
	pod.State = podRunning
	pod.SandboxIP = result.SandboxIP
	if result.SandboxUUID != "" {
		pod.SandboxUUID = result.SandboxUUID
	}
	if err := e.commit(*pod); err != nil {
		return nil, err
	}
	log.Printf("[CubeEngine] resumed cri=%s sandbox=%s ip=%s", pod.CRISandboxID, pod.SandboxID, result.SandboxIP)
	return &runtime.RunPodSandboxResponse{PodSandboxId: pod.CRISandboxID}, nil
}

// Pause 把运行中的沙箱打成暂停包。失败时索引保持 Running。
// Resume 是对同一个 cri id 再次 RunPodSandbox，用的是这里分配的 snap-<24 hex>。
// 目录号 csnap- 不是 Cube 模板，也不能代替这个暂停包。
func (e *Engine) Pause(ctx context.Context, criSandboxID string) (string, error) {
	pod, err := e.begin(criSandboxID)
	if err != nil {
		return "", err
	}
	defer e.end(criSandboxID)
	if pod == nil {
		return "", status.Errorf(codes.NotFound, "sandbox %s not found", criSandboxID)
	}
	if pod.State == podPaused {
		return pod.PauseSnapshotID, nil
	}
	if pod.State != podRunning {
		return "", status.Errorf(codes.FailedPrecondition, "sandbox %s is %s", criSandboxID, pod.State)
	}
	snap, err := newSnapshotID()
	if err != nil {
		return "", status.Errorf(codes.Internal, "allocate snapshot id: %v", err)
	}
	annotations := map[string]string{
		annUpdateAction:      "pause",
		annPauseSnapshotID:   snap,
		annRuntimeSnapshotID: snap,
		annInstanceType:      instanceTypeCubebox,
	}
	if pod.Backend != "" {
		annotations[annStorageBackend] = pod.Backend
	}
	if err := e.client.Update(ctx, pod.SandboxID, annotations); err != nil {
		if isTimeout(err) {
			e.reconcilePause(pod, snap)
			return "", status.Error(codes.Unknown, "cube pause timed out")
		}
		return "", err
	}
	pod.State = podPaused
	pod.PauseSnapshotID = snap
	if pod.ContainerState == ctrRunning {
		pod.ContainerState = ctrExited
		pod.ContainerFinishedAt = time.Now()
		pod.ContainerExitCode = 0
	}
	// Pause snap id must survive crash before Resume; flush synchronously.
	if err := e.commitSync(*pod); err != nil {
		return "", err
	}
	log.Printf("[CubeEngine] paused cri=%s sandbox=%s snap=%s", criSandboxID, pod.SandboxID, snap)
	return snap, nil
}

func (e *Engine) StopPodSandbox(ctx context.Context, req *runtime.StopPodSandboxRequest) (*runtime.StopPodSandboxResponse, error) {
	if err := e.destroy(ctx, req.GetPodSandboxId()); err != nil {
		return nil, err
	}
	return &runtime.StopPodSandboxResponse{}, nil
}

func (e *Engine) RemovePodSandbox(ctx context.Context, req *runtime.RemovePodSandboxRequest) (*runtime.RemovePodSandboxResponse, error) {
	if err := e.destroy(ctx, req.GetPodSandboxId()); err != nil {
		return nil, err
	}
	return &runtime.RemovePodSandboxResponse{}, nil
}

func (e *Engine) destroy(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	pod, err := e.begin(id)
	if err != nil {
		return err
	}
	defer e.end(id)
	if pod == nil || pod.SandboxID == "" {
		return e.forget(id)
	}
	if pod.State == podStopped {
		return nil
	}
	if pod.State == podPaused || pod.State == podParked {
		if pod.State == podParked && !pod.ParkedAt.IsZero() && time.Since(pod.ParkedAt) >= pauseDetachGrace {
			return e.hardDestroy(ctx, pod)
		}
		pod.State = podParked
		if pod.ParkedAt.IsZero() {
			pod.ParkedAt = time.Now()
		}
		log.Printf("[CubeEngine] parked paused sandbox cri=%s snap=%s", id, pod.PauseSnapshotID)
		return e.commitSync(*pod)
	}
	return e.hardDestroy(ctx, pod)
}

// hardDestroy tears the cubelet sandbox down and leaves a NOTREADY record.
// Forgetting the id immediately makes the scheduler treat the runtime as
// missing, and that confirm window is too slow for the idle check. The
// record stays listable so the agent reports phase stopped.
func (e *Engine) hardDestroy(ctx context.Context, pod *CubePodState) error {
	annotations := map[string]string{
		annInstanceType:                    instanceTypeCubebox,
		"cube.master.instance.kill_reason": "request",
	}
	if pod.State == podPaused || pod.State == podParked {
		annotations[annDeleteTombstone] = "true"
	}
	if err := e.client.Destroy(ctx, pod.SandboxID, annotations); err != nil {
		return err
	}
	pod.State = podStopped
	pod.StoppedAt = time.Now()
	pod.SandboxIP = ""
	if pod.ContainerState == ctrRunning {
		pod.ContainerState = ctrExited
		pod.ContainerFinishedAt = pod.StoppedAt
		pod.ContainerExitCode = 0
	}
	log.Printf("[CubeEngine] stopped cri=%s sandbox=%s", pod.CRISandboxID, pod.SandboxID)
	return e.commitSync(*pod)
}

func (e *Engine) PodSandboxStatus(ctx context.Context, req *runtime.PodSandboxStatusRequest) (*runtime.PodSandboxStatusResponse, error) {
	pod, ok := e.get(req.GetPodSandboxId())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "sandbox %s not found", req.GetPodSandboxId())
	}
	return &runtime.PodSandboxStatusResponse{Status: podStatus(pod)}, nil
}

func (e *Engine) ListPodSandbox(ctx context.Context, req *runtime.ListPodSandboxRequest) (*runtime.ListPodSandboxResponse, error) {
	e.dropExpiredStopped()
	e.mu.Lock()
	items := make([]*runtime.PodSandbox, 0, len(e.pods))
	for _, pod := range e.pods {
		items = append(items, &runtime.PodSandbox{
			Id:          pod.CRISandboxID,
			Metadata:    podMetadata(*pod),
			State:       sandboxState(pod.State),
			CreatedAt:   unixNano(pod.CreatedAt),
			Labels:      copyMap(pod.Labels),
			Annotations: copyMap(pod.Annotations),
		})
	}
	e.mu.Unlock()
	return &runtime.ListPodSandboxResponse{Items: filterPodSandbox(items, req.GetFilter())}, nil
}

func (e *Engine) dropExpiredStopped() {
	now := time.Now()
	e.mu.Lock()
	var expired []string
	for id, pod := range e.pods {
		if pod.State != podStopped || pod.StoppedAt.IsZero() {
			continue
		}
		if now.Sub(pod.StoppedAt) >= stoppedRetain {
			expired = append(expired, id)
		}
	}
	for _, id := range expired {
		delete(e.pods, id)
	}
	e.mu.Unlock()
	if len(expired) > 0 {
		e.schedulePersist()
	}
}

func coldSpecFromAnnotations(annotations map[string]string) (coldSpec, error) {
	templateID := strings.TrimSpace(annotations[annTemplateID])
	if templateID == "" {
		return coldSpec{}, status.Error(codes.InvalidArgument, "cube.dev/template-id is required")
	}
	if strings.HasPrefix(templateID, "csnap-") {
		return coldSpec{}, status.Errorf(codes.InvalidArgument,
			"template id %q is a catalog snapshot, not a cube template; resume uses the local pause snapshot", templateID)
	}
	if !strings.HasPrefix(templateID, "tpl-") && !strings.HasPrefix(templateID, "snap-") {
		return coldSpec{}, status.Errorf(codes.InvalidArgument, "template id %q must start with tpl- or snap-", templateID)
	}
	cpu := strings.TrimSpace(annotations[annVCPU])
	if cpu == "" {
		return coldSpec{}, status.Error(codes.InvalidArgument, "cube.dev/vcpu is required")
	}
	mem, err := memoryQuantity(annotations[annRAMMB], "cube.dev/ram-mb")
	if err != nil {
		return coldSpec{}, err
	}
	disk := defaultDiskLimit
	if raw := strings.TrimSpace(annotations[annDiskMB]); raw != "" {
		disk, err = memoryQuantity(raw, "cube.dev/disk-mb")
		if err != nil {
			return coldSpec{}, err
		}
	}
	spec := coldSpec{
		TemplateID:   templateID,
		CPU:          cpu,
		Memory:       mem,
		DiskLimit:    disk,
		Container:    strings.TrimSpace(annotations[annContainer]),
		VolumeName:   strings.TrimSpace(annotations[annVolumeName]),
		Image:        strings.TrimSpace(annotations[annImage]),
		StorageMedia: strings.TrimSpace(annotations[annStorageMedia]),
	}
	for k, v := range annotations {
		if strings.HasPrefix(k, "cube.master.rootfs.") || k == "cube.master.template.spec_fingerprint" {
			if spec.ImageAnns == nil {
				spec.ImageAnns = map[string]string{}
			}
			spec.ImageAnns[k] = v
		}
	}
	if raw := strings.TrimSpace(annotations[annCommand]); raw != "" {
		spec.Command = splitAnnotationList(raw)
	}
	if raw := strings.TrimSpace(annotations[annArgs]); raw != "" {
		spec.Args = splitAnnotationList(raw)
	}
	return spec, nil
}

func splitAnnotationList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if strings.HasPrefix(raw, "[") {
		// JSON array: ["a","b"] — keep simple split for e2e/native args.
		raw = strings.TrimSuffix(strings.TrimPrefix(raw, "["), "]")
		parts := strings.Split(raw, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			p = strings.Trim(p, `"'`)
			if p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	return []string{raw}
}

func memoryQuantity(raw, key string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", status.Errorf(codes.InvalidArgument, "%s is required", key)
	}
	if _, err := strconv.Atoi(raw); err == nil {
		return raw + "Mi", nil
	}
	return raw, nil
}

func (e *Engine) begin(id string) (*CubePodState, error) {
	if err := e.ensureEnabled(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.inflight[id]; ok {
		return nil, status.Errorf(codes.Aborted, "cube sandbox %s has an operation in progress", id)
	}
	e.inflight[id] = struct{}{}
	pod, ok := e.pods[id]
	if !ok {
		return nil, nil
	}
	cp := clonePod(*pod)
	return &cp, nil
}

func (e *Engine) end(id string) {
	e.mu.Lock()
	delete(e.inflight, id)
	e.mu.Unlock()
}

func (e *Engine) get(id string) (CubePodState, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	pod, ok := e.pods[id]
	if !ok {
		return CubePodState{}, false
	}
	return clonePod(*pod), true
}

func (e *Engine) podByContainer(id string) (CubePodState, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, pod := range e.pods {
		if pod.ContainerID == id {
			return clonePod(*pod), true
		}
	}
	return CubePodState{}, false
}

// commit updates the in-memory map under a short lock and schedules a
// background persist. Create/Resume never wait on disk (e2b-style).
func (e *Engine) commit(pod CubePodState) error {
	cp := clonePod(pod)
	e.mu.Lock()
	e.pods[pod.CRISandboxID] = &cp
	e.mu.Unlock()
	e.schedulePersist()
	return nil
}

// commitSync is commit + wait for the coalesced persist worker. Use when the
// new state must survive a crash before the RPC returns (Pause snap id).
func (e *Engine) commitSync(pod CubePodState) error {
	cp := clonePod(pod)
	e.mu.Lock()
	e.pods[pod.CRISandboxID] = &cp
	e.mu.Unlock()
	e.schedulePersist()
	return e.waitPersist()
}

func (e *Engine) persist(pod CubePodState) error {
	return e.commitSync(pod)
}

func (e *Engine) forget(id string) error {
	e.mu.Lock()
	delete(e.pods, id)
	e.mu.Unlock()
	e.schedulePersist()
	return e.waitPersist()
}

func (e *Engine) schedulePersist() {
	if e == nil || e.cfg.StateStore == nil {
		return
	}
	e.persistMu.Lock()
	e.persistDirty = true
	e.persistGen++
	e.persistMu.Unlock()
	select {
	case e.persistCh <- struct{}{}:
	default:
	}
}

func (e *Engine) waitPersist() error {
	if e == nil || e.cfg.StateStore == nil {
		return nil
	}
	e.persistMu.Lock()
	want := e.persistGen
	for e.flushedGen < want {
		e.persistCond.Wait()
	}
	err := e.persistErr
	e.persistMu.Unlock()
	return err
}

func (e *Engine) persistLoop() {
	defer close(e.persistDone)
	for {
		select {
		case <-e.persistStop:
			e.flushPersistOnce()
			return
		case <-e.persistCh:
			e.flushPersistOnce()
		}
	}
}

func (e *Engine) flushPersistOnce() {
	for {
		e.persistMu.Lock()
		if !e.persistDirty {
			e.persistMu.Unlock()
			return
		}
		gen := e.persistGen
		e.persistDirty = false
		e.persistMu.Unlock()

		e.mu.Lock()
		snap := snapshotPods(e.pods)
		e.mu.Unlock()

		err := e.savePods(snap)
		if err != nil {
			log.Printf("[CubeEngine] WARNING: persist pods failed: %v", err)
		}

		e.persistMu.Lock()
		e.flushedGen = gen
		e.persistErr = err
		e.persistCond.Broadcast()
		dirty := e.persistDirty
		e.persistMu.Unlock()
		if !dirty {
			return
		}
	}
}

func (e *Engine) savePods(pods map[string]CubePodState) error {
	if e.cfg.StateStore == nil {
		return nil
	}
	return e.cfg.StateStore.SaveBackendState(cubeStatePath, pods)
}

func snapshotPods(pods map[string]*CubePodState) map[string]CubePodState {
	out := make(map[string]CubePodState, len(pods))
	for id, pod := range pods {
		if pod == nil {
			continue
		}
		out[id] = clonePod(*pod)
	}
	return out
}

func (e *Engine) ensureEnabled() error {
	if e == nil || !e.cfg.Enabled {
		return status.Error(codes.FailedPrecondition, "cube runtime is not enabled")
	}
	return nil
}

func (e *Engine) CleanupSandbox(ctx context.Context, sandboxID, reason string) error {
	if e == nil || !e.cfg.Enabled || sandboxID == "" {
		return nil
	}
	return e.destroy(ctx, sandboxID)
}

func (e *Engine) reconcilePause(pod *CubePodState, snap string) {
	if e.client == nil || pod == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	listed, err := e.client.List(ctx)
	if err != nil {
		return
	}
	for _, item := range listed {
		if item.ID == pod.SandboxID && item.Paused {
			pod.State = podPaused
			pod.PauseSnapshotID = snap
			if pod.ContainerState == ctrRunning {
				pod.ContainerState = ctrExited
				pod.ContainerFinishedAt = time.Now()
				pod.ContainerExitCode = 0
			}
			_ = e.commit(*pod)
			return
		}
	}
}

func isTimeout(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || status.Code(err) == codes.DeadlineExceeded
}

func clonePod(pod CubePodState) CubePodState {
	pod.Labels = copyMap(pod.Labels)
	pod.Annotations = copyMap(pod.Annotations)
	pod.ContainerLabels = copyMap(pod.ContainerLabels)
	pod.ContainerAnnotations = copyMap(pod.ContainerAnnotations)
	return pod
}

func copyMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func podMetadata(pod CubePodState) *runtime.PodSandboxMetadata {
	return &runtime.PodSandboxMetadata{Name: pod.Name, Uid: pod.PodUID, Namespace: pod.Namespace}
}

func podStatus(pod CubePodState) *runtime.PodSandboxStatus {
	return &runtime.PodSandboxStatus{
		Id:          pod.CRISandboxID,
		Metadata:    podMetadata(pod),
		State:       sandboxState(pod.State),
		CreatedAt:   unixNano(pod.CreatedAt),
		Labels:      copyMap(pod.Labels),
		Annotations: copyMap(pod.Annotations),
		Network:     &runtime.PodSandboxNetworkStatus{Ip: pod.SandboxIP},
	}
}

func sandboxState(state string) runtime.PodSandboxState {
	if state == podRunning {
		return runtime.PodSandboxState_SANDBOX_READY
	}
	return runtime.PodSandboxState_SANDBOX_NOTREADY
}

func unixNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func filterPodSandbox(items []*runtime.PodSandbox, f *runtime.PodSandboxFilter) []*runtime.PodSandbox {
	if f == nil {
		return items
	}
	out := make([]*runtime.PodSandbox, 0, len(items))
	for _, item := range items {
		if f.Id != "" && item.Id != f.Id {
			continue
		}
		if f.State != nil && item.State != f.State.State {
			continue
		}
		if !labelsMatch(item.Labels, f.LabelSelector) {
			continue
		}
		out = append(out, item)
	}
	return out
}

func labelsMatch(labels, selector map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}
