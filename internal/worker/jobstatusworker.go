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

// jobStatusWorker watches Kubernetes Job and CronJob statuses and stores them in the vector database.
type jobStatusWorker struct {
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

// NewJobStatusWorker creates a new Job/CronJob status worker that watches batch job conditions.
func NewJobStatusWorker(
	k8s k8s.Interface, db database.Interface, llm llm.Client,
	resync time.Duration, scope nsscope.Scope,
) JobStatusWorker {
	return &jobStatusWorker{
		id:         fmt.Sprintf("job-status-%s", uuid.New().String()),
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

func (w *jobStatusWorker) ID() string                { return w.id }
func (w *jobStatusWorker) Type() string              { return "job-status" }
func (w *jobStatusWorker) IsHealthy() bool           { return true }
func (w *jobStatusWorker) GetAssignedPods() []string { return nil }
func (w *jobStatusWorker) GetErrorCount() int        { return 0 }

func (w *jobStatusWorker) StartHealthReporting(ctx context.Context) error {
	// Job status worker does not register with Redis health system since it is
	// not queue-driven and does not receive pod assignments.
	return nil
}

func (w *jobStatusWorker) Start(ctx context.Context) error {
	logger.Logger.Info("Starting job status worker",
		zap.String("worker_id", w.id),
		zap.Duration("resync_interval", w.resync),
		zap.Strings("namespaces", w.scope.Namespaces()))

	w.startStatsReporting(ctx)

	// Watch-driven since #96; see internal/k8s/statusinformers.go. A failed
	// informer is a returned error, which the supervisor restarts.
	err := w.k8s.WatchJobStatuses(ctx, w.resync, staticNamespaces(w.scope), w.scope.Contains,
		func(status *k8s.JobStatusEntry) {
			if err := w.processJobStatus(ctx, status); err != nil {
				logger.Logger.Error("Failed to process Job status",
					zap.String("worker_id", w.id), zap.String("job", status.JobName), zap.Error(err))
			}
		}, func(status *k8s.CronJobStatusEntry) {
			if err := w.processCronJobStatus(ctx, status); err != nil {
				logger.Logger.Error("Failed to process CronJob status",
					zap.String("worker_id", w.id), zap.String("cronjob", status.CronJobName), zap.Error(err))
			}
		})

	logger.Logger.Info("Job status worker stopping", zap.String("worker_id", w.id), zap.Error(err))
	return err
}

func (w *jobStatusWorker) Stop(_ context.Context) error {
	logger.Logger.Info("Job status worker stopped", zap.String("worker_id", w.id))
	return nil
}

func (w *jobStatusWorker) processJobStatus(ctx context.Context, status *k8s.JobStatusEntry) error {
	conditionsJSON, err := json.Marshal(status.Conditions)
	if err != nil {
		return fmt.Errorf("failed to marshal conditions: %w", err)
	}

	hashInput := fmt.Sprintf("job|%s|%s|%d|%d|%d|%v|%s",
		status.Namespace, status.JobName,
		status.Active, status.Succeeded, status.Failed,
		status.DeadlineExceeded, string(conditionsJSON))
	contentHash := fmt.Sprintf("%x", sha256.Sum256([]byte(hashInput)))

	if w.seenHash.contains(contentHash) {
		return nil
	}
	w.seenHash.add(contentHash)

	w.stats.RecordEventIngestion(status.Namespace)

	embeddingText := buildJobStatusEmbeddingText(status)

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

	statusInfo, err := json.Marshal(map[string]interface{}{
		"active":            status.Active,
		"succeeded":         status.Succeeded,
		"failed":            status.Failed,
		"duration":          status.Duration.String(),
		"deadline_exceeded": status.DeadlineExceeded,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal status info: %w", err)
	}

	data, jsonErr := json.Marshal(map[string]interface{}{
		"conditions":  string(conditionsJSON),
		"status_info": string(statusInfo),
	})
	if jsonErr != nil {
		return fmt.Errorf("failed to marshal Job status data: %w", jsonErr)
	}

	dbEntry := &database.UnifiedEntry{
		ResourceType: "job_status",
		Namespace:    status.Namespace,
		ResourceName: status.JobName,
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

func (w *jobStatusWorker) processCronJobStatus(ctx context.Context, status *k8s.CronJobStatusEntry) error {
	hashInput := fmt.Sprintf("cronjob|%s|%s|%s|%d|%v|%v",
		status.Namespace, status.CronJobName, status.Schedule,
		status.ActiveJobs, status.Suspended, status.MissedSchedule)

	// Include last schedule time in hash if available.
	if status.LastScheduleTime != nil {
		hashInput += fmt.Sprintf("|%d", status.LastScheduleTime.Unix())
	}

	contentHash := fmt.Sprintf("%x", sha256.Sum256([]byte(hashInput)))

	if w.seenHash.contains(contentHash) {
		return nil
	}
	w.seenHash.add(contentHash)

	w.stats.RecordEventIngestion(status.Namespace)

	embeddingText := buildCronJobStatusEmbeddingText(status)

	var embedding []float32
	var embedErr error

	err := w.llmBreaker.Execute(ctx, func(ctx context.Context) error {
		return resilience.WithRetry(ctx, resilience.DefaultRetryConfig(), func(ctx context.Context) error {
			embedding, embedErr = w.llm.GenerateEmbedding(ctx, embeddingText)
			return embedErr
		})
	})
	if err != nil {
		return fmt.Errorf("embedding generation failed: %w", err)
	}

	scheduleInfo, err := json.Marshal(map[string]interface{}{
		"schedule":  status.Schedule,
		"suspended": status.Suspended,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal schedule info: %w", err)
	}

	statusInfoMap := map[string]interface{}{
		"active_jobs":     status.ActiveJobs,
		"missed_schedule": status.MissedSchedule,
	}
	if status.LastScheduleTime != nil {
		statusInfoMap["last_schedule_time"] = *status.LastScheduleTime
	}
	if status.LastSuccessfulTime != nil {
		statusInfoMap["last_successful_time"] = *status.LastSuccessfulTime
	}

	statusInfo, err := json.Marshal(statusInfoMap)
	if err != nil {
		return fmt.Errorf("failed to marshal status info: %w", err)
	}

	cronData, jsonErr := json.Marshal(map[string]interface{}{
		"schedule_info": string(scheduleInfo),
		"status_info":   string(statusInfo),
	})
	if jsonErr != nil {
		return fmt.Errorf("failed to marshal CronJob status data: %w", jsonErr)
	}

	dbEntry := &database.UnifiedEntry{
		ResourceType: "cronjob_status",
		Namespace:    status.Namespace,
		ResourceName: status.CronJobName,
		Data:         cronData,
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

func buildJobStatusEmbeddingText(status *k8s.JobStatusEntry) string {
	text := fmt.Sprintf("Job Status: %s/%s, Active: %d, Succeeded: %d, Failed: %d",
		status.Namespace, status.JobName,
		status.Active, status.Succeeded, status.Failed)

	if status.DeadlineExceeded {
		text += ", deadline exceeded"
	}

	if status.Failed > 0 {
		text += ", job has failures"
	}

	if status.Duration > 0 {
		text += fmt.Sprintf(", Duration: %s", status.Duration)
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

	return text
}

func buildCronJobStatusEmbeddingText(status *k8s.CronJobStatusEntry) string {
	text := fmt.Sprintf("CronJob Status: %s/%s, Schedule: %s",
		status.Namespace, status.CronJobName, status.Schedule)

	if status.Suspended {
		text += ", suspended"
	}

	if status.MissedSchedule {
		text += ", missed schedule detected"
	}

	text += fmt.Sprintf(", ActiveJobs: %d", status.ActiveJobs)

	if status.LastScheduleTime != nil {
		text += fmt.Sprintf(", LastScheduled: %s", status.LastScheduleTime.Format(time.RFC3339))
	}

	if status.LastSuccessfulTime != nil {
		text += fmt.Sprintf(", LastSuccess: %s", status.LastSuccessfulTime.Format(time.RFC3339))
	}

	return text
}

func (w *jobStatusWorker) startStatsReporting(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				logger.Logger.Info("Job status worker stats",
					zap.String("worker_id", w.id),
					zap.Any("stats", w.stats.GetStats()))
			}
		}
	}()
}
