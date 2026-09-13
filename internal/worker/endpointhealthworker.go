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

// endpointHealthWorker watches Kubernetes endpoint health and stores transitions in the vector database.
type endpointHealthWorker struct {
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

// NewEndpointHealthWorker creates a new endpoint health worker that watches Kubernetes endpoint conditions.
func NewEndpointHealthWorker(
	k8s k8s.Interface, db database.Interface, llm llm.Client,
	resync time.Duration, scope nsscope.Scope,
) EndpointHealthWorker {
	return &endpointHealthWorker{
		id:         fmt.Sprintf("endpoint-health-%s", uuid.New().String()),
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

func (w *endpointHealthWorker) ID() string                { return w.id }
func (w *endpointHealthWorker) Type() string              { return "endpoint-health" }
func (w *endpointHealthWorker) IsHealthy() bool           { return true }
func (w *endpointHealthWorker) GetAssignedPods() []string { return nil }
func (w *endpointHealthWorker) GetErrorCount() int        { return 0 }

func (w *endpointHealthWorker) StartHealthReporting(ctx context.Context) error {
	// Endpoint health worker does not register with Redis health system since it is
	// not queue-driven and does not receive pod assignments.
	return nil
}

func (w *endpointHealthWorker) Start(ctx context.Context) error {
	logger.Logger.Info("Starting endpoint health worker",
		zap.String("worker_id", w.id),
		zap.Duration("resync_interval", w.resync),
		zap.Strings("namespaces", w.scope.Namespaces()))

	w.startStatsReporting(ctx)

	// Watch-driven since #96; see internal/k8s/statusinformers.go. A failed
	// informer is a returned error, which the supervisor restarts.
	err := w.k8s.WatchEndpointHealth(ctx, w.resync, staticNamespaces(w.scope), w.scope.Contains,
		func(status *k8s.EndpointHealthEntry) {
			if err := w.processEndpointHealth(ctx, status); err != nil {
				logger.Logger.Error("Failed to process endpoint health",
					zap.String("worker_id", w.id), zap.String("service", status.ServiceName), zap.Error(err))
			}
		})

	logger.Logger.Info("Endpoint health worker stopping", zap.String("worker_id", w.id), zap.Error(err))
	return err
}

func (w *endpointHealthWorker) Stop(_ context.Context) error {
	logger.Logger.Info("Endpoint health worker stopped", zap.String("worker_id", w.id))
	return nil
}

func (w *endpointHealthWorker) processEndpointHealth(ctx context.Context, status *k8s.EndpointHealthEntry) error {
	// Build a content hash from the endpoint state to deduplicate unchanged statuses.
	endpointsJSON, err := json.Marshal(status.Endpoints)
	if err != nil {
		return fmt.Errorf("failed to marshal endpoints: %w", err)
	}

	hashInput := fmt.Sprintf("endpoint|%s|%s|%d|%d|%d|%s",
		status.Namespace, status.ServiceName,
		status.ReadyCount, status.NotReadyCount, status.TotalCount,
		string(endpointsJSON))
	contentHash := fmt.Sprintf("%x", sha256.Sum256([]byte(hashInput)))

	if w.seenHash.contains(contentHash) {
		return nil
	}
	w.seenHash.add(contentHash)

	w.stats.RecordEventIngestion(status.Namespace)

	// Build human-readable text for embedding.
	embeddingText := buildEndpointHealthEmbeddingText(status)

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

	healthCounts, err := json.Marshal(map[string]interface{}{
		"ready":     status.ReadyCount,
		"not_ready": status.NotReadyCount,
		"total":     status.TotalCount,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal health counts: %w", err)
	}

	data, jsonErr := json.Marshal(map[string]interface{}{
		"endpoints":     string(endpointsJSON),
		"health_counts": string(healthCounts),
	})
	if jsonErr != nil {
		return fmt.Errorf("failed to marshal endpoint health data: %w", jsonErr)
	}

	dbEntry := &database.UnifiedEntry{
		ResourceType: "endpoint_health",
		Namespace:    status.Namespace,
		ResourceName: status.ServiceName,
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

func buildEndpointHealthEmbeddingText(status *k8s.EndpointHealthEntry) string {
	text := fmt.Sprintf("Endpoint Health: %s/%s, Ready: %d, NotReady: %d, Total: %d",
		status.Namespace, status.ServiceName,
		status.ReadyCount, status.NotReadyCount, status.TotalCount)

	if status.TotalCount == 0 {
		text += ", no endpoints"
	} else if status.ReadyCount == 0 {
		text += ", zero ready endpoints (complete outage)"
	} else if status.NotReadyCount > 0 {
		text += ", some endpoints not ready"
	}

	for _, ep := range status.Endpoints {
		if !ep.Conditions.Ready {
			text += ", Endpoint not-ready"
			if ep.TargetRef != "" {
				text += fmt.Sprintf(" (target: %s)", ep.TargetRef)
			}
			if ep.Conditions.Terminating {
				text += " (terminating)"
			}
		}
	}

	return text
}

func (w *endpointHealthWorker) startStatsReporting(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				logger.Logger.Info("Endpoint health worker stats",
					zap.String("worker_id", w.id),
					zap.Any("stats", w.stats.GetStats()))
			}
		}
	}()
}
