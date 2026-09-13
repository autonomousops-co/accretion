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

// hpaStatusWorker watches Kubernetes HPA statuses and stores them in the vector database.
// It is driven by a client-go shared informer rather than a queue (#96).
type hpaStatusWorker struct {
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

// NewHPAStatusWorker creates a new HPA status worker that watches Kubernetes HPA conditions.
func NewHPAStatusWorker(
	k8s k8s.Interface, db database.Interface, llm llm.Client,
	resync time.Duration, scope nsscope.Scope,
) HPAStatusWorker {
	return &hpaStatusWorker{
		id:         fmt.Sprintf("hpa-status-%s", uuid.New().String()),
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

func (w *hpaStatusWorker) ID() string                { return w.id }
func (w *hpaStatusWorker) Type() string              { return "hpa-status" }
func (w *hpaStatusWorker) IsHealthy() bool           { return true }
func (w *hpaStatusWorker) GetAssignedPods() []string { return nil }
func (w *hpaStatusWorker) GetErrorCount() int        { return 0 }

func (w *hpaStatusWorker) StartHealthReporting(ctx context.Context) error {
	// HPA status worker does not register with Redis health system since it is
	// not queue-driven and does not receive pod assignments.
	return nil
}

func (w *hpaStatusWorker) Start(ctx context.Context) error {
	logger.Logger.Info("Starting HPA status worker",
		zap.String("worker_id", w.id),
		zap.Duration("resync_interval", w.resync),
		zap.Strings("namespaces", w.scope.Namespaces()))

	w.startStatsReporting(ctx)

	// Watch-driven since #96; see internal/k8s/statusinformers.go. A failed
	// informer is a returned error, which the supervisor restarts.
	err := w.k8s.WatchHPAStatuses(ctx, w.resync, staticNamespaces(w.scope), w.scope.Contains,
		func(status *k8s.HPAStatusEntry) {
			if err := w.processHPAStatus(ctx, status); err != nil {
				logger.Logger.Error("Failed to process HPA status",
					zap.String("worker_id", w.id), zap.String("hpa", status.HPAName), zap.Error(err))
			}
		})

	logger.Logger.Info("HPA status worker stopping", zap.String("worker_id", w.id), zap.Error(err))
	return err
}

func (w *hpaStatusWorker) Stop(_ context.Context) error {
	logger.Logger.Info("HPA status worker stopped", zap.String("worker_id", w.id))
	return nil
}

func (w *hpaStatusWorker) processHPAStatus(ctx context.Context, status *k8s.HPAStatusEntry) error {
	// Build a content hash from the HPA state to deduplicate unchanged statuses.
	conditionsJSON, err := json.Marshal(status.Conditions)
	if err != nil {
		return fmt.Errorf("failed to marshal conditions: %w", err)
	}
	metricsJSON, err := json.Marshal(status.Metrics)
	if err != nil {
		return fmt.Errorf("failed to marshal metrics: %w", err)
	}

	hashInput := fmt.Sprintf("hpa|%s|%s|%d|%d|%d|%d|%s|%s",
		status.Namespace, status.HPAName,
		status.CurrentReplicas, status.DesiredReplicas,
		status.MinReplicas, status.MaxReplicas,
		string(conditionsJSON), string(metricsJSON))
	contentHash := fmt.Sprintf("%x", sha256.Sum256([]byte(hashInput)))

	if w.seenHash.contains(contentHash) {
		return nil
	}
	w.seenHash.add(contentHash)

	w.stats.RecordEventIngestion(status.Namespace)

	// Build human-readable text for embedding.
	embeddingText := buildHPAStatusEmbeddingText(status)

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

	scalingInfo, err := json.Marshal(map[string]interface{}{
		"current_replicas": status.CurrentReplicas,
		"desired_replicas": status.DesiredReplicas,
		"min_replicas":     status.MinReplicas,
		"max_replicas":     status.MaxReplicas,
		"last_scale_time":  status.LastScaleTime,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal scaling info: %w", err)
	}

	data, jsonErr := json.Marshal(map[string]interface{}{
		"conditions":   string(conditionsJSON),
		"metrics":      string(metricsJSON),
		"scaling_info": string(scalingInfo),
	})
	if jsonErr != nil {
		return fmt.Errorf("failed to marshal HPA status data: %w", jsonErr)
	}

	dbEntry := &database.UnifiedEntry{
		ResourceType: "hpa_status",
		Namespace:    status.Namespace,
		ResourceName: status.HPAName,
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

func buildHPAStatusEmbeddingText(status *k8s.HPAStatusEntry) string {
	text := fmt.Sprintf("HPA Status: %s/%s, Replicas: %d/%d (min: %d, max: %d)",
		status.Namespace, status.HPAName,
		status.CurrentReplicas, status.DesiredReplicas,
		status.MinReplicas, status.MaxReplicas)

	if status.CurrentReplicas >= status.MaxReplicas {
		text += ", at max replicas"
	}
	if status.DesiredReplicas > status.CurrentReplicas {
		text += ", scaling up"
	} else if status.DesiredReplicas < status.CurrentReplicas {
		text += ", scaling down"
	}

	for _, cond := range status.Conditions {
		text += fmt.Sprintf(", Condition %s=%s", cond.Type, cond.Status)
		if cond.Reason != "" {
			text += fmt.Sprintf(" (reason: %s)", cond.Reason)
		}
		if cond.Message != "" {
			text += fmt.Sprintf(" (message: %s)", cond.Message)
		}
	}

	for _, metric := range status.Metrics {
		text += fmt.Sprintf(", Metric %s: current=%s", metric.Name, metric.CurrentValue)
		if metric.TargetValue != "" {
			text += fmt.Sprintf(" target=%s", metric.TargetValue)
		}
	}

	if status.LastScaleTime != nil {
		text += fmt.Sprintf(", LastScaleTime: %s", status.LastScaleTime.Format(time.RFC3339))
	}

	return text
}

func (w *hpaStatusWorker) startStatsReporting(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				logger.Logger.Info("HPA status worker stats",
					zap.String("worker_id", w.id),
					zap.Any("stats", w.stats.GetStats()))
			}
		}
	}()
}
