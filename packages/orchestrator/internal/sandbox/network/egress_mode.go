package network

import (
	"context"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

// SandboxConfig.Metadata keys carrying the egress proxy selection and identity
// for one sandbox. They are written by cri-multiplex from CRI annotations
// (same key names); the orchestrator only ever reads the metadata map and
// never parses raw annotation strings.
const (
	// MetadataKeyEgressMode selects the per-sandbox egress routing mode: "per-sandbox"
	// (run a dedicated proxy process in the sandbox netns) or "off".
	MetadataKeyEgressMode = "cri-multiplex.dev/egress-mode"
	// MetadataKeyEgressUpstream overrides the node-default upstream proxy for
	// this sandbox ("http://[user:pass@]host:port"); "off" = direct egress
	// after local interception. Consumed only in per-sandbox mode.
	MetadataKeyEgressUpstream = "cri-multiplex.dev/egress-upstream"
	// MetadataKeySandboxMIS is the sandbox identity (strategy identity /
	// X-AI-UserId data source). Mandatory in per-sandbox mode: an empty value
	// would silently disable the addon's policy module.
	MetadataKeySandboxMIS = "cri-multiplex.dev/sandbox-mis"
	// MetadataKeyEgressProfile is the addon SPOTBOX_PROFILE
	// (only internal is supported; it is also the default).
	MetadataKeyEgressProfile = "cri-multiplex.dev/egress-profile"
	// MetadataKeyEgressMitm gates MITM decryption ("true"/"false", default
	// true); an explicit false derives a per-sandbox no-MITM MITMPROXY_CONFIG.
	MetadataKeyEgressMitm = "cri-multiplex.dev/egress-mitm"
)

// Node-level SANDBOX_EGRESS_PROXY_MODE values.
const (
	// EgressProxyModePerSandbox marks the node as capable of running
	// per-sandbox netns egress proxies.
	EgressProxyModePerSandbox = "per-sandbox"
	// EgressProxyModeShared is reserved for the shared-proxy form; it is
	// rejected as not-implemented at config parse time.
	EgressProxyModeShared = "shared"
)

// EgressMode is the resolved per-sandbox egress routing decision.
type EgressMode string

const (
	// EgressModeOff: the sandbox's traffic egresses directly (current
	// behavior); no proxy process, no redirect rules.
	EgressModeOff EgressMode = "off"
	// EgressModePerSandbox: the sandbox gets a dedicated proxy process in its
	// netns and a catch-all REDIRECT of its TCP egress into that proxy.
	EgressModePerSandbox EgressMode = "per-sandbox"
)

// EgressIdentityFromMetadata extracts the egress proxy keys from a
// SandboxConfig.Metadata map. Returns nil when none are present.
func EgressIdentityFromMetadata(metadata map[string]string) map[string]string {
	var identity map[string]string
	for _, key := range []string{
		MetadataKeyEgressMode,
		MetadataKeyEgressUpstream,
		MetadataKeySandboxMIS,
		MetadataKeyEgressProfile,
		MetadataKeyEgressMitm,
	} {
		if value := metadata[key]; value != "" {
			if identity == nil {
				identity = make(map[string]string, 5)
			}
			identity[key] = value
		}
	}

	return identity
}

// ResolveEgressMode decides the egress routing mode for one sandbox at create
// time. The explicit annotation wins; without it the mode is derived from the
// node capability (SANDBOX_EGRESS_PROXY_MODE=per-sandbox) plus the presence of
// the identity annotation: capable node + non-empty sandbox-mis → per-sandbox,
// otherwise off.
//
// Validation (fail, not silent downgrade, so a sandbox that believes it is
// controlled never silently egresses directly):
//   - invalid egress-mode value            → InvalidArgument
//   - per-sandbox on a CNI (external netns) sandbox → ignored with a WARNING,
//     downgraded to off (this form is native-mode only; never fails, so pod
//     creation is not blocked)
//   - per-sandbox without node capability  → FailedPrecondition
//   - per-sandbox with empty sandbox-mis   → InvalidArgument
func ResolveEgressMode(ctx context.Context, cfg Config, externalNetNS bool, sandboxID string, metadata map[string]string) (EgressMode, error) {
	annotated := metadata[MetadataKeyEgressMode]
	switch annotated {
	case "", string(EgressModePerSandbox), string(EgressModeOff):
	default:
		return "", status.Errorf(codes.InvalidArgument,
			"invalid %s value %q: must be %q or %q",
			MetadataKeyEgressMode, annotated, EgressModePerSandbox, EgressModeOff)
	}

	mode := annotated
	if mode == "" {
		if cfg.SandboxEgressProxyMode == EgressProxyModePerSandbox && metadata[MetadataKeySandboxMIS] != "" {
			mode = string(EgressModePerSandbox)
		} else {
			mode = string(EgressModeOff)
		}
	}

	if mode == string(EgressModeOff) {
		return EgressModeOff, nil
	}

	// From here on the sandbox selected (explicitly or by derivation) per-sandbox.

	if externalNetNS {
		logger.L().Warn(ctx,
			"egress-mode=per-sandbox ignored for CNI (external netns) sandbox; the per-sandbox proxy form is native-mode only, sandbox egresses via the shared-proxy path",
			zap.String("sandbox_id", sandboxID),
		)

		return EgressModeOff, nil
	}

	if cfg.SandboxEgressProxyMode != EgressProxyModePerSandbox {
		return "", status.Errorf(codes.FailedPrecondition,
			"sandbox %s requests egress-mode=per-sandbox but this node does not provide the capability (SANDBOX_EGRESS_PROXY_MODE=%q)",
			sandboxID, cfg.SandboxEgressProxyMode)
	}

	if metadata[MetadataKeySandboxMIS] == "" {
		return "", status.Errorf(codes.InvalidArgument,
			"egress-mode=per-sandbox requires %s to be set: an empty identity would silently disable the proxy policy module",
			MetadataKeySandboxMIS)
	}

	return EgressModePerSandbox, nil
}
