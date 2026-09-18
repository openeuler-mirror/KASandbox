package network

import (
	"context"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

// Apply modes for the per-sandbox egress proxy (SANDBOX_PROXY_APPLY).
const (
	// EgressProxyApplyReady spawns the proxy and installs the redirect rules
	// asynchronously, off the sandbox create path. Boot-time traffic (guest
	// boot, template provisioning, code download) egresses directly without
	// traversing the proxy; startup failures never block or fail creation.
	EgressProxyApplyReady = "ready"
	// EgressProxyApplyImmediate runs proxy spawn + rule installation
	// synchronously inside sandbox creation; any failure fails the create and
	// the cleanup stack rolls everything back.
	EgressProxyApplyImmediate = "immediate"
)

// Restart policies for the per-sandbox egress proxy (SANDBOX_PROXY_RESTART).
const (
	// EgressProxyRestartOnCrash restarts a crashed proxy via the hostservice
	// supervisor backoff. The redirect rules live in the netns and the port is
	// fixed, so a restarted proxy resumes service without reinstalling rules.
	EgressProxyRestartOnCrash = "on-crash"
	// EgressProxyRestartNever keeps the sandbox fail-close after a proxy
	// crash (REDIRECT target unlistened → TCP RST).
	EgressProxyRestartNever = "never"
)

// egressProxyRetryBackoffs are the waits between rule-install attempts (one
// initial try plus one retry per entry).
var egressProxyRetryBackoffs = []time.Duration{200 * time.Millisecond, 500 * time.Millisecond, time.Second}

// EgressProxyApplyTimeout bounds the whole ready-mode startup goroutine
// (spawn + ready check + rule install) so it always terminates.
const EgressProxyApplyTimeout = 10 * time.Second

// validateEgressProxy implements the SANDBOX_PROXY_* fail-fast validation
// rules. The addon/confdir checks run only in per-sandbox mode; a missing CA
// in the confdir is a WARNING (every sandbox treated as mitm_capable=false),
// never a startup blocker — CA material may be provisioned later.
func (c Config) validateEgressProxy() error {
	switch c.SandboxEgressProxyMode {
	case "", EgressProxyModePerSandbox:
	case EgressProxyModeShared:
		return fmt.Errorf("SANDBOX_EGRESS_PROXY_MODE=%q is reserved for the shared proxy form and is not implemented yet", c.SandboxEgressProxyMode)
	default:
		return fmt.Errorf("SANDBOX_EGRESS_PROXY_MODE must be empty, %q or %q, got %q",
			EgressProxyModePerSandbox, EgressProxyModeShared, c.SandboxEgressProxyMode)
	}

	switch c.SandboxProxyApply {
	case EgressProxyApplyReady, EgressProxyApplyImmediate:
	default:
		return fmt.Errorf("SANDBOX_PROXY_APPLY must be %q or %q, got %q",
			EgressProxyApplyReady, EgressProxyApplyImmediate, c.SandboxProxyApply)
	}

	switch c.SandboxProxyRestart {
	case EgressProxyRestartOnCrash, EgressProxyRestartNever:
	default:
		return fmt.Errorf("SANDBOX_PROXY_RESTART must be %q or %q, got %q",
			EgressProxyRestartOnCrash, EgressProxyRestartNever, c.SandboxProxyRestart)
	}

	for _, cidr := range c.SandboxProxyExemptCIDRs {
		if _, err := netip.ParsePrefix(strings.TrimSpace(cidr)); err != nil {
			return fmt.Errorf("SANDBOX_PROXY_EXEMPT_CIDRS entry %q: %w", cidr, err)
		}
	}

	if c.SandboxProxyPoolSize != nil && *c.SandboxProxyPoolSize < 0 {
		return fmt.Errorf("SANDBOX_PROXY_POOL_SIZE must be >= 0, got %d", *c.SandboxProxyPoolSize)
	}

	if c.SandboxProxyReusedPoolSize != nil && *c.SandboxProxyReusedPoolSize < 0 {
		return fmt.Errorf("SANDBOX_PROXY_REUSED_POOL_SIZE must be >= 0, got %d", *c.SandboxProxyReusedPoolSize)
	}

	if c.SandboxProxyUpstream != "" {
		if err := validateProxyUpstream(c.SandboxProxyUpstream); err != nil {
			return fmt.Errorf("SANDBOX_PROXY_UPSTREAM: %w", err)
		}
	}

	if c.SandboxEgressProxyMode != EgressProxyModePerSandbox {
		return nil
	}

	if c.SandboxProxyListenPort == 0 {
		return fmt.Errorf("SANDBOX_PROXY_LISTEN_PORT must be in 1-65535, got %d", c.SandboxProxyListenPort)
	}

	if _, err := os.Stat(c.SandboxProxyAddon); err != nil {
		return fmt.Errorf("SANDBOX_PROXY_ADDON %q: %w", c.SandboxProxyAddon, err)
	}

	if !egressProxyCAPresent(c.SandboxProxyConfDir) {
		logger.L().Warn(context.Background(),
			"SANDBOX_PROXY_CONFDIR has no mitmproxy CA material; per-sandbox proxies run with mitm_capable=false until it is provisioned",
			zap.String("confdir", c.SandboxProxyConfDir),
		)
	}

	return nil
}

// egressProxyCAPresent reports whether the confdir holds CA material in the
// mitmproxy confdir layout (mitmproxy-ca.pem or mitmproxy-ca-cert.pem).
func egressProxyCAPresent(confdir string) bool {
	for _, name := range []string{"mitmproxy-ca.pem", "mitmproxy-ca-cert.pem"} {
		if st, err := os.Stat(filepath.Join(confdir, name)); err == nil && !st.IsDir() {
			return true
		}
	}

	return false
}

// validateProxyUpstream validates an upstream proxy URL of the form
// http://[user:pass@]host:port (scheme http/https, non-empty host, port
// 1-65535).
func validateProxyUpstream(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("must be a valid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https: %q", raw)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("host must be non-empty: %q", raw)
	}
	port := u.Port()
	if port == "" {
		return fmt.Errorf("port is required: %q", raw)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("port must be in 1-65535: %q", raw)
	}

	return nil
}

// egressProxyChain is the dedicated nat chain (inside the slot netns) holding
// the per-sandbox egress proxy redirect rules. nat PREROUTING carries a single
// "-i tap0 -j E2B_EGRESS_PROXY" jump; all exemptions and the catch-all
// REDIRECT live inside the chain. The chain shape exists so pooled slot reuse
// can peel the whole per-sandbox redirect off with one flush + jump delete.
const egressProxyChain = "E2B_EGRESS_PROXY"

// buildEgressProxyRules renders the iptables-restore payload that
// transparently redirects all outbound TCP from the tap interface to the
// per-sandbox proxy listen port, via the dedicated E2B_EGRESS_PROXY chain.
// Exempted destinations RETURN before the catch-all REDIRECT: the
// orchestrator management IP, the tap link-local subnet, and
// SANDBOX_PROXY_EXEMPT_CIDRS. UDP is deliberately untouched.
//
// includeJump controls whether the PREROUTING jump is (re)installed: the jump
// is append-only and iptables-restore does not dedupe it, so re-applies
// (install retries) must omit it. The chain itself is declared and flushed in
// the same payload, so chain rules are rewritten, never duplicated — verified
// idempotent on both the legacy and nf_tables iptables backends.
//
// Unlike the retired DNAT gateway form there is no "gateway self" exemption:
// the proxy lives in this same netns and its own upstream connections leave
// via OUTPUT, never hitting the -i tap0 PREROUTING jump.
func buildEgressProxyRules(tapIf, orchestratorIP string, exemptCIDRs []string, port uint16, includeJump bool) (string, error) {
	var b strings.Builder
	b.WriteString("*nat\n")
	fmt.Fprintf(&b, ":%s - [0:0]\n", egressProxyChain)
	fmt.Fprintf(&b, "-F %s\n", egressProxyChain)
	if includeJump {
		fmt.Fprintf(&b, "-A PREROUTING -i %s -j %s\n", tapIf, egressProxyChain)
	}
	fmt.Fprintf(&b, "-A %s -p tcp -d %s/32 -j RETURN\n", egressProxyChain, orchestratorIP)
	fmt.Fprintf(&b, "-A %s -p tcp -d 169.254.0.0/30 -j RETURN\n", egressProxyChain)
	for _, cidr := range exemptCIDRs {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(cidr))
		if err != nil {
			return "", fmt.Errorf("invalid exempt CIDR %q: %w", cidr, err)
		}
		fmt.Fprintf(&b, "-A %s -p tcp -d %s -j RETURN\n", egressProxyChain, prefix)
	}
	fmt.Fprintf(&b, "-A %s -p tcp -j REDIRECT --to-ports %d\n", egressProxyChain, port)
	b.WriteString("COMMIT\n")

	return b.String(), nil
}

// buildEgressProxyRemoveRules renders the iptables-restore payload that peels
// the per-sandbox redirect off a slot netns before the slot re-enters the
// proxy reuse pool: delete the PREROUTING jump, then flush and delete the
// chain. jumpPresent/chainPresent come from the pre-install state probe so the
// payload never references absent objects (restore is atomic per table and a
// single bad line fails the whole commit).
func buildEgressProxyRemoveRules(tapIf string, jumpPresent, chainPresent bool) string {
	var b strings.Builder
	b.WriteString("*nat\n")
	if jumpPresent {
		fmt.Fprintf(&b, "-D PREROUTING -i %s -j %s\n", tapIf, egressProxyChain)
	}
	if chainPresent {
		fmt.Fprintf(&b, "-F %s\n-X %s\n", egressProxyChain, egressProxyChain)
	}
	b.WriteString("COMMIT\n")

	return b.String()
}

// egressProxyRulesState probes the slot netns (callers run this inside it) for
// the installed redirect state: whether the PREROUTING jump and the chain
// exist. A missing chain (iptables -S exit error) is reported as absent, not
// an error.
func egressProxyRulesState(ctx context.Context, tapIf string) (jumpPresent, chainPresent bool, err error) {
	out, err := exec.CommandContext(ctx, "iptables", "-t", "nat", "-S", "PREROUTING").CombinedOutput()
	if err != nil {
		return false, false, fmt.Errorf("error listing nat PREROUTING rules: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	for line := range strings.Lines(string(out)) {
		if strings.Contains(line, "-i "+tapIf) && strings.Contains(line, "-j "+egressProxyChain) {
			jumpPresent = true
			break
		}
	}

	err = exec.CommandContext(ctx, "iptables", "-t", "nat", "-S", egressProxyChain).Run()
	chainPresent = err == nil

	return jumpPresent, chainPresent, nil
}

// applyEgressProxy installs the egress proxy redirect chain with a single
// iptables-restore --noflush call inside the slot netns. Re-applies (install
// retries) are idempotent: the chain is flushed and rewritten in the same
// payload, and the PREROUTING jump is only appended when absent.
//
// Unlike the pre-chain form, the rules no longer die only with the netns:
// pooled proxy slots outlive their sandbox, so Pool.Return strips the chain
// via removeEgressProxyRules before a slot re-enters the reuse pool.
func (s *Slot) applyEgressProxy(ctx context.Context) error {
	n, err := ns.GetNS(s.NamespacePath())
	if err != nil {
		return fmt.Errorf("failed to get slot network namespace '%s': %w", s.NamespaceID(), err)
	}
	defer n.Close()

	return n.Do(func(_ ns.NetNS) error {
		jumpPresent, _, err := egressProxyRulesState(ctx, s.TapName())
		if err != nil {
			return err
		}

		rules, err := buildEgressProxyRules(s.TapName(), s.config.OrchestratorInSandboxIPAddress, s.config.SandboxProxyExemptCIDRs, s.config.SandboxProxyListenPort, !jumpPresent)
		if err != nil {
			return fmt.Errorf("error building egress proxy rules: %w", err)
		}

		cmd := exec.CommandContext(ctx, "iptables-restore", "--noflush")
		cmd.Stdin = strings.NewReader(rules)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("error applying egress proxy rules via iptables-restore: %w (output: %s)", err, strings.TrimSpace(string(out)))
		}

		return nil
	})
}

// removeSlotEgressProxyRules is the egress-proxy chain removal used by
// Pool.Return; a variable so pool unit tests can stub the netns-dependent
// removal.
var removeSlotEgressProxyRules = func(ctx context.Context, s *Slot) error {
	return s.removeEgressProxyRules(ctx)
}

// removeEgressProxyRules peels the per-sandbox egress proxy redirect (jump +
// chain) off the slot netns. It is idempotent and tolerant of the "already
// clean" state — slots where the rules were never installed or were already
// removed — because it runs on every proxy slot pool return. Any real failure
// makes the caller discard the slot instead of pooling it.
func (s *Slot) removeEgressProxyRules(ctx context.Context) error {
	n, err := ns.GetNS(s.NamespacePath())
	if err != nil {
		return fmt.Errorf("failed to get slot network namespace '%s': %w", s.NamespaceID(), err)
	}
	defer n.Close()

	return n.Do(func(_ ns.NetNS) error {
		jumpPresent, chainPresent, err := egressProxyRulesState(ctx, s.TapName())
		if err != nil {
			return err
		}
		if !jumpPresent && !chainPresent {
			return nil
		}

		cmd := exec.CommandContext(ctx, "iptables-restore", "--noflush")
		cmd.Stdin = strings.NewReader(buildEgressProxyRemoveRules(s.TapName(), jumpPresent, chainPresent))
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("error removing egress proxy rules via iptables-restore: %w (output: %s)", err, strings.TrimSpace(string(out)))
		}

		return nil
	})
}

// InstallEgressProxyRulesWithRetry installs the egress proxy redirect rules
// with bounded backoff retries. It runs after the proxy process is up and
// ready, so there is no "rules installed but proxy not listening" window.
func (s *Slot) InstallEgressProxyRulesWithRetry(ctx context.Context) error {
	var err error
	for attempt := 0; ; attempt++ {
		err = s.applyEgressProxy(ctx)
		if err == nil {
			return nil
		}
		if attempt >= len(egressProxyRetryBackoffs) {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("install egress proxy rules aborted: %w (last error: %v)", ctx.Err(), err)
		case <-time.After(egressProxyRetryBackoffs[attempt]):
		}
	}

	return fmt.Errorf("failed to install egress proxy rules after %d attempts: %w", len(egressProxyRetryBackoffs)+1, err)
}

// installTCPProxy reports whether the host-side tcpProxy REDIRECT rules must
// be installed for this slot. Per-sandbox egress proxy slots skip them: the
// in-netns proxy's upstream connections (vpeer → host veth) would otherwise
// hit the tcpProxy catch-all REDIRECT and be double-proxied. The TCP user
// policy duty moves to the all-protocols slot firewall rules plus the addon.
func (s *Slot) installTCPProxy() bool {
	return !s.egressProxy
}

// userRulesAllProtocols reports whether the slot firewall user allow/deny
// rules must cover all protocols (including TCP): true in external netns
// (CNI) mode where no TCP egress proxy is installed, and for per-sandbox
// egress proxy slots, where the user rules are the only TCP enforcement point
// that still runs (slot-firewall priority -150) before the netns REDIRECT
// (nat PREROUTING priority -100).
func (s *Slot) userRulesAllProtocols() bool {
	return s.ExternalNetNS || s.egressProxy
}

// egressProxyVrtSNATSpec is the netns-side nat POSTROUTING rule that SNATs
// the proxy's upstream connections to the slot HostIP. The in-netns proxy
// sources its upstream connections from the vpeer (vrt network) address,
// which neither the existing SNAT rules (-s 169.254.0.21, cvdTapNetwork) nor
// the host MASQUERADE (-s HostIP/32) cover — without this rule the proxy's
// upstream traffic could not leave the node.
func (s *Slot) egressProxyVrtSNATSpec() []string {
	return []string{"-o", s.VpeerName(), "-s", vrtNetworkCIDR.String(), "-j", "SNAT", "--to", s.HostIPString()}
}
