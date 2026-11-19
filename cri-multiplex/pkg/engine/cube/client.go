package cube

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/cri-multiplex/pkg/cubelet"
)

const (
	retSuccess      int32 = 200
	retNotFound     int32 = 130505
	retInvalid      int32 = 130483
	retPrecondition int32 = 130568

	instanceTypeCubebox = "cubebox"
	networkTypeTap      = "tap"
	templateVersion     = "v2"
	rootVolumeName      = "rootfs"
	rootMountPath       = "/"
	containerName       = "sandbox"
)

type coldSpec struct {
	TemplateID   string
	CPU          string
	Memory       string
	DiskLimit    string
	Container    string
	VolumeName   string
	Image        string
	StorageMedia string
	ImageAnns    map[string]string
	Command      []string
	Args         []string
}

type resumeSpec struct {
	SandboxID       string
	PauseSnapshotID string
	AttachedAt      time.Time
	Backend         string
}

type createRequest struct {
	Cold   *coldSpec
	Resume *resumeSpec
}

type createResult struct {
	SandboxID   string
	SandboxUUID string
	SandboxIP   string
}

type listedSandbox struct {
	ID     string
	Paused bool
}

// cubeClient 是 Cubelet CubeboxMgr 的最小调用面。测试注入假实现。
type cubeClient interface {
	Create(ctx context.Context, req createRequest) (createResult, error)
	Destroy(ctx context.Context, sandboxID string, annotations map[string]string) error
	List(ctx context.Context) ([]listedSandbox, error)
	Update(ctx context.Context, sandboxID string, annotations map[string]string) error
	Exec(ctx context.Context, sandboxID string, args []string) error
	Commit(ctx context.Context, sandboxID, templateID, backend string) (string, error)
	Close() error
}

type grpcCubeClient struct {
	conn    *grpc.ClientConn
	api     cubelet.CubeboxMgrClient
	timeout time.Duration
}

func dialCubelet(addr string, timeout time.Duration) (*grpcCubeClient, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("cubelet dial: %w", err)
	}
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &grpcCubeClient{conn: conn, api: cubelet.NewCubeboxMgrClient(conn), timeout: timeout}, nil
}

func (c *grpcCubeClient) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func (c *grpcCubeClient) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.timeout)
}

func (c *grpcCubeClient) Create(ctx context.Context, req createRequest) (createResult, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	msg := &cubelet.RunCubeSandboxRequest{
		RequestID:    newRequestID(),
		InstanceType: instanceTypeCubebox,
		NetworkType:  networkTypeTap,
	}
	if req.Resume != nil {
		msg.Backend = req.Resume.Backend
		msg.Annotations = map[string]string{
			annDesiredSandboxID:   req.Resume.SandboxID,
			annPauseSnapshotID:    req.Resume.PauseSnapshotID,
			annRuntimeSnapshotID:  req.Resume.PauseSnapshotID,
			annSnapshotAttachedAt: req.Resume.AttachedAt.UTC().Format(time.RFC3339Nano),
		}
	} else if req.Cold != nil {
		volName := req.Cold.VolumeName
		if volName == "" {
			volName = rootVolumeName
		}
		ctrName := req.Cold.Container
		if ctrName == "" {
			ctrName = containerName
		}
		msg.Volumes = []*cubelet.Volume{{
			Name: volName,
			VolumeSource: &cubelet.VolumeSource{
				EmptyDir: &cubelet.EmptyDirVolumeSource{SizeLimit: req.Cold.DiskLimit},
			},
		}}
		ctr := &cubelet.ContainerConfig{
			Name:    ctrName,
			Command: append([]string(nil), req.Cold.Command...),
			Args:    append([]string(nil), req.Cold.Args...),
			Resources: &cubelet.Resource{
				Cpu: req.Cold.CPU,
				Mem: req.Cold.Memory,
			},
			VolumeMounts: []*cubelet.VolumeMounts{{
				Name:          volName,
				ContainerPath: rootMountPath,
			}},
		}
		if req.Cold.Image != "" {
			ctr.Image = &cubelet.ImageSpec{
				Image:        req.Cold.Image,
				StorageMedia: req.Cold.StorageMedia,
				Annotations:  req.Cold.ImageAnns,
			}
		}
		msg.Containers = []*cubelet.ContainerConfig{ctr}
		msg.Annotations = map[string]string{
			annMasterTemplateID:      req.Cold.TemplateID,
			annMasterTemplateVersion: templateVersion,
			annMasterAppSnapshotVer:  templateVersion,
		}
	} else {
		return createResult{}, status.Error(codes.InvalidArgument, "cubelet create: empty request")
	}

	resp, err := c.api.Create(ctx, msg)
	if err != nil {
		return createResult{}, mapTransport(err)
	}
	if err := mapRet(resp.GetRet()); err != nil {
		return createResult{}, err
	}
	return createResult{
		SandboxID:   resp.GetSandboxID(),
		SandboxUUID: resp.GetSandboxUUID(),
		SandboxIP:   resp.GetSandboxIP(),
	}, nil
}

func (c *grpcCubeClient) Destroy(ctx context.Context, sandboxID string, annotations map[string]string) error {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	resp, err := c.api.Destroy(ctx, &cubelet.DestroyCubeSandboxRequest{
		RequestID:   newRequestID(),
		SandboxID:   sandboxID,
		Annotations: annotations,
	})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil
		}
		return mapTransport(err)
	}
	if err := mapRet(resp.GetRet()); err != nil {
		if status.Code(err) == codes.NotFound {
			return nil
		}
		return err
	}
	return nil
}

func (c *grpcCubeClient) List(ctx context.Context) ([]listedSandbox, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	resp, err := c.api.List(ctx, &cubelet.ListCubeSandboxRequest{})
	if err != nil {
		return nil, mapTransport(err)
	}
	out := make([]listedSandbox, 0, len(resp.GetItems()))
	for _, item := range resp.GetItems() {
		out = append(out, listedSandbox{ID: item.GetId(), Paused: sandboxPaused(item)})
	}
	return out, nil
}

func (c *grpcCubeClient) Update(ctx context.Context, sandboxID string, annotations map[string]string) error {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	resp, err := c.api.Update(ctx, &cubelet.UpdateCubeSandboxRequest{
		RequestID:   newRequestID(),
		SandboxID:   sandboxID,
		Annotations: annotations,
	})
	if err != nil {
		return mapTransport(err)
	}
	return mapRet(resp.GetRet())
}

func (c *grpcCubeClient) Commit(ctx context.Context, sandboxID, templateID, backend string) (string, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	resp, err := c.api.CommitSandbox(ctx, &cubelet.CommitSandboxRequest{
		RequestID:  newRequestID(),
		SandboxID:  sandboxID,
		TemplateID: templateID,
		Backend:    backend,
	})
	if err != nil {
		return "", mapTransport(err)
	}
	if err := mapRet(resp.GetRet()); err != nil {
		return "", err
	}
	if resp.GetSnapshotPath() == "" {
		return "", status.Error(codes.Internal, "cubelet commit returned empty snapshot path")
	}
	return resp.GetSnapshotPath(), nil
}

func (c *grpcCubeClient) Exec(ctx context.Context, sandboxID string, args []string) error {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	resp, err := c.api.Exec(ctx, &cubelet.ExecCubeSandboxRequest{
		RequestID: newRequestID(),
		SandboxId: sandboxID,
		Args:      append([]string(nil), args...),
	})
	if err != nil {
		return mapTransport(err)
	}
	return mapRet(resp.GetRet())
}

func sandboxPaused(item *cubelet.CubeSandbox) bool {
	if item == nil {
		return false
	}
	for _, ctr := range item.GetContainers() {
		if ctr.GetId() == item.GetId() && ctr.GetState() == cubelet.ContainerState_CONTAINER_PAUSED {
			return true
		}
	}
	return false
}

func mapRet(ret *cubelet.Ret) error {
	if ret == nil {
		return status.Error(codes.Internal, "cubelet: empty ret")
	}
	switch ret.GetRetCode() {
	case retSuccess:
		return nil
	case retInvalid:
		return status.Errorf(codes.InvalidArgument, "cubelet: %s", ret.GetRetMsg())
	case retNotFound:
		return status.Errorf(codes.NotFound, "cubelet: %s", ret.GetRetMsg())
	case retPrecondition:
		return status.Errorf(codes.FailedPrecondition, "cubelet: %s", ret.GetRetMsg())
	default:
		return status.Errorf(codes.Internal, "cubelet ret %d: %s", ret.GetRetCode(), ret.GetRetMsg())
	}
}

func mapTransport(err error) error {
	if err == nil {
		return nil
	}
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
			return status.Errorf(codes.Unavailable, "cubelet: %s", st.Message())
		default:
			return err
		}
	}
	return status.Errorf(codes.Unavailable, "cubelet: %v", err)
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("cri-%d", time.Now().UnixNano())
	}
	return "cri-" + hex.EncodeToString(b[:])
}

func newSnapshotID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "snap-" + hex.EncodeToString(b[:]), nil
}
