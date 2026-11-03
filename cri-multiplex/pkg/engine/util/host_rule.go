package util

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/vishvananda/netns"
)

type HostResourceOps struct {
	ListIPTables     func(context.Context) ([]byte, error)
	DeleteIPTables   func(context.Context, IPTablesOwnerRule) error
	ListNFTables     func(context.Context) ([]byte, error)
	DeleteNFTables   func(context.Context, NFTablesOwnerRule) error
	DeleteNamedNetNS func(string) error
}

type IPTablesOwnerRule struct {
	Table   string
	Chain   string
	Spec    []string
	Comment string
}

type NFTablesOwnerRule struct {
	Family  string
	Table   string
	Chain   string
	Handle  string
	Comment string
}

func DefaultHostResourceOps() HostResourceOps {
	return HostResourceOps{
		ListIPTables: func(ctx context.Context) ([]byte, error) {
			out, err := exec.CommandContext(ctx, "iptables-save").CombinedOutput()
			if err != nil {
				return nil, fmt.Errorf("iptables-save: %w: %s", err, strings.TrimSpace(string(out)))
			}
			return out, nil
		},
		DeleteIPTables: func(ctx context.Context, rule IPTablesOwnerRule) error {
			args := []string{"-t", rule.Table, "-D", rule.Chain}
			args = append(args, rule.Spec...)
			out, err := exec.CommandContext(ctx, "iptables", args...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("iptables %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
			}
			return nil
		},
		ListNFTables: func(ctx context.Context) ([]byte, error) {
			out, err := exec.CommandContext(ctx, "nft", "-a", "list", "ruleset").CombinedOutput()
			if err != nil {
				return nil, fmt.Errorf("nft list ruleset: %w: %s", err, strings.TrimSpace(string(out)))
			}
			return out, nil
		},
		DeleteNFTables: func(ctx context.Context, rule NFTablesOwnerRule) error {
			args := []string{"delete", "rule", rule.Family, rule.Table, rule.Chain, "handle", rule.Handle}
			out, err := exec.CommandContext(ctx, "nft", args...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("nft %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
			}
			return nil
		},
		DeleteNamedNetNS: netns.DeleteNamed,
	}
}

func ParseIPTablesOwnerRules(data []byte) []IPTablesOwnerRule {
	var rules []IPTablesOwnerRule
	table := ""
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "*") {
			table = strings.TrimPrefix(line, "*")
			continue
		}
		if line == "COMMIT" {
			table = ""
			continue
		}
		if table == "" || !strings.HasPrefix(line, "-A ") || !strings.Contains(line, "cri-multiplex:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		comment := ""
		for i := 2; i+1 < len(fields); i++ {
			if fields[i] == "--comment" {
				comment = strings.Trim(fields[i+1], `"`)
				break
			}
		}
		if !strings.HasPrefix(comment, "cri-multiplex:") {
			continue
		}
		spec := append([]string(nil), fields[2:]...)
		for i := range spec {
			spec[i] = strings.Trim(spec[i], `"`)
		}
		rules = append(rules, IPTablesOwnerRule{Table: table, Chain: fields[1], Spec: spec, Comment: comment})
	}
	return rules
}

var (
	nftTablePattern   = regexp.MustCompile(`^table\s+(\S+)\s+(\S+)\s+\{`)
	nftChainPattern   = regexp.MustCompile(`^chain\s+(\S+)\s+\{`)
	nftCommentPattern = regexp.MustCompile(`comment\s+"(cri-multiplex:[^"]+)"`)
	nftHandlePattern  = regexp.MustCompile(`#\s*handle\s+(\d+)`)
)

func ParseNFTablesOwnerRules(data []byte) []NFTablesOwnerRule {
	var rules []NFTablesOwnerRule
	family, table, chain := "", "", ""
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if match := nftTablePattern.FindStringSubmatch(line); len(match) == 3 {
			family, table, chain = match[1], match[2], ""
			continue
		}
		if match := nftChainPattern.FindStringSubmatch(line); len(match) == 2 {
			chain = match[1]
			continue
		}
		commentMatch := nftCommentPattern.FindStringSubmatch(line)
		handleMatch := nftHandlePattern.FindStringSubmatch(line)
		if family != "" && table != "" && chain != "" && len(commentMatch) == 2 && len(handleMatch) == 2 {
			rules = append(rules, NFTablesOwnerRule{
				Family: family, Table: table, Chain: chain,
				Handle: handleMatch[1], Comment: commentMatch[1],
			})
		}
	}
	return rules
}
