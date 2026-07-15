package systeminit

import (
	_ "embed"
	"errors"
	"runtime"
)

//go:embed busybox_1.36.1-2
var busyboxX86 []byte

//go:embed busybox_1.35_arm64
var busyboxArm64 []byte

var BusyboxBinary []byte

func init() {
	switch runtime.GOARCH {
	case "amd64", "386":
		BusyboxBinary = busyboxX86
	case "arm64":
		BusyboxBinary = busyboxArm64
	default:
		panic(errors.New("unsupported arch: " + runtime.GOARCH))
	}
}

