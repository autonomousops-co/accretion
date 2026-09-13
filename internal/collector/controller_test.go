package collector

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/autonomousops-co/accretion/internal/config"
	"github.com/autonomousops-co/accretion/internal/k8s"
	"github.com/autonomousops-co/accretion/internal/nsscope"
	"github.com/autonomousops-co/accretion/internal/queue"
	"github.com/autonomousops-co/accretion/internal/testutil"
)

// fakeQueue is a minimal queue.Client that records the log/metrics work the
// controller dispatches. GetAllWorkerHealth reports one healthy log worker and
// one healthy metrics worker so the distributor always resolves a worker (and a
// missed log gate would therefore publish real log work, not fall back).
type fakeQueue struct {
	mu              sync.Mutex
	logPublishes    int
	metricPublishes int
	logFallbacks    int
	metricFallbacks int
	// logPublishErr, when set, is returned by PublishLogWorkToWorker. It stands
	// in for a worker whose queue is full.
	logPublishErr error
}

func (f *fakeQueue) PublishLogWorkToWorker(_ context.Context, _ *queue.LogCollectionWork, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.logPublishErr != nil {
		return f.logPublishErr
	}
	f.logPublishes++
	return nil
}

func (f *fakeQueue) PublishMetricsWorkToWorker(_ context.Context, _ *queue.MetricsCollectionWork, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.metricPublishes++
	return nil
}

func (f *fakeQueue) PublishToFallbackQueue(_ context.Context, workType string, _ interface{}) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch workType {
	case queue.LogsWorkerType:
		f.logFallbacks++
	case queue.MetricsWorkerType:
		f.metricFallbacks++
	}
	return nil
}

func (f *fakeQueue) Close() error { return nil }

func (f *fakeQueue) ConsumeLogWorkForWorker(_ context.Context, _ string, _ func(*queue.LogCollectionWork) error) error {
	return nil
}

func (f *fakeQueue) ConsumeMetricsWorkForWorker(_ context.Context, _ string, _ func(*queue.MetricsCollectionWork) error) error {
	return nil
}

func (f *fakeQueue) StoreWorkerHealth(_ context.Context, _ *queue.WorkerHealth) error { return nil }

func (f *fakeQueue) GetWorkerHealth(_ context.Context, _ string) (*queue.WorkerHealth, error) {
	return nil, nil
}

func (f *fakeQueue) GetAllWorkerHealth(_ context.Context) (map[string]*queue.WorkerHealth, error) {
	return map[string]*queue.WorkerHealth{
		"log-worker-1":     {WorkerID: "log-worker-1", Type: "logs", Healthy: true},
		"metrics-worker-1": {WorkerID: "metrics-worker-1", Type: "metrics", Healthy: true},
	}, nil
}

func (f *fakeQueue) DeleteWorkerHealth(_ context.Context, _ string) error { return nil }

func (f *fakeQueue) StorePodAssignment(_ context.Context, _ *queue.PodAssignment) error { return nil }

func (f *fakeQueue) UpdatePodAssignment(_ context.Context, _ *queue.PodAssignment) error { return nil }

func (f *fakeQueue) GetAllPodAssignments(_ context.Context) (map[string]*queue.PodAssignment, error) {
	return map[string]*queue.PodAssignment{}, nil
}

func (f *fakeQueue) DeletePodAssignment(_ context.Context, _ string) error { return nil }

func (f *fakeQueue) ConsumeFromFallbackQueue(_ context.Context, _ string) (interface{}, error) {
	return nil, nil
}

func (f *fakeQueue) GetFallbackQueueLength(_ context.Context, _ string) (int64, error) {
	return 0, nil
}

// storeInformer is a PodInformer backed by a real cache.Store so publishPodRunning
// can resolve a pod by key.
type storeInformer struct {
	store cache.Store
}

func (s *storeInformer) AddEventHandler(_ cache.ResourceEventHandler) error { return nil }
func (s *storeInformer) Run(_ <-chan struct{})                              {}
func (s *storeInformer) HasSynced() bool                                    { return true }
func (s *storeInformer) GetStore() cache.Store                              { return s.store }

// newControllerWithPod builds a controller whose informer already contains pod,
// scoped so metrics/status watch every namespace but logs exclude logExclude.
func newControllerWithPod(t *testing.T, pod *corev1.Pod, metricsNamespaces, logNamespaces []string) (*Controller, *fakeQueue) {
	t.Helper()
	testutil.SetupTestEnvironment(t)

	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	if err := store.Add(pod); err != nil {
		t.Fatalf("failed to seed pod store: %v", err)
	}

	fq := &fakeQueue{}
	c := &Controller{
		k8sClient:   k8s.NewMockClient(),
		queueClient: fq,
		podInformer: &storeInformer{store: store},
		distributor: NewWorkDistributor(fq),
		scope:       nsscope.Static(config.NamespaceSelector{Include: metricsNamespaces}),
		logScope:    nsscope.Static(config.NamespaceSelector{Include: logNamespaces}),
		ctx:         context.Background(),
	}
	return c, fq
}

// TestPublishPodRunning_SelfNamespaceGetsMetricsButNotLogs is the core #145
// enforcement test. A pod in accretion's own namespace (watched for metrics/status
// but excluded from the log scope) must have metrics work dispatched but NOT log
// work. This fails against the pre-fix controller, which published log work for
// every watched pod.
func TestPublishPodRunning_SelfNamespaceGetsMetricsButNotLogs(t *testing.T) {
	pod := k8s.CreateMockPod("accretion-core", "accretion-collector-abc", []string{"collector"})
	// Metrics/status watch accretion-core; the log scope deliberately excludes it.
	c, fq := newControllerWithPod(t, &pod, []string{"default", "accretion-core"}, []string{"default"})

	if err := c.publishPodRunning("accretion-core/accretion-collector-abc"); err != nil {
		t.Fatalf("publishPodRunning returned error: %v", err)
	}

	if fq.metricPublishes != 1 {
		t.Errorf("expected 1 metrics publish for accretion-core pod, got %d", fq.metricPublishes)
	}
	if fq.logPublishes != 0 {
		t.Errorf("expected 0 log publishes for accretion-core pod (self-exclusion), got %d", fq.logPublishes)
	}
	if fq.logFallbacks != 0 {
		t.Errorf("expected 0 log fallback publishes for accretion-core pod, got %d", fq.logFallbacks)
	}
}

// TestPublishPodRunning_WatchedNamespaceGetsBoth confirms the gate is not
// over-broad: a pod in an in-scope namespace still gets both log and metrics
// work, so the self-namespace assertion above is not vacuously true.
func TestPublishPodRunning_WatchedNamespaceGetsBoth(t *testing.T) {
	pod := k8s.CreateMockPod("default", "app-xyz", []string{"app"})
	c, fq := newControllerWithPod(t, &pod, []string{"default", "accretion-core"}, []string{"default"})

	if err := c.publishPodRunning("default/app-xyz"); err != nil {
		t.Fatalf("publishPodRunning returned error: %v", err)
	}

	if fq.metricPublishes != 1 {
		t.Errorf("expected 1 metrics publish for default pod, got %d", fq.metricPublishes)
	}
	if fq.logPublishes != 1 {
		t.Errorf("expected 1 log publish for default pod, got %d", fq.logPublishes)
	}
}

// TestPublishPodRunning_FullLogQueueDoesNotStarveMetrics is the back-pressure
// criterion of #92.
//
// Log and metric work were dispatched sequentially on one goroutine, and a log
// failure returned before metric work was ever published. That was harmless
// with Redis, whose LPUSH does not block, and is not harmless with an
// in-process queue: a backed-up log collector would stop metric work being
// dispatched at all, even though the metric collector was sitting idle. The
// controller runs a small fixed pool of these goroutines, so a handful of
// wedged pod keys stalled the whole dispatcher.
func TestPublishPodRunning_FullLogQueueDoesNotStarveMetrics(t *testing.T) {
	pod := k8s.CreateMockPod("default", "app-xyz", []string{"app"})
	c, fq := newControllerWithPod(t, &pod, []string{"default"}, []string{"default"})
	fq.logPublishErr = fmt.Errorf("worker w1: %w", queue.ErrQueueFull)

	if err := c.publishPodRunning("default/app-xyz"); err != nil {
		t.Fatalf("a full log queue must be shed, not returned as an error: %v", err)
	}

	if fq.metricPublishes != 1 {
		t.Errorf("metric work was not published (%d) while the log queue was full; "+
			"the two must dispatch independently", fq.metricPublishes)
	}
	if fq.logPublishes != 0 {
		t.Errorf("expected 0 direct log publishes, got %d", fq.logPublishes)
	}
	if fq.logFallbacks != 1 {
		t.Errorf("log work should have been shed to the fallback queue, got %d", fq.logFallbacks)
	}
}

// TestPublishPodRunning_RealPublishErrorStillSurfaces keeps the test above from
// proving too much: an error that is NOT a full queue must still fail the work
// item so the controller retries it.
func TestPublishPodRunning_RealPublishErrorStillSurfaces(t *testing.T) {
	pod := k8s.CreateMockPod("default", "app-xyz", []string{"app"})
	c, fq := newControllerWithPod(t, &pod, []string{"default"}, []string{"default"})
	fq.logPublishErr = errors.New("connection refused")

	err := c.publishPodRunning("default/app-xyz")
	if err == nil {
		t.Fatal("a genuine publish failure must be returned so the item is retried")
	}
	// Metric work still went out: independence cuts both ways.
	if fq.metricPublishes != 1 {
		t.Errorf("metric work was not published (%d) despite an unrelated log failure", fq.metricPublishes)
	}
}
