package k8s

import (
	"fmt"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
)

// HPAConditionInfo captures a single HPA condition.
type HPAConditionInfo struct {
	// LastTransitionTime is when this condition last changed.
	LastTransitionTime time.Time
	// Type is the condition type (ScalingActive, AbleToScale, ScalingLimited).
	Type string
	// Status is True, False, or Unknown.
	Status string
	// Reason provides a machine-readable reason for the condition.
	Reason string
	// Message provides a human-readable explanation.
	Message string
}

// HPAMetricStatus captures current vs target for a single metric.
type HPAMetricStatus struct {
	// Type is the metric source type (Resource, Pods, Object, External).
	Type string
	// Name is the metric name (e.g., cpu, memory, or a custom metric name).
	Name string
	// CurrentValue is the current metric value as a string.
	CurrentValue string
	// TargetValue is the target metric value as a string.
	TargetValue string
}

// HPAStatusEntry represents the status snapshot of a single HPA.
type HPAStatusEntry struct {
	// Timestamp is when the status was captured.
	Timestamp time.Time
	// LastScaleTime is when the HPA last scaled the target.
	LastScaleTime *time.Time
	// Namespace is the Kubernetes namespace.
	Namespace string
	// HPAName is the name of the HorizontalPodAutoscaler.
	HPAName string
	// Conditions are the HPA conditions.
	Conditions []HPAConditionInfo
	// Metrics are the current metric statuses.
	Metrics []HPAMetricStatus
	// CurrentReplicas is the current number of replicas.
	CurrentReplicas int32
	// DesiredReplicas is the desired number of replicas.
	DesiredReplicas int32
	// MinReplicas is the minimum number of replicas.
	MinReplicas int32
	// MaxReplicas is the maximum number of replicas.
	MaxReplicas int32
}

// buildHPAEntry converts a single HPA object into an HPAStatusEntry.
func buildHPAEntry(hpa *autoscalingv2.HorizontalPodAutoscaler, now time.Time) HPAStatusEntry {
	var minReplicas int32 = 1
	if hpa.Spec.MinReplicas != nil {
		minReplicas = *hpa.Spec.MinReplicas
	}

	entry := HPAStatusEntry{
		Timestamp:       now,
		Namespace:       hpa.Namespace,
		HPAName:         hpa.Name,
		CurrentReplicas: hpa.Status.CurrentReplicas,
		DesiredReplicas: hpa.Status.DesiredReplicas,
		MinReplicas:     minReplicas,
		MaxReplicas:     hpa.Spec.MaxReplicas,
	}

	if hpa.Status.LastScaleTime != nil {
		t := hpa.Status.LastScaleTime.Time
		entry.LastScaleTime = &t
	}

	candidates := make([]time.Time, 0, len(hpa.Status.Conditions)+1)
	if entry.LastScaleTime != nil {
		candidates = append(candidates, *entry.LastScaleTime)
	}
	for _, cond := range hpa.Status.Conditions {
		entry.Conditions = append(entry.Conditions, HPAConditionInfo{
			Type:               string(cond.Type),
			Status:             string(cond.Status),
			Reason:             cond.Reason,
			Message:            cond.Message,
			LastTransitionTime: cond.LastTransitionTime.Time,
		})
		candidates = append(candidates, cond.LastTransitionTime.Time)
	}

	for _, metric := range hpa.Status.CurrentMetrics {
		ms := extractCurrentMetric(metric)
		ms.TargetValue = findTargetValue(hpa.Spec.Metrics, ms.Name, metric.Type)
		entry.Metrics = append(entry.Metrics, ms)
	}

	entry.Timestamp = deterministicStatusTime(&hpa.ObjectMeta, now, candidates...)
	return entry
}

// extractCurrentMetric extracts the name and current value from a metric status.
func extractCurrentMetric(metric autoscalingv2.MetricStatus) HPAMetricStatus {
	ms := HPAMetricStatus{Type: string(metric.Type)}

	switch metric.Type {
	case autoscalingv2.ResourceMetricSourceType:
		if metric.Resource != nil {
			ms.Name = string(metric.Resource.Name)
			if metric.Resource.Current.AverageUtilization != nil {
				ms.CurrentValue = fmt.Sprintf("%d%%", *metric.Resource.Current.AverageUtilization)
			} else if metric.Resource.Current.AverageValue != nil {
				ms.CurrentValue = metric.Resource.Current.AverageValue.String()
			}
		}
	case autoscalingv2.PodsMetricSourceType:
		if metric.Pods != nil {
			ms.Name = metric.Pods.Metric.Name
			ms.CurrentValue = metric.Pods.Current.AverageValue.String()
		}
	case autoscalingv2.ObjectMetricSourceType:
		if metric.Object != nil {
			ms.Name = metric.Object.Metric.Name
			ms.CurrentValue = metric.Object.Current.Value.String()
		}
	case autoscalingv2.ExternalMetricSourceType:
		if metric.External != nil {
			ms.Name = metric.External.Metric.Name
			if metric.External.Current.AverageValue != nil {
				ms.CurrentValue = metric.External.Current.AverageValue.String()
			} else {
				ms.CurrentValue = metric.External.Current.Value.String()
			}
		}
	}

	return ms
}

// findTargetValue finds the target value for a named metric from the HPA spec.
func findTargetValue(
	specs []autoscalingv2.MetricSpec,
	metricName string,
	metricType autoscalingv2.MetricSourceType,
) string {
	for _, spec := range specs {
		if spec.Type != metricType {
			continue
		}
		if v := extractSpecTarget(spec, metricName); v != "" {
			return v
		}
	}
	return ""
}

// extractSpecTarget extracts the target value from a single metric spec
// if the name matches.
func extractSpecTarget(spec autoscalingv2.MetricSpec, name string) string {
	switch spec.Type {
	case autoscalingv2.ResourceMetricSourceType:
		return resourceTarget(spec.Resource, name)
	case autoscalingv2.PodsMetricSourceType:
		return podsTarget(spec.Pods, name)
	case autoscalingv2.ObjectMetricSourceType:
		return objectTarget(spec.Object, name)
	case autoscalingv2.ExternalMetricSourceType:
		return externalTarget(spec.External, name)
	}
	return ""
}

func resourceTarget(r *autoscalingv2.ResourceMetricSource, name string) string {
	if r == nil || string(r.Name) != name {
		return ""
	}
	if r.Target.AverageUtilization != nil {
		return fmt.Sprintf("%d%%", *r.Target.AverageUtilization)
	}
	if r.Target.AverageValue != nil {
		return r.Target.AverageValue.String()
	}
	return ""
}

func podsTarget(p *autoscalingv2.PodsMetricSource, name string) string {
	if p == nil || p.Metric.Name != name || p.Target.AverageValue == nil {
		return ""
	}
	return p.Target.AverageValue.String()
}

func objectTarget(o *autoscalingv2.ObjectMetricSource, name string) string {
	if o == nil || o.Metric.Name != name || o.Target.Value == nil {
		return ""
	}
	return o.Target.Value.String()
}

func externalTarget(e *autoscalingv2.ExternalMetricSource, name string) string {
	if e == nil || e.Metric.Name != name {
		return ""
	}
	if e.Target.AverageValue != nil {
		return e.Target.AverageValue.String()
	}
	if e.Target.Value != nil {
		return e.Target.Value.String()
	}
	return ""
}
