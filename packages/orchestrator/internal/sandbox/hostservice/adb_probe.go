package hostservice

import (
	"encoding/binary"
	"fmt"
)

// Readiness requires a complete A_CNXN response; A_AUTH is not yet online.
const (
	adbCNXN = 0x4e584e43 // "CNXN" in little-endian byte order
	adbAUTH = 0x48545541 // "AUTH" in little-endian byte order

	adbHeaderSize = 24
	adbVersion    = 0x01000001
	adbMaxData    = 4096
	adbHostBanner = "host::\x00"
)

func adbMessage(command, arg0, arg1 uint32, payload []byte) []byte {
	message := make([]byte, adbHeaderSize+len(payload))
	binary.LittleEndian.PutUint32(message[0:], command)
	binary.LittleEndian.PutUint32(message[4:], arg0)
	binary.LittleEndian.PutUint32(message[8:], arg1)
	binary.LittleEndian.PutUint32(message[12:], uint32(len(payload)))
	binary.LittleEndian.PutUint32(message[16:], 0)
	binary.LittleEndian.PutUint32(message[20:], command^uint32(0xffffffff))
	copy(message[adbHeaderSize:], payload)
	return message
}

func adbClientCNXN() []byte {
	return adbMessage(adbCNXN, adbVersion, adbMaxData, []byte(adbHostBanner))
}

func adbReplyCommand(header []byte) (uint32, error) {
	if len(header) < adbHeaderSize {
		return 0, fmt.Errorf("short ADB reply header: got %d bytes, want %d", len(header), adbHeaderSize)
	}

	command := binary.LittleEndian.Uint32(header[0:])
	magic := binary.LittleEndian.Uint32(header[20:])
	if magic != command^uint32(0xffffffff) {
		return 0, fmt.Errorf("malformed ADB reply: magic %#x does not match command %#x", magic, command)
	}

	return command, nil
}

func adbReplyProvesPath(command uint32) bool {
	return command == adbCNXN
}
