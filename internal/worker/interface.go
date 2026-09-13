// Package worker provides interfaces and implementations for Pod monitoring workers.
//
// This package implements a distributed worker architecture:
// - LogWorker: Consumes log messages from a queue
// - MetricWorker: Collects metrics from assigned pods
package worker

import (
	"context"
)

// Worker defines the core interface for all workers.
type Worker interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Type() string
	ID() string
	IsHealthy() bool
	GetAssignedPods() []string
	GetErrorCount() int
	StartHealthReporting(ctx context.Context) error
}

// LogWorker consumes log messages from a queue.
type LogWorker interface {
	Worker
}

// MetricWorker consumes metrics collection work from a queue.
type MetricWorker interface {
	Worker
}

// EventWorker watches Kubernetes events and stores them for correlation.
type EventWorker interface {
	Worker
}

// PodStatusWorker watches pod status conditions and container states for troubleshooting.
type PodStatusWorker interface {
	Worker
}

// NodeStatusWorker watches node conditions and resource info for cluster-level troubleshooting.
type NodeStatusWorker interface {
	Worker
}

// DeploymentStatusWorker watches deployment rollout status and ReplicaSet history.
type DeploymentStatusWorker interface {
	Worker
}

// HPAStatusWorker watches HPA status to track scaling decisions and capacity constraints.
type HPAStatusWorker interface {
	Worker
}

// EndpointHealthWorker watches endpoint health to detect services with no healthy backends.
type EndpointHealthWorker interface {
	Worker
}

// PVCStatusWorker watches PVC/PV status to detect storage issues.
type PVCStatusWorker interface {
	Worker
}

// ConfigMapWorker watches ConfigMap/Secret metadata to track resource version changes.
type ConfigMapWorker interface {
	Worker
}

// JobStatusWorker watches Job/CronJob status to track completion, failures, and missed schedules.
type JobStatusWorker interface {
	Worker
}

// TraceWorker receives OTLP traces via HTTP and ingests them into the RAG system.
type TraceWorker interface {
	Worker
}
