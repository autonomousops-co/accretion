package worker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync/atomic"
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
	"github.com/autonomousops-co/accretion/internal/telemetry"
)

// eventWorker polls Kubernetes events and stores them in the vector database.
// Unlike log and metric workers, the event worker is not queue-driven. It
// directly polls the k8s API for events because events are namespace-scoped
// cluster resources, not tied to individual pod assignments.
type eventWorker struct {
	k8s          k8s.Interface
	db           database.Interface
	llm          llm.Client
	seenHash     *lruCache
	dbBreaker    *resilience.CircuitBreaker
	llmBreaker   *resilience.CircuitBreaker
	stats        *WorkerStats
	id           string
	scope        nsscope.Scope
	lastPollUnix atomic.Int64
	pollInterval time.Duration
}

// NewEventWorker creates a new event worker that polls Kubernetes events.
func NewEventWorker(
	k8s k8s.Interface, db database.Interface, llm llm.Client,
	pollInterval time.Duration, scope nsscope.Scope,
) EventWorker {
	return &eventWorker{
		id:           fmt.Sprintf("event-%s", uuid.New().String()),
		k8s:          k8s,
		db:           db,
		llm:          llm,
		seenHash:     newLRUCache(10000),
		dbBreaker:    resilience.NewCircuitBreaker(resilience.DefaultCircuitBreakerConfig()),
		llmBreaker:   resilience.NewCircuitBreaker(resilience.DefaultCircuitBreakerConfig()),
		stats:        NewWorkerStats(),
		scope:        scope,
		pollInterval: pollInterval,
	}
}

func (w *eventWorker) ID() string                { return w.id }
func (w *eventWorker) Type() string              { return "events" }
func (w *eventWorker) IsHealthy() bool           { return true }
func (w *eventWorker) GetAssignedPods() []string { return nil }
func (w *eventWorker) GetErrorCount() int        { return 0 }

func (w *eventWorker) StartHealthReporting(ctx context.Context) error {
	// Event worker does not register with Redis health system since it is
	// not queue-driven and does not receive pod assignments.
	return nil
}

func (w *eventWorker) Start(ctx context.Context) error {
	logger.Logger.Info("Starting event worker",
		zap.String("worker_id", w.id),
		zap.Duration("poll_interval", w.pollInterval),
		zap.Strings("namespaces", w.scope.Namespaces()))

	w.StartStatsReporting(ctx)

	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	// Run an initial poll immediately.
	w.pollEvents(ctx)

	for {
		select {
		case <-ctx.Done():
			logger.Logger.Info("Event worker stopping", zap.String("worker_id", w.id))
			return ctx.Err()
		case <-ticker.C:
			w.pollEvents(ctx)
		}
	}
}

func (w *eventWorker) Stop(_ context.Context) error {
	logger.Logger.Info("Event worker stopped", zap.String("worker_id", w.id))
	return nil
}

func (w *eventWorker) pollEvents(ctx context.Context) {
	ctx, span := telemetry.StartSpan(ctx, "worker.events", "pollEvents")
	defer span.End()

	var since time.Time
	if last := w.lastPollUnix.Load(); last > 0 {
		since = time.Unix(last, 0)
	}
	w.lastPollUnix.Store(time.Now().Unix())

	for _, ns := range w.scope.Namespaces() {
		events, err := w.k8s.ListEvents(ctx, ns, since)
		if err != nil {
			logger.Logger.Error("Failed to list events",
				zap.String("worker_id", w.id), zap.String("namespace", ns), zap.Error(err))
			continue
		}

		for i := range events {
			if err := w.processEvent(ctx, &events[i]); err != nil {
				logger.Logger.Error("Failed to process event",
					zap.String("worker_id", w.id), zap.String("event", events[i].Name), zap.Error(err))
			}
		}
	}
}

func (w *eventWorker) processEvent(ctx context.Context, event *k8s.EventEntry) error {
	// Build a content hash to deduplicate events.
	// Count is intentionally excluded so repeated events don't create duplicates.
	hashInput := fmt.Sprintf("%s|%s|%s|%s|%s",
		event.Namespace, event.InvolvedObject, event.Reason,
		event.Message, event.Timestamp.Format(time.RFC3339))
	contentHash := fmt.Sprintf("%x", sha256.Sum256([]byte(hashInput)))

	if w.seenHash.contains(contentHash) {
		return nil
	}
	w.seenHash.add(contentHash)

	w.stats.RecordEventIngestion(event.Namespace)

	// Build embedding text that captures the semantic meaning of the event.
	eventText := fmt.Sprintf("Kubernetes Event: %s %s, Object: %s, Reason: %s, Message: %s, Source: %s, Timestamp: %s",
		event.Type, event.Kind, event.InvolvedObject, event.Reason, event.Message, event.Source,
		event.Timestamp.Format(time.RFC3339))

	var embedding []float32
	var err error

	err = w.llmBreaker.Execute(ctx, func(ctx context.Context) error {
		return resilience.WithRetry(ctx, resilience.DefaultRetryConfig(), func(ctx context.Context) error {
			embedding, err = w.llm.GenerateEmbedding(ctx, eventText)
			return err
		})
	})
	if err != nil {
		return fmt.Errorf("embedding generation failed: %w", err)
	}

	data, jsonErr := json.Marshal(map[string]interface{}{
		"name":            event.Name,
		"reason":          event.Reason,
		"message":         event.Message,
		"involved_object": event.InvolvedObject,
		"kind":            event.Kind,
		"source":          event.Source,
		"event_type":      event.Type,
		"count":           event.Count,
	})
	if jsonErr != nil {
		return fmt.Errorf("failed to marshal event data: %w", jsonErr)
	}

	dbEntry := &database.UnifiedEntry{
		ResourceType: "event",
		Namespace:    event.Namespace,
		ResourceName: event.Name,
		Data:         data,
		Timestamp:    event.Timestamp,
		ContentHash:  contentHash,
		Embedding:    pgvector.NewVector(embedding),
	}

	return w.dbBreaker.Execute(ctx, func(ctx context.Context) error {
		return resilience.WithRetry(ctx, resilience.DefaultRetryConfig(), func(ctx context.Context) error {
			return w.db.InsertEntry(dbEntry)
		})
	})
}

// StartStatsReporting starts periodic stats reporting for the event worker.
func (w *eventWorker) StartStatsReporting(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				logger.Logger.Info("Event worker stats",
					zap.String("worker_id", w.id),
					zap.Any("stats", w.stats.GetStats()))
			}
		}
	}()
}
