package collector

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/autonomousops-co/accretion/internal/k8s"
	"github.com/autonomousops-co/accretion/internal/logger"
	"github.com/autonomousops-co/accretion/internal/nsscope"
	"github.com/autonomousops-co/accretion/internal/queue"
)

// Controller is a stateless Kubernetes controller that watches Pods and publishes work to Redis.
type Controller struct {
	k8sClient    k8s.Interface
	queueClient  queue.Client
	podInformer  k8s.PodInformer
	workqueue    workqueue.TypedRateLimitingInterface[string]
	distributor  *WorkDistributor
	ctx          context.Context
	cancel       context.CancelFunc
	healthTicker *time.Ticker
	scope        nsscope.Scope
	logScope     nsscope.Scope
	numWorkers   int
	workerPeriod time.Duration
}

// NewController builds the pod-discovery controller. scope covers pod watching,
// metrics, and status dispatch (K8sNamespaceSelector — includes accretion's own
// namespace). logScope covers LOG-collection dispatch only (LogNamespaceSelector
// — self-excludes accretion's namespace) so accretion never ingests its own log lines and
// re-opens the amplification loop. See issue #145.
//
// Both scopes are read live on every check rather than snapshotted here, so
// background re-resolution (#152) takes effect without restarting the collector.
// Those reads happen inside informer event handlers, which is why a Scope read
// must never block on an API call.
func NewController(
	k8sClient k8s.Interface,
	queueClient queue.Client,
	scope nsscope.Scope,
	logScope nsscope.Scope,
	numWorkers int,
	workerPeriod time.Duration,
) (*Controller, error) {
	podInformer, err := k8sClient.CreatePodInformer()
	if err != nil {
		return nil, fmt.Errorf("failed to create pod informer: %w", err)
	}

	controller := &Controller{
		k8sClient:    k8sClient,
		queueClient:  queueClient,
		podInformer:  podInformer,
		workqueue:    workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		distributor:  NewWorkDistributor(queueClient),
		scope:        scope,
		logScope:     logScope,
		numWorkers:   numWorkers,
		workerPeriod: workerPeriod,
	}

	// Set up event handlers
	if err := podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.handlePodAdd,
		UpdateFunc: func(old, new any) {
			controller.handlePodUpdate(old, new)
		},
		DeleteFunc: controller.handlePodDelete,
	}); err != nil {
		return nil, fmt.Errorf("failed to add event handlers: %w", err)
	}

	return controller, nil
}

// Start begins the controller's operation.
func (c *Controller) Start(ctx context.Context) error {
	defer runtime.HandleCrash()

	logger.Logger.Info("Starting controller", zap.Strings("namespaces", c.scope.Namespaces()))

	c.ctx, c.cancel = context.WithCancel(ctx)

	// Start the informer
	go c.podInformer.Run(ctx.Done())

	// Wait for cache sync
	logger.Logger.Info("Waiting for informer cache to sync")
	if !cache.WaitForCacheSync(ctx.Done(), c.podInformer.HasSynced) {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	logger.Logger.Info("Controller cache synced, starting workers")

	// Start multiple workers to process work items
	for i := 0; i < c.numWorkers; i++ {
		go wait.Until(func() {
			for c.processNextWorkItem() {
				// Keep processing until processNextWorkItem returns false
			}
		}, c.workerPeriod, ctx.Done())
	}

	// Start health monitoring
	c.startHealthMonitoring(ctx)

	logger.Logger.Info("Controller started successfully",
		zap.Int("workers", c.numWorkers),
		zap.Duration("worker_period", c.workerPeriod))
	return nil
}

// Stop gracefully shuts down the controller.
func (c *Controller) Stop(ctx context.Context) error {
	logger.Logger.Info("Stopping controller")

	if c.cancel != nil {
		c.cancel()
	}

	if c.healthTicker != nil {
		c.healthTicker.Stop()
	}

	// Shutdown the workqueue to stop workers
	c.workqueue.ShutDown()

	logger.Logger.Info("Controller stopped")
	return nil
}

// startHealthMonitoring starts periodic health monitoring of workers
func (c *Controller) startHealthMonitoring(ctx context.Context) {
	c.healthTicker = time.NewTicker(30 * time.Second)

	go func() {
		defer c.healthTicker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-c.healthTicker.C:
				c.checkWorkerHealth()
			}
		}
	}()

	logger.Logger.Info("Started worker health monitoring")

	// Start fallback queue retry mechanism
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Logger.Error("Fallback queue retry goroutine panicked", zap.Any("panic", r))
			}
		}()

		ticker := time.NewTicker(10 * time.Second) // Check fallback queue every 10 seconds
		defer ticker.Stop()

		for {
			select {
			case <-c.ctx.Done():
				logger.Logger.Info("Stopping fallback queue retry mechanism")
				return
			case <-ticker.C:
				c.processFallbackQueues()
			}
		}
	}()

	logger.Logger.Info("Started fallback queue retry mechanism")

	// Start periodic full sync to catch pods that were missed during startup
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Logger.Error("Periodic sync goroutine panicked", zap.Any("panic", r))
			}
		}()

		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-c.ctx.Done():
				logger.Logger.Info("Stopping periodic sync mechanism")
				return
			case <-ticker.C:
				c.syncUnassignedPods()
			}
		}
	}()

	logger.Logger.Info("Started periodic pod sync mechanism")
}

// checkWorkerHealth checks worker health and handles failures
func (c *Controller) checkWorkerHealth() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Get all data in parallel
	workerHealth, podAssignments, err := c.getHealthData(ctx)
	if err != nil {
		logger.Logger.Error("Failed to get health data", zap.Error(err))
		return
	}

	// Find dead workers
	deadWorkers := c.findDeadWorkers(workerHealth)

	// Reassign pods from dead workers sequentially
	c.reassignDeadWorkers(deadWorkers, podAssignments)

	// Clean up stale assignments for non-existent pods or unknown workers
	c.cleanupStaleAssignments(ctx, podAssignments, workerHealth)

	// Log detailed health summary
	logWorkers := 0
	metricsWorkers := 0
	logAssignments := 0
	metricsAssignments := 0

	for _, health := range workerHealth {
		switch health.Type {
		case queue.LogsWorkerType:
			logWorkers++
			logAssignments += len(health.AssignedPods)
		case queue.MetricsWorkerType:
			metricsWorkers++
			metricsAssignments += len(health.AssignedPods)
		}
	}

	logger.Logger.Debug("Worker health check completed",
		zap.Int("total_workers", len(workerHealth)),
		zap.Int("log_workers", logWorkers),
		zap.Int("metrics_workers", metricsWorkers),
		zap.Int("dead_workers", len(deadWorkers)),
		zap.Int("total_assignments", len(podAssignments)),
		zap.Int("log_assignments", logAssignments),
		zap.Int("metrics_assignments", metricsAssignments),
	)
}

// getHealthData fetches worker health and pod assignments in parallel
func (c *Controller) getHealthData(ctx context.Context) (
	map[string]*queue.WorkerHealth, map[string]*queue.PodAssignment, error) {
	var wg sync.WaitGroup
	var workerHealth map[string]*queue.WorkerHealth
	var podAssignments map[string]*queue.PodAssignment
	var workerErr, assignmentErr error

	// Fetch worker health concurrently
	wg.Add(1)
	go func() {
		defer wg.Done()
		workerHealth, workerErr = c.queueClient.GetAllWorkerHealth(ctx)
	}()

	// Fetch pod assignments concurrently
	wg.Add(1)
	go func() {
		defer wg.Done()
		podAssignments, assignmentErr = c.queueClient.GetAllPodAssignments(ctx)
	}()

	// Wait for both operations to complete
	wg.Wait()

	// Check for errors
	if workerErr != nil {
		return nil, nil, fmt.Errorf("failed to get worker health: %w", workerErr)
	}
	if assignmentErr != nil {
		return nil, nil, fmt.Errorf("failed to get pod assignments: %w", assignmentErr)
	}

	return workerHealth, podAssignments, nil
}

// findDeadWorkers identifies workers that haven't been seen recently
func (c *Controller) findDeadWorkers(workerHealth map[string]*queue.WorkerHealth) []string {
	now := time.Now()
	deadWorkers := make([]string, 0, len(workerHealth))

	for workerID, health := range workerHealth {
		// Consider worker dead if not seen in last 60 seconds
		if now.Sub(health.LastSeen) > 60*time.Second {
			deadWorkers = append(deadWorkers, workerID)
			logger.Logger.Warn("Worker appears dead",
				zap.String("worker_id", workerID),
				zap.String("type", health.Type),
				zap.Time("last_seen", health.LastSeen),
				zap.Int("assigned_pods", len(health.AssignedPods)),
			)
		}
	}

	return deadWorkers
}

// reassignDeadWorkers reassigns pods from multiple dead workers sequentially
func (c *Controller) reassignDeadWorkers(deadWorkers []string,
	podAssignments map[string]*queue.PodAssignment) {
	if len(deadWorkers) == 0 {
		return
	}

	// Process each dead worker sequentially
	for _, deadWorkerID := range deadWorkers {
		c.reassignDeadWorkerPods(deadWorkerID, podAssignments)
	}
}

// reassignDeadWorkerPods reassigns pods from a dead worker
func (c *Controller) reassignDeadWorkerPods(deadWorkerID string,
	podAssignments map[string]*queue.PodAssignment) {
	// Find pods assigned to the dead worker
	podsToReassign := make([]string, 0)
	for podKey, assignment := range podAssignments {
		if assignment.WorkerID == deadWorkerID {
			podsToReassign = append(podsToReassign, podKey)
		}
	}

	if len(podsToReassign) == 0 {
		return
	}

	logger.Logger.Info("Reassigning pods from dead worker",
		zap.String("dead_worker_id", deadWorkerID),
		zap.Strings("pods_to_reassign", podsToReassign),
	)

	// Process pod reassignments
	for _, podKey := range podsToReassign {
		c.reassignSinglePod(podKey, deadWorkerID)
	}
}

// reassignSinglePod handles the reassignment of a single pod
func (c *Controller) reassignSinglePod(podKey, deadWorkerID string) {
	// Remove old assignments for both worker types
	if err := c.queueClient.DeletePodAssignment(context.Background(), podKey); err != nil {
		logger.Logger.Warn("Failed to delete metrics assignment for reassignment", zap.String("pod", podKey), zap.Error(err))
	}

	// Republish work to queue for reassignment
	c.workqueue.Add(fmt.Sprintf("pod-running/%s", podKey))

	logger.Logger.Info("Reassigned pod from dead worker",
		zap.String("pod_key", podKey),
		zap.String("dead_worker_id", deadWorkerID),
	)
}

// cleanupStaleAssignments removes assignments for pods that no longer exist
// or assignments pointing to workers that are no longer registered.
func (c *Controller) cleanupStaleAssignments(
	ctx context.Context,
	podAssignments map[string]*queue.PodAssignment,
	workerHealth map[string]*queue.WorkerHealth,
) {
	if len(podAssignments) == 0 {
		return
	}

	// Build set of known worker IDs
	knownWorkers := make(map[string]bool)
	for workerID := range workerHealth {
		knownWorkers[workerID] = true
	}

	staleCount := 0
	for _, assignment := range podAssignments {
		shouldDelete := false

		// Check if pod still exists in informer cache
		_, exists, err := c.podInformer.GetStore().GetByKey(assignment.PodKey)
		if err != nil {
			logger.Logger.Warn("Error checking pod existence", zap.String("pod_key", assignment.PodKey), zap.Error(err))
			continue
		}

		if !exists {
			shouldDelete = true
		} else if !knownWorkers[assignment.WorkerID] {
			// Pod exists but assigned worker is gone (stale from previous run)
			shouldDelete = true
		}

		if shouldDelete {
			if err := c.queueClient.DeletePodAssignment(ctx, assignment.PodKey); err != nil {
				logger.Logger.Warn("Failed to delete stale assignment", zap.String("pod_key", assignment.PodKey), zap.Error(err))
			} else {
				staleCount++
				logger.Logger.Debug("Cleaned up stale assignment",
					zap.String("pod_key", assignment.PodKey),
					zap.String("worker_id", assignment.WorkerID),
					zap.Bool("pod_exists", exists),
				)
			}
		}
	}

	if staleCount > 0 {
		logger.Logger.Info("Cleaned up stale assignments", zap.Int("count", staleCount))
	}
}

// Event handlers for pod informer - stateless, just publish events
func (c *Controller) handlePodAdd(obj any) {
	podInfo, err := c.k8sClient.ExtractPodInfo(obj)
	if err != nil {
		logger.Logger.Error("Failed to extract pod info", zap.Error(err))
		return
	}

	// Only handle pods in our configured namespaces
	if !c.isNamespaceWatched(podInfo.Namespace) {
		return
	}

	if c.k8sClient.IsPodRunning(podInfo) {
		key, err := c.k8sClient.GeneratePodKey(podInfo)
		if err != nil {
			runtime.HandleError(err)
			return
		}
		logger.Logger.Debug("Pod added and running - queuing for work",
			zap.String("pod", key),
			zap.String("phase", podInfo.Phase),
			zap.String("namespace", podInfo.Namespace),
			zap.String("pod_name", podInfo.Name))
		c.workqueue.Add(fmt.Sprintf("pod-running/%s", key))
	}
}

func (c *Controller) handlePodUpdate(old, newObj any) {
	newPodInfo, err := c.k8sClient.ExtractPodInfo(newObj)
	if err != nil {
		logger.Logger.Error("Failed to extract new pod info", zap.Error(err))
		return
	}

	// Only handle pods in our configured namespaces
	if !c.isNamespaceWatched(newPodInfo.Namespace) {
		return
	}

	oldPodInfo, err := c.k8sClient.ExtractPodInfo(old)
	if err != nil {
		logger.Logger.Error("Failed to extract old pod info", zap.Error(err))
		return
	}

	// Only care about phase changes
	if oldPodInfo.Phase != newPodInfo.Phase {
		key, err := c.k8sClient.GeneratePodKey(newPodInfo)
		if err != nil {
			runtime.HandleError(err)
			return
		}

		if c.k8sClient.IsPodRunning(newPodInfo) {
			logger.Logger.Debug("Pod transitioned to running - queuing for work",
				zap.String("pod", key),
				zap.String("old_phase", oldPodInfo.Phase),
				zap.String("new_phase", newPodInfo.Phase),
				zap.String("namespace", newPodInfo.Namespace),
				zap.String("pod_name", newPodInfo.Name))
			c.workqueue.Add(fmt.Sprintf("pod-running/%s", key))
		} else {
			// Pod is no longer running, clean up assignment
			logger.Logger.Debug("Pod phase changed to non-running, cleaning up assignment",
				zap.String("pod_key", key),
				zap.String("old_phase", oldPodInfo.Phase),
				zap.String("new_phase", newPodInfo.Phase))

			if err := c.queueClient.DeletePodAssignment(context.Background(), key); err != nil {
				logger.Logger.Warn("Failed to delete pod assignment on phase change",
					zap.String("pod_key", key), zap.String("new_phase", newPodInfo.Phase), zap.Error(err))
			} else {
				logger.Logger.Debug("Cleaned up pod assignment on phase change",
					zap.String("pod_key", key), zap.String("new_phase", newPodInfo.Phase))
			}
		}
	}
}

func (c *Controller) handlePodDelete(obj any) {
	podInfo, err := c.k8sClient.ExtractPodInfo(obj)
	if err != nil {
		logger.Logger.Error("Failed to extract pod info", zap.Error(err))
		return
	}

	// Only handle pods in our configured namespaces
	if !c.isNamespaceWatched(podInfo.Namespace) {
		return
	}

	// Clean up pod assignment when pod is deleted
	podKey, err := c.k8sClient.GeneratePodKey(podInfo)
	if err != nil {
		logger.Logger.Error("Failed to generate pod key for deletion", zap.String("pod", podInfo.Name), zap.Error(err))
		return
	}

	// Delete the pod assignment to prevent orphaned assignments
	logger.Logger.Debug("Pod deleted, cleaning up assignment",
		zap.String("pod_key", podKey), zap.String("pod_name", podInfo.Name))
	if err := c.queueClient.DeletePodAssignment(context.Background(), podKey); err != nil {
		logger.Logger.Warn("Failed to delete pod assignment on pod deletion",
			zap.String("pod_key", podKey), zap.Error(err))
	}
	logger.Logger.Debug("Pod deleted and cleaned up", zap.String("pod", podInfo.Name), zap.String("pod_key", podKey))
}

// Helper to check if we should watch this namespace
func (c *Controller) isNamespaceWatched(namespace string) bool {
	return c.scope.Contains(namespace)
}

// isLogNamespaceWatched reports whether LOG-collection work may be dispatched for
// pods in this namespace. It is a strict subset of isNamespaceWatched: accretion's own
// namespace is watched for metrics/status but excluded here so its log lines are
// never ingested (the amplification loop). See issue #145.
func (c *Controller) isLogNamespaceWatched(namespace string) bool {
	return c.logScope.Contains(namespace)
}

// dispatchLogWork publishes log-collection work for a pod, load-balanced across
// log workers with a fallback queue. It is a no-op for namespaces outside the
// log scope: accretion's own namespace is watched for metrics/status yet excluded
// here so the log worker never ingests accretion's own log lines and re-opens the
// amplification loop. This is the authoritative enforcement point for the log
// self-exclusion (issue #145); the log worker's handleWork gate is a
// belt-and-suspenders backstop. It returns a human-readable assignment label for
// logging and any hard publish error.
func (c *Controller) dispatchLogWork(podKey string, podInfo *k8s.PodInfo) (string, error) {
	if !c.isLogNamespaceWatched(podInfo.Namespace) {
		logger.Logger.Debug("Skipping log work dispatch for log-excluded namespace",
			zap.String("pod", podKey), zap.String("namespace", podInfo.Namespace))
		return "skipped_log_excluded_namespace", nil
	}

	containers := make([]string, len(podInfo.Containers))
	for i := range podInfo.Containers {
		containers[i] = podInfo.Containers[i].Name
	}
	logWork := &queue.LogCollectionWork{
		BaseWork: queue.BaseWork{
			PodUID:    podInfo.UID,
			PodName:   podInfo.Name,
			Namespace: podInfo.Namespace,
			NodeName:  podInfo.NodeName,
			Timestamp: time.Now(),
		},
		Containers: containers,
	}

	// Use load balancer to select optimal worker
	selectedWorkerID, err := c.distributor.DistributeWork(c.ctx, podKey, queue.LogsWorkerType)
	if err != nil {
		logger.Logger.Warn("No healthy log workers available, using fallback queue",
			zap.String("pod", podKey), zap.Error(err))
		return c.shedToFallback(podKey, queue.LogsWorkerType, logWork)
	}

	// Send to specific worker
	publishErr := c.queueClient.PublishLogWorkToWorker(c.ctx, logWork, selectedWorkerID)
	switch {
	case publishErr == nil:
		logger.Logger.Debug("Published log work to worker",
			zap.String("pod", podKey), zap.String("worker_id", selectedWorkerID))
		return selectedWorkerID, nil
	case errors.Is(publishErr, queue.ErrQueueFull):
		logger.Logger.Warn("Log worker queue is full, using fallback queue",
			zap.String("pod", podKey), zap.String("worker_id", selectedWorkerID))
		return c.shedToFallback(podKey, queue.LogsWorkerType, logWork)
	default:
		logger.Logger.Error("Failed to publish log work to worker",
			zap.String("pod", podKey), zap.String("worker_id", selectedWorkerID), zap.Error(publishErr))
		return "", publishErr
	}
}

func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()
	if shutdown {
		logger.Logger.Info("Workqueue shutdown detected")
		return false
	}

	defer c.workqueue.Done(obj)

	key := obj

	if err := c.processWorkItem(key); err != nil {
		// Requeue with rate limiting on error
		c.workqueue.AddRateLimited(key)
		logger.Logger.Warn("Work item failed, requeuing with rate limiting", zap.String("key", key), zap.Error(err))
		runtime.HandleError(fmt.Errorf("error processing '%s': %s", key, err.Error()))
		return true
	}

	// Successfully processed, remove from queue
	c.workqueue.Forget(obj)
	return true
}

func (c *Controller) processWorkItem(key string) error {
	// Parse key: "pod-running/namespace/podname"
	parts := strings.SplitN(key, "/", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid key format: %s", key)
	}

	eventType := parts[0]
	podKey := parts[1]

	switch eventType {
	case "pod-running":
		return c.publishPodRunning(podKey)
	default:
		return fmt.Errorf("unknown event type: %s", eventType)
	}
}

func (c *Controller) publishPodRunning(podKey string) error {
	logger.Logger.Debug("Publishing pod running work", zap.String("pod", podKey))

	// Get pod from informer cache
	obj, exists, err := c.podInformer.GetStore().GetByKey(podKey)
	if err != nil {
		return err
	}
	if !exists {
		logger.Logger.Debug("Pod not found in cache, skipping", zap.String("pod", podKey))
		return nil
	}

	podInfo, err := c.k8sClient.ExtractPodInfo(obj)
	if err != nil {
		return fmt.Errorf("failed to extract pod info for key %s: %w", podKey, err)
	}

	// Double-check pod is still running
	if !c.k8sClient.IsPodRunning(podInfo) {
		logger.Logger.Debug("Pod no longer running, skipping", zap.String("pod", podKey), zap.String("phase", podInfo.Phase))
		return nil
	}

	// Dispatch log collection work only for namespaces in the log scope (issue
	// #145 enforcement point).
	//
	// Log and metric dispatch are INDEPENDENT. They used to be sequential, with
	// a log failure returning before metric work was ever published, which was
	// harmless while the queue was Redis (LPUSH does not block) and is not
	// harmless with an in-process queue: a full log queue would hold this
	// goroutine and starve a metric consumer that was sitting idle. Each side
	// now runs and reports on its own, and the errors are joined so a retry
	// still happens for whichever half actually failed.
	logWorkerAssignment, logErr := c.dispatchLogWork(podKey, podInfo)

	// Publish metrics collection work with load balancing
	metricsWork := &queue.MetricsCollectionWork{
		BaseWork: queue.BaseWork{
			PodUID:    podInfo.UID,
			PodName:   podInfo.Name,
			Namespace: podInfo.Namespace,
			NodeName:  podInfo.NodeName,
			Timestamp: time.Now(),
		},
	}

	// Use load balancer to select optimal worker for metrics
	metricsWorkerAssignment, metricsErr := c.dispatchMetricsWork(podKey, metricsWork)

	logger.Logger.Info("Published work for running pod",
		zap.String("pod", podKey),
		zap.String("pod_uid", podInfo.UID),
		zap.String("namespace", podInfo.Namespace),
		zap.String("pod_name", podInfo.Name),
		zap.Int("containers", len(podInfo.Containers)),
		zap.String("phase", podInfo.Phase),
		zap.String("log_worker", logWorkerAssignment),
		zap.String("metrics_worker", metricsWorkerAssignment))

	return errors.Join(logErr, metricsErr)
}

// dispatchMetricsWork publishes metrics work for one pod, shedding to the
// fallback queue when no worker is available or the chosen worker's queue is
// full. It returns the assignment for logging.
func (c *Controller) dispatchMetricsWork(
	podKey string, metricsWork *queue.MetricsCollectionWork,
) (string, error) {
	selectedWorkerID, err := c.distributor.DistributeWork(c.ctx, podKey, queue.MetricsWorkerType)
	if err != nil {
		logger.Logger.Warn("No healthy metrics workers available, using fallback queue",
			zap.String("pod", podKey), zap.Error(err))
		return c.shedToFallback(podKey, queue.MetricsWorkerType, metricsWork)
	}

	publishErr := c.queueClient.PublishMetricsWorkToWorker(c.ctx, metricsWork, selectedWorkerID)
	switch {
	case publishErr == nil:
		logger.Logger.Debug("Published metrics work to worker",
			zap.String("pod", podKey), zap.String("worker_id", selectedWorkerID))
		return selectedWorkerID, nil
	case errors.Is(publishErr, queue.ErrQueueFull):
		// The worker is behind. Hold the work rather than blocking the
		// dispatcher on it -- the controller redistributes the fallback queue.
		logger.Logger.Warn("Metrics worker queue is full, using fallback queue",
			zap.String("pod", podKey), zap.String("worker_id", selectedWorkerID))
		return c.shedToFallback(podKey, queue.MetricsWorkerType, metricsWork)
	default:
		return "", fmt.Errorf("failed to publish metrics work to worker %s for pod %s: %w",
			selectedWorkerID, podKey, publishErr)
	}
}

// shedToFallback holds work that could not be delivered to a worker. A full
// fallback queue is a drop, logged by the queue, and must not fail the work
// item: retrying into a queue that is already at its cap only spins.
func (c *Controller) shedToFallback(podKey, workType string, work interface{}) (string, error) {
	err := c.queueClient.PublishToFallbackQueue(c.ctx, workType, work)
	switch {
	case err == nil:
		logger.Logger.Debug("Published work to fallback queue",
			zap.String("pod", podKey), zap.String("work_type", workType))
		return "fallback_queue", nil
	case errors.Is(err, queue.ErrFallbackFull):
		return "dropped_fallback_full", nil
	default:
		logger.Logger.Error("Failed to publish to fallback queue",
			zap.String("pod", podKey), zap.String("work_type", workType), zap.Error(err))
		return "", err
	}
}

func (c *Controller) GetStatus() Status {
	// Get worker health information
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	workerHealth, err := c.queueClient.GetAllWorkerHealth(ctx)
	if err != nil {
		logger.Logger.Error("Failed to get worker health for status", zap.Error(err))
		workerHealth = make(map[string]*queue.WorkerHealth)
	}

	podAssignments, err := c.queueClient.GetAllPodAssignments(ctx)
	if err != nil {
		logger.Logger.Error("Failed to get pod assignments for status", zap.Error(err))
		podAssignments = make(map[string]*queue.PodAssignment)
	}

	// Count healthy workers
	healthyWorkers := 0
	for _, health := range workerHealth {
		if health.Healthy {
			healthyWorkers++
		}
	}

	return Status{
		WorkerPeriod:   c.workerPeriod.String(),
		Namespaces:     c.scope.Namespaces(),
		Workers:        c.numWorkers,
		QueueLength:    c.workqueue.Len(),
		Healthy:        c.podInformer.HasSynced(),
		WorkerHealth:   workerHealth,
		PodAssignments: podAssignments,
		HealthyWorkers: healthyWorkers,
		TotalWorkers:   len(workerHealth),
	}
}

type Status struct {
	WorkerHealth   map[string]*queue.WorkerHealth  `json:"worker_health"`
	PodAssignments map[string]*queue.PodAssignment `json:"pod_assignments"`
	WorkerPeriod   string                          `json:"worker_period"`
	Namespaces     []string                        `json:"namespaces"`
	Workers        int                             `json:"workers"`
	QueueLength    int                             `json:"queue_length"`
	HealthyWorkers int                             `json:"healthy_workers"`
	TotalWorkers   int                             `json:"total_workers"`
	Healthy        bool                            `json:"healthy"`
}

// syncUnassignedPods checks for running pods that have no healthy worker
// assignment and re-enqueues them for distribution. This handles the case
// where pods were discovered before any workers were registered, and the
// fallback queue has already been drained without successful redistribution.
func (c *Controller) syncUnassignedPods() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Get current worker health to know which workers are alive
	workerHealth, err := c.queueClient.GetAllWorkerHealth(ctx)
	if err != nil {
		logger.Logger.Warn("Periodic sync: failed to get worker health", zap.Error(err))
		return
	}

	// Build set of healthy worker IDs
	healthyWorkers := make(map[string]bool)
	for _, health := range workerHealth {
		if health.Healthy {
			healthyWorkers[health.WorkerID] = true
		}
	}

	if len(healthyWorkers) == 0 {
		return // No healthy workers, nothing to do
	}

	// Get current pod assignments
	podAssignments, err := c.queueClient.GetAllPodAssignments(ctx)
	if err != nil {
		logger.Logger.Warn("Periodic sync: failed to get pod assignments", zap.Error(err))
		return
	}

	// Build a set of pod keys that have active assignments to healthy workers
	activePodKeys := make(map[string]bool)
	for _, assignment := range podAssignments {
		if healthyWorkers[assignment.WorkerID] {
			activePodKeys[assignment.PodKey] = true
		}
	}

	// List all pods from the informer cache and find unassigned running pods
	items := c.podInformer.GetStore().List()
	enqueued := 0

	for _, item := range items {
		podInfo, err := c.k8sClient.ExtractPodInfo(item)
		if err != nil {
			continue
		}

		if !c.isNamespaceWatched(podInfo.Namespace) {
			continue
		}

		if !c.k8sClient.IsPodRunning(podInfo) {
			continue
		}

		key, err := c.k8sClient.GeneratePodKey(podInfo)
		if err != nil {
			continue
		}

		if !activePodKeys[key] {
			c.workqueue.Add(fmt.Sprintf("pod-running/%s", key))
			enqueued++
		}
	}

	if enqueued > 0 {
		logger.Logger.Info("Periodic sync: re-enqueued unassigned pods",
			zap.Int("enqueued", enqueued),
			zap.Int("total_pods", len(items)),
			zap.Int("healthy_workers", len(healthyWorkers)),
		)
	}
}

// processFallbackQueues attempts to redistribute work from fallback queues
func (c *Controller) processFallbackQueues() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The same constants the dispatch path publishes under. They were literals
	// here; the in-process queue now REJECTS unknown work types (matching the
	// Redis client), so a rename that missed this line would break requeueing at
	// runtime with nothing to catch it at compile time.
	workTypes := []string{queue.LogsWorkerType, queue.MetricsWorkerType}

	for _, workType := range workTypes {
		// Check queue length first
		queueLength, err := c.queueClient.GetFallbackQueueLength(ctx, workType)
		if err != nil {
			logger.Logger.Warn("Failed to get fallback queue length", zap.String("work_type", workType), zap.Error(err))
			continue
		}

		if queueLength == 0 {
			continue // No work to process
		}

		// Process up to 10 items at a time to avoid blocking
		maxItems := int64(10)
		if queueLength < maxItems {
			maxItems = queueLength
		}

		for i := int64(0); i < maxItems; i++ {
			work, err := c.queueClient.ConsumeFromFallbackQueue(ctx, workType)
			if err != nil {
				logger.Logger.Warn("Failed to consume from fallback queue", zap.String("work_type", workType), zap.Error(err))
				break
			}

			if work == nil {
				break // No more work available
			}

			// Try to redistribute the work
			if err := c.redistributeFallbackWork(ctx, workType, work); err != nil {
				logger.Logger.Warn("Failed to redistribute fallback work, requeueing",
					zap.String("work_type", workType), zap.Error(err))
				// Put it back in the fallback queue
				if requeueErr := c.queueClient.PublishToFallbackQueue(ctx, workType, work); requeueErr != nil {
					logger.Logger.Error("Failed to requeue fallback work",
						zap.String("work_type", workType), zap.Error(requeueErr))
				}
				break // Stop processing this type to avoid infinite loops
			}
		}
	}
}

// redistributeFallbackWork attempts to redistribute a single work item from fallback queue
func (c *Controller) redistributeFallbackWork(ctx context.Context, workType string, work interface{}) error {
	var podKey string

	// Extract pod key based on work type
	switch w := work.(type) {
	case *queue.LogCollectionWork:
		podKey = fmt.Sprintf("%s/%s", w.Namespace, w.PodName)
	case *queue.MetricsCollectionWork:
		podKey = fmt.Sprintf("%s/%s", w.Namespace, w.PodName)
	default:
		return fmt.Errorf("unknown work type: %T", work)
	}

	// Try to find a healthy worker
	selectedWorkerID, err := c.distributor.DistributeWork(ctx, podKey, workType)
	if err != nil {
		return fmt.Errorf("no healthy workers available for redistribution: %w", err)
	}

	// Send to specific worker based on work type
	switch w := work.(type) {
	case *queue.LogCollectionWork:
		if err := c.queueClient.PublishLogWorkToWorker(ctx, w, selectedWorkerID); err != nil {
			return fmt.Errorf("failed to publish redistributed log work: %w", err)
		}
		logger.Logger.Debug("Redistributed log work from fallback queue",
			zap.String("pod", podKey), zap.String("worker_id", selectedWorkerID))
	case *queue.MetricsCollectionWork:
		if err := c.queueClient.PublishMetricsWorkToWorker(ctx, w, selectedWorkerID); err != nil {
			return fmt.Errorf("failed to publish redistributed metrics work: %w", err)
		}
		logger.Logger.Debug("Redistributed metrics work from fallback queue",
			zap.String("pod", podKey), zap.String("worker_id", selectedWorkerID))
	}

	return nil
}
