package k8s

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// MockClient is a mock implementation of the Interface for testing purposes.
// It stores data in memory and provides the same interface as the Kubernetes client.
type MockClient struct {
	clientset          kubernetes.Interface
	pods               map[string][]corev1.Pod
	logs               map[string][]LogEntry
	metrics            map[string][]PodMetrics
	events             map[string][]EventEntry
	deploymentStatuses map[string][]DeploymentStatusEntry
	hpaStatuses        map[string][]HPAStatusEntry
	endpointHealth     map[string][]EndpointHealthEntry
	pvcStatuses        map[string][]PVCStatusEntry
	configMaps         map[string][]ConfigMapEntry
	jobStatuses        map[string][]JobStatusEntry
	cronJobStatuses    map[string][]CronJobStatusEntry
	pvStatuses         []PVStatusEntry
	nodeStatuses       []NodeStatusEntry
	namespaces         []string
	// ListNamespacesFunc, when set, fully overrides ListNamespaces so tests can
	// drive label-selector behavior (and errors). When nil, ListNamespaces
	// returns the preset namespaces slice regardless of labelSelector.
	ListNamespacesFunc func(ctx context.Context, labelSelector string) ([]string, error)
	streamCount        int
	streamCallCount    int
	mu                 sync.RWMutex
	closed             bool
}

// NewMockClient creates a new mock Kubernetes client for testing.
func NewMockClient() Interface {
	return &MockClient{
		pods:               make(map[string][]corev1.Pod),
		logs:               make(map[string][]LogEntry),
		metrics:            make(map[string][]PodMetrics),
		events:             make(map[string][]EventEntry),
		deploymentStatuses: make(map[string][]DeploymentStatusEntry),
		hpaStatuses:        make(map[string][]HPAStatusEntry),
		endpointHealth:     make(map[string][]EndpointHealthEntry),
		pvcStatuses:        make(map[string][]PVCStatusEntry),
		configMaps:         make(map[string][]ConfigMapEntry),
		jobStatuses:        make(map[string][]JobStatusEntry),
		cronJobStatuses:    make(map[string][]CronJobStatusEntry),
	}
}

// GetPods retrieves pods from mock storage.
func (m *MockClient) GetPods(ctx context.Context, namespace string) (*corev1.PodList, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.closed {
		return nil, fmt.Errorf("mock client is closed")
	}

	pods, exists := m.pods[namespace]
	if !exists {
		pods = []corev1.Pod{}
	}

	return &corev1.PodList{
		TypeMeta: metav1.TypeMeta{
			Kind:       "PodList",
			APIVersion: "v1",
		},
		Items: pods,
	}, nil
}

// GetPod retrieves a specific pod by name from mock storage.
func (m *MockClient) GetPod(ctx context.Context, namespace, podName string) (*corev1.Pod, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.closed {
		return nil, fmt.Errorf("mock client is closed")
	}

	pods, exists := m.pods[namespace]
	if !exists {
		return nil, fmt.Errorf("pod %s not found in namespace %s", podName, namespace)
	}

	for i := range pods {
		if pods[i].Name == podName {
			return &pods[i], nil
		}
	}

	return nil, fmt.Errorf("pod %s not found in namespace %s", podName, namespace)
}

// IsPodRunningByName checks if a pod is running by namespace and name
func (m *MockClient) IsPodRunningByName(ctx context.Context, namespace, podName string) bool {
	pod, err := m.GetPod(ctx, namespace, podName)
	if err != nil {
		return false
	}

	podInfo, err := m.ExtractPodInfo(pod)
	if err != nil {
		return false
	}

	return m.IsPodRunning(podInfo)
}

// GetPodLogs retrieves logs from mock storage.
func (m *MockClient) GetPodLogs(
	ctx context.Context,
	namespace, podName, containerName string,
	tailLines int64,
) ([]LogEntry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.closed {
		return nil, fmt.Errorf("mock client is closed")
	}

	podKey := fmt.Sprintf("%s/%s/%s", namespace, podName, containerName)
	logs, exists := m.logs[podKey]
	if !exists {
		return []LogEntry{}, nil
	}

	// Apply tail lines limit
	if tailLines > 0 && int64(len(logs)) > tailLines {
		logs = logs[len(logs)-int(tailLines):]
	}

	// Return a copy to avoid race conditions
	result := make([]LogEntry, len(logs))
	copy(result, logs)
	return result, nil
}

// StreamPodLogs simulates streaming logs by sending existing logs to the channel.
func (m *MockClient) StreamPodLogs(
	ctx context.Context,
	namespace, podName, containerName string,
	logChan chan<- LogEntry,
) error {
	m.mu.Lock()
	m.streamCount++
	m.streamCallCount++ // cumulative; never decremented, for deterministic assertions
	streamID := m.streamCount
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		m.streamCount--
		m.mu.Unlock()
	}()

	if m.closed {
		return fmt.Errorf("mock client is closed")
	}

	podKey := fmt.Sprintf("%s/%s/%s", namespace, podName, containerName)

	// Get existing logs
	m.mu.RLock()
	logs, exists := m.logs[podKey]
	if !exists {
		logs = []LogEntry{}
	}
	// Create a copy to avoid race conditions
	logsCopy := make([]LogEntry, len(logs))
	copy(logsCopy, logs)
	m.mu.RUnlock()

	// Send existing logs
	for _, log := range logsCopy {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case logChan <- log:
		}
	}

	// Simulate continuous streaming by periodically sending new logs
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	logCounter := len(logsCopy)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			// Generate a new mock log entry
			newLog := LogEntry{
				Timestamp: time.Now(),
				Namespace: namespace,
				PodName:   podName,
				Container: containerName,
				Message:   fmt.Sprintf("Mock log entry %d for stream %d", logCounter, streamID),
			}

			// Add to storage
			m.mu.Lock()
			m.logs[podKey] = append(m.logs[podKey], newLog)
			m.mu.Unlock()

			// Send to channel
			select {
			case <-ctx.Done():
				return ctx.Err()
			case logChan <- newLog:
				logCounter++
			}
		}
	}
}

// GetPodMetrics retrieves metrics from mock storage.
func (m *MockClient) GetPodMetrics(ctx context.Context, namespace string) ([]PodMetrics, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.closed {
		return nil, fmt.Errorf("mock client is closed")
	}

	metrics, exists := m.metrics[namespace]
	if !exists {
		metrics = []PodMetrics{}
	}

	// Return a copy to avoid race conditions
	result := make([]PodMetrics, len(metrics))
	copy(result, metrics)
	return result, nil
}

// ListNamespaces returns namespace names from mock storage. If
// ListNamespacesFunc is set it fully overrides the behavior (letting tests match
// on labelSelector or return errors); otherwise the preset namespaces slice is
// returned regardless of labelSelector.
func (m *MockClient) ListNamespaces(ctx context.Context, labelSelector string) ([]string, error) {
	if m.ListNamespacesFunc != nil {
		return m.ListNamespacesFunc(ctx, labelSelector)
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.closed {
		return nil, fmt.Errorf("mock client is closed")
	}

	result := make([]string, len(m.namespaces))
	copy(result, m.namespaces)
	return result, nil
}

// SetNamespaces presets the namespace names returned by ListNamespaces when
// ListNamespacesFunc is not set.
func (m *MockClient) SetNamespaces(namespaces ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.namespaces = append([]string(nil), namespaces...)
}

// CreatePodInformer creates a mock pod informer
func (m *MockClient) CreatePodInformer() (PodInformer, error) {
	return &MockPodInformer{}, nil
}

// ExtractPodInfo extracts PodInfo from a Kubernetes object
func (m *MockClient) ExtractPodInfo(obj interface{}) (*PodInfo, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil, fmt.Errorf("failed to cast object to Pod, got type: %T", obj)
	}

	containers := make([]ContainerInfo, len(pod.Spec.Containers))
	for i := range pod.Spec.Containers {
		containers[i] = ContainerInfo{Name: pod.Spec.Containers[i].Name}
	}

	return &PodInfo{
		UID:        string(pod.UID),
		Name:       pod.Name,
		Namespace:  pod.Namespace,
		NodeName:   pod.Spec.NodeName,
		Phase:      string(pod.Status.Phase),
		Containers: containers,
	}, nil
}

// IsPodRunning checks if a pod is in running state
func (m *MockClient) IsPodRunning(podInfo *PodInfo) bool {
	return podInfo.Phase == string(corev1.PodRunning)
}

// GeneratePodKey generates a pod key from PodInfo
func (m *MockClient) GeneratePodKey(podInfo *PodInfo) (string, error) {
	if podInfo.Namespace == "" || podInfo.Name == "" {
		return "", fmt.Errorf("pod namespace and name cannot be empty")
	}
	return fmt.Sprintf("%s/%s", podInfo.Namespace, podInfo.Name), nil
}

// MockPodInformer is a mock implementation of PodInformer
type MockPodInformer struct{}

func (m *MockPodInformer) AddEventHandler(handler cache.ResourceEventHandler) error {
	return nil
}

func (m *MockPodInformer) Run(stopCh <-chan struct{}) {
	// Do nothing in mock
}

func (m *MockPodInformer) HasSynced() bool {
	return true
}

func (m *MockPodInformer) GetStore() cache.Store {
	return &MockStore{}
}

// MockStore is a mock implementation of cache.Store
type MockStore struct{}

func (m *MockStore) Add(obj interface{}) error {
	return nil
}

func (m *MockStore) Update(obj interface{}) error {
	return nil
}

func (m *MockStore) Delete(obj interface{}) error {
	return nil
}

func (m *MockStore) List() []interface{} {
	return nil
}

func (m *MockStore) ListKeys() []string {
	return nil
}

func (m *MockStore) Get(obj interface{}) (item interface{}, exists bool, err error) {
	return nil, false, nil
}

func (m *MockStore) GetByKey(key string) (item interface{}, exists bool, err error) {
	return nil, false, nil
}

func (m *MockStore) Replace([]interface{}, string) error {
	return nil
}

func (m *MockStore) Resync() error {
	return nil
}

// Test helper methods

// AddPod adds a pod to the mock storage.
func (m *MockClient) AddPod(namespace string, pod *corev1.Pod) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.pods[namespace] == nil {
		m.pods[namespace] = []corev1.Pod{}
	}
	m.pods[namespace] = append(m.pods[namespace], *pod)
}

// AddLogEntry adds a log entry to the mock storage.
func (m *MockClient) AddLogEntry(namespace, podName, containerName string, entry *LogEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()

	podKey := fmt.Sprintf("%s/%s/%s", namespace, podName, containerName)
	if m.logs[podKey] == nil {
		m.logs[podKey] = []LogEntry{}
	}
	m.logs[podKey] = append(m.logs[podKey], *entry)
}

// AddPodMetrics adds pod metrics to the mock storage.
func (m *MockClient) AddPodMetrics(namespace string, metrics *PodMetrics) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.metrics[namespace] == nil {
		m.metrics[namespace] = []PodMetrics{}
	}
	m.metrics[namespace] = append(m.metrics[namespace], *metrics)
}

// CreateMockPod creates a mock pod for testing.
func CreateMockPod(namespace, name string, containers []string) corev1.Pod {
	containerSpecs := make([]corev1.Container, 0, len(containers))
	for _, containerName := range containers {
		containerSpecs = append(containerSpecs, corev1.Container{
			Name:  containerName,
			Image: "mock-image:latest",
		})
	}

	return corev1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1.PodSpec{
			Containers: containerSpecs,
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}
}

// CreateMockLogEntry creates a mock log entry for testing.
func CreateMockLogEntry(namespace, podName, containerName, message string) LogEntry {
	return LogEntry{
		Timestamp: time.Now(),
		Namespace: namespace,
		PodName:   podName,
		Container: containerName,
		Message:   message,
	}
}

// CreateMockPodMetrics creates mock pod metrics for testing.
func CreateMockPodMetrics(namespace, podName string, containers map[string]ContainerMetrics) PodMetrics {
	containerMetrics := make([]ContainerMetrics, 0, len(containers))
	for name, metrics := range containers {
		metrics.Name = name
		containerMetrics = append(containerMetrics, metrics)
	}

	return PodMetrics{
		Namespace:  namespace,
		PodName:    podName,
		Containers: containerMetrics,
		Timestamp:  time.Now(),
	}
}

// CreateMockContainerMetrics creates mock container metrics for testing.
func CreateMockContainerMetrics(cpuMillis, memoryMB int) ContainerMetrics {
	cpu := resource.NewMilliQuantity(int64(cpuMillis), resource.DecimalSI)
	memory := resource.NewQuantity(int64(memoryMB)*1024*1024, resource.BinarySI)

	return ContainerMetrics{
		CPU:    cpu.String(),
		Memory: memory.String(),
	}
}

// ListEvents retrieves events from mock storage.
func (m *MockClient) ListEvents(ctx context.Context, namespace string, since time.Time) ([]EventEntry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.closed {
		return nil, fmt.Errorf("mock client is closed")
	}

	events, exists := m.events[namespace]
	if !exists {
		return []EventEntry{}, nil
	}

	filtered := make([]EventEntry, 0, len(events))
	for i := range events {
		ev := events[i]
		if !since.IsZero() && ev.Timestamp.Before(since) {
			continue
		}
		filtered = append(filtered, ev)
	}

	return filtered, nil
}

// AddDeploymentStatusEntry adds a deployment status entry to the mock storage.
func (m *MockClient) AddDeploymentStatusEntry(namespace string, entry *DeploymentStatusEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.deploymentStatuses[namespace] == nil {
		m.deploymentStatuses[namespace] = []DeploymentStatusEntry{}
	}
	m.deploymentStatuses[namespace] = append(m.deploymentStatuses[namespace], *entry)
}

// AddHPAStatusEntry adds an HPA status entry to the mock storage.
func (m *MockClient) AddHPAStatusEntry(namespace string, entry *HPAStatusEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.hpaStatuses[namespace] == nil {
		m.hpaStatuses[namespace] = []HPAStatusEntry{}
	}
	m.hpaStatuses[namespace] = append(m.hpaStatuses[namespace], *entry)
}

// AddEndpointHealthEntry adds an endpoint health entry to the mock storage.
func (m *MockClient) AddEndpointHealthEntry(namespace string, entry *EndpointHealthEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.endpointHealth[namespace] == nil {
		m.endpointHealth[namespace] = []EndpointHealthEntry{}
	}
	m.endpointHealth[namespace] = append(m.endpointHealth[namespace], *entry)
}

// AddPVCStatusEntry adds a PVC status entry to the mock storage.
func (m *MockClient) AddPVCStatusEntry(namespace string, entry *PVCStatusEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.pvcStatuses[namespace] == nil {
		m.pvcStatuses[namespace] = []PVCStatusEntry{}
	}
	m.pvcStatuses[namespace] = append(m.pvcStatuses[namespace], *entry)
}

// AddPVStatusEntry adds a PV status entry to the mock storage.
func (m *MockClient) AddPVStatusEntry(entry *PVStatusEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.pvStatuses = append(m.pvStatuses, *entry)
}

// AddNodeStatusEntry adds a node status entry to the mock storage.
func (m *MockClient) AddNodeStatusEntry(entry *NodeStatusEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.nodeStatuses = append(m.nodeStatuses, *entry)
}

// AddEventEntry adds an event entry to the mock storage.
func (m *MockClient) AddEventEntry(namespace string, entry *EventEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.events[namespace] == nil {
		m.events[namespace] = []EventEntry{}
	}
	m.events[namespace] = append(m.events[namespace], *entry)
}

// AddConfigMapEntry adds a ConfigMap entry to the mock storage.
func (m *MockClient) AddConfigMapEntry(namespace string, entry *ConfigMapEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.configMaps[namespace] == nil {
		m.configMaps[namespace] = []ConfigMapEntry{}
	}
	m.configMaps[namespace] = append(m.configMaps[namespace], *entry)
}

// AddJobStatusEntry adds a Job status entry to the mock storage.
func (m *MockClient) AddJobStatusEntry(namespace string, entry *JobStatusEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.jobStatuses[namespace] == nil {
		m.jobStatuses[namespace] = []JobStatusEntry{}
	}
	m.jobStatuses[namespace] = append(m.jobStatuses[namespace], *entry)
}

// AddCronJobStatusEntry adds a CronJob status entry to the mock storage.
func (m *MockClient) AddCronJobStatusEntry(namespace string, entry *CronJobStatusEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cronJobStatuses[namespace] == nil {
		m.cronJobStatuses[namespace] = []CronJobStatusEntry{}
	}
	m.cronJobStatuses[namespace] = append(m.cronJobStatuses[namespace], *entry)
}

// Reset clears all data from the mock client (useful for tests).
func (m *MockClient) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.pods = make(map[string][]corev1.Pod)
	m.logs = make(map[string][]LogEntry)
	m.metrics = make(map[string][]PodMetrics)
	m.events = make(map[string][]EventEntry)
	m.nodeStatuses = nil
	m.deploymentStatuses = make(map[string][]DeploymentStatusEntry)
	m.hpaStatuses = make(map[string][]HPAStatusEntry)
	m.endpointHealth = make(map[string][]EndpointHealthEntry)
	m.pvcStatuses = make(map[string][]PVCStatusEntry)
	m.configMaps = make(map[string][]ConfigMapEntry)
	m.jobStatuses = make(map[string][]JobStatusEntry)
	m.cronJobStatuses = make(map[string][]CronJobStatusEntry)
	m.pvStatuses = nil
	m.namespaces = nil
	m.closed = false
	m.streamCount = 0
	m.streamCallCount = 0
}

// Close marks the mock client as closed.
func (m *MockClient) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.closed = true
}

// GetStreamCount returns the number of active streams (useful for testing).
func (m *MockClient) GetStreamCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.streamCount
}

// GetStreamCallCount returns the cumulative number of StreamPodLogs invocations
// since construction (or the last Reset). Unlike GetStreamCount it never
// decrements, so tests can deterministically assert whether streaming was ever
// started for a given namespace.
func (m *MockClient) GetStreamCallCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.streamCallCount
}

// The Watch* emulations deliver the stored entries for every namespace the
// filter admits -- the informer's initial list -- and then block until ctx
// ends, returning ctx.Err() as the real informers do. They ignore the static
// namespace list: the filter alone decides. They do not resync; a test that
// needs re-delivery calls the worker's process method.

// deliverStored emits a snapshot of every stored item in the admitted
// namespaces, in namespace order, without holding the lock during callbacks.
func deliverStored[T any](mu *sync.RWMutex, store map[string][]T, inScope NamespaceFilter, emit func(*T)) {
	mu.RLock()
	keys := make([]string, 0, len(store))
	for k := range store {
		keys = append(keys, k)
	}
	mu.RUnlock()
	sort.Strings(keys)
	for _, ns := range keys {
		if inScope != nil && !inScope(ns) {
			continue
		}
		mu.RLock()
		items := slices.Clone(store[ns])
		mu.RUnlock()
		for i := range items {
			emit(&items[i])
		}
	}
}

func (m *MockClient) WatchPodStatuses(
	ctx context.Context, _ time.Duration, _ []string, inScope NamespaceFilter, emit func(*PodStatusEntry),
) error {
	now := time.Now()
	deliverStored(&m.mu, m.pods, inScope, func(pod *corev1.Pod) {
		e := podStatusEntry(pod, now)
		emit(&e)
	})
	<-ctx.Done()
	return ctx.Err()
}

func (m *MockClient) WatchNodeStatuses(ctx context.Context, _ time.Duration, emit func(*NodeStatusEntry)) error {
	m.mu.RLock()
	items := slices.Clone(m.nodeStatuses)
	m.mu.RUnlock()
	for i := range items {
		emit(&items[i])
	}
	<-ctx.Done()
	return ctx.Err()
}

func (m *MockClient) WatchDeploymentStatuses(
	ctx context.Context, _ time.Duration, _ []string, inScope NamespaceFilter, emit func(*DeploymentStatusEntry),
) error {
	deliverStored(&m.mu, m.deploymentStatuses, inScope, emit)
	<-ctx.Done()
	return ctx.Err()
}

func (m *MockClient) WatchHPAStatuses(
	ctx context.Context, _ time.Duration, _ []string, inScope NamespaceFilter, emit func(*HPAStatusEntry),
) error {
	deliverStored(&m.mu, m.hpaStatuses, inScope, emit)
	<-ctx.Done()
	return ctx.Err()
}

func (m *MockClient) WatchEndpointHealth(
	ctx context.Context, _ time.Duration, _ []string, inScope NamespaceFilter, emit func(*EndpointHealthEntry),
) error {
	deliverStored(&m.mu, m.endpointHealth, inScope, emit)
	<-ctx.Done()
	return ctx.Err()
}

func (m *MockClient) WatchVolumeStatuses(
	ctx context.Context, _ time.Duration, _ []string, inScope NamespaceFilter,
	emitPVC func(*PVCStatusEntry), emitPV func(*PVStatusEntry),
) error {
	deliverStored(&m.mu, m.pvcStatuses, inScope, emitPVC)
	m.mu.RLock()
	pvs := slices.Clone(m.pvStatuses)
	m.mu.RUnlock()
	for i := range pvs {
		emitPV(&pvs[i])
	}
	<-ctx.Done()
	return ctx.Err()
}

func (m *MockClient) WatchConfigMetadata(
	ctx context.Context, _ time.Duration, _ []string, inScope NamespaceFilter,
	emitConfigMap func(*ConfigMapEntry),
) error {
	deliverStored(&m.mu, m.configMaps, inScope, emitConfigMap)
	<-ctx.Done()
	return ctx.Err()
}

func (m *MockClient) WatchJobStatuses(
	ctx context.Context, _ time.Duration, _ []string, inScope NamespaceFilter,
	emitJob func(*JobStatusEntry), emitCronJob func(*CronJobStatusEntry),
) error {
	deliverStored(&m.mu, m.jobStatuses, inScope, emitJob)
	deliverStored(&m.mu, m.cronJobStatuses, inScope, emitCronJob)
	<-ctx.Done()
	return ctx.Err()
}
