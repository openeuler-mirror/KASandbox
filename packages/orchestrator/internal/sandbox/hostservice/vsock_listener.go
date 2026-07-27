package hostservice

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// VMADDR_CID_ANY is the host-side CID for listeners that accept connections
// from any local guest. Re-exported under a shorter name; also available
// as unix.VMADDR_CID_ANY.
const VMADDR_CID_ANY uint32 = 0xFFFFFFFF

const vsockListenBacklog = 5

// VsockListen creates an AF_VSOCK SOCK_STREAM listening socket bound to
// (cid, port) and returns it as an *os.File for passing to a child process
// via exec.Cmd.ExtraFiles (child sees it as fd 3+i). The child calls
// accept(2); the host never accepts on this socket.
func VsockListen(cid, port uint32) (*os.File, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket(cid=%d, port=%d): %w", cid, port, err)
	}

	// golang.org/x/sys/unix v0.40.0 exports vsock sockaddr as "SockaddrVM".
	sa := &unix.SockaddrVM{
		CID:  cid,
		Port: port,
	}
	if err := unix.Bind(fd, sa); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("vsock bind(cid=%d, port=%d): %w", cid, port, err)
	}
	if err := unix.Listen(fd, vsockListenBacklog); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("vsock listen(cid=%d, port=%d): %w", cid, port, err)
	}

	// os.NewFile takes ownership of the FD; Close() will close it.
	return os.NewFile(uintptr(fd), fmt.Sprintf("vsock-listen:%d:%d", cid, port)), nil
}
