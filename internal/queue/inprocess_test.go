package queue

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/autonomousops-co/accretion/internal/testutil"
)

// TestInProcessRoundTrip is the single-replica path of #93: work published by an
// in-process controller must reach the collector in the same process, with no
// Redis anywhere.
func TestInProcessRoundTrip(t *testing.T) {
	testutil.SetupTestLogger(t)

	q := NewInProcessClient()
	defer q.Close() //nolint:errcheck // test cleanup

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got := make(chan *LogCollectionWork, 1)
	go func() {
		_ = q.ConsumeLogWorkForWorker(ctx, "worker-1", func(w *LogCollectionWork) error {
			got <- w
			return nil
		})
	}()

	work := &LogCollectionWork{BaseWork: BaseWork{PodName: "api-0", Namespace: "default"}}
	if err := q.PublishLogWorkToWorker(ctx, work, "worker-1"); err != nil {
		t.Fatalf("PublishLogWorkToWorker: %v", err)
	}

	select {
	case delivered := <-got:
		if delivered.PodName != "api-0" || delivered.Namespace != "default" {
			t.Fatalf("delivered %+v, want the published work", delivered)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("log work was never delivered")
	}
}

// TestInProcessMetricsRoundTrip covers the metrics queue, which is a separate
// key space from logs and would otherwise be untested.
func TestInProcessMetricsRoundTrip(t *testing.T) {
	testutil.SetupTestLogger(t)

	q := NewInProcessClient()
	defer q.Close() //nolint:errcheck // test cleanup

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got := make(chan *MetricsCollectionWork, 1)
	go func() {
		_ = q.ConsumeMetricsWorkForWorker(ctx, "worker-1", func(w *MetricsCollectionWork) error {
			got <- w
			return nil
		})
	}()

	if err := q.PublishMetricsWorkToWorker(ctx, &MetricsCollectionWork{
		BaseWork: BaseWork{PodName: "api-0"},
	}, "worker-1"); err != nil {
		t.Fatalf("PublishMetricsWorkToWorker: %v", err)
	}

	select {
	case delivered := <-got:
		if delivered.PodName != "api-0" {
			t.Fatalf("delivered %+v, want the published work", delivered)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("metrics work was never delivered")
	}
}

// TestInProcessWorkIsAddressedByWorker guards the property the controller's
// distribution logic depends on: work goes to the worker it was addressed to
// and to no other.
func TestInProcessWorkIsAddressedByWorker(t *testing.T) {
	testutil.SetupTestLogger(t)

	q := NewInProcessClient()
	defer q.Close() //nolint:errcheck // test cleanup

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mine := make(chan *LogCollectionWork, 1)
	theirs := make(chan *LogCollectionWork, 1)
	go func() {
		_ = q.ConsumeLogWorkForWorker(ctx, "a", func(w *LogCollectionWork) error { mine <- w; return nil })
	}()
	go func() {
		_ = q.ConsumeLogWorkForWorker(ctx, "b", func(w *LogCollectionWork) error { theirs <- w; return nil })
	}()

	if err := q.PublishLogWorkToWorker(ctx, &LogCollectionWork{}, "a"); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case <-mine:
	case <-theirs:
		t.Fatal("work addressed to worker a was delivered to worker b")
	case <-time.After(3 * time.Second):
		t.Fatal("work was never delivered")
	}

	select {
	case <-theirs:
		t.Fatal("worker b received work it was not addressed")
	case <-time.After(50 * time.Millisecond):
	}
}

// TestInProcessHealthStorage covers what the distributor reads to pick a worker.
// Without this the single-replica path would route everything to the fallback
// queue and collect nothing.
func TestInProcessHealthStorage(t *testing.T) {
	testutil.SetupTestLogger(t)

	q := NewInProcessClient()
	defer q.Close() //nolint:errcheck // test cleanup
	ctx := context.Background()

	health := &WorkerHealth{WorkerID: "w1", Type: "logs", Healthy: true, LastSeen: time.Now()}
	if err := q.StoreWorkerHealth(ctx, health); err != nil {
		t.Fatalf("StoreWorkerHealth: %v", err)
	}

	all, err := q.GetAllWorkerHealth(ctx)
	if err != nil {
		t.Fatalf("GetAllWorkerHealth: %v", err)
	}
	if len(all) != 1 || !all["w1"].Healthy || all["w1"].Type != "logs" {
		t.Fatalf("GetAllWorkerHealth returned %+v", all)
	}

	// Stored records must be copies: a later mutation of the caller's struct
	// must not rewrite what the distributor reads.
	health.Healthy = false
	again, err := q.GetAllWorkerHealth(ctx)
	if err != nil {
		t.Fatalf("GetAllWorkerHealth: %v", err)
	}
	if !again["w1"].Healthy {
		t.Fatal("stored health aliases the caller's struct")
	}

	if err := q.DeleteWorkerHealth(ctx, "w1"); err != nil {
		t.Fatalf("DeleteWorkerHealth: %v", err)
	}
	if all, _ := q.GetAllWorkerHealth(ctx); len(all) != 0 {
		t.Fatalf("worker health survived deletion: %+v", all)
	}
}

func TestInProcessFallbackQueue(t *testing.T) {
	testutil.SetupTestLogger(t)

	q := NewInProcessClient()
	defer q.Close() //nolint:errcheck // test cleanup
	ctx := context.Background()

	if err := q.PublishToFallbackQueue(ctx, LogsWorkerType, "held"); err != nil {
		t.Fatalf("PublishToFallbackQueue: %v", err)
	}
	n, err := q.GetFallbackQueueLength(ctx, LogsWorkerType)
	if err != nil || n != 1 {
		t.Fatalf("GetFallbackQueueLength = %d, %v; want 1, nil", n, err)
	}
	item, err := q.ConsumeFromFallbackQueue(ctx, LogsWorkerType)
	if err != nil || item != "held" {
		t.Fatalf("ConsumeFromFallbackQueue = %v, %v; want \"held\", nil", item, err)
	}
	if item, _ := q.ConsumeFromFallbackQueue(ctx, LogsWorkerType); item != nil {
		t.Fatalf("an empty fallback queue returned %v, want nil", item)
	}
}

// TestInProcessFallbackRejectsUnknownWorkType keeps this client's behavior
// identical to RedisClient's. Accepting an unknown type here would turn an
// error into a map entry nothing drains -- the controller only ever polls the
// two known types.
func TestInProcessFallbackRejectsUnknownWorkType(t *testing.T) {
	testutil.SetupTestLogger(t)

	q := NewInProcessClient()
	defer q.Close() //nolint:errcheck // test cleanup

	if err := q.PublishToFallbackQueue(context.Background(), "log_collection", "x"); err == nil {
		t.Fatal("expected an error for an unknown fallback work type")
	}
}

// TestInProcessFallbackIsCappedAndReclaims covers the leak: the queue grew
// without bound, and draining it left every consumed value still referenced by
// the backing array, so a queue reporting empty went on pinning everything ever
// put through it.
func TestInProcessFallbackIsCappedAndReclaims(t *testing.T) {
	testutil.SetupTestLogger(t)

	q := NewInProcessClientWithBuffer(4)
	q.maxFallback = 8
	defer q.Close() //nolint:errcheck // test cleanup
	ctx := context.Background()

	for i := 0; i < q.maxFallback; i++ {
		if err := q.PublishToFallbackQueue(ctx, LogsWorkerType, i); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}

	// At the cap the queue sheds rather than growing.
	err := q.PublishToFallbackQueue(ctx, LogsWorkerType, "one too many")
	if !errors.Is(err, ErrFallbackFull) {
		t.Fatalf("publish past the cap returned %v, want ErrFallbackFull", err)
	}
	if got := q.FallbackDropped(LogsWorkerType); got != 1 {
		t.Fatalf("FallbackDropped = %d, want 1", got)
	}
	if n, _ := q.GetFallbackQueueLength(ctx, LogsWorkerType); n != int64(q.maxFallback) {
		t.Fatalf("queue length = %d, want %d", n, q.maxFallback)
	}

	// Draining releases the slice rather than resliceing over a live array.
	for i := 0; i < q.maxFallback; i++ {
		if _, err := q.ConsumeFromFallbackQueue(ctx, LogsWorkerType); err != nil {
			t.Fatalf("consume %d: %v", i, err)
		}
	}
	q.mu.Lock()
	_, stillThere := q.fallback[LogsWorkerType]
	q.mu.Unlock()
	if stillThere {
		t.Fatal("a fully drained fallback queue still holds its backing slice")
	}

	// And it accepts work again afterwards.
	if err := q.PublishToFallbackQueue(ctx, LogsWorkerType, "after drain"); err != nil {
		t.Fatalf("publish after drain: %v", err)
	}
}

// TestInProcessPublishShedsWhenFull is the back-pressure fix. The dispatcher
// publishes log work and then metric work on one goroutine, so a publish that
// blocks until shutdown on a full log queue starves metric collection even
// though its consumer is idle. A full queue must time out and be shed.
func TestInProcessPublishShedsWhenFull(t *testing.T) {
	testutil.SetupTestLogger(t)

	q := NewInProcessClientWithBuffer(1)
	q.publishTimeout = 20 * time.Millisecond
	defer q.Close() //nolint:errcheck // test cleanup

	// No consumer, so the single buffer slot fills and stays full.
	ctx := context.Background()
	if err := q.PublishLogWorkToWorker(ctx, &LogCollectionWork{}, "w1"); err != nil {
		t.Fatalf("first publish should fit in the buffer: %v", err)
	}

	start := time.Now()
	err := q.PublishLogWorkToWorker(ctx, &LogCollectionWork{}, "w1")
	elapsed := time.Since(start)

	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("publish to a full queue returned %v, want ErrQueueFull", err)
	}
	if elapsed > time.Second {
		t.Fatalf("publish blocked for %v; it must give up after the publish timeout", elapsed)
	}

	// The metrics queue is a separate key space and must be unaffected: this is
	// the starvation the fix exists to prevent.
	if err := q.PublishMetricsWorkToWorker(ctx, &MetricsCollectionWork{}, "w1"); err != nil {
		t.Fatalf("a full log queue blocked an unrelated metrics publish: %v", err)
	}
}

// TestInProcessCloseReleasesHeldState guards the other half of the leak: Close
// used to clear only the channel maps and leave held work, health records and
// assignments pinned for the life of the process.
func TestInProcessCloseReleasesHeldState(t *testing.T) {
	testutil.SetupTestLogger(t)

	q := NewInProcessClient()
	ctx := context.Background()

	if err := q.PublishToFallbackQueue(ctx, LogsWorkerType, "held"); err != nil {
		t.Fatalf("PublishToFallbackQueue: %v", err)
	}
	if err := q.StoreWorkerHealth(ctx, &WorkerHealth{WorkerID: "w1", Type: "logs"}); err != nil {
		t.Fatalf("StoreWorkerHealth: %v", err)
	}
	if err := q.StorePodAssignment(ctx, &PodAssignment{PodKey: "ns/pod", WorkerID: "w1"}); err != nil {
		t.Fatalf("StorePodAssignment: %v", err)
	}

	if err := q.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	q.mu.Lock()
	held, health, assigned := len(q.fallback), len(q.health), len(q.assignments)
	q.mu.Unlock()
	if held != 0 || health != 0 || assigned != 0 {
		t.Fatalf("after Close: fallback=%d health=%d assignments=%d, want all 0", held, health, assigned)
	}
}

func TestInProcessPublishAfterClose(t *testing.T) {
	testutil.SetupTestLogger(t)

	q := NewInProcessClient()
	if err := q.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close is idempotent -- both the owning process and a collector's Cleanup
	// may reach it.
	if err := q.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	err := q.PublishLogWorkToWorker(context.Background(), &LogCollectionWork{}, "w1")
	if !errors.Is(err, ErrQueueClosed) {
		t.Fatalf("publish after close returned %v, want ErrQueueClosed", err)
	}
}

// TestSharedClientCloseIsNoOp covers the shutdown ordering bug that sharing one
// queue client between the log and metric collectors would otherwise cause: each
// closes its queue from its own Cleanup, and the first would take the connection
// out from under the second.
func TestSharedClientCloseIsNoOp(t *testing.T) {
	testutil.SetupTestLogger(t)

	q := NewInProcessClient()
	shared := Shared(q)

	if err := shared.Close(); err != nil {
		t.Fatalf("shared Close: %v", err)
	}
	if err := q.PublishLogWorkToWorker(context.Background(), &LogCollectionWork{}, "w1"); err != nil {
		t.Fatalf("underlying queue was closed by the shared view: %v", err)
	}

	if err := q.Close(); err != nil {
		t.Fatalf("owner Close: %v", err)
	}
	if err := q.PublishLogWorkToWorker(context.Background(), &LogCollectionWork{}, "w1"); !errors.Is(err, ErrQueueClosed) {
		t.Fatalf("owner Close did not close the queue: %v", err)
	}
}

// TestInProcessDropLogIsRateLimited covers the drop path's logging behavior.
//
// One ERROR line per dropped item means a collector that is shedding logs at
// shed rate -- a measured run produced 190,000 lines -- which is the worst
// thing to hand an operator who is already losing work. The volume belongs on
// the counter; the log carries the transitions. Asserted against the rate-limit
// state the log line is derived from, since zap's observer is not vendored.
func TestInProcessDropLogIsRateLimited(t *testing.T) {
	testutil.SetupTestLogger(t)

	q := NewInProcessClient()
	q.maxFallback = 2
	defer q.Close() //nolint:errcheck // test cleanup
	ctx := context.Background()

	for i := 0; i < q.maxFallback; i++ {
		if err := q.PublishToFallbackQueue(ctx, LogsWorkerType, i); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}

	// First drop enters the dropping state and logs.
	if err := q.PublishToFallbackQueue(ctx, LogsWorkerType, "drop"); !errors.Is(err, ErrFallbackFull) {
		t.Fatalf("first drop returned %v, want ErrFallbackFull", err)
	}
	q.mu.Lock()
	firstLog := q.lastDropLog[LogsWorkerType]
	dropping := q.dropping[LogsWorkerType]
	q.mu.Unlock()
	if !dropping {
		t.Fatal("the first drop did not enter the dropping state")
	}

	// A further 99 drops inside the interval must not log again.
	for i := 0; i < 99; i++ {
		_ = q.PublishToFallbackQueue(ctx, LogsWorkerType, i) //nolint:errcheck // expected to drop
	}
	q.mu.Lock()
	sameLog := q.lastDropLog[LogsWorkerType].Equal(firstLog)
	total := q.fallbackDropped[LogsWorkerType]
	q.mu.Unlock()
	if !sameLog {
		t.Error("a sustained run of drops logged more than once inside the interval")
	}
	if total != 100 {
		t.Errorf("dropped_total = %d, want 100 -- the counter must carry the volume", total)
	}

	// Once the interval has passed, a summary is emitted again.
	q.mu.Lock()
	q.lastDropLog[LogsWorkerType] = time.Now().Add(-2 * dropLogInterval)
	q.mu.Unlock()
	_ = q.PublishToFallbackQueue(ctx, LogsWorkerType, "later") //nolint:errcheck // expected to drop
	q.mu.Lock()
	advanced := q.lastDropLog[LogsWorkerType].After(firstLog)
	q.mu.Unlock()
	if !advanced {
		t.Error("no periodic summary after the drop-log interval elapsed")
	}

	// Draining below the cap lets work through again and ends the incident.
	if _, err := q.ConsumeFromFallbackQueue(ctx, LogsWorkerType); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if err := q.PublishToFallbackQueue(ctx, LogsWorkerType, "recovered"); err != nil {
		t.Fatalf("publish after draining below the cap: %v", err)
	}
	q.mu.Lock()
	stillDropping := q.dropping[LogsWorkerType]
	q.mu.Unlock()
	if stillDropping {
		t.Error("the dropping state was never cleared, so recovery is invisible in the log")
	}
}
