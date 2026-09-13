package k8s

import (
	"time"

	appsv1 "k8s.io/api/apps/v1"
)

// DeploymentConditionInfo captures a single deployment condition.
type DeploymentConditionInfo struct {
	// LastTransitionTime is when this condition last changed.
	LastTransitionTime time.Time
	// LastUpdateTime is when this condition was last updated.
	LastUpdateTime time.Time
	// Type is the condition type (Available, Progressing, ReplicaFailure).
	Type string
	// Status is True, False, or Unknown.
	Status string
	// Reason provides a machine-readable reason for the condition.
	Reason string
	// Message provides a human-readable explanation.
	Message string
}

// DeploymentReplicaCounts captures the replica counts for a deployment.
type DeploymentReplicaCounts struct {
	// Desired is the desired number of replicas.
	Desired int32
	// Ready is the number of ready replicas.
	Ready int32
	// Available is the number of available replicas.
	Available int32
	// Unavailable is the number of unavailable replicas.
	Unavailable int32
	// Updated is the number of updated replicas.
	Updated int32
}

// kindDeployment is the OwnerReference kind that marks a ReplicaSet as owned by
// a Deployment; both the List path and the informer path key on it.
const kindDeployment = "Deployment"

// ReplicaSetInfo captures ownership and revision information for a ReplicaSet.
type ReplicaSetInfo struct {
	// Name is the name of the ReplicaSet.
	Name string
	// Revision is the deployment revision annotation value.
	Revision string
	// Replicas is the current number of replicas.
	Replicas int32
	// ReadyReplicas is the number of ready replicas.
	ReadyReplicas int32
}

// DeploymentStatusEntry represents the status snapshot of a single deployment.
type DeploymentStatusEntry struct {
	Timestamp          time.Time
	Namespace          string
	DeploymentName     string
	ChangeCause        string
	Conditions         []DeploymentConditionInfo
	ReplicaSets        []ReplicaSetInfo
	ObservedGeneration int64
	Replicas           DeploymentReplicaCounts
}

// replicaSetInfo converts one owned ReplicaSet.
func replicaSetInfo(rs *appsv1.ReplicaSet) ReplicaSetInfo {
	return ReplicaSetInfo{
		Name:          rs.Name,
		Revision:      rs.Annotations["deployment.kubernetes.io/revision"],
		Replicas:      rs.Status.Replicas,
		ReadyReplicas: rs.Status.ReadyReplicas,
	}
}

// deploymentStatusEntry converts one Deployment. sets are its owned
// ReplicaSets in name order, or nil when it has none -- the informer path
// (#96) must hand over exactly that, because the slice is marshaled into the
// record and nil and empty do not serialize the same way.
func deploymentStatusEntry(deploy *appsv1.Deployment, sets []ReplicaSetInfo, now time.Time) DeploymentStatusEntry {
	var desired int32 = 1
	if deploy.Spec.Replicas != nil {
		desired = *deploy.Spec.Replicas
	}

	entry := DeploymentStatusEntry{
		Timestamp:      now,
		Namespace:      deploy.Namespace,
		DeploymentName: deploy.Name,
		Replicas: DeploymentReplicaCounts{
			Desired:     desired,
			Ready:       deploy.Status.ReadyReplicas,
			Available:   deploy.Status.AvailableReplicas,
			Unavailable: deploy.Status.UnavailableReplicas,
			Updated:     deploy.Status.UpdatedReplicas,
		},
		ObservedGeneration: deploy.Status.ObservedGeneration,
		ChangeCause:        deploy.Annotations["kubernetes.io/change-cause"],
	}

	// Both transition and update times feed the hash (via conditionsJSON), so
	// both are timestamp candidates to keep same-hash => same-timestamp.
	condTimes := make([]time.Time, 0, len(deploy.Status.Conditions)*2)
	for _, cond := range deploy.Status.Conditions {
		entry.Conditions = append(entry.Conditions, DeploymentConditionInfo{
			Type:               string(cond.Type),
			Status:             string(cond.Status),
			Reason:             cond.Reason,
			Message:            cond.Message,
			LastTransitionTime: cond.LastTransitionTime.Time,
			LastUpdateTime:     cond.LastUpdateTime.Time,
		})
		condTimes = append(condTimes, cond.LastTransitionTime.Time, cond.LastUpdateTime.Time)
	}

	entry.ReplicaSets = sets

	entry.Timestamp = deterministicStatusTime(&deploy.ObjectMeta, now, condTimes...)
	return entry
}
