package orchestrator

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/e2b-dev/infra/packages/api/internal/orchestrator/nodemanager"
	"github.com/e2b-dev/infra/packages/shared/pkg/consts"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

type k8sDiscovery struct {
	client kubernetes.Interface
	// 通过 label 挑选出“可运行 sandbox”的节点
	selector labels.Selector
}

func NewK8sDiscovery(client kubernetes.Interface) NodeDiscovery {
	selector, _ := labels.Parse("node-role.kubernetes.io/sandbox=true")
	return &k8sDiscovery{
		client:   client,
		selector: selector,
	}
}

func (k *k8sDiscovery) ListNodes(ctx context.Context) ([]nodemanager.NomadServiceDiscovery, error) {
	nodeList, err := k.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: k.selector.String(),
	})
	if err != nil {
		return nil, err
	}
	out := make([]nodemanager.NomadServiceDiscovery, 0, len(nodeList.Items))
	for _, node := range nodeList.Items {
		addr := nodeInternalIP(node)
		if addr == "" {
			continue
		}
		out = append(out, nodemanager.NomadServiceDiscovery{
			NomadNodeShortID:    node.Name,
			OrchestratorAddress: fmt.Sprintf("%s:%d", addr, consts.OrchestratorAPIPort),
			IPAddress:           addr,
		})
	}
	return out, nil
}

// 取节点 InternalIP
func nodeInternalIP(node corev1.Node) string {
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			return addr.Address
		}
	}
	return ""
}

