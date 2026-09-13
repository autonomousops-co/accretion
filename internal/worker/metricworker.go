package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pgvector/pgvector-go"

	"go.uber.org/zap"

	"github.com/autonomousops-co/accretion/internal/database"
	"github.com/autonomousops-co/accretion/internal/k8s"
	"github.com/autonomousops-co/accretion/internal/llm"

	"github.com/autonomousops-co/accretion/internal/logger"
	"github.com/autonomousops-co/accretion/internal/nsscope"
	"github.com/autonomousops-co/accretion/internal/queue"
	"github.com/autonomousops-co/accretion/internal/resilience"
)

// metricWorker processes metrics collection work from dedicated queues.
type metricWorker struct {
	*BaseWorker
	k8s          k8s.Interface
	db           database.Interface
	llm          llm.Client
	pollInterval time.Duration
}

// NewMetricWorker creates a new metric worker.
func NewMetricWorker(
	queue queue.Consumer, k8s k8s.Interface, db database.Interface,
	llm llm.Client, pollInterval time.Duration, scope nsscope.Scope,
) MetricWorker {
	return &metricWorker{
		BaseWorker:   NewBaseWorker("metrics", queue, scope),
		k8s:          k8s,
		db:           db,
		llm:          llm,
		pollInterval: pollInterval,
	}
}

func (w *metricWorker) Start(ctx context.Context) error {
	logger.Logger.Info("Starting metric worker",
		zap.String("worker_id", w.ID()), zap.String("type", w.Type()),
		zap.Duration("poll_interval", w.pollInterval))

	if err := w.StartHealthReporting(ctx); err != nil {
		return fmt.Errorf("failed to start health reporting: %w", err)
	}

	w.StartStatsReporting(ctx)

	return w.queue.ConsumeMetricsWorkForWorker(ctx, w.ID(), w.handleWork)
}

func (w *metricWorker) Stop(ctx context.Context) error {
	return w.Cleanup(ctx)
}

func (w *metricWorker) handleWork(work *queue.MetricsCollectionWork) error {
	_, span := w.StartSpan(context.Background(), "handleWork")
	defer span.End()

	podKey := fmt.Sprintf("%s/%s", work.Namespace, work.PodName)

	w.MarkPodAssigned(podKey)
	w.StorePodAssignment(podKey)

	return w.collectMetrics(work)
}

// metricsCallTimeout bounds a single Metrics API call.
//
// This used to be context.Background(), which is unbounded: a metrics-server
// that accepts the connection and never answers pinned the goroutine forever.
// That was survivable when the metric worker was its own pod; now it shares a
// process with the log, event and trace collectors, so an unbounded call is a
// slot held against all of them.
const metricsCallTimeout = 30 * time.Second

func (w *metricWorker) getPodMetrics(work *queue.MetricsCollectionWork) error {
	ctx, cancel := context.WithTimeout(context.Background(), metricsCallTimeout)
	defer cancel()

	allMetrics, err := w.k8s.GetPodMetrics(ctx, work.Namespace)
	if err != nil {
		return fmt.Errorf("failed to get pod metrics for namespace %s: %w", work.Namespace, err)
	}

	for _, metric := range allMetrics {
		if metric.PodName == work.PodName {
			return w.processMetrics(&metric)
		}
	}

	return fmt.Errorf("metrics not found for pod %s", work.PodName)
}

func (w *metricWorker) processMetrics(podMetrics *k8s.PodMetrics) error {
	for _, container := range podMetrics.Containers {
		metricsText := fmt.Sprintf("Pod: %s, Container: %s, CPU: %s, Memory: %s, Timestamp: %s",
			podMetrics.PodName, container.Name, container.CPU, container.Memory, podMetrics.Timestamp.Format(time.RFC3339))

		w.stats.RecordMetricIngestion(podMetrics.Namespace)

		var embedding []float32
		var err error

		// Generate embedding with resilience
		err = w.llmBreaker.Execute(context.Background(), func(ctx context.Context) error {
			return resilience.WithRetry(ctx, resilience.DefaultRetryConfig(), func(ctx context.Context) error {
				embedding, err = w.llm.GenerateEmbedding(ctx, metricsText)
				return err
			})
		})
		if err != nil {
			return fmt.Errorf("embedding generation failed: %w", err)
		}

		// Create and insert unified database entry
		data, jsonErr := json.Marshal(map[string]interface{}{
			"container": container.Name,
			"cpu":       container.CPU,
			"memory":    container.Memory,
		})
		if jsonErr != nil {
			return fmt.Errorf("failed to marshal metrics data: %w", jsonErr)
		}

		dbEntry := &database.UnifiedEntry{
			ResourceType: "metrics",
			Namespace:    podMetrics.Namespace,
			ResourceName: podMetrics.PodName,
			Data:         data,
			Timestamp:    podMetrics.Timestamp,
			Embedding:    pgvector.NewVector(embedding),
		}

		// Insert with resilience
		err = w.dbBreaker.Execute(context.Background(), func(ctx context.Context) error {
			return resilience.WithRetry(ctx, resilience.DefaultRetryConfig(), func(ctx context.Context) error {
				return w.db.InsertEntry(dbEntry)
			})
		})
		if err != nil {
			return fmt.Errorf("database insert failed: %w", err)
		}
	}

	return nil
}

//nolint:unparam // error always nil by design
func (w *metricWorker) collectMetrics(work *queue.MetricsCollectionWork) error {
	go w.startPolling(work)
	logger.Logger.Info("Started metrics collection",
		zap.String("worker_id", w.ID()), zap.String("pod", work.PodName),
		zap.Duration("interval", w.pollInterval))
	return nil
}

func (w *metricWorker) startPolling(work *queue.MetricsCollectionWork) {
	podKey := fmt.Sprintf("%s/%s", work.Namespace, work.PodName)
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	for range ticker.C {
		if err := w.getPodMetrics(work); err != nil {
			w.IncrementErrors()

			// Check if pod is still running
			if !w.k8s.IsPodRunningByName(context.Background(), work.Namespace, work.PodName) {
				logger.Logger.Info("Pod stopped, ending collection",
					zap.String("worker_id", w.ID()), zap.String("pod", work.PodName))
				w.MarkPodUnassigned(podKey)
				return
			}

			logger.Logger.Error("Metrics collection failed",
				zap.String("worker_id", w.ID()), zap.String("pod", work.PodName), zap.Error(err))
		}
	}
}
