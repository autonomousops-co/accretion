// Package queue provides interfaces for distributed work processing.
package queue

import (
	"context"
	"time"
)

// Work represents a unit of work to be processed.
type Work interface {
	GetWorkType() string
	GetLogFields() map[string]interface{}
}

// BaseWork contains common fields for all work types.
type BaseWork struct {
	Timestamp time.Time `json:"timestamp"`
	PodUID    string    `json:"pod_uid"`
	PodName   string    `json:"pod_name"`
	Namespace string    `json:"namespace"`
	NodeName  string    `json:"node_name"`
}

// LogCollectionWork represents log collection work for a pod.
type LogCollectionWork struct {
	BaseWork   `json:",inline"`
	Containers []string `json:"containers"`
}

// MetricsCollectionWork represents metrics collection work for a pod.
type MetricsCollectionWork struct {
	BaseWork `json:",inline"`
}

// WorkerHealth represents worker health and status.
type WorkerHealth struct {
	LastSeen     time.Time `json:"last_seen"`
	WorkerID     string    `json:"worker_id"`
	Type         string    `json:"type"`
	AssignedPods []string  `json:"assigned_pods"`
	ErrorCount   int       `json:"error_count"`
	Healthy      bool      `json:"healthy"`
}

// PodAssignment tracks which worker is handling a pod.
type PodAssignment struct {
	AssignedAt time.Time `json:"assigned_at"`
	LastSeen   time.Time `json:"last_seen"`
	PodKey     string    `json:"pod_key"`
	WorkerID   string    `json:"worker_id"`
	WorkerType string    `json:"worker_type"`
}

// GetWorkType returns the work type for LogCollectionWork.
func (w *LogCollectionWork) GetWorkType() string {
	return "log_collection"
}

// GetWorkType returns the work type for MetricsCollectionWork.
func (w *MetricsCollectionWork) GetWorkType() string {
	return "metrics_collection"
}

// Publisher sends work to specific workers based on load balancing.
type Publisher interface {
	PublishLogWorkToWorker(ctx context.Context, work *LogCollectionWork, workerID string) error
	PublishMetricsWorkToWorker(ctx context.Context, work *MetricsCollectionWork, workerID string) error
	PublishToFallbackQueue(ctx context.Context, workType string, work interface{}) error
	Close() error
}

// Consumer processes work from dedicated worker queues.
type Consumer interface {
	ConsumeLogWorkForWorker(ctx context.Context, workerID string, handler func(*LogCollectionWork) error) error
	ConsumeMetricsWorkForWorker(ctx context.Context, workerID string, handler func(*MetricsCollectionWork) error) error
	Close() error
}

// HealthStorage manages worker health and pod assignments.
type HealthStorage interface {
	StoreWorkerHealth(ctx context.Context, health *WorkerHealth) error
	GetWorkerHealth(ctx context.Context, workerID string) (*WorkerHealth, error)
	GetAllWorkerHealth(ctx context.Context) (map[string]*WorkerHealth, error)
	DeleteWorkerHealth(ctx context.Context, workerID string) error

	StorePodAssignment(ctx context.Context, assignment *PodAssignment) error
	UpdatePodAssignment(ctx context.Context, assignment *PodAssignment) error
	GetAllPodAssignments(ctx context.Context) (map[string]*PodAssignment, error)
	DeletePodAssignment(ctx context.Context, podKey string) error
}

// FallbackQueue manages fallback work when no workers are available.
type FallbackQueue interface {
	ConsumeFromFallbackQueue(ctx context.Context, workType string) (interface{}, error)
	GetFallbackQueueLength(ctx context.Context, workType string) (int64, error)
}

// Client combines Publisher, Consumer, HealthStorage, and FallbackQueue interfaces.
type Client interface {
	Publisher
	Consumer
	HealthStorage
	FallbackQueue
}
