package network

import (
	"os"
	"strings"
	"testing"

	"github.com/coreos/go-iptables/iptables"
)

// TestDropTokenRulesFromTable 是批量删除的回归测试：iptables-save -c 的规则行
// 以 "[pkts:bytes] " 计数器开头，曾导致 HasPrefix(line, "-A ") 判定恒假、
// 删除静默失效（每次重启/压测泄漏整代规则，表膨胀到 2 万条）。
// 用 mangle 表测试：orchestrator 只写 nat/filter，整表 restore 不会与运行中
// 的槽位创建竞争；计数器前缀格式各表一致。
func TestDropTokenRulesFromTable(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}

	tables, err := iptables.New()
	if err != nil {
		t.Skipf("iptables unavailable: %v", err)
	}

	const (
		table = "mangle"
		chain = "E2B-DROP-TEST"
		token = "veth-t0k99"
	)

	if err := tables.NewChain(table, chain); err != nil {
		t.Fatalf("NewChain: %v", err)
	}

	defer func() {
		if err := tables.ClearAndDeleteChain(table, chain); err != nil {
			t.Errorf("cleanup chain: %v", err)
		}
	}()

	// 一条带 token、一条不带；不带 token 的规则必须原样保留
	if err := tables.Append(table, chain, "-i", token, "-j", "RETURN"); err != nil {
		t.Fatalf("Append token rule: %v", err)
	}

	if err := tables.Append(table, chain, "-j", "RETURN"); err != nil {
		t.Fatalf("Append plain rule: %v", err)
	}

	if err := dropTokenRulesFromTable(table, map[string]struct{}{token: {}}); err != nil {
		t.Fatalf("dropTokenRulesFromTable: %v", err)
	}

	rules, err := tables.List(table, chain)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	var tokenLeft, plainLeft int

	for _, r := range rules {
		if strings.Contains(r, token) {
			tokenLeft++
		}

		if r == "-A "+chain+" -j RETURN" {
			plainLeft++
		}
	}

	if tokenLeft != 0 {
		t.Fatalf("token rule not removed, remaining rules: %v", rules)
	}

	if plainLeft != 1 {
		t.Fatalf("plain rule should be kept exactly once, got %d in %v", plainLeft, rules)
	}
}
