package nbd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Merovius/nbd/nbdnl"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

// ioctl numbers from linux/nbd.h: _IO(0xab, nr)
const (
	ioctlNBDClearSock  = 0xab04
	ioctlNBDDisconnect = 0xab08
)

// ForceDisconnect tears down a connected NBD device even when userspace
// sockets are already dead (EPIPE/-32).
//
// Individual disconnect mechanisms may fail; success is determined by whether
// the device becomes free afterwards.
func ForceDisconnect(ctx context.Context, idx DeviceSlot) error {
	var errs []error

	if err := nbdnl.Disconnect(idx); err != nil {
		errs = append(errs, fmt.Errorf("nbdnl disconnect: %w", err))
		logger.L().Warn(ctx, "nbdnl force disconnect failed",
			zap.Uint32("device_index", idx),
			zap.Error(err),
		)
	}

	if err := ioctlDisconnect(idx); err != nil {
		errs = append(errs, fmt.Errorf("ioctl disconnect: %w", err))
		logger.L().Warn(ctx, "ioctl force disconnect failed",
			zap.Uint32("device_index", idx),
			zap.Error(err),
		)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return errors.Join(append(errs, ctx.Err())...)
		default:
		}
		free, err := IsDeviceFree(idx)
		if err == nil && free {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}

	free, err := IsDeviceFree(idx)
	if err == nil && free {
		return nil
	}
	if err != nil {
		errs = append(errs, err)
	}

	return fmt.Errorf("device %d still in use after force disconnect: %w", idx, errors.Join(errs...))
}

func ioctlDisconnect(idx DeviceSlot) error {
	f, err := os.OpenFile(GetDevicePath(idx), os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()

	fd := int(f.Fd())
	var errs []error
	if err := unix.IoctlSetInt(fd, ioctlNBDDisconnect, 0); err != nil {
		errs = append(errs, fmt.Errorf("NBD_DISCONNECT: %w", err))
	}
	if err := unix.IoctlSetInt(fd, ioctlNBDClearSock, 0); err != nil {
		errs = append(errs, fmt.Errorf("NBD_CLEAR_SOCK: %w", err))
	}

	return errors.Join(errs...)
}
