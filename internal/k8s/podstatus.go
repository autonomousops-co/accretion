package k8s

import (
	"time"

	corev1 "k8s.io/api/core/v1"
)

// ContainerStateInfo captures the state of a single container.
type ContainerStateInfo struct {
	// StartedAt is when the container started (for running containers).
	StartedAt time.Time
	// FinishedAt is when the container finished (for terminated containers).
	FinishedAt time.Time
	// Name is the container name.
	Name string
	// State is one of: waiting, running, terminated.
	State string
	// Reason provides detail for waiting or terminated states (e.g., CrashLoopBackOff, OOMKilled).
	Reason string
	// Message provides additional context for the state.
	Message string
	// ExitCode is the exit code for terminated containers.
	ExitCode int32
	// Signal is the signal that caused termination.
	Signal int32
	// RestartCount is how many times the container has restarted.
	RestartCount int32
}

// PodConditionInfo captures a single pod condition.
type PodConditionInfo struct {
	// LastTransitionTime is when this condition last changed.
	LastTransitionTime time.Time
	// Type is the condition type (PodScheduled, Initialized, ContainersReady, Ready).
	Type string
	// Status is True, False, or Unknown.
	Status string
	// Reason provides a machine-readable reason for the condition.
	Reason string
	// Message provides a human-readable explanation.
	Message string
}

// PodStatusEntry represents the status snapshot of a single pod.
type PodStatusEntry struct {
	// Timestamp is when the status was captured.
	Timestamp time.Time
	// Namespace is the Kubernetes namespace.
	Namespace string
	// PodName is the name of the pod.
	PodName string
	// Phase is the pod phase: Pending, Running, Succeeded, Failed, Unknown.
	Phase string
	// Conditions are the pod conditions.
	Conditions []PodConditionInfo
	// ContainerStatuses are the container statuses.
	ContainerStatuses []ContainerStateInfo
}

// podStatusEntry converts one Pod. It is the single conversion for the List
// path and the informer path (#96), which is what keeps the two byte-identical.
func podStatusEntry(pod *corev1.Pod, now time.Time) PodStatusEntry {
	entry := PodStatusEntry{
		Timestamp: now,
		Namespace: pod.Namespace,
		PodName:   pod.Name,
		Phase:     string(pod.Status.Phase),
	}

	condTimes := make([]time.Time, 0, len(pod.Status.Conditions))
	for _, cond := range pod.Status.Conditions {
		entry.Conditions = append(entry.Conditions, PodConditionInfo{
			Type:               string(cond.Type),
			Status:             string(cond.Status),
			Reason:             cond.Reason,
			Message:            cond.Message,
			LastTransitionTime: cond.LastTransitionTime.Time,
		})
		condTimes = append(condTimes, cond.LastTransitionTime.Time)
	}

	for j := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[j]
		info := ContainerStateInfo{
			Name:         cs.Name,
			RestartCount: cs.RestartCount,
		}

		switch {
		case cs.State.Waiting != nil:
			info.State = "waiting"
			info.Reason = cs.State.Waiting.Reason
			info.Message = cs.State.Waiting.Message
		case cs.State.Running != nil:
			info.State = "running"
			info.StartedAt = cs.State.Running.StartedAt.Time
		case cs.State.Terminated != nil:
			info.State = "terminated"
			info.Reason = cs.State.Terminated.Reason
			info.Message = cs.State.Terminated.Message
			info.ExitCode = cs.State.Terminated.ExitCode
			info.Signal = cs.State.Terminated.Signal
			info.StartedAt = cs.State.Terminated.StartedAt.Time
			info.FinishedAt = cs.State.Terminated.FinishedAt.Time
		}

		entry.ContainerStatuses = append(entry.ContainerStatuses, info)
	}

	entry.Timestamp = deterministicStatusTime(&pod.ObjectMeta, now, condTimes...)
	return entry
}
