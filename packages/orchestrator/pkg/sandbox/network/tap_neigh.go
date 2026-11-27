package network

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

// Fast ARP retransmission on the sandbox tap device.
//
// When Firecracker opens the tap device the carrier comes up, but the kernel
// activates the transmit queue later from its linkwatch work queue (7-26 ms
// later in KASandbox measurements, longer with more concurrent creates). The
// orchestrator sends envd /init a few milliseconds after resume, while the
// neighbour entry for the guest in the slot netns is not resolved yet, so the
// first ARP request can hit the inactive queue and be dropped
// (kfree_skb reason=QDISC_DROP). With the kernel default retrans_time_ms=1000
// that sandbox create then waits about one second more. Retrying in the
// application does not help: later SYNs queue behind the same unresolved
// neighbour entry.
//
// With a short retransmission interval a lost first ARP costs only a few more
// milliseconds. The probe counts are raised with it so that the time until an
// unanswered neighbour is declared FAILED stays close to the kernel default of
// 3 x 1 s, keeping the tolerance for a stalled guest. Only the tap device's own
// settings inside the slot netns are written, never the global defaults (the
// kernel copies the defaults to existing host devices, physical NICs included).

// Paths under procSysNetIPv4Neigh resolve in the netns of the calling thread;
// the caller must have locked the OS thread and switched to the target netns.
const procSysNetIPv4Neigh = "/proc/sys/net/ipv4/neigh"

type neighSysctl struct {
	name  string
	value string
}

// tapNeighSysctls returns the neighbour settings to write; a non-positive
// value for either setting keeps the kernel defaults. The probe counts are
// raised before the interval is shortened, so there is never a short interval
// with only 3 probes in between.
func tapNeighSysctls(retransTimeMs, probes int) []neighSysctl {
	if retransTimeMs <= 0 || probes <= 0 {
		return nil
	}

	return []neighSysctl{
		{name: "mcast_solicit", value: strconv.Itoa(probes)},
		{name: "ucast_solicit", value: strconv.Itoa(probes)},
		{name: "retrans_time_ms", value: strconv.Itoa(retransTimeMs)},
	}
}

// applyNeighSysctls writes sysctls into dir. If any step fails, the settings
// already written are restored to their previous values, so no half-applied
// combination is left behind (190 probes x 1 s would stretch the window until
// FAILED to three minutes).
func applyNeighSysctls(dir string, sysctls []neighSysctl, write func(path, value string) error) error {
	previous := make([]neighSysctl, 0, len(sysctls))

	for _, sysctl := range sysctls {
		path := filepath.Join(dir, sysctl.name)

		current, err := os.ReadFile(path)
		if err == nil {
			err = write(path, sysctl.value)
		}

		if err != nil {
			errs := []error{fmt.Errorf("error setting %s to %s: %w", path, sysctl.value, err)}

			for _, restore := range previous {
				restoreErr := write(filepath.Join(dir, restore.name), restore.value)
				if restoreErr != nil {
					errs = append(errs, fmt.Errorf("error restoring %s to %s: %w", restore.name, restore.value, restoreErr))
				}
			}

			return errors.Join(errs...)
		}

		previous = append(previous, neighSysctl{name: sysctl.name, value: strings.TrimSpace(string(current))})
	}

	return nil
}

// tuneTapNeighbor shortens the ARP retransmission interval of the tap device
// in the current netns. A failure is only logged: the kernel defaults still
// work, at the cost of the occasional one-second start delay.
func (s *Slot) tuneTapNeighbor(ctx context.Context) {
	sysctls := tapNeighSysctls(s.config.TapNeighRetransTimeMs, s.config.TapNeighProbes)
	if len(sysctls) == 0 {
		return
	}

	err := applyNeighSysctls(filepath.Join(procSysNetIPv4Neigh, s.TapName()), sysctls, func(path, value string) error {
		return os.WriteFile(path, []byte(value), 0o644)
	})
	if err != nil {
		logger.L().Warn(ctx, "failed to tune ARP retransmission on the sandbox tap device, kernel defaults stay in effect",
			zap.String("namespace", s.NamespaceID()),
			zap.Error(err))
	}
}
