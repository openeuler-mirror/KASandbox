package platform

import (
	"errors"
	"fmt"
	"net/netip"

	"github.com/txn2/txeh"
)

type DiskSpace struct {
	Total     uint64
	Available uint64
}

const eventsHost = "events.e2b.local"

var (
	ErrInvalidAddress       = errors.New("invalid IP address")
	ErrUnknownAddressFormat = errors.New("unknown IP address format")
	ErrUnsupportedPTY       = errors.New("PTY is not supported on Windows")
)

func getIPFamily(address string) (txeh.IPFamily, error) {
	addressIP, err := netip.ParseAddr(address)
	if err != nil {
		return txeh.IPFamilyV4, fmt.Errorf("failed to parse IP address: %w", err)
	}

	switch {
	case addressIP.Is4():
		return txeh.IPFamilyV4, nil
	case addressIP.Is6():
		return txeh.IPFamilyV6, nil
	default:
		return txeh.IPFamilyV4, fmt.Errorf("%w: %s", ErrUnknownAddressFormat, address)
	}
}
