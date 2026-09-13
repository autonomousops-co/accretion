package k8s

import (
	"time"

	corev1 "k8s.io/api/core/v1"
)

// NodeConditionInfo captures a single node condition.
type NodeConditionInfo struct {
	// LastTransitionTime is when this condition last changed.
	LastTransitionTime time.Time
	// Type is the condition type (Ready, DiskPressure, MemoryPressure, PIDPressure, NetworkUnavailable).
	Type string
	// Status is True, False, or Unknown.
	Status string
	// Reason provides a machine-readable reason for the condition.
	Reason string
	// Message provides a human-readable explanation.
	Message string
}

// NodeResourceInfo captures allocatable vs capacity for a resource.
type NodeResourceInfo struct {
	// Allocatable is the amount of the resource available for pods.
	Allocatable string
	// Capacity is the total amount of the resource on the node.
	Capacity string
}

// NodeInfoDetails captures node system information.
type NodeInfoDetails struct {
	// KubeletVersion is the version of kubelet running on the node.
	KubeletVersion string
	// OSImage is the OS image reported by the node.
	OSImage string
	// KernelVersion is the kernel version reported by the node.
	KernelVersion string
	// ContainerRuntime is the container runtime version (e.g., containerd://1.6.0).
	ContainerRuntime string
}

// NodeStatusEntry represents the status snapshot of a single node.
type NodeStatusEntry struct {
	Timestamp        time.Time
	Info             NodeInfoDetails
	CPU              NodeResourceInfo
	Memory           NodeResourceInfo
	EphemeralStorage NodeResourceInfo
	Pods             NodeResourceInfo
	NodeName         string
	Conditions       []NodeConditionInfo
}

// nodeStatusEntry converts one Node; shared by the List and informer paths (#96).
func nodeStatusEntry(node *corev1.Node, now time.Time) NodeStatusEntry {
	entry := NodeStatusEntry{
		Timestamp: now,
		NodeName:  node.Name,
		Info: NodeInfoDetails{
			KubeletVersion:   node.Status.NodeInfo.KubeletVersion,
			OSImage:          node.Status.NodeInfo.OSImage,
			KernelVersion:    node.Status.NodeInfo.KernelVersion,
			ContainerRuntime: node.Status.NodeInfo.ContainerRuntimeVersion,
		},
	}

	// Capture conditions. LastTransitionTime (not the churning LastHeartbeatTime,
	// which is deliberately not hashed) is the deterministic timestamp candidate.
	condTimes := make([]time.Time, 0, len(node.Status.Conditions))
	for _, cond := range node.Status.Conditions {
		entry.Conditions = append(entry.Conditions, NodeConditionInfo{
			Type:               string(cond.Type),
			Status:             string(cond.Status),
			Reason:             cond.Reason,
			Message:            cond.Message,
			LastTransitionTime: cond.LastTransitionTime.Time,
		})
		condTimes = append(condTimes, cond.LastTransitionTime.Time)
	}

	// Capture allocatable vs capacity.
	entry.CPU = NodeResourceInfo{
		Allocatable: node.Status.Allocatable.Cpu().String(),
		Capacity:    node.Status.Capacity.Cpu().String(),
	}
	entry.Memory = NodeResourceInfo{
		Allocatable: node.Status.Allocatable.Memory().String(),
		Capacity:    node.Status.Capacity.Memory().String(),
	}
	if es, ok := node.Status.Allocatable["ephemeral-storage"]; ok {
		esCap := node.Status.Capacity["ephemeral-storage"]
		entry.EphemeralStorage = NodeResourceInfo{
			Allocatable: es.String(),
			Capacity:    esCap.String(),
		}
	}
	if pods, ok := node.Status.Allocatable["pods"]; ok {
		podsCap := node.Status.Capacity["pods"]
		entry.Pods = NodeResourceInfo{
			Allocatable: pods.String(),
			Capacity:    podsCap.String(),
		}
	}

	entry.Timestamp = deterministicStatusTime(&node.ObjectMeta, now, condTimes...)
	return entry
}
