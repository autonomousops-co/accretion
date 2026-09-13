package collector

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/autonomousops-co/accretion/internal/config"
	"github.com/autonomousops-co/accretion/internal/k8s"
	"github.com/autonomousops-co/accretion/internal/logger"
	"github.com/autonomousops-co/accretion/internal/nsscope"
	"github.com/autonomousops-co/accretion/internal/queue"
)

// Manager provides a high-level interface for managing the controller.
// It handles initialization, configuration, and lifecycle management.
type Manager struct {
	controller  *Controller
	k8sClient   k8s.Interface
	queueClient queue.Client
	config      *config.CollectorConfig
	// k8sScope is the live K8s namespace scope, held so Start can log the current
	// namespace set without re-listing.
	k8sScope nsscope.Scope
}

// NewManager creates a new Manager instance. ctx bounds the lifetime of the
// namespace scopes' background re-resolution, so it must be the caller's
// shutdown context rather than context.Background().
func NewManager(ctx context.Context, cfg *config.CollectorConfig) (*Manager, error) {
	// Initialize Kubernetes client
	qps, burst := cfg.K8s.RateLimits()
	k8sClient, err := k8s.NewClientWithRateLimit(cfg.K8s.ConfigPath, qps, burst)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	// Initialize queue client
	queueClient, err := queue.NewRedisClient(
		cfg.Queue.RedisAddr,
		cfg.Queue.RedisPassword,
		cfg.Queue.RedisDB,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create queue client: %w", err)
	}

	return NewManagerWithQueue(ctx, cfg, k8sClient, queueClient)
}

// NewManagerWithQueue creates a Manager over clients the caller already holds.
//
// This is what lets the controller run embedded in a collector process against
// an in-process queue (#93): with one replica of each collector and no HPA there
// is nothing to shard, so the dispatcher hands work to the collectors in its own
// process rather than routing it through Redis to assign work to itself. The
// standalone controller Deployment takes the same path with a Redis client.
func NewManagerWithQueue(
	ctx context.Context, cfg *config.CollectorConfig,
	k8sClient k8s.Interface, queueClient queue.Client,
) (*Manager, error) {
	// Apply defaults for controller configuration
	numWorkers := cfg.ControllerWorkerCount
	if numWorkers <= 0 {
		numWorkers = 5
	}

	workerPeriod, err := parseDurationWithDefault(cfg.ControllerWorkerPeriod, time.Second)
	if err != nil {
		return nil, fmt.Errorf("invalid controller worker period: %w", err)
	}

	// Build the namespace scopes. Resolution is a no-op (zero API calls, no
	// refresh goroutine) unless the label/"*" features are configured; otherwise
	// the scope resolves now and re-resolves every refreshInterval so namespaces
	// created later come into scope without a restart (#152). An initial
	// resolution failure must surface, not silently scope to nothing/everything.
	refreshInterval, err := cfg.NamespaceRefreshInterval()
	if err != nil {
		return nil, err
	}
	k8sScope, err := nsscope.New(ctx, cfg.K8sNamespaceSelector(), k8sClient, refreshInterval)
	if err != nil {
		return nil, fmt.Errorf("resolving k8s namespace scope: %w", err)
	}
	// The log scope shares the k8s scope's include/label/"*" resolution and
	// differs only by the log-only name excludes (self namespace + logs.exclude),
	// which are applied at read time. Derive it as a view over k8sScope instead of
	// resolving the identical query a second time: no extra LIST, and it tracks
	// k8sScope's background refreshes automatically (#155). Self-exclusion of
	// accretion's own namespace from logs (#145) still holds — Derived subtracts it.
	logScope := nsscope.Derived(k8sScope, cfg.LogOnlyExcludes()...)

	// Create controller
	controller, err := NewController(
		k8sClient, queueClient,
		k8sScope, logScope,
		numWorkers, workerPeriod,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create controller: %w", err)
	}

	return &Manager{
		controller:  controller,
		k8sClient:   k8sClient,
		queueClient: queueClient,
		config:      cfg,
		k8sScope:    k8sScope,
	}, nil
}

// Start initializes and starts the controller.
func (m *Manager) Start(ctx context.Context) error {
	logger.Logger.Info("Starting controller manager",
		zap.Strings("namespaces", m.k8sScope.Namespaces()),
	)

	if err := m.controller.Start(ctx); err != nil {
		return fmt.Errorf("failed to start controller: %w", err)
	}

	logger.Logger.Info("Controller manager started successfully")
	return nil
}

// Stop gracefully shuts down the controller manager.
func (m *Manager) Stop(ctx context.Context) error {
	logger.Logger.Info("Stopping controller manager")

	if err := m.controller.Stop(ctx); err != nil {
		return fmt.Errorf("failed to stop controller: %w", err)
	}

	if err := m.queueClient.Close(); err != nil {
		logger.Logger.Error("Failed to close queue client", zap.Error(err))
	}

	logger.Logger.Info("Controller manager stopped successfully")
	return nil
}

// GetStatus returns the current status of the controller.
func (m *Manager) GetStatus() Status {
	return m.controller.GetStatus()
}

// GetController returns the underlying controller instance.
func (m *Manager) GetController() *Controller {
	return m.controller
}

// HealthCheck performs a health check on the controller.
func (m *Manager) HealthCheck(ctx context.Context) error {
	status := m.GetStatus()

	if !status.Healthy {
		return fmt.Errorf("controller informer cache is not synced")
	}

	if status.QueueLength > 100 {
		logger.Logger.Warn("High queue length detected", zap.Int("queue_length", status.QueueLength))
	}

	// Check worker health
	if status.TotalWorkers == 0 {
		logger.Logger.Warn("No workers detected")
	} else if status.HealthyWorkers < status.TotalWorkers {
		logger.Logger.Warn("Some workers are unhealthy",
			zap.Int("healthy_workers", status.HealthyWorkers),
			zap.Int("total_workers", status.TotalWorkers),
		)
	}

	// Test Kubernetes connectivity
	for _, namespace := range status.Namespaces {
		_, err := m.k8sClient.GetPods(ctx, namespace)
		if err != nil {
			return fmt.Errorf("failed to connect to kubernetes for namespace %s: %w", namespace, err)
		}
	}

	return nil
}

// Wait blocks until the controller is stopped or the context is canceled.
func (m *Manager) Wait(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// parseDurationWithDefault parses a duration string with a default value
func parseDurationWithDefault(durationStr string, defaultDuration time.Duration) (time.Duration, error) {
	if durationStr == "" {
		return defaultDuration, nil
	}
	return time.ParseDuration(durationStr)
}

// Supervised adapts the Manager to the Supervisor's Supervised interface, so an
// embedded controller is restarted and health-reported exactly like a collector
// (#93). Controller.Start returns as soon as the informer has synced and the
// work loops are running, so this blocks on the context afterwards; a cache-sync
// failure surfaces as an error and the supervisor restarts it.
func (m *Manager) Supervised() Supervised { return supervisedManager{m} }

type supervisedManager struct{ m *Manager }

func (s supervisedManager) Type() string { return "controller" }

func (s supervisedManager) Start(ctx context.Context) error {
	if err := s.m.Start(ctx); err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}

func (s supervisedManager) Stop(ctx context.Context) error { return s.m.Stop(ctx) }
