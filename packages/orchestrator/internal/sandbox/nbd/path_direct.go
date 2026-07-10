package nbd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Merovius/nbd/nbdnl"
	"go.opentelemetry.io/otel"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"
	featureflags "github.com/e2b-dev/infra/packages/shared/pkg/feature-flags"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
)

const (
	connectTimeout = 30 * time.Second

	// disconnectTimeout should not be necessary if the disconnect is reliable
	disconnectTimeout = 30 * time.Second
)

var tracer = otel.Tracer("github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/nbd")

type DirectPathMount struct {
	cancelfn     context.CancelFunc
	devicePool   *DevicePool
	featureFlags *featureflags.Client

	Backend     block.Device
	deviceIndex uint32
	blockSize   uint64

	dispatchers []*Dispatch
	socksClient []*os.File
	socksServer []io.Closer

	handlersWg sync.WaitGroup
}

func NewDirectPathMount(b block.Device, devicePool *DevicePool, featureFlags *featureflags.Client) *DirectPathMount {
	return &DirectPathMount{
		Backend:      b,
		blockSize:    4096,
		devicePool:   devicePool,
		featureFlags: featureFlags,
		socksClient:  make([]*os.File, 0),
		socksServer:  make([]io.Closer, 0),
		deviceIndex:  math.MaxUint32,
	}
}

func (d *DirectPathMount) Open(ctx context.Context) (retDeviceIndex uint32, err error) {
	ctx, d.cancelfn = context.WithCancel(ctx)

	openStart := time.Now()
	defer func() {
		// Set the device index to the one returned, correctly capture error values
		d.deviceIndex = retDeviceIndex
		logger.L().Info(ctx, "[NBD-Open] open completed",
			zap.Uint32("device_index", d.deviceIndex),
			zap.Duration("total_ms", time.Since(openStart)),
			zap.Error(err),
		)
	}()

	telemetry.ReportEvent(ctx, "opening direct path mount")

	phaseStart := time.Now()
	size, err := d.Backend.Size(ctx)
	if err != nil {
		return math.MaxUint32, err
	}
	logger.L().Info(ctx, "[NBD-Open] backend size obtained",
		zap.Uint64("size", uint64(size)),
		zap.Duration("phase_ms", time.Since(phaseStart)),
	)

	telemetry.ReportEvent(ctx, "got backend size")

	deviceIndex := uint32(math.MaxUint32)

	for {
		loopStart := time.Now()
		phaseStart = time.Now()
		deviceIndex, err = d.devicePool.GetDevice(ctx)
		if err != nil {
			return math.MaxUint32, err
		}
		logger.L().Info(ctx, "[NBD-Open] got device from pool",
			zap.Uint32("device_index", deviceIndex),
			zap.Duration("phase_ms", time.Since(phaseStart)),
		)

		telemetry.ReportEvent(ctx, "got device index")

		d.socksClient = make([]*os.File, 0)
		d.socksServer = make([]io.Closer, 0)
		d.dispatchers = make([]*Dispatch, 0)

		connections := d.featureFlags.IntFlag(ctx, featureflags.NBDConnectionsPerDevice)

		phaseStart = time.Now()
		for i := range connections {
			// Create the socket pairs
			sockPair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
			if err != nil {
				closeErr := closeSocketPairs(d.socksClient, d.socksServer)
				releaseErr := d.devicePool.ReleaseDevice(ctx, deviceIndex)

				return math.MaxUint32, errors.Join(err, closeErr, releaseErr)
			}

			client := os.NewFile(uintptr(sockPair[0]), "client")
			server := os.NewFile(uintptr(sockPair[1]), "server")
			serverc, err := net.FileConn(server)
			if err != nil {
				// Close the current iteration's FDs (not yet added to d.socksClient/d.socksServer)
				client.Close()
				server.Close()
				closeErr := closeSocketPairs(d.socksClient, d.socksServer)
				releaseErr := d.devicePool.ReleaseDevice(ctx, deviceIndex)

				return math.MaxUint32, errors.Join(err, closeErr, releaseErr)
			}
			server.Close()

			dispatch := NewDispatch(serverc, d.Backend)
			// Start reading commands on the socket and dispatching them to our provider
			d.handlersWg.Go(func() {
				handleErr := dispatch.Handle(ctx)
				// The error is expected to happen if the nbd (socket connection) is closed
				logger.L().Info(ctx, "closing handler for NBD commands",
					zap.Error(handleErr),
					zap.Uint32("device_index", deviceIndex),
					zap.Int("socket_index", i),
				)
			})

			d.socksServer = append(d.socksServer, serverc)
			d.socksClient = append(d.socksClient, client)
			d.dispatchers = append(d.dispatchers, dispatch)
		}
		logger.L().Info(ctx, "[NBD-Open] socket pairs created",
			zap.Int("connections", connections),
			zap.Duration("phase_ms", time.Since(phaseStart)),
		)

		var opts []nbdnl.ConnectOption
		opts = append(opts, nbdnl.WithBlockSize(d.blockSize))
		opts = append(opts, nbdnl.WithTimeout(connectTimeout))
		opts = append(opts, nbdnl.WithDeadconnTimeout(connectTimeout))

		serverFlags := nbdnl.FlagHasFlags | nbdnl.FlagCanMulticonn

		phaseStart = time.Now()
		idx, connectErr := nbdnl.Connect(deviceIndex, d.socksClient, uint64(size), 0, serverFlags, opts...)
		connectCost := time.Since(phaseStart)
		if connectErr == nil {
			// The idx should be the same as deviceIndex, because we are connecting to it,
			// but we will use the one returned by nbdnl
			deviceIndex = idx
			logger.L().Info(ctx, "[NBD-Open] nbdnl.Connect succeeded",
				zap.Uint32("device_index", deviceIndex),
				zap.Duration("phase_ms", connectCost),
			)

			break
		}

		logger.L().Error(ctx, "[NBD-Open] error opening NBD, retrying",
			zap.Error(connectErr),
			zap.Uint32("device_index", deviceIndex),
			zap.Duration("connect_ms", connectCost),
			zap.Duration("loop_ms", time.Since(loopStart)),
		)

		// Sometimes (rare), there seems to be a BADF error here. Lets just retry for now...
		// Close things down and try again...
		err := closeSocketPairs(d.socksClient, d.socksServer)
		if err != nil {
			logger.L().Error(ctx, "[NBD-Open] error closing socket pairs on error opening NBD", zap.Error(err))
		}

		// Release the device back to the pool
		err = d.devicePool.ReleaseDevice(ctx, deviceIndex)
		if err != nil {
			logger.L().Error(ctx, "[NBD-Open] error opening NBD, error releasing device", zap.Error(err), zap.Uint32("device_index", deviceIndex))
		}

		if strings.Contains(connectErr.Error(), "invalid argument") {
			return math.MaxUint32, connectErr
		}

		select {
		case <-ctx.Done():
			return math.MaxUint32, errors.Join(connectErr, ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}

	// Wait until it's connected...
	phaseStart = time.Now()
	waitIterations := 0
	for {
		select {
		case <-ctx.Done():
			return math.MaxUint32, ctx.Err()
		default:
		}

		telemetry.ReportEvent(ctx, "waiting for NBD connection")

		s, err := nbdnl.Status(deviceIndex)
		if err == nil && s.Connected {
			logger.L().Info(ctx, "[NBD-Open] NBD status connected",
				zap.Uint32("device_index", deviceIndex),
				zap.Int("wait_iterations", waitIterations),
				zap.Duration("phase_ms", time.Since(phaseStart)),
			)
			break
		}
		waitIterations++
		time.Sleep(100 * time.Nanosecond)
	}

	telemetry.ReportEvent(ctx, "connected to NBD")

	retDeviceIndex = deviceIndex
	logger.L().Info(ctx, "[NBD-Open] open success",
		zap.Uint32("device_index", retDeviceIndex),
		zap.Duration("total_ms", time.Since(openStart)),
	)
	return retDeviceIndex, nil
}

func (d *DirectPathMount) Close(ctx context.Context) error {
	ctx, span := tracer.Start(ctx, "direct-path-mount-close")
	defer span.End()

	var errs []error

	idx := d.deviceIndex

	// First cancel the context, which will stop waiting on pending readAt/writeAt...
	telemetry.ReportEvent(ctx, "canceling context")
	if d.cancelfn != nil {
		d.cancelfn()
	}

	// Close all server socket pairs...
	telemetry.ReportEvent(ctx, "closing socket pairs server")
	for _, v := range d.socksServer {
		err := v.Close()
		if err != nil {
			errs = append(errs, fmt.Errorf("error closing server pair: %w", err))
		}
	}

	// Now wait until the handlers return
	telemetry.ReportEvent(ctx, "await handlers return")
	d.handlersWg.Wait()

	// Now wait for any pending responses to be sent
	telemetry.ReportEvent(ctx, "waiting for pending responses")
	for _, d := range d.dispatchers {
		d.Drain()
	}

	// Disconnect NBD
	if idx != math.MaxUint32 {
		err := disconnectNBDWithTimeout(ctx, idx, disconnectTimeout)
		if err != nil {
			errs = append(errs, fmt.Errorf("error disconnecting NBD: %w", err))
		}
	}

	// Close all client socket pairs...
	telemetry.ReportEvent(ctx, "closing socket pairs client")
	for _, v := range d.socksClient {
		err := v.Close()
		if err != nil {
			errs = append(errs, fmt.Errorf("error closing socket pair client: %w", err))
		}
	}

	// Release the device back to the pool, retry if it is in use
	if idx != math.MaxUint32 {
		telemetry.ReportEvent(ctx, "releasing device to the pool")
		err := d.devicePool.ReleaseDevice(ctx, idx, WithInfiniteRetry())
		if err != nil {
			errs = append(errs, fmt.Errorf("error releasing overlay device: %w", err))
		}
	}

	return errors.Join(errs...)
}

func disconnectNBDWithTimeout(ctx context.Context, deviceIndex uint32, timeout time.Duration) error {
	// Now ask to disconnect
	telemetry.ReportEvent(ctx, "disconnecting NBD")
	err := nbdnl.Disconnect(deviceIndex)
	if err != nil {
		return err
	}

	// Wait until it's completely disconnected...
	telemetry.ReportEvent(ctx, "waiting for complete disconnection")
	ctxTimeout, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		select {
		case <-ctxTimeout.Done():
			return ctxTimeout.Err()
		default:
		}

		s, err := nbdnl.Status(deviceIndex)
		if err == nil && !s.Connected {
			break
		}
		time.Sleep(100 * time.Nanosecond)
	}

	return nil
}

func closeSocketPairs(socksClient []*os.File, socksServer []io.Closer) error {
	var errs []error
	for _, sock := range socksClient {
		if sock != nil {
			errs = append(errs, sock.Close())
		}
	}
	for _, sock := range socksServer {
		if sock != nil {
			errs = append(errs, sock.Close())
		}
	}

	return errors.Join(errs...)
}
