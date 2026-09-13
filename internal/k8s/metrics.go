package k8s

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PodMetrics represents metrics for a pod
type PodMetrics struct {
	Timestamp  time.Time
	Namespace  string
	PodName    string
	Containers []ContainerMetrics
}

// ContainerMetrics represents metrics for a container
type ContainerMetrics struct {
	Name   string
	CPU    string
	Memory string
}

// GetPodMetrics retrieves metrics for pods in a namespace
func (c *Client) GetPodMetrics(ctx context.Context, namespace string) ([]PodMetrics, error) {
	metrics, err := c.metricsClientset.MetricsV1beta1().PodMetricses(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get pod metrics: %w", err)
	}

	podMetrics := make([]PodMetrics, 0, len(metrics.Items))
	for i := range metrics.Items {
		metric := &metrics.Items[i]
		pm := PodMetrics{
			Namespace: metric.Namespace,
			PodName:   metric.Name,
			Timestamp: metric.Timestamp.Time,
		}

		for _, container := range metric.Containers {
			cm := ContainerMetrics{
				Name: container.Name,
			}

			if cpu := container.Usage[corev1.ResourceCPU]; !cpu.IsZero() {
				cm.CPU = cpu.String()
			}

			if memory := container.Usage[corev1.ResourceMemory]; !memory.IsZero() {
				cm.Memory = memory.String()
			}

			pm.Containers = append(pm.Containers, cm)
		}

		podMetrics = append(podMetrics, pm)
	}

	return podMetrics, nil
}
