package orchestrator

import (
	"context"
	"fmt"
	"testing"

	"github.com/e2b-dev/infra/packages/shared/pkg/consts"
)

// systemd 模式：节点池来自 E2B_STATIC_ALLOCATIONS，字段映射与 k8s/nomad 发现对齐
func TestStaticNodeDiscoveryListNodes(t *testing.T) {
	t.Setenv("E2B_STATIC_ALLOCATIONS", "node1=10.0.0.1,node2=10.0.0.2")
	t.Setenv("E2B_STATIC_DISCOVERY_HEALTHCHECK", "false")

	d := NewStaticNodeDiscovery(context.Background())
	nodes, err := d.ListNodes(context.Background())
	if err != nil {
		t.Fatalf("ListNodes failed: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d (%+v)", len(nodes), nodes)
	}

	wantAddr := fmt.Sprintf("10.0.0.1:%d", consts.OrchestratorAPIPort)
	if nodes[0].NomadNodeShortID != "node1" || nodes[0].IPAddress != "10.0.0.1" || nodes[0].OrchestratorAddress != wantAddr {
		t.Errorf("unexpected first node: %+v, want OrchestratorAddress %s", nodes[0], wantAddr)
	}
	if nodes[1].NomadNodeShortID != "node2" || nodes[1].IPAddress != "10.0.0.2" {
		t.Errorf("unexpected second node: %+v", nodes[1])
	}
}

// 空清单：返回空节点池而非报错（deploy.sh 对空清单仅告警不阻断）
func TestStaticNodeDiscoveryEmpty(t *testing.T) {
	t.Setenv("E2B_STATIC_ALLOCATIONS", "")
	t.Setenv("E2B_STATIC_ALLOCATIONS_FILE", "")
	t.Setenv("E2B_STATIC_DISCOVERY_HEALTHCHECK", "false")

	d := NewStaticNodeDiscovery(context.Background())
	nodes, err := d.ListNodes(context.Background())
	if err != nil {
		t.Fatalf("ListNodes failed: %v", err)
	}
	if len(nodes) != 0 {
		t.Fatalf("expected empty nodes, got %+v", nodes)
	}
}
