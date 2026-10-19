package userfaultfd

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"syscall"
	"unsafe"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/uffd/fdexit"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/uffd/memory"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

var tracer = otel.Tracer("github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/uffd/userfaultfd")

const maxRequestsInProgress = 4096

var ErrUnexpectedEventType = errors.New("unexpected event type")

// uffdCopySize returns the destination alignment and length used for a single
// UFFDIO_COPY.
//
// The kernel requires the destination to be aligned to the host page size and
// the length to be a multiple of it (validate_range/validate_unaligned_range
// in fs/userfaultfd.c). The snapshot memfile may use a smaller block size
// (e.g. 4KiB), so the copy must cover at least one full host page. For
// hugepage-backed regions the region page size is already a multiple of the
// host page size, in which case the larger value is used.
func uffdCopySize(regionPageSize uintptr) uintptr {
	if regionPageSize > uintptr(block.PAGE_SIZE) {
		return regionPageSize
	}

	return uintptr(block.PAGE_SIZE)
}

// hasEvent checks if a specific poll event flag is set in revents.
func hasEvent(revents, event int16) bool {
	return revents&event != 0
}

type Userfaultfd struct {
	fd Fd

	src block.Slicer
	ma  *memory.Mapping

	// We don't skip the already mapped pages, because if the memory is swappable the page *might* under some conditions be mapped out.
	// For hugepages this should not be a problem, but might theoretically happen to normal pages with swap
	missingRequests *block.Tracker
	// We use the settleRequests to guard the missingRequests so we can access a consistent state of the missingRequests after the requests are finished.
	settleRequests sync.RWMutex

	prefetchTracker *block.PrefetchTracker

	wg errgroup.Group

	logger logger.Logger
}

// NewUserfaultfdFromFd creates a new userfaultfd instance with optional configuration.
func NewUserfaultfdFromFd(fd uintptr, src block.Slicer, m *memory.Mapping, logger logger.Logger) (*Userfaultfd, error) {
	blockSize := src.BlockSize()

	for _, region := range m.Regions {
		if region.PageSize != uintptr(blockSize) {
			return nil, fmt.Errorf("block size mismatch: %d != %d for region %d", region.PageSize, blockSize, region.BaseHostVirtAddr)
		}
	}

	u := &Userfaultfd{
		fd:              Fd(fd),
		src:             src,
		missingRequests: block.NewTracker(blockSize),
		prefetchTracker: block.NewPrefetchTracker(blockSize),
		ma:              m,
		logger:          logger,
	}

	// By default this was unlimited.
	// Now that we don't skip previously faulted pages we add at least some boundaries to the concurrency.
	// Also, in some brief tests, adding a limit actually improved the handling at high concurrency.
	u.wg.SetLimit(maxRequestsInProgress)

	return u, nil
}

func (u *Userfaultfd) Close() error {
	return u.fd.close()
}

func (u *Userfaultfd) Serve(
	ctx context.Context,
	fdExit *fdexit.FdExit,
) error {
	pollFds := []unix.PollFd{
		{Fd: int32(u.fd), Events: unix.POLLIN},
		{Fd: fdExit.Reader(), Events: unix.POLLIN},
	}

	eagainCounter := newCounterReporter(u.logger, "uffd: eagain during fd read (accumulated)")
	defer eagainCounter.Close(ctx)

	noDataCounter := newCounterReporter(u.logger, "uffd: no data in fd (accumulated)")
	defer noDataCounter.Close(ctx)

	exitFdErrorCounter := newCounterReporter(u.logger, "uffd: exit fd poll errors (accumulated)")
	defer exitFdErrorCounter.Close(ctx)

	uffdErrorCounter := newCounterReporter(u.logger, "uffd: uffd fd poll errors (accumulated)")
	defer uffdErrorCounter.Close(ctx)

	pollErrorEvents := map[int16]string{
		unix.POLLHUP:  "POLLHUP",
		unix.POLLERR:  "POLLERR",
		unix.POLLNVAL: "POLLNVAL",
	}

outerLoop:
	for {
		if _, err := unix.Poll(
			pollFds,
			-1,
		); err != nil {
			if err == unix.EINTR {
				u.logger.Debug(ctx, "uffd: interrupted polling, going back to polling")

				continue
			}

			if err == unix.EAGAIN {
				u.logger.Debug(ctx, "uffd: eagain during polling, going back to polling")

				continue
			}

			u.logger.Error(ctx, "UFFD serve polling error", zap.Error(err))

			return fmt.Errorf("failed polling: %w", err)
		}

		exitFd := pollFds[1]
		if hasEvent(exitFd.Revents, unix.POLLIN) {
			errMsg := u.wg.Wait()
			if errMsg != nil {
				u.logger.Warn(ctx, "UFFD fd exit error while waiting for goroutines to finish", zap.Error(errMsg))

				return fmt.Errorf("failed to handle uffd: %w", errMsg)
			}

			return nil
		}

		// Track exit fd error events
		for event, name := range pollErrorEvents {
			if hasEvent(exitFd.Revents, event) {
				exitFdErrorCounter.Increase(name)
			}
		}

		uffdFd := pollFds[0]

		// Track uffd error events
		for event, name := range pollErrorEvents {
			if hasEvent(uffdFd.Revents, event) {
				uffdErrorCounter.Increase(name)
			}
		}

		if !hasEvent(uffdFd.Revents, unix.POLLIN) {
			// Uffd is not ready for reading as there is nothing to read on the fd.
			// https://github.com/firecracker-microvm/firecracker/issues/5056
			// https://elixir.bootlin.com/linux/v6.8.12/source/fs/userfaultfd.c#L1149
			// TODO: Check for all the errors
			// - https://docs.kernel.org/admin-guide/mm/userfaultfd.html
			// - https://elixir.bootlin.com/linux/v6.8.12/source/fs/userfaultfd.c
			// - https://man7.org/linux/man-pages/man2/userfaultfd.2.html
			// It might be possible to just check for data != 0 in the syscall.Read loop
			// but I don't feel confident about doing that.
			noDataCounter.Increase("POLLIN")

			continue
		}

		buf := make([]byte, unsafe.Sizeof(UffdMsg{}))

		for {
			_, err := syscall.Read(int(u.fd), buf)
			if err == syscall.EINTR {
				u.logger.Debug(ctx, "uffd: interrupted read, reading again")

				continue
			}

			if err == nil {
				// There is no error so we can proceed.

				eagainCounter.Log(ctx)
				noDataCounter.Log(ctx)

				break
			}

			if err == syscall.EAGAIN {
				eagainCounter.Increase("EAGAIN")

				// Continue polling the fd.
				continue outerLoop
			}

			u.logger.Error(ctx, "uffd: read error", zap.Error(err))

			return fmt.Errorf("failed to read: %w", err)
		}

		msg := *(*UffdMsg)(unsafe.Pointer(&buf[0]))

		if msgEvent := getMsgEvent(&msg); msgEvent != UFFD_EVENT_PAGEFAULT {
			u.logger.Error(ctx, "UFFD serve unexpected event type", zap.Any("event_type", msgEvent))

			return ErrUnexpectedEventType
		}

		arg := getMsgArg(&msg)
		pagefault := (*(*UffdPagefault)(unsafe.Pointer(&arg[0])))
		flags := pagefault.flags

		addr := getPagefaultAddress(&pagefault)

		offset, regionPageSize, err := u.ma.GetOffset(addr)
		if err != nil {
			u.logger.Error(ctx, "UFFD serve get mapping error", zap.Error(err))

			return fmt.Errorf("failed to map: %w", err)
		}

		// The kernel requires UFFDIO_COPY to operate on a full host page. Page
		// fault addresses are usually already host-page aligned, but align
		// defensively (e.g. if UFFD_FEATURE_EXACT_ADDRESS is negotiated) and
		// recompute the memfile offset for the aligned address.
		copySize := uffdCopySize(regionPageSize)
		if aligned := addr &^ (copySize - 1); aligned != addr {
			alignedOffset, _, mapErr := u.ma.GetOffset(aligned)
			if mapErr != nil {
				u.logger.Error(ctx, "UFFD serve get mapping error", zap.Error(mapErr))

				return fmt.Errorf("failed to map: %w", mapErr)
			}

			addr = aligned
			offset = alignedOffset
		}

		// Handle write to missing page (WRITE flag)
		// If the event has WRITE flag, it was a write to a missing page.
		// For the write to be executed, we first need to copy the page from the source to the guest memory.
		if flags&UFFD_PAGEFAULT_FLAG_WRITE != 0 {
			u.wg.Go(func() error {
				return u.faultPage(ctx, addr, offset, copySize, u.src, fdExit.SignalExit, block.Write)
			})

			continue
		}

		// Handle read to missing page ("MISSING" flag)
		// If the event has no flags, it was a read to a missing page and we need to copy the page from the source to the guest memory.
		if flags == 0 {
			u.wg.Go(func() error {
				return u.faultPage(ctx, addr, offset, copySize, u.src, fdExit.SignalExit, block.Read)
			})

			continue
		}

		// MINOR and WP flags are not expected as we don't register the uffd with these flags.
		return fmt.Errorf("unexpected event type: %d, closing uffd", flags)
	}
}

func (u *Userfaultfd) faulted() *block.Tracker {
	// This will be at worst cancelled when the uffd is closed.
	u.settleRequests.Lock()
	// The locking here would work even without using defer (just lock-then-unlock the mutex), but at this point let's make it lock to the clone,
	// so it is consistent even if there is a another uffd call after.
	defer u.settleRequests.Unlock()

	return u.missingRequests.Clone()
}

func (u *Userfaultfd) PrefetchData() block.PrefetchData {
	// This will be at worst cancelled when the uffd is closed.
	u.settleRequests.Lock()
	// The locking here would work even without using defer (just lock-then-unlock the mutex), but at this point let's make it lock to the clone,
	// so it is consistent even if there is a another uffd call after.
	defer u.settleRequests.Unlock()

	return u.prefetchTracker.PrefetchData()
}

// Prefault proactively copies a page to guest memory at the given offset.
// This is used to speed up sandbox starts by prefetching pages that are known to be needed.
// Returns nil on success, or if the page is already mapped (EEXIST is handled gracefully).
func (u *Userfaultfd) Prefault(ctx context.Context, offset int64, data []byte) error {
	ctx, span := tracer.Start(ctx, "prefault page")
	defer span.End()

	// Get host virtual address and page size for this offset
	addr, regionPageSize, err := u.ma.GetHostVirtAddr(offset)
	if err != nil {
		return fmt.Errorf("failed to get host virtual address: %w", err)
	}

	copySize := uffdCopySize(regionPageSize)
	if len(data) != int(copySize) {
		return fmt.Errorf("data length (%d) does not match copy size (%d)", len(data), copySize)
	}

	// Prefault bypasses the page-fault address alignment done in Serve, so the
	// address must already be aligned to the copy size. Otherwise UFFDIO_COPY
	// would write the data at a different address than it was read from.
	if addr&(uintptr(copySize)-1) != 0 {
		return fmt.Errorf("prefault address %#x is not aligned to copy size %d", addr, copySize)
	}

	return u.faultPage(ctx, addr, offset, copySize, directDataSource{data, int64(copySize)}, nil, block.Prefetch)
}

// directDataSource wraps a byte slice to implement block.Slicer for prefaulting.
type directDataSource struct {
	data     []byte
	pagesize int64
}

func (d directDataSource) Slice(_ context.Context, _, _ int64) ([]byte, error) {
	return d.data, nil
}

func (d directDataSource) BlockSize() int64 {
	return d.pagesize
}

func (u *Userfaultfd) faultPage(
	ctx context.Context,
	addr uintptr,
	offset int64,
	copySize uintptr,
	source block.Slicer,
	onFailure func() error,
	accessType block.AccessType,
) error {
	span := trace.SpanFromContext(ctx)

	// The RLock must be called inside the goroutine to ensure RUnlock runs via defer,
	// even if the errgroup is cancelled or the goroutine returns early.
	// This check protects us against race condition between marking the request as missing and accessing the missingRequests tracker.
	// The Firecracker pause should return only after the requested memory is faulted in, so we don't need to guard the pagefault from the moment it is created.
	u.settleRequests.RLock()
	defer u.settleRequests.RUnlock()

	defer func() {
		if r := recover(); r != nil {
			u.logger.Error(ctx, "UFFD serve panic", zap.Any("copy_size", copySize), zap.Any("panic", r))
		}
	}()

	b, dataErr := block.ReadSlice(ctx, source, offset, int64(copySize))
	if dataErr != nil {
		var signalErr error
		if onFailure != nil {
			signalErr = onFailure()
		}

		joinedErr := errors.Join(dataErr, signalErr)

		span.RecordError(joinedErr)
		u.logger.Error(ctx, "UFFD serve data fetch error", zap.Error(joinedErr))

		return fmt.Errorf("failed to read from source: %w", joinedErr)
	}

	// A single UFFDIO_COPY must not read past the source slice, so make sure the
	// source returned exactly one full copy unit.
	if uintptr(len(b)) != copySize {
		lenErr := fmt.Errorf("source returned %d bytes, expected %d", len(b), copySize)

		var signalErr error
		if onFailure != nil {
			signalErr = onFailure()
		}

		joinedErr := errors.Join(lenErr, signalErr)

		span.RecordError(joinedErr)
		u.logger.Error(ctx, "UFFD serve data fetch error", zap.Error(joinedErr))

		return fmt.Errorf("failed to read from source: %w", joinedErr)
	}

	var copyMode CULong

	// Performing copy() on UFFD clears the WP bit unless we explicitly tell
	// it not to. We do that for faults caused by a read access. Write accesses
	// would anyways cause clear the write-protection bit.
	//if accessType != block.Write {
	//	copyMode |= UFFDIO_COPY_MODE_WP
	//}

	copyErr := u.fd.copy(addr, copySize, b, copyMode)
	if errors.Is(copyErr, unix.EEXIST) {
		// Page is already mapped
		span.SetAttributes(attribute.Bool("uffd.already_mapped", true))

		return nil
	}

	if copyErr != nil {
		var signalErr error
		if onFailure != nil {
			signalErr = onFailure()
		}

		joinedErr := errors.Join(copyErr, signalErr)

		span.RecordError(joinedErr)
		u.logger.Error(ctx, "UFFD serve uffdio copy error", zap.Error(joinedErr))

		return fmt.Errorf("failed uffdio copy %w", joinedErr)
	}

	// Add the offset to the missing requests tracker with metadata.
	// A single UFFDIO_COPY can cover multiple source blocks (e.g. a 64KiB host
	// page over a 4KiB memfile block size), so track every covered block to keep
	// the prefetch mapping and missing-request accounting accurate.
	blockSize := u.src.BlockSize()
	for o := offset; o < offset+int64(copySize); o += blockSize {
		u.missingRequests.Add(o)
		u.prefetchTracker.Add(o, accessType)
	}

	return nil
}
