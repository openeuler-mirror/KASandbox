package nbd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Merovius/nbd/nbdnl"
	"golang.org/x/sys/unix"
)

// ioctl numbers from linux/nbd.h: _IO(0xab, nr)
const (
	ioctlNBDClearSock  = 0xab04
	ioctlNBDDisconnect = 0xab08
)

// ForceDisconnect tears down a connected NBD device even when userspace
// sockets are already dead (EPIPE/-32).
func ForceDisconnect(ctx context.Context, idx DeviceSlot) error {
	_ = nbdnl.Disconnect(idx)
	_ = ioctlDisconnect(idx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
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

	return fmt.Errorf("device %d still in use after force disconnect", idx)
}

func ioctlDisconnect(idx DeviceSlot) error {
	f, err := os.OpenFile(GetDevicePath(idx), os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()

	fd := int(f.Fd())
	_ = unix.IoctlSetInt(fd, ioctlNBDDisconnect, 0)

	return unix.IoctlSetInt(fd, ioctlNBDClearSock, 0)
}
