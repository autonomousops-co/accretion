package k8s

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// PVCConditionInfo captures a single PVC condition.
type PVCConditionInfo struct {
	// LastTransitionTime is when this condition last changed.
	LastTransitionTime time.Time
	// Type is the condition type (e.g., Resizing, FileSystemResizePending).
	Type string
	// Status is True, False, or Unknown.
	Status string
	// Reason provides a machine-readable reason for the condition.
	Reason string
	// Message provides a human-readable explanation.
	Message string
}

// PVCStatusEntry represents the status snapshot of a single PersistentVolumeClaim.
type PVCStatusEntry struct {
	// Timestamp is when the status was captured.
	Timestamp time.Time
	// Namespace is the Kubernetes namespace.
	Namespace string
	// PVCName is the name of the PersistentVolumeClaim.
	PVCName string
	// Phase is the current phase (Pending, Bound, Lost).
	Phase string
	// VolumeName is the name of the bound PersistentVolume.
	VolumeName string
	// StorageClass is the storage class name.
	StorageClass string
	// AccessModes are the requested access modes.
	AccessModes []string
	// Capacity is the actual storage capacity.
	Capacity string
	// Conditions are the PVC conditions.
	Conditions []PVCConditionInfo
}

// PVStatusEntry represents the status snapshot of a single PersistentVolume.
type PVStatusEntry struct {
	// Timestamp is when the status was captured.
	Timestamp time.Time
	// PVName is the name of the PersistentVolume.
	PVName string
	// Phase is the current phase (Available, Bound, Released, Failed).
	Phase string
	// ReclaimPolicy is the reclaim policy (Retain, Delete, Recycle).
	ReclaimPolicy string
	// StorageClass is the storage class name.
	StorageClass string
	// Capacity is the storage capacity.
	Capacity string
	// ClaimRef is the reference to the bound PVC (namespace/name).
	ClaimRef string
	// Reason is the reason for the current phase (if any).
	Reason string
	// Message is a human-readable message about the current phase.
	Message string
}

// pvcStatusEntry converts one PVC; shared by the List and informer paths (#96).
func pvcStatusEntry(pvc *corev1.PersistentVolumeClaim, now time.Time) PVCStatusEntry {
	entry := PVCStatusEntry{
		Timestamp: now,
		Namespace: pvc.Namespace,
		PVCName:   pvc.Name,
		Phase:     string(pvc.Status.Phase),
	}

	if pvc.Spec.VolumeName != "" {
		entry.VolumeName = pvc.Spec.VolumeName
	}

	if pvc.Spec.StorageClassName != nil {
		entry.StorageClass = *pvc.Spec.StorageClassName
	}

	for _, mode := range pvc.Spec.AccessModes {
		entry.AccessModes = append(entry.AccessModes, string(mode))
	}

	if capacity, ok := pvc.Status.Capacity["storage"]; ok {
		entry.Capacity = capacity.String()
	}

	condTimes := make([]time.Time, 0, len(pvc.Status.Conditions))
	for _, cond := range pvc.Status.Conditions {
		entry.Conditions = append(entry.Conditions, PVCConditionInfo{
			Type:               string(cond.Type),
			Status:             string(cond.Status),
			Reason:             cond.Reason,
			Message:            cond.Message,
			LastTransitionTime: cond.LastTransitionTime.Time,
		})
		condTimes = append(condTimes, cond.LastTransitionTime.Time)
	}

	// Bound PVCs usually carry no conditions; fall back to creation time.
	entry.Timestamp = deterministicStatusTime(&pvc.ObjectMeta, now, condTimes...)
	return entry
}

// pvStatusEntry converts one PV; shared by the List and informer paths (#96).
func pvStatusEntry(pv *corev1.PersistentVolume, now time.Time) PVStatusEntry {
	entry := PVStatusEntry{
		Timestamp:     now,
		PVName:        pv.Name,
		Phase:         string(pv.Status.Phase),
		ReclaimPolicy: string(pv.Spec.PersistentVolumeReclaimPolicy),
		StorageClass:  pv.Spec.StorageClassName,
	}

	if capacity, ok := pv.Spec.Capacity["storage"]; ok {
		entry.Capacity = capacity.String()
	}

	if pv.Spec.ClaimRef != nil {
		entry.ClaimRef = fmt.Sprintf("%s/%s", pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name)
	}

	if pv.Status.Reason != "" {
		entry.Reason = pv.Status.Reason
	}
	if pv.Status.Message != "" {
		entry.Message = pv.Status.Message
	}

	// PVs have no conditions; creation time is the stable, deterministic stamp.
	entry.Timestamp = deterministicStatusTime(&pv.ObjectMeta, now)
	return entry
}
