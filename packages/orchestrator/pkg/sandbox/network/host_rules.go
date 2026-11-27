//go:build linux

package network

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// hostRuleTables are the host tables holding per-slot rules: the FORWARD
// accepts (filter) and the MASQUERADE and REDIRECT rules, the egress proxy's
// included (nat). The DSCP mangle rule lives inside the slot namespace.
var hostRuleTables = []string{"filter", "nat"}

// DeleteSlotsHostRules removes the host iptables rules of all the slots with
// one iptables-restore per table instead of one iptables -D per rule. Every
// host rule a slot owns names its veth or its host CIDR, so the rules are
// picked from iptables-save output by exact field match and deleted in their
// saved form. --noflush applies only these deletes, atomically, and leaves
// alone whatever kube-proxy or docker change in the meantime. A rule that
// disappears between the save and the restore fails that table's restore;
// the caller then deletes the rules per slot.
func DeleteSlotsHostRules(slots []*Slot) error {
	tokens := make(map[string]struct{}, 2*len(slots))
	for _, s := range slots {
		tokens[s.VethName()] = struct{}{}
		if cidr := s.HostCIDR(); cidr != "" {
			tokens[cidr] = struct{}{}
		}
	}
	if len(tokens) == 0 {
		return nil
	}

	var errs []error
	for _, table := range hostRuleTables {
		if err := deleteHostRulesWithTokens(table, tokens); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func deleteHostRulesWithTokens(table string, tokens map[string]struct{}) error {
	dump, err := exec.Command("iptables-save", "-t", table).Output()
	if err != nil {
		return fmt.Errorf("iptables-save -t %s: %w", table, err)
	}

	script := hostRulesDeleteScript(table, string(dump), tokens)
	if script == "" {
		return nil
	}

	cmd := exec.Command("iptables-restore", "--noflush", "-w", "60")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("iptables-restore --noflush (%s): %w: %s", table, err, bytes.TrimSpace(out))
	}

	return nil
}

// hostRulesDeleteScript turns the rules of an iptables-save dump that carry
// one of the tokens as a whole field into an iptables-restore script deleting
// them, or returns "" when there is none.
func hostRulesDeleteScript(table, dump string, tokens map[string]struct{}) string {
	var b strings.Builder
	for line := range strings.SplitSeq(dump, "\n") {
		rule, ok := strings.CutPrefix(line, "-A ")
		if !ok {
			continue
		}
		for _, field := range strings.Fields(rule) {
			if _, ok := tokens[field]; ok {
				b.WriteString("-D " + rule + "\n")

				break
			}
		}
	}
	if b.Len() == 0 {
		return ""
	}

	return "*" + table + "\n" + b.String() + "COMMIT\n"
}
