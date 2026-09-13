package queue

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/autonomousops-co/accretion/internal/logger"
)

// DefaultInProcessBuffer is how many items one worker's in-process queue holds
// before publishing blocks.
//
// Blocking rather than dropping is the point: with Redis the queue absorbs a
// slow worker at the cost of unbounded memory, and here the controller is in the
// same process, so back-pressure reaching the dispatcher is both possible and
// correct. The buffer is sized to hold a burst from one poll cycle of a
// reasonably large namespace without stalling the dispatcher on every tick.
const DefaultInProcessBuffer = 1024

// DefaultPublishTimeout bounds how long a publish waits for room in a full
// worker queue before shedding the item to the fallback queue.
//
// It must be SHORT. The dispatcher publishes log work and then metric work from
// one goroutine, and the controller runs a small fixed pool of them, so every
// millisecond spent blocked on a full log queue is a millisecond in which no
// pod's metric work is dispatched either -- by a handful of wedged pod keys the
// whole controller stalls. Redis never had this problem because LPUSH does not
// block. Shedding to the fallback queue keeps the work (the controller drains
// and redistributes it) while keeping the dispatcher moving.
const DefaultPublishTimeout = 250 * time.Millisecond

// DefaultMaxFallbackItems caps each fallback work type.
//
// The fallback queue holds work that had no healthy worker. Unbounded, it is a
// memory leak with a slow fuse: this memory now lives in the collector's own
// limit, and an OOMKill there takes the log, metric, event and trace collectors
// and the embedded controller down together. Past the cap the queue sheds
// loudly rather than growing quietly.
const DefaultMaxFallbackItems = 10000

// ErrQueueFull is returned when a worker's queue stayed full for the whole
// publish timeout. Callers should shed the item rather than retry in place.
var ErrQueueFull = errors.New("worker queue is full")

// ErrFallbackFull is returned when a fallback work type is at its cap. The item
// was dropped and the drop was logged.
var ErrFallbackFull = errors.New("fallback queue is full")

// InProcessClient is a queue.Client that delivers work over Go channels within
// a single process, with no Redis.
//
// It exists for the default single-replica install (#93). The controller and
// Redis are there to shard work across horizontally scaled workers; with one
// replica of each collector and no HPA, there is nothing to shard, and routing
// a pod's log work through Redis so a process can assign that work to itself is
// two pods and a queue's worth of operational surface for no benefit. When
// scale-out is turned on the chart deploys the real controller and Redis, and
// the collectors switch to RedisClient -- the interface is the only thing either
// side sees.
//
// Work is addressed by worker ID exactly as in Redis, so the controller's
// distribution logic is unchanged. A publish to an unknown worker ID creates
// that worker's channel, matching Redis's behavior of pushing to a list key
// that does not exist yet.
type InProcessClient struct {
	logQueues    map[string]chan *LogCollectionWork
	metricQueues map[string]chan *MetricsCollectionWork
	health       map[string]*WorkerHealth
	assignments  map[string]*PodAssignment
	fallback     map[string][]interface{}
	bufferSize   int
	// publishTimeout bounds a blocked publish; see DefaultPublishTimeout.
	publishTimeout time.Duration
	// maxFallback caps each fallback work type; see DefaultMaxFallbackItems.
	maxFallback int
	// fallbackDropped counts items shed at the cap, so the drop is visible in
	// more than a log line.
	fallbackDropped map[string]int
	// dropping and lastDropLog rate-limit the drop log. A collector that emits
	// one ERROR line per dropped item logs at shed rate, which is the worst
	// thing to hand an operator who is already losing work -- a measured run
	// produced 190,000 lines. The counter carries the volume; the log carries
	// the transitions.
	dropping    map[string]bool
	lastDropLog map[string]time.Time
	mu          sync.Mutex
	closed      bool
}

// NewInProcessClient creates an in-process queue with the default buffer size.
func NewInProcessClient() *InProcessClient {
	return NewInProcessClientWithBuffer(DefaultInProcessBuffer)
}

// NewInProcessClientWithBuffer creates an in-process queue whose per-worker
// channels hold bufferSize items.
func NewInProcessClientWithBuffer(bufferSize int) *InProcessClient {
	if bufferSize <= 0 {
		bufferSize = DefaultInProcessBuffer
	}
	return &InProcessClient{
		logQueues:       make(map[string]chan *LogCollectionWork),
		metricQueues:    make(map[string]chan *MetricsCollectionWork),
		health:          make(map[string]*WorkerHealth),
		assignments:     make(map[string]*PodAssignment),
		fallback:        make(map[string][]interface{}),
		fallbackDropped: make(map[string]int),
		dropping:        make(map[string]bool),
		lastDropLog:     make(map[string]time.Time),
		bufferSize:      bufferSize,
		publishTimeout:  DefaultPublishTimeout,
		maxFallback:     DefaultMaxFallbackItems,
	}
}

// ErrQueueClosed is returned by publishes and consumes after Close.
var ErrQueueClosed = errors.New("in-process queue is closed")

func (c *InProcessClient) logQueue(workerID string) (chan *LogCollectionWork, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrQueueClosed
	}
	q, ok := c.logQueues[workerID]
	if !ok {
		q = make(chan *LogCollectionWork, c.bufferSize)
		c.logQueues[workerID] = q
	}
	return q, nil
}

func (c *InProcessClient) metricQueue(workerID string) (chan *MetricsCollectionWork, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrQueueClosed
	}
	q, ok := c.metricQueues[workerID]
	if !ok {
		q = make(chan *MetricsCollectionWork, c.bufferSize)
		c.metricQueues[workerID] = q
	}
	return q, nil
}

// PublishLogWorkToWorker delivers log work to one worker's queue.
func (c *InProcessClient) PublishLogWorkToWorker(
	ctx context.Context, work *LogCollectionWork, workerID string,
) error {
	q, err := c.logQueue(workerID)
	if err != nil {
		return err
	}

	// A fast path that avoids allocating a timer in the common case.
	select {
	case q <- work:
		return nil
	default:
	}

	timer := time.NewTimer(c.publishTimeout)
	defer timer.Stop()
	select {
	case q <- work:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("log work for worker %s: %w", workerID, ErrQueueFull)
	}
}

// PublishMetricsWorkToWorker delivers metrics work to one worker's queue.
func (c *InProcessClient) PublishMetricsWorkToWorker(
	ctx context.Context, work *MetricsCollectionWork, workerID string,
) error {
	q, err := c.metricQueue(workerID)
	if err != nil {
		return err
	}

	select {
	case q <- work:
		return nil
	default:
	}

	timer := time.NewTimer(c.publishTimeout)
	defer timer.Stop()
	select {
	case q <- work:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("metrics work for worker %s: %w", workerID, ErrQueueFull)
	}
}

// PublishToFallbackQueue holds work that had no healthy worker to go to.
//
// Unknown work types are rejected, matching RedisClient. Accepting them would
// turn what is an error there into a map entry here that nothing ever drains --
// the controller only polls the two known types.
func (c *InProcessClient) PublishToFallbackQueue(_ context.Context, workType string, work interface{}) error {
	switch workType {
	case LogsWorkerType, MetricsWorkerType:
	default:
		return fmt.Errorf("unknown work type for fallback: %s", workType)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrQueueClosed
	}

	if len(c.fallback[workType]) >= c.maxFallback {
		c.fallbackDropped[workType]++
		c.logDropLocked(workType)
		return fmt.Errorf("%s: %w", workType, ErrFallbackFull)
	}

	// Accepting again after a run of drops is worth exactly one line, so the
	// end of an incident is as visible in the log as its start.
	if c.dropping[workType] {
		c.dropping[workType] = false
		logger.Logger.Warn("Fallback queue is accepting work again",
			zap.String("work_type", workType),
			zap.Int("dropped_total", c.fallbackDropped[workType]))
	}

	c.fallback[workType] = append(c.fallback[workType], work)
	return nil
}

// dropLogInterval is how often a sustained run of drops is summarized.
const dropLogInterval = 30 * time.Second

// logDropLocked logs on entering the dropping state and periodically after
// that, rather than once per dropped item. Callers must hold c.mu.
func (c *InProcessClient) logDropLocked(workType string) {
	now := time.Now()
	entering := !c.dropping[workType]
	if !entering && now.Sub(c.lastDropLog[workType]) < dropLogInterval {
		return
	}

	c.dropping[workType] = true
	c.lastDropLog[workType] = now
	logger.Logger.Error("Fallback queue is full; dropping work",
		zap.String("work_type", workType),
		zap.Int("cap", c.maxFallback),
		zap.Int("dropped_total", c.fallbackDropped[workType]),
		zap.Bool("first", entering))
}

// ConsumeLogWorkForWorker runs handler over this worker's log work until ctx is
// canceled. A handler error is logged and consumption continues, matching
// RedisClient: one bad pod must not end the consume loop.
func (c *InProcessClient) ConsumeLogWorkForWorker(
	ctx context.Context, workerID string, handler func(*LogCollectionWork) error,
) error {
	q, err := c.logQueue(workerID)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case work, ok := <-q:
			if !ok {
				return ErrQueueClosed
			}
			if err := handler(work); err != nil {
				logger.Logger.Error("Failed to handle log work",
					zap.String("worker_id", workerID), zap.Error(err))
			}
		}
	}
}

// ConsumeMetricsWorkForWorker runs handler over this worker's metrics work until
// ctx is canceled.
func (c *InProcessClient) ConsumeMetricsWorkForWorker(
	ctx context.Context, workerID string, handler func(*MetricsCollectionWork) error,
) error {
	q, err := c.metricQueue(workerID)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case work, ok := <-q:
			if !ok {
				return ErrQueueClosed
			}
			if err := handler(work); err != nil {
				logger.Logger.Error("Failed to handle metrics work",
					zap.String("worker_id", workerID), zap.Error(err))
			}
		}
	}
}

// StoreWorkerHealth records a worker's health so the distributor can find it.
func (c *InProcessClient) StoreWorkerHealth(_ context.Context, health *WorkerHealth) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	stored := *health
	c.health[health.WorkerID] = &stored
	return nil
}

// GetWorkerHealth returns one worker's last reported health.
func (c *InProcessClient) GetWorkerHealth(_ context.Context, workerID string) (*WorkerHealth, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h, ok := c.health[workerID]
	if !ok {
		return nil, fmt.Errorf("no health record for worker %s", workerID)
	}
	copied := *h
	return &copied, nil
}

// GetAllWorkerHealth returns every worker's last reported health.
func (c *InProcessClient) GetAllWorkerHealth(_ context.Context) (map[string]*WorkerHealth, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]*WorkerHealth, len(c.health))
	for id, h := range c.health {
		copied := *h
		out[id] = &copied
	}
	return out, nil
}

// DeleteWorkerHealth forgets a worker.
func (c *InProcessClient) DeleteWorkerHealth(_ context.Context, workerID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.health, workerID)
	return nil
}

// StorePodAssignment records which worker owns a pod.
func (c *InProcessClient) StorePodAssignment(_ context.Context, assignment *PodAssignment) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	stored := *assignment
	c.assignments[assignment.PodKey] = &stored
	return nil
}

// UpdatePodAssignment replaces a pod's assignment.
func (c *InProcessClient) UpdatePodAssignment(ctx context.Context, assignment *PodAssignment) error {
	return c.StorePodAssignment(ctx, assignment)
}

// GetAllPodAssignments returns every pod assignment.
func (c *InProcessClient) GetAllPodAssignments(_ context.Context) (map[string]*PodAssignment, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]*PodAssignment, len(c.assignments))
	for key, a := range c.assignments {
		copied := *a
		out[key] = &copied
	}
	return out, nil
}

// DeletePodAssignment forgets a pod assignment.
func (c *InProcessClient) DeletePodAssignment(_ context.Context, podKey string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.assignments, podKey)
	return nil
}

// ConsumeFromFallbackQueue pops one item of held work, or nil when empty.
//
// The consumed slot is cleared and the slice is released once drained. Plain
// reslicing (items[1:]) keeps the original backing array alive with every
// consumed value still referenced in it, so a queue that had drained to empty
// went on pinning everything ever put through it for the life of the process.
func (c *InProcessClient) ConsumeFromFallbackQueue(_ context.Context, workType string) (interface{}, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	items := c.fallback[workType]
	if len(items) == 0 {
		return nil, nil
	}

	item := items[0]
	items[0] = nil // let the consumed value be collected
	if len(items) == 1 {
		delete(c.fallback, workType)
	} else {
		c.fallback[workType] = items[1:]
	}
	return item, nil
}

// FallbackDropped reports how many items were shed at the cap for a work type.
func (c *InProcessClient) FallbackDropped(workType string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fallbackDropped[workType]
}

// GetFallbackQueueLength reports how much work is held for a work type.
func (c *InProcessClient) GetFallbackQueueLength(_ context.Context, workType string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return int64(len(c.fallback[workType])), nil
}

// CleanupWorkerQueue drops a worker's queues. It mirrors the RedisClient method
// that BaseWorker.Cleanup looks for.
func (c *InProcessClient) CleanupWorkerQueue(_ context.Context, workerID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.logQueues, workerID)
	delete(c.metricQueues, workerID)
	return nil
}

// Close releases the queue. It is idempotent, and consumers blocked on a
// channel are released by their own context rather than by a channel close, so
// there is no send-on-closed-channel race with an in-flight publish.
func (c *InProcessClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.logQueues = make(map[string]chan *LogCollectionWork)
	c.metricQueues = make(map[string]chan *MetricsCollectionWork)
	// Held work, health records and assignments are released too; a closed
	// queue that still pins them is the leak this method exists to end.
	c.fallback = make(map[string][]interface{})
	c.health = make(map[string]*WorkerHealth)
	c.assignments = make(map[string]*PodAssignment)
	return nil
}
