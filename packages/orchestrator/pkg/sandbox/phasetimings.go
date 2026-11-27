package sandbox

import "time"

// PhaseTimings records where the time went inside the host during one
// checkpoint or one rollback, in milliseconds.
//
// It is the third of three clocks and the only one that says what to fix. The
// client's wall clock is what an SDK call takes; the guest-visible freeze is
// what the workload actually feels; this is where either of them went. When a
// latency target is missed, the first two say by how much and this one says
// where.
//
// Values are milliseconds unless the key says otherwise: a key ending in
// `_mb` holds megabytes. Mixing the two in one map is deliberate — these are
// read together, and a phase's duration means little without knowing whether
// it went to the disk.
//
// A nil map is a working no-op, so a caller that does not want the breakdown
// passes nil and pays nothing for it.
type PhaseTimings map[string]float64

// Mark records the time from start until now under name.
func (t PhaseTimings) Mark(name string, start time.Time) {
	if t == nil {
		return
	}

	t[name] = float64(time.Since(start).Microseconds()) / 1000
}

// SetUs records a duration already expressed in microseconds. Firecracker
// reports its own internal rollback breakdown that way.
func (t PhaseTimings) SetUs(name string, us uint64) {
	if t == nil {
		return
	}

	t[name] = float64(us) / 1000
}

// SetMB records a byte count in megabytes, for the `_mb` keys.
func (t PhaseTimings) SetMB(name string, mb float64) {
	if t == nil {
		return
	}

	t[name] = mb
}

// Timed runs fn and records how long it took. It exists so a phase can be
// measured by wrapping the call in place, without restructuring the error
// handling around it -- which matters here because several of these calls sit
// inside error branches that would be awkward to split.
func (t PhaseTimings) Timed(name string, fn func() error) error {
	start := time.Now()
	err := fn()
	t.Mark(name, start)

	return err
}
