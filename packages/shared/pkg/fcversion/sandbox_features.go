package fcversion

import (
	"os"
	"strconv"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// Per-feature release floors. The gates are release-based on purpose: legacy
// (_hash) and bare dev builds never qualify even when a particular binary
// happens to carry the endpoints — the version string is the support
// contract, not a capability probe. The floors differ because the features
// shipped in different releases: fs-only snapshots never touch the balloon,
// so they must not inherit the in-place checkpoint's floor (0.1.x fleets run
// fs-only in production today).
var (
	// 0.2.0 introduced PATCH /balloon/reporting/{pause,resume} +
	// GET /balloon/reporting/status, which the in-place checkpoint's CoW
	// memory window drives while free-page reporting is live.
	inPlaceCheckpointMinE2B = semver.New(0, 2, 0, "", "")
	// Filesystem-only snapshots are part of the e2b release contract from
	// its first release.
	filesystemSnapshotsMinE2B = semver.New(0, 1, 0, "", "")
)

func (v *Info) atLeastE2B(minVersion *semver.Version) bool {
	return v.format == formatE2B && !v.e2bVersion.LessThan(minVersion)
}

// HasInPlaceCheckpoint reports whether this build's release contract includes
// the in-place checkpoint (pause, snapshot, resume the same FC process, with
// the deferred CoW memory export). Callers fall back to the resume-fresh
// checkpoint when false.
func (v *Info) HasInPlaceCheckpoint() bool {
	return v.atLeastE2B(inPlaceCheckpointMinE2B)
}

// HasFilesystemSnapshots reports whether this build's release contract
// includes producing filesystem-only (memoryless) snapshots. Callers refuse
// the request when false — silently taking a memory snapshot instead would
// betray an explicit memory:false.
func (v *Info) HasFilesystemSnapshots() bool {
	return v.atLeastE2B(filesystemSnapshotsMinE2B)
}

// UseHugePagesEnv overrides the huge-pages decision when it holds a boolean
// (strconv.ParseBool). Empty or unparsable values keep the release check.
// The API reads it for sandbox creates and the template manager for template
// builds (which fixes the memfile page size), so set it on both.
const UseHugePagesEnv = "E2B_USE_HUGE_PAGES"

func (v *Info) HasHugePages() bool {
	if value := strings.TrimSpace(os.Getenv(UseHugePagesEnv)); value != "" {
		if useHugePages, err := strconv.ParseBool(value); err == nil {
			return useHugePages
		}
	}

	if v.lastReleaseVersion.Major() > 1 || (v.lastReleaseVersion.Major() == 1 && v.lastReleaseVersion.Minor() >= 7) {
		return true
	}

	return false
}

func (v *Info) HasFreePageReporting() bool {
	return v.lastReleaseVersion.Major() > 1 || (v.lastReleaseVersion.Major() == 1 && v.lastReleaseVersion.Minor() >= 14)
}

func (v *Info) HasFreePageHinting() bool {
	return v.lastReleaseVersion.Major() > 1 || (v.lastReleaseVersion.Major() == 1 && v.lastReleaseVersion.Minor() >= 14)
}

func (v *Info) HasMemfd() bool {
	return v.lastReleaseVersion.Major() > 1 || (v.lastReleaseVersion.Major() == 1 && v.lastReleaseVersion.Minor() >= 14)
}
