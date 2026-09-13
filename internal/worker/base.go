package worker

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"go.opentelemetry.io/otel/trace"

	"github.com/autonomousops-co/accretion/internal/logger"
	"github.com/autonomousops-co/accretion/internal/nsscope"
	"github.com/autonomousops-co/accretion/internal/queue"
	"github.com/autonomousops-co/accretion/internal/resilience"
	"github.com/autonomousops-co/accretion/internal/telemetry"
)

// BaseWorker contains common functionality for all worker types.
type BaseWorker struct {
	queue        queue.Consumer
	assignedPods map[string]bool
	ticker       *time.Ticker
	dbBreaker    *resilience.CircuitBreaker
	llmBreaker   *resilience.CircuitBreaker
	stats        *WorkerStats
	id           string
	workerType   string
	scope        nsscope.Scope
	errorCount   int
	mu           sync.RWMutex
	healthy      bool
}

// NewBaseWorker creates a new base worker with common setup. scope is the
// worker's live namespace scope; its current namespace set drives active
// verification. It is read at the point of use rather than snapshotted into a
// field, so a scope that re-resolves label selectors in the background (#152) is
// reflected without restarting the worker.
func NewBaseWorker(workerType string, queue queue.Consumer, scope nsscope.Scope) *BaseWorker {
	return &BaseWorker{
		id:           uuid.New().String(),
		workerType:   workerType,
		queue:        queue,
		scope:        scope,
		assignedPods: make(map[string]bool),
		healthy:      true,
		dbBreaker:    resilience.NewCircuitBreaker(resilience.DefaultCircuitBreakerConfig()),
		llmBreaker:   resilience.NewCircuitBreaker(resilience.DefaultCircuitBreakerConfig()),
		stats:        NewWorkerStats(),
	}
}

// ID returns the worker's unique identifier.
func (b *BaseWorker) ID() string { return b.id }

// Type returns the worker type.
func (b *BaseWorker) Type() string { return b.workerType }

// IsHealthy returns the current health status.
func (b *BaseWorker) IsHealthy() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.healthy
}

// GetAssignedPods returns a copy of currently assigned pods.
func (b *BaseWorker) GetAssignedPods() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()

	pods := make([]string, 0, len(b.assignedPods))
	for podKey := range b.assignedPods {
		pods = append(pods, podKey)
	}
	return pods
}

// GetErrorCount returns the current error count.
func (b *BaseWorker) GetErrorCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.errorCount
}

// MarkPodAssigned marks a pod as assigned to this worker.
func (b *BaseWorker) MarkPodAssigned(podKey string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.assignedPods[podKey] = true
}

// MarkPodUnassigned removes a pod from this worker's assignment.
func (b *BaseWorker) MarkPodUnassigned(podKey string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.assignedPods, podKey)
}

// IncrementErrors increments the error counter.
func (b *BaseWorker) IncrementErrors() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.errorCount++
}

// StartHealthReporting starts periodic health reporting to Redis.
// It reports health immediately so the controller can discover this worker
// before the first tick, preventing work from being routed to the fallback queue.
func (b *BaseWorker) StartHealthReporting(ctx context.Context) error {
	// Report health immediately so the controller can discover this worker
	b.reportHealth()

	b.ticker = time.NewTicker(30 * time.Second)

	go func() {
		defer b.ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-b.ticker.C:
				b.reportHealth()
			}
		}
	}()

	logger.Logger.Info("Started health reporting", zap.String("worker_id", b.id), zap.String("type", b.workerType))
	return nil
}

// reportHealth sends health status to Redis.
func (b *BaseWorker) reportHealth() {
	health := &queue.WorkerHealth{
		WorkerID:     b.id,
		Type:         b.workerType,
		Healthy:      b.IsHealthy(),
		LastSeen:     time.Now(),
		AssignedPods: b.GetAssignedPods(),
		ErrorCount:   b.GetErrorCount(),
	}

	if storage, ok := b.queue.(queue.HealthStorage); ok {
		if err := storage.StoreWorkerHealth(context.Background(), health); err != nil {
			logger.Logger.Error("Failed to store health report", zap.String("worker_id", b.id), zap.Error(err))
		}
	}
}

// StorePodAssignment stores pod assignment in Redis.
func (b *BaseWorker) StorePodAssignment(podKey string) {
	assignment := &queue.PodAssignment{
		PodKey:     podKey,
		WorkerID:   b.id,
		WorkerType: b.workerType,
		AssignedAt: time.Now(),
		LastSeen:   time.Now(),
	}

	if storage, ok := b.queue.(queue.HealthStorage); ok {
		if err := storage.StorePodAssignment(context.Background(), assignment); err != nil {
			logger.Logger.Error("Failed to store pod assignment",
				zap.String("worker_id", b.id), zap.String("pod_key", podKey), zap.Error(err))
		}
	}
}

// Cleanup performs worker shutdown cleanup.
func (b *BaseWorker) Cleanup(ctx context.Context) error {
	logger.Logger.Info("Cleaning up worker", zap.String("worker_id", b.id), zap.String("type", b.workerType))

	if b.ticker != nil {
		b.ticker.Stop()
	}

	// Clean up health data and queue
	if storage, ok := b.queue.(queue.HealthStorage); ok {
		if err := storage.DeleteWorkerHealth(ctx, b.id); err != nil {
			logger.Logger.Warn("Failed to delete worker health", zap.String("worker_id", b.id), zap.Error(err))
		}
	}

	// Clean up worker queue
	type QueueCleaner interface {
		CleanupWorkerQueue(ctx context.Context, workerID string) error
	}
	if cleaner, ok := b.queue.(QueueCleaner); ok {
		if err := cleaner.CleanupWorkerQueue(ctx, b.id); err != nil {
			logger.Logger.Warn("Failed to cleanup worker queue", zap.String("worker_id", b.id), zap.Error(err))
		}
	}

	return b.queue.Close()
}

// VerifyNamespaces checks whether each namespace in scoped has received data,
// returning those that have not. The caller passes the namespace set rather than
// having this re-read the scope: the scope refreshes in the background (#152),
// so a second read could return a different set than the caller already acted
// on, and the two would disagree about what was checked.
func (b *BaseWorker) VerifyNamespaces(scoped []string) []string {
	stats := b.stats.GetStats()
	missingNamespaces := make([]string, 0)

	for _, ns := range scoped {
		if _, ok := stats[ns]; !ok {
			missingNamespaces = append(missingNamespaces, ns)
		}
	}
	return missingNamespaces
}

// StartStatsReporting starts periodic stats reporting to the logger.
func (b *BaseWorker) StartStatsReporting(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)

	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				statsStr := b.stats.String()
				scoped := b.scope.Namespaces()

				// A worker scoped to zero namespaces ingests nothing; surface it
				// as a warning rather than reporting "verification passed". Do
				// NOT prune stats here: this branch fails open (an empty scope lets
				// the log worker ingest all namespaces), so pruning against an empty
				// keep set would wipe legitimate stats.
				if len(scoped) == 0 {
					logger.Logger.Warn("worker scoped to zero namespaces — ingesting nothing; check namespaces.include/exclude",
						zap.String("worker_id", b.id),
						zap.String("type", b.workerType),
						zap.String("stats", statsStr),
					)
					continue
				}

				// Active verification against the SAME set the guard above saw.
				missingNamespaces := b.VerifyNamespaces(scoped)

				if len(missingNamespaces) > 0 {
					logger.Logger.Warn("Active verification failed: Missing data for namespaces",
						zap.String("worker_id", b.id),
						zap.String("type", b.workerType),
						zap.Strings("missing_namespaces", missingNamespaces),
						zap.String("stats", statsStr),
					)
				} else {
					logger.Logger.Info("Active verification passed",
						zap.String("worker_id", b.id),
						zap.String("type", b.workerType),
						zap.Int("checked_namespaces", len(scoped)),
						zap.String("stats", statsStr),
					)
				}

				// Bound the stats map to the current scope, reusing the SAME
				// scoped set verified above (re-reading b.scope could return a
				// different set after a background refresh). In-scope-but-idle
				// namespaces stay in scoped, so their "missing data" signal is
				// preserved; only namespaces that dropped OUT of scope are pruned.
				b.stats.RetainNamespaces(scoped)
			}
		}
	}()

	logger.Logger.Info("Started stats reporting", zap.String("worker_id", b.id), zap.String("type", b.workerType))
}

// StartSpan creates a new span for the worker operation.
func (b *BaseWorker) StartSpan(ctx context.Context, operation string) (context.Context, trace.Span) {
	return telemetry.StartSpan(ctx, "worker."+b.workerType, operation)
}

// staticNamespaces returns the scope's namespaces when the scope is an
// explicit list, which lets the k8s layer open one informer per namespace and
// cache only those (#96); nil for a label or "*" scope, whose membership
// changes and which is watched cluster-wide and filtered per object instead.
func staticNamespaces(scope nsscope.Scope) []string {
	if scope.IsStatic() {
		return scope.Namespaces()
	}
	return nil
}
