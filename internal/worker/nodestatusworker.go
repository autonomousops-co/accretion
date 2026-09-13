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

	"github.com/autonomousops-co/accretion/internal/logger"
	"github.com/autonomousops-co/accretion/internal/resilience"
)

// nodeStatusWorker watches Kubernetes node statuses and stores them in the vector database.
// Nodes are cluster-scoped, so this worker does not use namespaces.
type nodeStatusWorker struct {
	k8s        k8s.Interface
	db         database.Interface
	llm        llm.Client
	seenHash   *lruCache
	dbBreaker  *resilience.CircuitBreaker
	llmBreaker *resilience.CircuitBreaker
	stats      *WorkerStats
	id         string
	resync     time.Duration
}

// NewNodeStatusWorker creates a new node status worker that watches Kubernetes node conditions.
func NewNodeStatusWorker(
	k8s k8s.Interface, db database.Interface, llm llm.Client,
	resync time.Duration,
) NodeStatusWorker {
	return &nodeStatusWorker{
		id:         fmt.Sprintf("node-status-%s", uuid.New().String()),
		k8s:        k8s,
		db:         db,
		llm:        llm,
		seenHash:   newLRUCache(10000),
		dbBreaker:  resilience.NewCircuitBreaker(resilience.DefaultCircuitBreakerConfig()),
		llmBreaker: resilience.NewCircuitBreaker(resilience.DefaultCircuitBreakerConfig()),
		stats:      NewWorkerStats(),
		resync:     resync,
	}
}

func (w *nodeStatusWorker) ID() string                { return w.id }
func (w *nodeStatusWorker) Type() string              { return "node-status" }
func (w *nodeStatusWorker) IsHealthy() bool           { return true }
func (w *nodeStatusWorker) GetAssignedPods() []string { return nil }
func (w *nodeStatusWorker) GetErrorCount() int        { return 0 }

func (w *nodeStatusWorker) StartHealthReporting(ctx context.Context) error {
	// Node status worker does not register with Redis health system since it is
	// not queue-driven and does not receive pod assignments.
	return nil
}

func (w *nodeStatusWorker) Start(ctx context.Context) error {
	logger.Logger.Info("Starting node status worker",
		zap.String("worker_id", w.id),
		zap.Duration("resync_interval", w.resync))

	w.startStatsReporting(ctx)

	// Watch-driven since #96; see internal/k8s/statusinformers.go. A failed
	// informer is a returned error, which the supervisor restarts.
	err := w.k8s.WatchNodeStatuses(ctx, w.resync, func(status *k8s.NodeStatusEntry) {
		if err := w.processNodeStatus(ctx, status); err != nil {
			logger.Logger.Error("Failed to process node status",
				zap.String("worker_id", w.id), zap.String("node", status.NodeName), zap.Error(err))
		}
	})

	logger.Logger.Info("Node status worker stopping", zap.String("worker_id", w.id), zap.Error(err))
	return err
}

func (w *nodeStatusWorker) Stop(_ context.Context) error {
	logger.Logger.Info("Node status worker stopped", zap.String("worker_id", w.id))
	return nil
}

func (w *nodeStatusWorker) processNodeStatus(ctx context.Context, status *k8s.NodeStatusEntry) error {
	// Build a content hash from the node state to deduplicate unchanged statuses.
	conditionsJSON, err := json.Marshal(status.Conditions)
	if err != nil {
		return fmt.Errorf("failed to marshal conditions: %w", err)
	}

	resourcesJSON, err := json.Marshal([]k8s.NodeResourceInfo{
		status.CPU, status.Memory, status.EphemeralStorage, status.Pods,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal resources: %w", err)
	}
	infoJSON, err := json.Marshal(status.Info)
	if err != nil {
		return fmt.Errorf("failed to marshal node info: %w", err)
	}

	hashInput := fmt.Sprintf("node|%s|%s|%s|%s",
		status.NodeName, string(conditionsJSON), string(resourcesJSON), string(infoJSON))
	contentHash := fmt.Sprintf("%x", sha256.Sum256([]byte(hashInput)))

	if w.seenHash.contains(contentHash) {
		return nil
	}
	w.seenHash.add(contentHash)

	w.stats.RecordEventIngestion("cluster")

	// Build human-readable text for embedding.
	embeddingText := buildNodeStatusEmbeddingText(status)

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

	resourcesJSON, err = json.Marshal(map[string]k8s.NodeResourceInfo{
		"cpu":               status.CPU,
		"memory":            status.Memory,
		"ephemeral-storage": status.EphemeralStorage,
		"pods":              status.Pods,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal resources: %w", err)
	}

	infoJSON, err = json.Marshal(status.Info)
	if err != nil {
		return fmt.Errorf("failed to marshal node info: %w", err)
	}

	data, jsonErr := json.Marshal(map[string]interface{}{
		"conditions": string(conditionsJSON),
		"resources":  string(resourcesJSON),
		"node_info":  string(infoJSON),
	})
	if jsonErr != nil {
		return fmt.Errorf("failed to marshal node status data: %w", jsonErr)
	}

	dbEntry := &database.UnifiedEntry{
		ResourceType: "node_status",
		Namespace:    "",
		ResourceName: status.NodeName,
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

func buildNodeStatusEmbeddingText(status *k8s.NodeStatusEntry) string {
	text := fmt.Sprintf("Node Status: %s", status.NodeName)

	for _, cond := range status.Conditions {
		text += fmt.Sprintf(", Condition %s=%s", cond.Type, cond.Status)
		if cond.Reason != "" {
			text += fmt.Sprintf(" (reason: %s)", cond.Reason)
		}
		if cond.Message != "" {
			text += fmt.Sprintf(" (message: %s)", cond.Message)
		}
	}

	text += fmt.Sprintf(", CPU: %s/%s", status.CPU.Allocatable, status.CPU.Capacity)
	text += fmt.Sprintf(", Memory: %s/%s", status.Memory.Allocatable, status.Memory.Capacity)

	if status.EphemeralStorage.Capacity != "" {
		text += fmt.Sprintf(", EphemeralStorage: %s/%s",
			status.EphemeralStorage.Allocatable, status.EphemeralStorage.Capacity)
	}
	if status.Pods.Capacity != "" {
		text += fmt.Sprintf(", Pods: %s/%s", status.Pods.Allocatable, status.Pods.Capacity)
	}

	if status.Info.KubeletVersion != "" {
		text += fmt.Sprintf(", Kubelet: %s", status.Info.KubeletVersion)
	}
	if status.Info.ContainerRuntime != "" {
		text += fmt.Sprintf(", Runtime: %s", status.Info.ContainerRuntime)
	}

	return text
}

func (w *nodeStatusWorker) startStatsReporting(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				logger.Logger.Info("Node status worker stats",
					zap.String("worker_id", w.id),
					zap.Any("stats", w.stats.GetStats()))
			}
		}
	}()
}
