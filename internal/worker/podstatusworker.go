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

// podStatusWorker watches Kubernetes pod statuses and stores them in the vector database.
// It is driven by a client-go shared informer rather than a queue (#96).
type podStatusWorker struct {
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

// NewPodStatusWorker creates a new pod status worker that watches Kubernetes pod conditions.
func NewPodStatusWorker(
	k8s k8s.Interface, db database.Interface, llm llm.Client,
	resync time.Duration, scope nsscope.Scope,
) PodStatusWorker {
	return &podStatusWorker{
		id:         fmt.Sprintf("pod-status-%s", uuid.New().String()),
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

func (w *podStatusWorker) ID() string                { return w.id }
func (w *podStatusWorker) Type() string              { return "pod-status" }
func (w *podStatusWorker) IsHealthy() bool           { return true }
func (w *podStatusWorker) GetAssignedPods() []string { return nil }
func (w *podStatusWorker) GetErrorCount() int        { return 0 }

func (w *podStatusWorker) StartHealthReporting(ctx context.Context) error {
	// Pod status worker does not register with Redis health system since it is
	// not queue-driven and does not receive pod assignments.
	return nil
}

func (w *podStatusWorker) Start(ctx context.Context) error {
	logger.Logger.Info("Starting pod status worker",
		zap.String("worker_id", w.id),
		zap.Duration("resync_interval", w.resync),
		zap.Strings("namespaces", w.scope.Namespaces()))

	w.startStatsReporting(ctx)

	// Watch-driven since #96; see internal/k8s/statusinformers.go. A failed
	// informer is a returned error, which the supervisor restarts.
	err := w.k8s.WatchPodStatuses(ctx, w.resync, staticNamespaces(w.scope), w.scope.Contains,
		func(status *k8s.PodStatusEntry) {
			if err := w.processPodStatus(ctx, status); err != nil {
				logger.Logger.Error("Failed to process pod status",
					zap.String("worker_id", w.id), zap.String("pod", status.PodName), zap.Error(err))
			}
		})

	logger.Logger.Info("Pod status worker stopping", zap.String("worker_id", w.id), zap.Error(err))
	return err
}

func (w *podStatusWorker) Stop(_ context.Context) error {
	logger.Logger.Info("Pod status worker stopped", zap.String("worker_id", w.id))
	return nil
}

func (w *podStatusWorker) processPodStatus(ctx context.Context, status *k8s.PodStatusEntry) error {
	// Build a content hash from the pod state to deduplicate unchanged statuses.
	conditionsJSON, err := json.Marshal(status.Conditions)
	if err != nil {
		return fmt.Errorf("failed to marshal conditions: %w", err)
	}
	containersJSON, err := json.Marshal(status.ContainerStatuses)
	if err != nil {
		return fmt.Errorf("failed to marshal container statuses: %w", err)
	}

	hashInput := fmt.Sprintf("%s|%s|%s|%s|%s",
		status.Namespace, status.PodName, status.Phase,
		string(conditionsJSON), string(containersJSON))
	contentHash := fmt.Sprintf("%x", sha256.Sum256([]byte(hashInput)))

	if w.seenHash.contains(contentHash) {
		return nil
	}
	w.seenHash.add(contentHash)

	w.stats.RecordEventIngestion(status.Namespace)

	// Build human-readable text for embedding.
	embeddingText := buildPodStatusEmbeddingText(status)

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
		"phase":      status.Phase,
		"conditions": string(conditionsJSON),
		"containers": string(containersJSON),
	})
	if jsonErr != nil {
		return fmt.Errorf("failed to marshal pod status data: %w", jsonErr)
	}

	dbEntry := &database.UnifiedEntry{
		ResourceType: "pod_status",
		Namespace:    status.Namespace,
		ResourceName: status.PodName,
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

func buildPodStatusEmbeddingText(status *k8s.PodStatusEntry) string {
	text := fmt.Sprintf("Pod Status: %s/%s, Phase: %s", status.Namespace, status.PodName, status.Phase)

	for _, cond := range status.Conditions {
		text += fmt.Sprintf(", Condition %s=%s", cond.Type, cond.Status)
		if cond.Reason != "" {
			text += fmt.Sprintf(" (reason: %s)", cond.Reason)
		}
		if cond.Message != "" {
			text += fmt.Sprintf(" (message: %s)", cond.Message)
		}
	}

	for i := range status.ContainerStatuses {
		cs := &status.ContainerStatuses[i]
		text += fmt.Sprintf(", Container %s: %s", cs.Name, cs.State)
		if cs.Reason != "" {
			text += fmt.Sprintf(" (reason: %s)", cs.Reason)
		}
		if cs.RestartCount > 0 {
			text += fmt.Sprintf(" (restarts: %d)", cs.RestartCount)
		}
		if cs.ExitCode != 0 {
			text += fmt.Sprintf(" (exit_code: %d)", cs.ExitCode)
		}
		if cs.Signal != 0 {
			text += fmt.Sprintf(" (signal: %d)", cs.Signal)
		}
	}

	return text
}

func (w *podStatusWorker) startStatsReporting(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				logger.Logger.Info("Pod status worker stats",
					zap.String("worker_id", w.id),
					zap.Any("stats", w.stats.GetStats()))
			}
		}
	}()
}
