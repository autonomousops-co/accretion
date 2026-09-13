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

// configMapWorker watches Kubernetes ConfigMap and Secret metadata and stores changes in the vector database.
// SECURITY: Only metadata is tracked for Secrets. No secret data values are ever stored.
type configMapWorker struct {
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

// NewConfigMapWorker creates a new ConfigMap/Secret change tracking worker.
func NewConfigMapWorker(
	k8s k8s.Interface, db database.Interface, llm llm.Client,
	resync time.Duration, scope nsscope.Scope,
) ConfigMapWorker {
	return &configMapWorker{
		id:         fmt.Sprintf("configmap-%s", uuid.New().String()),
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

func (w *configMapWorker) ID() string                { return w.id }
func (w *configMapWorker) Type() string              { return "configmap" }
func (w *configMapWorker) IsHealthy() bool           { return true }
func (w *configMapWorker) GetAssignedPods() []string { return nil }
func (w *configMapWorker) GetErrorCount() int        { return 0 }

func (w *configMapWorker) StartHealthReporting(ctx context.Context) error {
	// ConfigMap worker does not register with Redis health system since it is
	// not queue-driven and does not receive pod assignments.
	return nil
}

func (w *configMapWorker) Start(ctx context.Context) error {
	logger.Logger.Info("Starting ConfigMap worker",
		zap.String("worker_id", w.id),
		zap.Duration("resync_interval", w.resync),
		zap.Strings("namespaces", w.scope.Namespaces()))

	w.startStatsReporting(ctx)

	// Watch-driven since #96; see internal/k8s/statusinformers.go. A failed
	// informer is a returned error, which the supervisor restarts.
	err := w.k8s.WatchConfigMetadata(ctx, w.resync, staticNamespaces(w.scope), w.scope.Contains,
		func(cm *k8s.ConfigMapEntry) {
			if err := w.processConfigMap(ctx, cm); err != nil {
				logger.Logger.Error("Failed to process ConfigMap",
					zap.String("worker_id", w.id), zap.String("configmap", cm.Name), zap.Error(err))
			}
		})

	logger.Logger.Info("ConfigMap worker stopping", zap.String("worker_id", w.id), zap.Error(err))
	return err
}

func (w *configMapWorker) Stop(_ context.Context) error {
	logger.Logger.Info("ConfigMap worker stopped", zap.String("worker_id", w.id))
	return nil
}

func (w *configMapWorker) processConfigMap(ctx context.Context, cm *k8s.ConfigMapEntry) error {
	labelsJSON, err := json.Marshal(cm.Labels)
	if err != nil {
		return fmt.Errorf("failed to marshal labels: %w", err)
	}
	annotationsJSON, err := json.Marshal(k8s.SafeAnnotations(cm.Annotations))
	if err != nil {
		return fmt.Errorf("failed to marshal annotations: %w", err)
	}

	hashInput := fmt.Sprintf("configmap|%s|%s|%s",
		cm.Namespace, cm.Name, cm.ResourceVersion)
	contentHash := fmt.Sprintf("%x", sha256.Sum256([]byte(hashInput)))

	if w.seenHash.contains(contentHash) {
		return nil
	}
	w.seenHash.add(contentHash)

	w.stats.RecordEventIngestion(cm.Namespace)

	embeddingText := buildConfigMapEmbeddingText(cm)

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

	metadata, err := json.Marshal(map[string]interface{}{
		"resource_version":   cm.ResourceVersion,
		"labels":             json.RawMessage(labelsJSON),
		"annotations":        json.RawMessage(annotationsJSON),
		"creation_timestamp": cm.CreationTimestamp,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}

	data, jsonErr := json.Marshal(map[string]interface{}{
		"metadata": string(metadata),
	})
	if jsonErr != nil {
		return fmt.Errorf("failed to marshal ConfigMap data: %w", jsonErr)
	}

	dbEntry := &database.UnifiedEntry{
		ResourceType: "configmap",
		Namespace:    cm.Namespace,
		ResourceName: cm.Name,
		Data:         data,
		Timestamp:    cm.Timestamp,
		ContentHash:  contentHash,
		Embedding:    pgvector.NewVector(embedding),
	}

	return w.dbBreaker.Execute(ctx, func(ctx context.Context) error {
		return resilience.WithRetry(ctx, resilience.DefaultRetryConfig(), func(ctx context.Context) error {
			return w.db.InsertEntry(dbEntry)
		})
	})
}

func buildConfigMapEmbeddingText(cm *k8s.ConfigMapEntry) string {
	text := fmt.Sprintf("ConfigMap Change: %s/%s, ResourceVersion: %s",
		cm.Namespace, cm.Name, cm.ResourceVersion)

	if len(cm.Labels) > 0 {
		text += fmt.Sprintf(", Labels: %d", len(cm.Labels))
	}
	if len(cm.Annotations) > 0 {
		text += fmt.Sprintf(", Annotations: %d", len(cm.Annotations))
	}

	return text
}

func (w *configMapWorker) startStatsReporting(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				logger.Logger.Info("ConfigMap worker stats",
					zap.String("worker_id", w.id),
					zap.Any("stats", w.stats.GetStats()))
			}
		}
	}()
}
