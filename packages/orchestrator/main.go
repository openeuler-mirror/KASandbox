//go:build linux

package main

import (
	"context"
	"os"
	"strings"

	"github.com/launchdarkly/go-sdk-common/v3/ldvalue"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/factories"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/tcpfirewall"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/version"
	"github.com/e2b-dev/infra/packages/shared/pkg/featureflags"
)

var commitSHA string

func main() {
	applyTestFlagOverrides()

	factories.Run(factories.Options{
		Version:       version.Version,
		CommitSHA:     commitSHA,
		EgressFactory: defaultEgressFactory,
	})
}

func applyTestFlagOverrides() {
	if mode := os.Getenv("TESTS_MEMFILE_DIFF_DEDUP_MODE"); mode != "" {
		featureflags.OverrideJSONFlag(featureflags.MemfileDiffDedupFlag, ldvalue.FromJSONMarshal(map[string]any{
			"enabled":    true,
			"bestEffort": mode == "best_effort",
			"directIO":   mode == "direct_io",
		}))
	}
	if os.Getenv("TESTS_DISABLE_MEMFD") == "true" {
		featureflags.OverrideBoolFlag(featureflags.UseMemFdFlag, false)
	}

	// Offline / local overrides when LaunchDarkly is unavailable. use-sync-wp /
	// in-place / defer-memory-export need an FC that accepts use_sync_wp —
	// stock v1.14.4 rejects it, so leave those off unless a patched FC is
	// deployed. Prefetch harvest/consume + pause-wait-* are safe on 6.6.
	overrideBool := func(env string, flag featureflags.BoolFlag) {
		switch strings.ToLower(os.Getenv(env)) {
		case "1", "true", "yes", "on":
			featureflags.OverrideBoolFlag(flag, true)
		case "0", "false", "no", "off":
			featureflags.OverrideBoolFlag(flag, false)
		}
	}
	overrideBool("E2B_FF_USE_SYNC_WP", featureflags.UseSyncWPFlag)
	overrideBool("E2B_FF_IN_PLACE_CHECKPOINT", featureflags.InPlaceCheckpointFlag)
	overrideBool("E2B_FF_DEFER_MEMORY_EXPORT", featureflags.DeferMemoryExportFlag)
	overrideBool("E2B_FF_COLLAPSE_ENVD_HEAP", featureflags.CollapseEnvdHeapFlag)
	overrideBool("E2B_FF_PAUSE_RESUME_PREFETCH_HARVEST", featureflags.PauseResumePrefetchHarvestFlag)
	overrideBool("E2B_FF_PAUSE_RESUME_PREFETCH_CONSUME", featureflags.PauseResumePrefetchConsumeFlag)
	overrideBool("E2B_FF_PAUSE_WAIT_UPLOAD", featureflags.PauseWaitUploadFlag)
	overrideBool("E2B_FF_PAUSE_WAIT_PREFETCH_READY", featureflags.PauseWaitPrefetchReadyFlag)
	if src := os.Getenv("E2B_FF_RESUME_PREFETCH_SOURCE"); src != "" {
		featureflags.OverrideStringFlag(featureflags.ResumePrefetchSourceFlag, src)
	}
}

func defaultEgressFactory(_ context.Context, deps *factories.Deps) (*factories.EgressSetup, error) {
	fw := tcpfirewall.New(
		deps.Logger,
		deps.Config.NetworkConfig,
		deps.Sandboxes,
		deps.MeterProvider,
		deps.FeatureFlags,
	)

	return &factories.EgressSetup{
		Proxy: fw,
		Start: fw.Start,
		Close: fw.Close,
	}, nil
}
