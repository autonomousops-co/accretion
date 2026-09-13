package k8s

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
)

// Interface defines the Kubernetes operations contract.
// This interface allows for different implementations and enables easier testing through mock implementations.
type Interface interface {
	// Pod operations
	GetPods(ctx context.Context, namespace string) (*corev1.PodList, error)
	GetPod(ctx context.Context, namespace, podName string) (*corev1.Pod, error)
	GetPodLogs(ctx context.Context, namespace, podName, containerName string, tailLines int64) ([]LogEntry, error)
	StreamPodLogs(ctx context.Context, namespace, podName, containerName string, logChan chan<- LogEntry) error

	// Metrics operations
	GetPodMetrics(ctx context.Context, namespace string) ([]PodMetrics, error)

	// Informer operations
	CreatePodInformer() (PodInformer, error)

	// Pod utility operations
	ExtractPodInfo(obj interface{}) (*PodInfo, error)
	IsPodRunning(podInfo *PodInfo) bool
	IsPodRunningByName(ctx context.Context, namespace, podName string) bool
	GeneratePodKey(podInfo *PodInfo) (string, error)

	// Event operations
	ListEvents(ctx context.Context, namespace string, since time.Time) ([]EventEntry, error)

	// Namespace listing (for label-selector scoping)
	ListNamespaces(ctx context.Context, labelSelector string) ([]string, error)

	// Watch-driven status collection (#96). Each blocks until ctx ends or an
	// informer fails, delivering an entry per in-scope object on the initial
	// list, on every change, and again at each resync. namespaces is the
	// explicit static scope (one informer per namespace) or empty for a
	// dynamic scope (one cluster-wide informer); inScope applies on both paths.
	// See statusinformers.go.
	WatchPodStatuses(ctx context.Context, resync time.Duration, namespaces []string, inScope NamespaceFilter,
		emit func(*PodStatusEntry)) error
	WatchNodeStatuses(ctx context.Context, resync time.Duration, emit func(*NodeStatusEntry)) error
	WatchDeploymentStatuses(ctx context.Context, resync time.Duration, namespaces []string, inScope NamespaceFilter,
		emit func(*DeploymentStatusEntry)) error
	WatchHPAStatuses(ctx context.Context, resync time.Duration, namespaces []string, inScope NamespaceFilter,
		emit func(*HPAStatusEntry)) error
	WatchEndpointHealth(ctx context.Context, resync time.Duration, namespaces []string, inScope NamespaceFilter,
		emit func(*EndpointHealthEntry)) error
	WatchVolumeStatuses(ctx context.Context, resync time.Duration, namespaces []string, inScope NamespaceFilter,
		emitPVC func(*PVCStatusEntry), emitPV func(*PVStatusEntry)) error
	WatchConfigMetadata(ctx context.Context, resync time.Duration, namespaces []string, inScope NamespaceFilter,
		emitConfigMap func(*ConfigMapEntry)) error
	WatchJobStatuses(ctx context.Context, resync time.Duration, namespaces []string, inScope NamespaceFilter,
		emitJob func(*JobStatusEntry), emitCronJob func(*CronJobStatusEntry)) error
}

// Client implements the Interface for Kubernetes operations.
// It provides concrete implementations for all Kubernetes operations using the official client-go library.
type Client struct {
	clientset        *kubernetes.Clientset
	metricsClientset *metricsclient.Clientset
	config           *rest.Config
}

// Compile-time verification that Client implements Interface
var _ Interface = (*Client)(nil)

// NewClientWithRateLimit creates a client with explicit client-side rate limits.
//
// A process that runs several collectors shares ONE client and therefore one
// rate limiter, so the library's 5 QPS / 10 burst default -- adequate when every
// worker had its own pod and its own client -- becomes the throughput ceiling
// for all of them together. Values of zero or less fall back to the defaults.
func NewClientWithRateLimit(configPath string, qps float32, burst int) (Interface, error) {
	return newKubernetesClient(configPath, qps, burst)
}

// Client-side rate-limit defaults, mirroring config.DefaultK8sQPS/Burst. They
// are duplicated rather than imported to keep this package free of a dependency
// on internal/config.
const (
	defaultQPS   float32 = 50
	defaultBurst int     = 100
)

// newKubernetesClient creates a new Kubernetes client implementation
func newKubernetesClient(configPath string, qps float32, burst int) (*Client, error) {
	var config *rest.Config
	var err error

	if configPath == "" {
		// Try in-cluster config first
		config, err = rest.InClusterConfig()
		if err != nil {
			// Fall back to local kubeconfig
			config, err = clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
			if err != nil {
				return nil, fmt.Errorf("failed to create kubernetes config: %w", err)
			}
		}
	} else {
		config, err = clientcmd.BuildConfigFromFlags("", configPath)
		if err != nil {
			return nil, fmt.Errorf("failed to create kubernetes config from path %s: %w", configPath, err)
		}
	}

	// Set the client-side rate limits BEFORE building either clientset; both
	// derive their limiter from this config, and unset means client-go's 5/10.
	if qps <= 0 {
		qps = defaultQPS
	}
	if burst <= 0 {
		burst = defaultBurst
	}
	config.QPS = qps
	config.Burst = burst

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes clientset: %w", err)
	}

	metricsClientset, err := metricsclient.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create metrics clientset: %w", err)
	}

	return &Client{
		clientset:        clientset,
		metricsClientset: metricsClientset,
		config:           config,
	}, nil
}

// ListNamespaces returns the names of namespaces matching labelSelector. An
// empty labelSelector lists every namespace. It backs config.NamespaceLister so
// NamespaceSelector.Expand can resolve label selectors and the "*" sentinel.
func (c *Client) ListNamespaces(ctx context.Context, labelSelector string) ([]string, error) {
	list, err := c.clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, fmt.Errorf("failed to list namespaces (labelSelector %q): %w", labelSelector, err)
	}

	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].Name)
	}
	return names, nil
}
