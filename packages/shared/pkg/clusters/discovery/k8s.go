package discovery

import (
	"context"
	"fmt"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

// KubernetesDiscovery K8s服务发现实现
type KubernetesDiscovery struct {
	client        kubernetes.Interface
	namespace     string
	labelSelector string
}

// NewKubernetesDiscovery 从集群内配置创建（使用默认selector查询template-manager）
func NewKubernetesDiscovery(namespace string) (ServiceDiscovery, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get in-cluster config: %w", err)
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	// 默认只查询 template-manager（对应 FilterTemplateBuilders）
	return &KubernetesDiscovery{
		client:        client,
		namespace:     namespace,
		labelSelector: "app=template-manager",
	}, nil
}

// NewKubernetesDiscoveryWithClient 使用提供的客户端创建
// labelSelector可选，为空时默认查询template-manager（对应FilterTemplateBuilders）
// 查询所有E2B组件请使用："app in (template-manager,orchestrator)"
func NewKubernetesDiscoveryWithClient(client kubernetes.Interface, namespace string, labelSelector ...string) ServiceDiscovery {
	selector := "app=template-manager" // 默认对应 FilterTemplateBuilders
	if len(labelSelector) > 0 && labelSelector[0] != "" {
		selector = labelSelector[0]
	}

	return &KubernetesDiscovery{
		client:        client,
		namespace:     namespace,
		labelSelector: selector,
	}
}

// ListOrchestratorAndTemplateBuilderAllocations 查询Pods
func (k *KubernetesDiscovery) ListOrchestratorAndTemplateBuilderAllocations(ctx context.Context) ([]Allocation, error) {
	pods, err := k.client.CoreV1().Pods(k.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: k.labelSelector,
		FieldSelector: "status.phase=Running",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods in service discovery: %w", err)
	}

	result := make([]Allocation, 0)
	for _, pod := range pods.Items {
		// 检查Pod是否Ready（类似Nomad的running状态）
		if !isPodReady(&pod) {
			logger.L().Warn(ctx, "Pod not ready, skipping",
				zap.String("pod", pod.Name),
				zap.String("namespace", pod.Namespace),
				zap.String("phase", string(pod.Status.Phase)))
			continue
		}

		// 获取Pod IP
		podIP := pod.Status.PodIP
		if podIP == "" {
			logger.L().Warn(ctx, "Pod has no IP assigned",
				zap.String("pod", pod.Name),
				zap.String("namespace", pod.Namespace))
			continue
		}

		// 获取节点信息（对应Nomad的NodeName）
		nodeName := pod.Spec.NodeName
		if nodeName == "" {
			nodeName = "unscheduled"
		}

		item := Allocation{
			NodeID:       nodeName,
			AllocationID: string(pod.UID),
			AllocationIP: podIP,
		}

		result = append(result, item)
	}

	return result, nil
}

// isPodReady 检查Pod是否处于Ready状态
func isPodReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

