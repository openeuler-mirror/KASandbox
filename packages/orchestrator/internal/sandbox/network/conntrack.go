package network

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sys/unix"
)

// FlushConntrack drops the kernel's connection tracking state for this slot's
// sandbox, in the sandbox's own network namespace and in the host's.
//
// It exists for rollback. A restored guest has forgotten connections the
// kernel still tracks, and its TCP sequence numbers have gone backwards.
// Entries left behind make the kernel judge the guest's packets invalid and
// drop them, so a connection that outlives a rollback hangs rather than
// failing — the worst failure mode to debug. Dropping the state lets the
// guest's packets start fresh flows, which is what the guest believes it is
// doing anyway.
//
// Call it while the VM is down, between stopping the old Firecracker process
// and resuming the restored one: no traffic exists then, so nothing can
// re-create an entry that is about to be wrong.
func (s *Slot) FlushConntrack(ctx context.Context) error {
	_, span := tracer.Start(ctx, "slot-conntrack-flush", trace.WithAttributes(
		attribute.String("namespace_id", s.NamespaceID()),
	))
	defer span.End()

	return errors.Join(s.flushNamespaceConntrack(), s.deleteHostConntrack())
}

// flushNamespaceConntrack empties the table inside the sandbox's namespace,
// which tracks nothing but this sandbox's flows.
func (s *Slot) flushNamespaceConntrack() error {
	n, err := ns.GetNS(filepath.Join(netNamespacesDir, s.NamespaceID()))
	if err != nil {
		return fmt.Errorf("failed to get slot network namespace '%s': %w", s.NamespaceID(), err)
	}
	defer n.Close()

	err = n.Do(func(_ ns.NetNS) error {
		// netlink opens a socket per request as long as the package handle
		// owns none, so this lands in the namespace this callback runs in
		// rather than in the host's.
		return netlink.ConntrackTableFlush(netlink.ConntrackTable)
	})
	if err != nil {
		return fmt.Errorf("failed to flush conntrack in namespace '%s': %w", s.NamespaceID(), err)
	}

	return nil
}

// deleteHostConntrack removes this slot's flows from the host's table, which
// is shared with every other sandbox on the node and must not be flushed
// wholesale. Guest traffic on its way out reaches the host already SNATed to
// the slot's address, and traffic on its way in is addressed to it, so
// matching that address in either direction covers both.
func (s *Slot) deleteHostConntrack() error {
	// One filter per direction rather than one filter with both: conditions
	// within a filter must all match, while ConntrackDeleteFilters deletes a
	// flow matching any of the filters it is given.
	filters := make([]netlink.CustomConntrackFilter, 0, 2)
	for _, direction := range []netlink.ConntrackFilterType{netlink.ConntrackOrigSrcIP, netlink.ConntrackOrigDstIP} {
		filter := &netlink.ConntrackFilter{}
		if err := filter.AddIP(direction, s.HostIP); err != nil {
			return fmt.Errorf("failed to build conntrack filter for %s: %w", s.HostIPString(), err)
		}

		filters = append(filters, filter)
	}

	_, err := netlink.ConntrackDeleteFilters(netlink.ConntrackTable, netlink.InetFamily(unix.AF_INET), filters...)
	if err != nil {
		return fmt.Errorf("failed to delete host conntrack entries for %s: %w", s.HostIPString(), err)
	}

	return nil
}
