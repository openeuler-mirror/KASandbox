package sandbox

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// diskReadBytes reports how many bytes a process has actually fetched from
// the storage layer, read out of /proc/<pid>/io. A page-cache hit adds
// nothing to it, which is the whole point: it is what separates "this restore
// read its inputs out of cache" from "this restore went to the disk".
//
// Three things to know before reading a number out of it:
//   - it includes readahead the kernel issued on the process's behalf, so it
//     is an upper bound on what was strictly needed;
//   - it is accounted when the read is submitted, not when it completes;
//   - work done in another process's context (an NBD server thread, say)
//     lands on that process, not this one.
//
// So the signal is the order of magnitude — near zero versus tens of
// megabytes — not the exact byte count.
//
// ok is false when the counter cannot be read: no /proc, the process already
// gone, permissions. Callers skip the measurement then. An unreadable counter
// must never fail the operation it was there to measure.
func diskReadBytes(pid int) (uint64, bool) {
	f, err := os.Open("/proc/" + strconv.Itoa(pid) + "/io")
	if err != nil {
		return 0, false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		value, found := strings.CutPrefix(scanner.Text(), "read_bytes:")
		if !found {
			continue
		}

		n, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return 0, false
		}

		return n, true
	}

	return 0, false
}

// diskReadSince returns the megabytes pid fetched from storage since before,
// and whether both ends of the measurement were readable. The counter is
// monotonic per process, so a decrease means the two ends did not come from
// the same process and the reading is discarded rather than reported as zero.
func diskReadSince(pid int, before uint64, beforeOK bool) (float64, bool) {
	after, ok := diskReadBytes(pid)
	if !beforeOK || !ok || after < before {
		return 0, false
	}

	return float64(after-before) / (1024 * 1024), true
}
