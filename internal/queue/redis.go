package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/go-redis/redis/v8"
	"go.uber.org/zap"

	"github.com/autonomousops-co/accretion/internal/logger"
)

const (
	WorkerHealthKeyPrefix        = "accretion:worker:health:"
	PodAssignmentKeyPrefix       = "accretion:pod:assignment:"
	HealthTTL                    = 60 * time.Second
	LogWorkWorkerQueuePrefix     = "accretion:log_work:worker:"
	MetricsWorkWorkerQueuePrefix = "accretion:metrics_work:worker:"
	FallbackLogWorkQueue         = "accretion:fallback:log_work"
	FallbackMetricsWorkQueue     = "accretion:fallback:metrics_work"
	LogsWorkerType               = "logs"
	MetricsWorkerType            = "metrics"
)

// RedisClient implements the queue interfaces using Redis.
type RedisClient struct {
	client *redis.Client
}

// NewRedisClient creates a new Redis client for queue operations.
func NewRedisClient(addr, password string, db int) (*RedisClient, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})

	// Test connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	return &RedisClient{client: client}, nil
}

// PublishLogWorkToWorker publishes log collection work to a specific worker's queue.
func (r *RedisClient) PublishLogWorkToWorker(ctx context.Context, work *LogCollectionWork, workerID string) error {
	data, err := json.Marshal(work)
	if err != nil {
		return fmt.Errorf("failed to marshal log work: %w", err)
	}

	workerQueueKey := fmt.Sprintf("%s%s", LogWorkWorkerQueuePrefix, workerID)
	if err := r.client.LPush(ctx, workerQueueKey, data).Err(); err != nil {
		return fmt.Errorf("failed to publish log work to worker %s: %w", workerID, err)
	}

	return nil
}

// PublishMetricsWorkToWorker publishes metrics collection work to a specific worker's queue.
func (r *RedisClient) PublishMetricsWorkToWorker(
	ctx context.Context, work *MetricsCollectionWork, workerID string,
) error {
	data, err := json.Marshal(work)
	if err != nil {
		return fmt.Errorf("failed to marshal metrics work: %w", err)
	}

	workerQueueKey := fmt.Sprintf("%s%s", MetricsWorkWorkerQueuePrefix, workerID)
	if err := r.client.LPush(ctx, workerQueueKey, data).Err(); err != nil {
		return fmt.Errorf("failed to publish metrics work to worker %s: %w", workerID, err)
	}

	return nil
}

// ConsumeLogWorkForWorker consumes log collection work from a specific worker's queue.
func (r *RedisClient) ConsumeLogWorkForWorker(
	ctx context.Context, workerID string, handler func(*LogCollectionWork) error,
) error {
	workerQueueKey := fmt.Sprintf("%s%s", LogWorkWorkerQueuePrefix, workerID)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			// Only consume from worker-specific queue
			result, err := r.client.BRPop(ctx, 1*time.Second, workerQueueKey).Result()
			if err != nil {
				if err == redis.Nil {
					continue // No data available
				}
				return fmt.Errorf("failed to consume log work for worker %s: %w", workerID, err)
			}

			if len(result) < 2 {
				continue
			}

			var work LogCollectionWork
			if err := json.Unmarshal([]byte(result[1]), &work); err != nil {
				logger.Logger.Error("Failed to unmarshal log work", zap.Error(err), zap.String("worker_id", workerID))
				continue
			}

			if err := handler(&work); err != nil {
				logger.Logger.Error("Failed to handle log work",
					zap.Error(err), zap.Any("work", work), zap.String("worker_id", workerID))
			}
		}
	}
}

// ConsumeMetricsWorkForWorker consumes metrics collection work from a specific worker's queue.
func (r *RedisClient) ConsumeMetricsWorkForWorker(
	ctx context.Context, workerID string, handler func(*MetricsCollectionWork) error,
) error {
	workerQueueKey := fmt.Sprintf("%s%s", MetricsWorkWorkerQueuePrefix, workerID)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			// Only consume from worker-specific queue
			result, err := r.client.BRPop(ctx, 1*time.Second, workerQueueKey).Result()
			if err != nil {
				if err == redis.Nil {
					continue // No data available
				}
				return fmt.Errorf("failed to consume metrics work for worker %s: %w", workerID, err)
			}

			if len(result) < 2 {
				continue
			}

			var work MetricsCollectionWork
			if err := json.Unmarshal([]byte(result[1]), &work); err != nil {
				logger.Logger.Error("Failed to unmarshal metrics work", zap.Error(err), zap.String("worker_id", workerID))
				continue
			}

			if err := handler(&work); err != nil {
				logger.Logger.Error("Failed to handle metrics work",
					zap.Error(err), zap.Any("work", work), zap.String("worker_id", workerID))
			}
		}
	}
}

// StoreWorkerHealth stores worker health status in Redis hash with TTL.
func (r *RedisClient) StoreWorkerHealth(ctx context.Context, health *WorkerHealth) error {
	key := fmt.Sprintf("%s%s", WorkerHealthKeyPrefix, health.WorkerID)

	// Convert health to map for HSET
	healthMap := map[string]interface{}{
		"worker_id":     health.WorkerID,
		"type":          health.Type,
		"healthy":       fmt.Sprintf("%t", health.Healthy), // Ensure consistent string format
		"last_seen":     health.LastSeen.Format(time.RFC3339),
		"assigned_pods": strings.Join(health.AssignedPods, ","),
		"error_count":   health.ErrorCount,
	}

	// Store in Redis hash
	if err := r.client.HMSet(ctx, key, healthMap).Err(); err != nil {
		return fmt.Errorf("failed to store worker health: %w", err)
	}

	// Set TTL for automatic cleanup
	if err := r.client.Expire(ctx, key, HealthTTL).Err(); err != nil {
		return fmt.Errorf("failed to set TTL for worker health: %w", err)
	}

	return nil
}

// GetWorkerHealth retrieves worker health status from Redis.
func (r *RedisClient) GetWorkerHealth(ctx context.Context, workerID string) (*WorkerHealth, error) {
	key := fmt.Sprintf("%s%s", WorkerHealthKeyPrefix, workerID)

	result, err := r.client.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get worker health: %w", err)
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("worker health not found: %s", workerID)
	}

	// Parse assigned pods
	assignedPods := []string{}
	if podsStr := result["assigned_pods"]; podsStr != "" {
		assignedPods = strings.Split(podsStr, ",")
	}

	// Parse last seen
	lastSeen, err := time.Parse(time.RFC3339, result["last_seen"])
	if err != nil {
		lastSeen = time.Now()
	}

	// Parse error count
	errorCount := 0
	if countStr := result["error_count"]; countStr != "" {
		if _, err := fmt.Sscanf(countStr, "%d", &errorCount); err != nil {
			logger.Logger.Warn("Failed to parse error count", zap.String("error_count", countStr), zap.Error(err))
		}
	}

	// Parse healthy (handle both "true"/"false" and "1"/"0" formats)
	healthyStr := result["healthy"]
	healthy := healthyStr == "true" || healthyStr == "1"

	health := &WorkerHealth{
		WorkerID:     result["worker_id"],
		Type:         result["type"],
		Healthy:      healthy,
		LastSeen:     lastSeen,
		AssignedPods: assignedPods,
		ErrorCount:   errorCount,
	}

	return health, nil
}

// GetAllWorkerHealth retrieves all worker health statuses from Redis.
func (r *RedisClient) GetAllWorkerHealth(ctx context.Context) (map[string]*WorkerHealth, error) {
	pattern := fmt.Sprintf("%s*", WorkerHealthKeyPrefix)
	healthMap := make(map[string]*WorkerHealth)

	iter := r.client.Scan(ctx, 0, pattern, 100).Iterator()
	for iter.Next(ctx) {
		key := iter.Val()
		workerID := strings.TrimPrefix(key, WorkerHealthKeyPrefix)
		health, err := r.GetWorkerHealth(ctx, workerID)
		if err != nil {
			logger.Logger.Warn("Failed to get worker health", zap.String("worker_id", workerID), zap.Error(err))
			continue
		}
		healthMap[workerID] = health
	}

	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("failed to scan worker health keys: %w", err)
	}

	return healthMap, nil
}

// DeleteWorkerHealth removes worker health status from Redis.
func (r *RedisClient) DeleteWorkerHealth(ctx context.Context, workerID string) error {
	key := fmt.Sprintf("%s%s", WorkerHealthKeyPrefix, workerID)

	if err := r.client.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("failed to delete worker health: %w", err)
	}

	logger.Logger.Debug("Deleted worker health", zap.String("worker_id", workerID))
	return nil
}

// StorePodAssignment stores pod assignment in Redis
func (r *RedisClient) StorePodAssignment(ctx context.Context, assignment *PodAssignment) error {
	// Use composite key: podKey + workerType to avoid collisions
	key := fmt.Sprintf("%s%s:%s", PodAssignmentKeyPrefix, assignment.PodKey, assignment.WorkerType)

	// Convert assignment to map for HSET
	assignmentMap := map[string]interface{}{
		"pod_key":     assignment.PodKey,
		"worker_id":   assignment.WorkerID,
		"worker_type": assignment.WorkerType,
		"assigned_at": assignment.AssignedAt.Format(time.RFC3339),
		"last_seen":   assignment.LastSeen.Format(time.RFC3339),
	}

	// Store in Redis hash
	if err := r.client.HMSet(ctx, key, assignmentMap).Err(); err != nil {
		return fmt.Errorf("failed to store pod assignment: %w", err)
	}

	return nil
}

// UpdatePodAssignment updates an existing pod assignment with new worker information.
func (r *RedisClient) UpdatePodAssignment(ctx context.Context, assignment *PodAssignment) error {
	key := fmt.Sprintf("%s%s:%s", PodAssignmentKeyPrefix, assignment.PodKey, assignment.WorkerType)

	// Check if assignment exists first
	exists, err := r.client.Exists(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("failed to check if pod assignment exists: %w", err)
	}

	if exists == 0 {
		// Assignment doesn't exist, store it as new
		return r.StorePodAssignment(ctx, assignment)
	}

	// Convert assignment to map for HSET
	assignmentMap := map[string]interface{}{
		"pod_key":     assignment.PodKey,
		"worker_id":   assignment.WorkerID,
		"worker_type": assignment.WorkerType,
		"assigned_at": assignment.AssignedAt.Format(time.RFC3339),
		"last_seen":   assignment.LastSeen.Format(time.RFC3339),
	}

	// Update existing assignment
	if err := r.client.HMSet(ctx, key, assignmentMap).Err(); err != nil {
		return fmt.Errorf("failed to update pod assignment: %w", err)
	}

	return nil
}

// GetAllPodAssignments retrieves all pod assignments from Redis.
func (r *RedisClient) GetAllPodAssignments(ctx context.Context) (map[string]*PodAssignment, error) {
	pattern := fmt.Sprintf("%s*", PodAssignmentKeyPrefix)
	assignmentMap := make(map[string]*PodAssignment)

	iter := r.client.Scan(ctx, 0, pattern, 100).Iterator()
	for iter.Next(ctx) {
		key := iter.Val()
		// Get the assignment directly from the key
		result, err := r.client.HGetAll(ctx, key).Result()
		if err != nil || len(result) == 0 {
			logger.Logger.Warn("Failed to get pod assignment", zap.String("key", key), zap.Error(err))
			continue
		}

		assignment := &PodAssignment{
			PodKey:     result["pod_key"],
			WorkerID:   result["worker_id"],
			WorkerType: result["worker_type"],
		}

		if assignedAt, err := time.Parse(time.RFC3339, result["assigned_at"]); err == nil {
			assignment.AssignedAt = assignedAt
		}
		if lastSeen, err := time.Parse(time.RFC3339, result["last_seen"]); err == nil {
			assignment.LastSeen = lastSeen
		}

		// Use the full key to avoid collisions between different worker types
		assignmentMap[key] = assignment
	}

	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("failed to scan pod assignment keys: %w", err)
	}

	return assignmentMap, nil
}

// DeletePodAssignment removes pod assignment from Redis.
func (r *RedisClient) DeletePodAssignment(ctx context.Context, podKey string) error {
	// Delete both log and metrics assignments for this pod
	logKey := fmt.Sprintf("%s%s:%s", PodAssignmentKeyPrefix, podKey, LogsWorkerType)
	metricsKey := fmt.Sprintf("%s%s:%s", PodAssignmentKeyPrefix, podKey, MetricsWorkerType)

	// Delete both keys (ignore errors as some may not exist)
	r.client.Del(ctx, logKey, metricsKey)

	logger.Logger.Debug("Deleted pod assignment", zap.String("pod_key", podKey))
	return nil
}

// CleanupWorkerQueue removes any remaining work items from a worker's queue when it shuts down.
// This helps prevent work from being stuck if a worker goes down unexpectedly.
func (r *RedisClient) CleanupWorkerQueue(ctx context.Context, workerID string) error {
	logQueueKey := fmt.Sprintf("%s%s", LogWorkWorkerQueuePrefix, workerID)
	metricsQueueKey := fmt.Sprintf("%s%s", MetricsWorkWorkerQueuePrefix, workerID)

	// Delete the worker-specific queues
	if err := r.client.Del(ctx, logQueueKey, metricsQueueKey).Err(); err != nil {
		return fmt.Errorf("failed to cleanup worker queues for %s: %w", workerID, err)
	}

	logger.Logger.Debug("Cleaned up worker queues", zap.String("worker_id", workerID))
	return nil
}

// PublishToFallbackQueue publishes work to fallback queue when no workers available
func (r *RedisClient) PublishToFallbackQueue(ctx context.Context, workType string, work interface{}) error {
	data, err := json.Marshal(work)
	if err != nil {
		return fmt.Errorf("failed to marshal fallback work: %w", err)
	}

	var queueKey string
	switch workType {
	case LogsWorkerType:
		queueKey = FallbackLogWorkQueue
	case MetricsWorkerType:
		queueKey = FallbackMetricsWorkQueue
	default:
		return fmt.Errorf("unknown work type for fallback: %s", workType)
	}

	if err := r.client.LPush(ctx, queueKey, data).Err(); err != nil {
		return fmt.Errorf("failed to publish to fallback queue %s: %w", queueKey, err)
	}

	logger.Logger.Debug("Published work to fallback queue",
		zap.String("work_type", workType), zap.String("queue", queueKey))
	return nil
}

// ConsumeFromFallbackQueue consumes work from fallback queue and attempts redistribution
func (r *RedisClient) ConsumeFromFallbackQueue(ctx context.Context, workType string) (interface{}, error) {
	var queueKey string
	switch workType {
	case LogsWorkerType:
		queueKey = FallbackLogWorkQueue
	case MetricsWorkerType:
		queueKey = FallbackMetricsWorkQueue
	default:
		return nil, fmt.Errorf("unknown work type for fallback: %s", workType)
	}

	result, err := r.client.BRPop(ctx, 1*time.Second, queueKey).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, nil // No work available
		}
		return nil, fmt.Errorf("failed to consume from fallback queue %s: %w", queueKey, err)
	}

	if len(result) < 2 {
		return nil, fmt.Errorf("invalid result from fallback queue")
	}

	// Unmarshal based on work type
	var work interface{}
	switch workType {
	case LogsWorkerType:
		var logWork LogCollectionWork
		if err := json.Unmarshal([]byte(result[1]), &logWork); err != nil {
			return nil, fmt.Errorf("failed to unmarshal log work: %w", err)
		}
		work = &logWork
	case MetricsWorkerType:
		var metricsWork MetricsCollectionWork
		if err := json.Unmarshal([]byte(result[1]), &metricsWork); err != nil {
			return nil, fmt.Errorf("failed to unmarshal metrics work: %w", err)
		}
		work = &metricsWork
	}

	logger.Logger.Debug("Consumed work from fallback queue",
		zap.String("work_type", workType), zap.String("queue", queueKey))
	return work, nil
}

// GetFallbackQueueLength returns the number of items in fallback queue
func (r *RedisClient) GetFallbackQueueLength(ctx context.Context, workType string) (int64, error) {
	var queueKey string
	switch workType {
	case LogsWorkerType:
		queueKey = FallbackLogWorkQueue
	case MetricsWorkerType:
		queueKey = FallbackMetricsWorkQueue
	default:
		return 0, fmt.Errorf("unknown work type for fallback: %s", workType)
	}

	length, err := r.client.LLen(ctx, queueKey).Result()
	if err != nil {
		return 0, fmt.Errorf("failed to get fallback queue length: %w", err)
	}

	return length, nil
}

// Close closes the Redis client connection.
func (r *RedisClient) Close() error {
	return r.client.Close()
}
