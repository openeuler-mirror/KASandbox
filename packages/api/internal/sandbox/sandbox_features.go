package sandbox

import (
	"strconv"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/e2b-dev/infra/packages/shared/pkg/env"
)

type VersionInfo struct {
	commitHash         string
	lastReleaseVersion semver.Version
}

func stripVersionPrefix(version string) string {
	return strings.TrimPrefix(version, "v")
}

func NewVersionInfo(fcVersion string) (info VersionInfo, err error) {
	// The structure of the fcVersion is last_tag[-prerelease]_commit_hash
	// Example: v1.0.0-release_1234567

	parts := strings.Split(fcVersion, "_")

	versionString := stripVersionPrefix(parts[0])

	version, versionErr := semver.NewVersion(versionString)
	if versionErr != nil {
		return info, versionErr
	}

	info.lastReleaseVersion = *version
	if len(parts) > 1 {
		info.commitHash = parts[1]
	} else {
		info.commitHash = ""
	}

	return info, nil
}

func (v *VersionInfo) Version() semver.Version {
	return v.lastReleaseVersion
}

// useHugePagesEnvVar is the environment variable that can override whether huge
// pages are used when creating sandboxes and templates.
const useHugePagesEnvVar = "E2B_USE_HUGE_PAGES"

func (v *VersionInfo) HasHugePages() bool {
	// If the environment variable is set to a valid boolean value, use its
	// value to decide whether to use huge pages. Otherwise (unset, empty or
	// invalid value), fall back to the firecracker version check below.
	if value := env.GetEnv(useHugePagesEnvVar, ""); value != "" {
		useHugePages, err := strconv.ParseBool(value)
		if err == nil {
			return useHugePages
		}
	}

	if v.lastReleaseVersion.Major() >= 1 && v.lastReleaseVersion.Minor() >= 7 {
		return true
	}

	return false
}
