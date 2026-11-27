//go:build linux

package v2

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"
)

// The host firewall table is a host-wide singleton, so elements left by a
// crashed run would fail the emptiness assertions of a later one. Reconcile it
// to empty once per process — the same call the orchestrator makes at startup.
func TestMain(m *testing.M) {
	if os.Geteuid() == 0 {
		if err := reconcileHostFirewall(); err != nil {
			// Gate and container environments can run the suite as root without
			// CAP_NET_ADMIN; skip instead of panicking so `go test ./...` there
			// stays green. Genuine failures on capable hosts still panic.
			if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EPERM) {
				fmt.Println("SKIP: resetting the host firewall table is not permitted in this environment")
				os.Exit(0)
			}
			panic(fmt.Sprintf("cannot reset the host firewall table: %v", err))
		}
	}

	m.Run()
}

func reconcileHostFirewall() error {
	hf, err := NewHostFirewall("lo", testConfig())
	if err != nil {
		return err
	}

	if err := hf.ReconcileSlots(context.Background(), nil); err != nil {
		return err
	}

	return hf.Close()
}
