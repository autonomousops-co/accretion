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

// pvcStatusWorker watches Kubernetes PVC and PV statuses and stores them in the vector database.
type pvcStatusWorker struct {
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

// NewPVCStatusWorker creates a new PVC/PV status worker that watches Kubernetes volume conditions.
func NewPVCStatusWorker(
	k8s k8s.Interface, db database.Interface, llm llm.Client,
	resync time.Duration, scope nsscope.Scope,
) PVCStatusWorker {
	return &pvcStatusWorker{
		id:         fmt.Sprintf("pvc-status-%s", uuid.New().String()),
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

func (w *pvcStatusWorker) ID() string                { return w.id }
func (w *pvcStatusWorker) Type() string              { return "pvc-status" }
func (w *pvcStatusWorker) IsHealthy() bool           { return true }
func (w *pvcStatusWorker) GetAssignedPods() []string { return nil }
func (w *pvcStatusWorker) GetErrorCount() int        { return 0 }

func (w *pvcStatusWorker) StartHealthReporting(ctx context.Context) error {
	// PVC status worker does not register with Redis health system since it is
	// not queue-driven and does not receive pod assignments.
	return nil
}

func (w *pvcStatusWorker) Start(ctx context.Context) error {
	logger.Logger.Info("Starting PVC status worker",
		zap.String("worker_id", w.id),
		zap.Duration("resync_interval", w.resync),
		zap.Strings("namespaces", w.scope.Namespaces()))

	w.startStatsReporting(ctx)

	// Watch-driven since #96; see internal/k8s/statusinformers.go. A failed
	// informer is a returned error, which the supervisor restarts.
	err := w.k8s.WatchVolumeStatuses(ctx, w.resync, staticNamespaces(w.scope), w.scope.Contains,
		func(status *k8s.PVCStatusEntry) {
			if err := w.processPVCStatus(ctx, status); err != nil {
				logger.Logger.Error("Failed to process PVC status",
					zap.String("worker_id", w.id), zap.String("pvc", status.PVCName), zap.Error(err))
			}
		}, func(status *k8s.PVStatusEntry) {
			if err := w.processPVStatus(ctx, status); err != nil {
				logger.Logger.Error("Failed to process PV status",
					zap.String("worker_id", w.id), zap.String("pv", status.PVName), zap.Error(err))
			}
		})

	logger.Logger.Info("PVC status worker stopping", zap.String("worker_id", w.id), zap.Error(err))
	return err
}

func (w *pvcStatusWorker) Stop(_ context.Context) error {
	logger.Logger.Info("PVC status worker stopped", zap.String("worker_id", w.id))
	return nil
}

func (w *pvcStatusWorker) processPVCStatus(ctx context.Context, status *k8s.PVCStatusEntry) error {
	conditionsJSON, err := json.Marshal(status.Conditions)
	if err != nil {
		return fmt.Errorf("failed to marshal conditions: %w", err)
	}
	accessModesJSON, err := json.Marshal(status.AccessModes)
	if err != nil {
		return fmt.Errorf("failed to marshal access modes: %w", err)
	}

	hashInput := fmt.Sprintf("pvc|%s|%s|%s|%s|%s|%s|%s",
		status.Namespace, status.PVCName, status.Phase,
		status.VolumeName, status.StorageClass,
		string(conditionsJSON), string(accessModesJSON))
	contentHash := fmt.Sprintf("%x", sha256.Sum256([]byte(hashInput)))

	if w.seenHash.contains(contentHash) {
		return nil
	}
	w.seenHash.add(contentHash)

	w.stats.RecordEventIngestion(status.Namespace)

	embeddingText := buildPVCStatusEmbeddingText(status)

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

	volumeInfo, err := json.Marshal(map[string]interface{}{
		"volume_name":   status.VolumeName,
		"storage_class": status.StorageClass,
		"access_modes":  status.AccessModes,
		"capacity":      status.Capacity,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal volume info: %w", err)
	}

	data, jsonErr := json.Marshal(map[string]interface{}{
		"phase":       status.Phase,
		"conditions":  string(conditionsJSON),
		"volume_info": string(volumeInfo),
	})
	if jsonErr != nil {
		return fmt.Errorf("failed to marshal PVC status data: %w", jsonErr)
	}

	dbEntry := &database.UnifiedEntry{
		ResourceType: "pvc_status",
		Namespace:    status.Namespace,
		ResourceName: status.PVCName,
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

func (w *pvcStatusWorker) processPVStatus(ctx context.Context, status *k8s.PVStatusEntry) error {
	hashInput := fmt.Sprintf("pv|%s|%s|%s|%s|%s|%s|%s",
		status.PVName, status.Phase, status.ReclaimPolicy,
		status.StorageClass, status.ClaimRef,
		status.Reason, status.Message)
	contentHash := fmt.Sprintf("%x", sha256.Sum256([]byte(hashInput)))

	if w.seenHash.contains(contentHash) {
		return nil
	}
	w.seenHash.add(contentHash)

	w.stats.RecordEventIngestion("cluster")

	embeddingText := buildPVStatusEmbeddingText(status)

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

	volumeInfo, err := json.Marshal(map[string]interface{}{
		"reclaim_policy": status.ReclaimPolicy,
		"storage_class":  status.StorageClass,
		"capacity":       status.Capacity,
		"claim_ref":      status.ClaimRef,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal volume info: %w", err)
	}

	statusInfo, err := json.Marshal(map[string]interface{}{
		"reason":  status.Reason,
		"message": status.Message,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal status info: %w", err)
	}

	pvData, jsonErr := json.Marshal(map[string]interface{}{
		"phase":       status.Phase,
		"volume_info": string(volumeInfo),
		"status_info": string(statusInfo),
	})
	if jsonErr != nil {
		return fmt.Errorf("failed to marshal PV status data: %w", jsonErr)
	}

	dbEntry := &database.UnifiedEntry{
		ResourceType: "pv_status",
		Namespace:    "",
		ResourceName: status.PVName,
		Data:         pvData,
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

func buildPVCStatusEmbeddingText(status *k8s.PVCStatusEntry) string {
	text := fmt.Sprintf("PVC Status: %s/%s, Phase: %s",
		status.Namespace, status.PVCName, status.Phase)

	if status.Phase == "Pending" {
		text += ", PVC stuck in Pending (may block pod scheduling)"
	} else if status.Phase == "Lost" {
		text += ", PVC lost (bound volume no longer exists)"
	}

	if status.StorageClass != "" {
		text += fmt.Sprintf(", StorageClass: %s", status.StorageClass)
	}
	if status.Capacity != "" {
		text += fmt.Sprintf(", Capacity: %s", status.Capacity)
	}
	if status.VolumeName != "" {
		text += fmt.Sprintf(", Volume: %s", status.VolumeName)
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

func buildPVStatusEmbeddingText(status *k8s.PVStatusEntry) string {
	text := fmt.Sprintf("PV Status: %s, Phase: %s",
		status.PVName, status.Phase)

	if status.Phase == "Failed" {
		text += ", PV in failed state"
	} else if status.Phase == "Released" {
		text += ", PV released (data may need manual cleanup)"
	}

	if status.ReclaimPolicy != "" {
		text += fmt.Sprintf(", ReclaimPolicy: %s", status.ReclaimPolicy)
	}
	if status.StorageClass != "" {
		text += fmt.Sprintf(", StorageClass: %s", status.StorageClass)
	}
	if status.Capacity != "" {
		text += fmt.Sprintf(", Capacity: %s", status.Capacity)
	}
	if status.ClaimRef != "" {
		text += fmt.Sprintf(", ClaimRef: %s", status.ClaimRef)
	}
	if status.Reason != "" {
		text += fmt.Sprintf(", Reason: %s", status.Reason)
	}
	if status.Message != "" {
		text += fmt.Sprintf(", Message: %s", status.Message)
	}

	return text
}

func (w *pvcStatusWorker) startStatsReporting(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				logger.Logger.Info("PVC status worker stats",
					zap.String("worker_id", w.id),
					zap.Any("stats", w.stats.GetStats()))
			}
		}
	}()
}
