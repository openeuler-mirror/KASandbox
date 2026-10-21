//go:build linux

// Package numa initializes optional locality preferences before service startup.
package numa

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

const preferredMany = 5 // MPOL_PREFERRED_MANY (Linux >= 5.15).

const reexecNodeEnv = "_E2B_NUMA_LOCALITY_NODE"

type placement struct {
	node int
	cpus []int
}

type nodeInfo struct {
	cpus         []int
	freeMemoryKB uint64
}

// Initialize retains the allowed resources and prefers one eligible NUMA node.
// Memory policy is per OS thread. Re-exec after setting it, rather than changing
// only the Go thread that happens to run main, so all runtime threads inherit it.
func Initialize() (err error) {
	switch os.Getenv("NUMA_LOCALITY") {
	case "", "off":
		return nil
	case "on":
	default:
		return fmt.Errorf("NUMA_LOCALITY must be off or on")
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	status, err := readStatus()
	if err != nil {
		return err
	}
	cpus, err := parseList(status["Cpus_allowed_list"])
	if err != nil {
		return fmt.Errorf("allowed CPUs: %w", err)
	}
	mems, err := parseList(status["Mems_allowed_list"])
	if err != nil {
		return fmt.Errorf("allowed memory nodes: %w", err)
	}
	online, err := readList("/sys/devices/system/node/has_memory")
	if err != nil {
		return err
	}
	nodes := make(map[int]nodeInfo)
	for _, node := range online {
		if !slices.Contains(mems, node) {
			continue
		}
		nodeCPUs, readErr := readList(fmt.Sprintf("/sys/devices/system/node/node%d/cpulist", node))
		if readErr != nil {
			return readErr
		}
		data, readErr := os.ReadFile(fmt.Sprintf("/sys/devices/system/node/node%d/meminfo", node))
		if readErr != nil {
			return readErr
		}
		freeMemoryKB, readErr := parseFreeMemory(string(data))
		if readErr != nil {
			return fmt.Errorf("node%d free memory: %w", node, readErr)
		}
		nodes[node] = nodeInfo{cpus: nodeCPUs, freeMemoryKB: freeMemoryKB}
	}
	// Re-exec inherits the chosen node once; a new service start selects afresh.
	reexecNode := os.Getenv(reexecNodeEnv)
	_ = os.Unsetenv(reexecNodeEnv)
	preferred, err := choosePlacement(cpus, mems, nodes, reexecNode)
	if err != nil {
		return err
	}
	cgroup, err := serviceCgroup()
	if err != nil {
		return err
	}
	preferredPath := filepath.Join(cgroup, "cpuset.preferred_cpus")
	if _, err = os.Stat(preferredPath); err != nil {
		return fmt.Errorf("NUMA locality requires writable cgroup v2 cpuset.preferred_cpus and openEuler dynamic_affinity=enable: %w", err)
	}

	// Use the architecture's syscall constants and native word size; no fixed
	// ARM64 syscall numbers or CPU/node counts are embedded in the binary.
	wordBits := int(unsafe.Sizeof(uintptr(0))) * 8
	possible, err := readList("/sys/devices/system/node/possible")
	if err != nil {
		return err
	}
	maxNode := slices.Max(possible)
	mask := make([]uintptr, maxNode/wordBits+1)
	mask[preferred.node/wordBits] = uintptr(1) << uint(preferred.node%wordBits)
	current := make([]uintptr, len(mask))
	var mode int32
	_, _, errno := unix.Syscall6(unix.SYS_GET_MEMPOLICY, uintptr(unsafe.Pointer(&mode)),
		uintptr(unsafe.Pointer(&current[0])), uintptr(len(mask)*wordBits), 0, 0, 0)
	if errno != 0 {
		return fmt.Errorf("get_mempolicy: %w", errno)
	}
	reexec := int(mode) != preferredMany || !slices.Equal(current, mask)
	if reexec {
		// get_nodes decrements maxnode before reading the mask; include its last bit.
		_, _, errno = unix.Syscall6(unix.SYS_SET_MEMPOLICY, preferredMany,
			uintptr(unsafe.Pointer(&mask[0])), uintptr(len(mask)*wordBits+1), 0, 0, 0)
		if errno != 0 {
			return fmt.Errorf("set_mempolicy(MPOL_PREFERRED_MANY): %w", errno)
		}
	}

	// Never widen administrator limits. Explicit CPU masks are required by
	// preferred_cpus; materialize the inherited effective mask if it is empty.
	// A populated cpuset cannot always be reset to an empty mask, so on failure
	// restore the preference and retain any equivalent explicit allowed mask.
	type change struct{ path, value string }
	var changes []change
	defer func() {
		if err != nil {
			for _, c := range slices.Backward(changes) {
				if restoreErr := os.WriteFile(c.path, []byte(c.value+"\n"), 0); restoreErr != nil {
					log.Printf("NUMA locality: restore %s: %v", c.path, restoreErr)
				}
			}
		}
	}()
	for _, name := range []string{"cpuset.cpus", "cpuset.preferred_cpus"} {
		path := filepath.Join(cgroup, name)
		original, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		value := strings.TrimSpace(string(original))
		if name == "cpuset.preferred_cpus" {
			value = formatList(preferred.cpus)
		} else if value == "" {
			effective, readErr := os.ReadFile(path + ".effective")
			if readErr != nil {
				return readErr
			}
			value = strings.TrimSpace(string(effective))
		}
		if value != strings.TrimSpace(string(original)) {
			if err = os.WriteFile(path, []byte(value+"\n"), 0); err != nil {
				return fmt.Errorf("set %s: %w", name, err)
			}
			if name == "cpuset.preferred_cpus" {
				changes = append(changes, change{path, strings.TrimSpace(string(original))})
			}
		}
	}
	status, err = readStatus()
	if err != nil {
		return err
	}
	actual, err := parseList(status["Cpus_preferred_list"])
	if err != nil || !slices.Equal(actual, preferred.cpus) {
		return fmt.Errorf("CPU preference is inactive; enable dynamic_affinity=enable on the host")
	}

	if reexec {
		executable, execErr := os.Executable()
		if execErr != nil {
			return execErr
		}
		return unix.Exec(executable, os.Args, append(os.Environ(), fmt.Sprintf("%s=%d", reexecNodeEnv, preferred.node)))
	}
	log.Printf("NUMA locality: prefer node%d CPUs=%s, memory=preferred-many; free memory=%d KiB; allowed CPUs=%s memory=%s",
		preferred.node, formatList(preferred.cpus), nodes[preferred.node].freeMemoryKB, status["Cpus_allowed_list"], status["Mems_allowed_list"])
	if features, readErr := os.ReadFile("/sys/kernel/debug/sched/features"); readErr == nil &&
		slices.Contains(strings.Fields(string(features)), "DA_UTIL_TASKGROUP") {
		log.Print("NUMA locality: DA_UTIL_TASKGROUP is enabled; CPU overflow considers this task group's utilization, not total CPU contention")
	}

	return nil
}

// Pick the eligible node with the most free ordinary memory once per service start.
func choosePlacement(cpus, mems []int, nodes map[int]nodeInfo, reexecNode string) (placement, error) {
	if reexecNode != "" {
		node, err := strconv.Atoi(reexecNode)
		if err != nil || !slices.Contains(mems, node) {
			return placement{}, fmt.Errorf("invalid NUMA re-exec node %q", reexecNode)
		}
		mems = []int{node}
	}
	var selected placement
	var mostFree uint64
	found := false
	for _, node := range mems {
		var preferred []int
		for _, cpu := range nodes[node].cpus {
			if slices.Contains(cpus, cpu) {
				preferred = append(preferred, cpu)
			}
		}
		free := nodes[node].freeMemoryKB
		if len(preferred) > 0 && (!found || free > mostFree || (free == mostFree && node < selected.node)) {
			selected, mostFree, found = placement{node, preferred}, free, true
		}
	}
	if !found {
		return placement{}, fmt.Errorf("no allowed NUMA node has both CPUs and memory")
	}
	return selected, nil
}

func parseFreeMemory(value string) (uint64, error) {
	for _, line := range strings.Split(value, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 5 && fields[2] == "MemFree:" && fields[4] == "kB" {
			return strconv.ParseUint(fields[3], 10, 64)
		}
	}
	return 0, fmt.Errorf("missing MemFree in node meminfo")
}

func serviceCgroup() (string, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if relative, ok := strings.CutPrefix(line, "0::"); ok {
			path := filepath.Join("/sys/fs/cgroup", relative)
			// The real v2 root has only cpuset.cpus.effective. A cgroup
			// namespace may expose a dedicated service cgroup at '/'.
			if _, statErr := os.Stat(filepath.Join(path, "cpuset.cpus")); statErr != nil {
				return "", fmt.Errorf("NUMA locality requires a dedicated writable cpuset cgroup: %w", statErr)
			}
			return path, nil
		}
	}
	return "", fmt.Errorf("NUMA locality requires cgroup v2")
}

func readStatus() (map[string]string, error) {
	// /proc/self/status is the thread-group leader, which may not be our thread.
	data, err := os.ReadFile("/proc/thread-self/status")
	if err != nil {
		return nil, err
	}
	status := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		if key, value, ok := strings.Cut(line, ":"); ok {
			status[key] = strings.TrimSpace(value)
		}
	}
	return status, nil
}

func readList(path string) ([]int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseList(strings.TrimSpace(string(data)))
}

func parseList(value string) ([]int, error) {
	var result []int
	if value == "" {
		return result, nil
	}
	for _, part := range strings.Split(value, ",") {
		first, last, isRange := strings.Cut(part, "-")
		start, err := strconv.Atoi(first)
		if err != nil || start < 0 {
			return nil, fmt.Errorf("invalid CPU/node list %q", value)
		}
		end := start
		if isRange {
			end, err = strconv.Atoi(last)
			if err != nil || end < start {
				return nil, fmt.Errorf("invalid CPU/node list %q", value)
			}
		}
		for id := start; id <= end; id++ {
			result = append(result, id)
		}
	}
	slices.Sort(result)
	return slices.Compact(result), nil
}

func formatList(ids []int) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.Itoa(id))
	}
	return strings.Join(parts, ",")
}
