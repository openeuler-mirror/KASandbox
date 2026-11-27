package stratovirt

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func boundQMPSocket(t *testing.T) (string, *os.File) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "qmp.sock")
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(fd), path)
	t.Cleanup(func() { file.Close() })
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: path}); err != nil {
		t.Fatal(err)
	}
	return path, file
}

func TestQMPConnectRetriesRefused(t *testing.T) {
	path, file := boundQMPSocket(t)
	t.Setenv("QMP_CONNECT_TIMEOUT_SECONDS", "5")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := newQMPClient(path)
	result := make(chan error, 1)
	go func() { result <- client.connect(ctx) }()

	// Keep the path bound but not listening for several retry intervals.
	select {
	case err := <-result:
		t.Fatalf("connect returned before listen: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := syscall.Listen(int(file.Fd()), 1); err != nil {
		t.Fatal(err)
	}
	listener, err := net.FileListener(file)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := listener.(*net.UnixListener).SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	peer, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if err := peer.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.Write([]byte("{\"QMP\":{\"version\":{\"qemu\":{\"major\":5,\"minor\":0,\"micro\":1}},\"capabilities\":[]}}\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
		defer client.conn.Close()
	case <-ctx.Done():
		t.Fatal("connect did not complete after listen and greeting")
	}
}

func TestQMPConnectRetryStops(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout string
		missing bool
		cancel  bool
		want    error
	}{
		{name: "caller cancellation", timeout: "5", cancel: true, want: context.Canceled},
		{name: "caller deadline", timeout: "5", want: context.DeadlineExceeded},
		{name: "configured timeout", timeout: "1", want: context.DeadlineExceeded},
		{name: "other errors are not retried", timeout: "5", missing: true, want: syscall.ENOENT},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("QMP_CONNECT_TIMEOUT_SECONDS", tc.timeout)
			path, _ := boundQMPSocket(t)
			if tc.missing {
				path += ".missing"
			}
			duration := 100 * time.Millisecond
			if tc.timeout == "1" {
				duration = 5 * time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), duration)
			defer cancel()
			if tc.cancel {
				timer := time.AfterFunc(30*time.Millisecond, cancel)
				defer timer.Stop()
			}
			client := newQMPClient(path)
			if err := client.connect(ctx); !errors.Is(err, tc.want) {
				t.Fatalf("connect error = %v, want %v", err, tc.want)
			}
			if client.conn != nil || client.decoder != nil {
				t.Fatal("failed connect published a connection")
			}
		})
	}
}
