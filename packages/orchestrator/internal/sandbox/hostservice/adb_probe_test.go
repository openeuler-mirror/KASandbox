package hostservice

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

func TestADBReplyCommand(t *testing.T) {
	header := adbMessage(adbCNXN, adbVersion, adbMaxData, nil)[:adbHeaderSize]
	command, err := adbReplyCommand(header)
	if err != nil {
		t.Fatalf("adbReplyCommand() error = %v", err)
	}
	if command != adbCNXN {
		t.Fatalf("adbReplyCommand() = %#x, want %#x", command, adbCNXN)
	}

	header[20] ^= 1
	if _, err := adbReplyCommand(header); err == nil {
		t.Fatal("adbReplyCommand() accepted malformed magic")
	}
}

func TestADBClientCNXNUsesSkipChecksum(t *testing.T) {
	if checksum := binary.LittleEndian.Uint32(adbClientCNXN()[16:]); checksum != 0 {
		t.Fatalf("ADB checksum = %#x, want 0", checksum)
	}
}

func TestProbeADBPath(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()

		header := make([]byte, adbHeaderSize)
		if _, err := io.ReadFull(conn, header); err != nil {
			serverErr <- err
			return
		}
		payload := make([]byte, binary.LittleEndian.Uint32(header[12:]))
		if _, err := io.ReadFull(conn, payload); err != nil {
			serverErr <- err
			return
		}
		if command := binary.LittleEndian.Uint32(header[0:]); command != adbCNXN {
			serverErr <- &unexpectedADBCommandError{got: command, want: adbCNXN}
			return
		}
		_, err = conn.Write(adbMessage(adbCNXN, adbVersion, adbMaxData, []byte("device::\x00")))
		serverErr <- err
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := probeADBPath(ctx, listener.Addr().String()); err != nil {
		t.Fatalf("probeADBPath() error = %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("mock adbd error = %v", err)
	}
}

func TestADBReplyProvesPathRequiresOnlineTransport(t *testing.T) {
	if !adbReplyProvesPath(adbCNXN) {
		t.Fatal("A_CNXN reply should prove the ADB transport is online")
	}
	if adbReplyProvesPath(adbAUTH) {
		t.Fatal("A_AUTH reply should not be accepted before authentication completes")
	}
}

type unexpectedADBCommandError struct {
	got  uint32
	want uint32
}

func (e *unexpectedADBCommandError) Error() string {
	return "unexpected ADB command"
}
