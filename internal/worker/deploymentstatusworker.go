package worker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"

	"go.uber.org/zap"

	"github.com/autonomousops-co/accretion/internal/database"
	"github.com/autonomousops-co/accretion/internal/k8s"
	"github.com/autonomousops-co/accretion/internal/llm"
	"github.com/autonomousops-co/accretion/internal/nsscope"

	"github.com/autonomousops-co/accretion/internal/logger"
	"github.com/autonomousops-co/accretion/internal/resilience"
)

// deploymentStatusWorker watches Kubernetes deployment statuses and stores them in the vector database.
// It captures deployment conditions, replica counts, and ReplicaSet revision history.
type deploymentStatusWorker struct {
	k8s        k8s.Interface
	db         database.Interface
	llm        llm.Client
	seenHash   *lruCache
	dbBreaker  *resilience.CircuitBreaker
	llmBreaker *resilience.CircuitBreaker
	stats      *WorkerStats
	id         string
	scope      nsscope.Scope
	resync     time.Duration
}

// NewDeploymentStatusWorker creates a new deployment status worker that watches Kubernetes deployment conditions.
func NewDeploymentStatusWorker(
	k8s k8s.Interface, db database.Interface, llm llm.Client,
	resync time.Duration, scope nsscope.Scope,
) DeploymentStatusWorker {
	return &deploymentStatusWorker{
		id:         fmt.Sprintf("deployment-status-%s", uuid.New().String()),
		k8s:        k8s,
		db:         db,
		llm:        llm,
		seenHash:   newLRUCache(10000),
		dbBreaker:  resilience.NewCircuitBreaker(resilience.DefaultCircuitBreakerConfig()),
		llmBreaker: resilience.NewCircuitBreaker(resilience.DefaultCircuitBreakerConfig()),
		stats:      NewWorkerStats(),
		scope:      scope,
		resync:     resync,
	}
}

func (w *deploymentStatusWorker) ID() string                { return w.id }
func (w *deploymentStatusWorker) Type() string              { return "deployment-status" }
func (w *deploymentStatusWorker) IsHealthy() bool           { return true }
func (w *deploymentStatusWorker) GetAssignedPods() []string { return nil }
func (w *deploymentStatusWorker) GetErrorCount() int        { return 0 }

func (w *deploymentStatusWorker) StartHealthReporting(ctx context.Context) error {
	// Deployment status worker does not register with Redis health system since it is
	// not queue-driven and does not receive pod assignments.
	return nil
}

func (w *deploymentStatusWorker) Start(ctx context.Context) error {
	logger.Logger.Info("Starting deployment status worker",
		zap.String("worker_id", w.id),
		zap.Duration("resync_interval", w.resync),
		zap.Strings("namespaces", w.scope.Namespaces()))

	w.startStatsReporting(ctx)

	// Watch-driven since #96; see internal/k8s/statusinformers.go. A failed
	// informer is a returned error, which the supervisor restarts.
	err := w.k8s.WatchDeploymentStatuses(ctx, w.resync, staticNamespaces(w.scope), w.scope.Contains,
		func(status *k8s.DeploymentStatusEntry) {
			if err := w.processDeploymentStatus(ctx, status); err != nil {
				logger.Logger.Error("Failed to process deployment status",
					zap.String("worker_id", w.id), zap.String("deployment", status.DeploymentName), zap.Error(err))
			}
		})

	logger.Logger.Info("Deployment status worker stopping", zap.String("worker_id", w.id), zap.Error(err))
	return err
}

func (w *deploymentStatusWorker) Stop(_ context.Context) error {
	logger.Logger.Info("Deployment status worker stopped", zap.String("worker_id", w.id))
	return nil
}

func (w *deploymentStatusWorker) processDeploymentStatus(ctx context.Context, status *k8s.DeploymentStatusEntry) error {
	// Build a content hash from the deployment state to deduplicate unchanged statuses.
	conditionsJSON, err := json.Marshal(status.Conditions)
	if err != nil {
		return fmt.Errorf("failed to marshal conditions: %w", err)
	}
	replicasJSON, err := json.Marshal(status.Replicas)
	if err != nil {
		return fmt.Errorf("failed to marshal replicas: %w", err)
	}
	replicaSetsJSON, err := json.Marshal(status.ReplicaSets)
	if err != nil {
		return fmt.Errorf("failed to marshal replica sets: %w", err)
	}

	hashInput := fmt.Sprintf("deployment|%s|%s|%s|%s|%s|%d|%s",
		status.Namespace, status.DeploymentName,
		string(conditionsJSON), string(replicasJSON), string(replicaSetsJSON),
		status.ObservedGeneration, status.ChangeCause)
	contentHash := fmt.Sprintf("%x", sha256.Sum256([]byte(hashInput)))

	if w.seenHash.contains(contentHash) {
		return nil
	}
	w.seenHash.add(contentHash)

	w.stats.RecordEventIngestion(status.Namespace)

	// Build human-readable text for embedding.
	embeddingText := buildDeploymentStatusEmbeddingText(status)

	var embedding []float32
	var embedErr error

	err = w.llmBreaker.Execute(ctx, func(ctx context.Context) error {
		return resilience.WithRetry(ctx, resilience.DefaultRetryConfig(), func(ctx context.Context) error {
			embedding, embedErr = w.llm.GenerateEmbedding(ctx, embeddingText)
			return embedErr
		})
	})
	if err != nil {
		return fmt.Errorf("embedding generation failed: %w", err)
	}

	data, jsonErr := json.Marshal(map[string]interface{}{
		"conditions":   string(conditionsJSON),
		"replicas":     string(replicasJSON),
		"replica_sets": string(replicaSetsJSON),
		"change_cause": status.ChangeCause,
	})
	if jsonErr != nil {
		return fmt.Errorf("failed to marshal deployment status data: %w", jsonErr)
	}

	dbEntry := &database.UnifiedEntry{
		ResourceType: "deployment_status",
		Namespace:    status.Namespace,
		ResourceName: status.DeploymentName,
		Data:         data,
		Timestamp:    status.Timestamp,
		ContentHash:  contentHash,
		Embedding:    pgvector.NewVector(embedding),
	}

	return w.dbBreaker.Execute(ctx, func(ctx context.Context) error {
		return resilience.WithRetry(ctx, resilience.DefaultRetryConfig(), func(ctx context.Context) error {
			return w.db.InsertEntry(dbEntry)
		})
	})
}

func buildDeploymentStatusEmbeddingText(status *k8s.DeploymentStatusEntry) string {
	text := fmt.Sprintf("Deployment Status: %s/%s", status.Namespace, status.DeploymentName)

	text += fmt.Sprintf(", Replicas: desired=%d ready=%d available=%d unavailable=%d updated=%d",
		status.Replicas.Desired, status.Replicas.Ready, status.Replicas.Available,
		status.Replicas.Unavailable, status.Replicas.Updated)

	for _, cond := range status.Conditions {
		text += fmt.Sprintf(", Condition %s=%s", cond.Type, cond.Status)
		if cond.Reason != "" {
			text += fmt.Sprintf(" (reason: %s)", cond.Reason)
		}
		if cond.Message != "" {
			text += fmt.Sprintf(" (message: %s)", cond.Message)
		}
	}

	for _, rs := range status.ReplicaSets {
		text += fmt.Sprintf(", ReplicaSet %s revision=%s replicas=%d ready=%d",
			rs.Name, rs.Revision, rs.Replicas, rs.ReadyReplicas)
	}

	if status.ChangeCause != "" {
		text += fmt.Sprintf(", ChangeCause: %s", status.ChangeCause)
	}

	return text
}

func (w *deploymentStatusWorker) startStatsReporting(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				logger.Logger.Info("Deployment status worker stats",
					zap.String("worker_id", w.id),
					zap.Any("stats", w.stats.GetStats()))
			}
		}
	}()
}
