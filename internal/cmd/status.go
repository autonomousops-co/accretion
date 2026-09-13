package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/autonomousops-co/accretion/internal/collector"
	"github.com/autonomousops-co/accretion/internal/config"
	"github.com/autonomousops-co/accretion/internal/database"
	"github.com/autonomousops-co/accretion/internal/k8s"
	"github.com/autonomousops-co/accretion/internal/llm"
	"github.com/autonomousops-co/accretion/internal/logger"
	"github.com/autonomousops-co/accretion/internal/nsscope"
	"github.com/autonomousops-co/accretion/internal/queue"
	"github.com/autonomousops-co/accretion/internal/telemetry"
	"github.com/autonomousops-co/accretion/internal/worker"
)

// statusCmd runs every enabled object-state collector in one process.
//
// These eight kinds -- pod, node, deployment, PVC, HPA, job, configmap and
// endpoint health -- were eight Deployments running the same image with
// different subcommands, and in a representative sample they produced 1-3
// records each against the metric worker's 1,297. Almost all of their cost was
// the ~37Mi Go runtime each one paid to poll a few times a minute, so eight
// processes cost ~290Mi of duplicated runtime for roughly 2% of the data.
//
// They keep their own conversion and record logic untouched, so the records
// written are byte-for-byte what the separate workers wrote: the same
// resource_type values and the same shape, leaving accretion_stats and
// existing queries alone. Since #96 each kind is driven by a client-go shared
// informer rather than a poll loop -- one LIST and one WATCH per resource for
// the cluster, the namespace scope applied per object -- so a change is
// written when it happens rather than at the next tick, and the steady-state
// API cost is near zero. Each kind is still independently switchable through
// collectors.<kind>.enabled, and each runs under the supervisor: a panicking
// handler, or an informer that cannot sync, restarts its own collector rather
// than killing the others, and enough consecutive failures take /ready down.
var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Start the consolidated Accretion object-status collectors",
	Long: `Start every enabled object-status collector (pod, node, deployment, PVC,
HPA, job, configmap and endpoint health) as supervised goroutines in a single
process. Individual kinds are switched off with collectors.<kind>.enabled.`,
	Run: func(cmd *cobra.Command, args []string) {
		cfg, shutdownTelemetry := mustLoadCollectorConfig("accretion-status-collector")
		defer shutdownTelemetry()

		logger.Logger.Info("Starting Accretion status collectors",
			zap.String("log_level", cfg.Logging.Level))

		// The shutdown context is created before anything that hangs a
		// background goroutine off it -- the namespace scope re-resolves on a
		// ticker parented here (#152).
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clients, err := newCollectorClients(cfg)
		if err != nil {
			logger.Logger.Fatal("Failed to create collector clients", zap.Error(err))
		}
		defer clients.close()

		scope, err := newNamespaceScope(ctx, cfg, clients.k8s)
		if err != nil {
			logger.Logger.Fatal("Failed to resolve namespace scope", zap.Error(err))
		}

		warnStaticFanOut(scope)

		units := statusUnits(&cfg.Collectors, clients, collectorPollInterval(cfg), scope)
		if len(units) == 0 {
			logger.Logger.Fatal("Every status collector is disabled; nothing to run. " +
				"Disable the status Deployment itself rather than running a process that collects nothing.")
		}

		supervisor, err := collector.NewSupervisor(collector.DefaultSupervisorConfig(), units...)
		if err != nil {
			logger.Logger.Fatal("Failed to create collector supervisor", zap.Error(err))
		}

		runSupervised(ctx, cancel, supervisor, "accretion-api-status-collector")
	},
}

// staticScopeWarnNamespaces is the static namespace count at which the
// status command warns, once, at startup: about ten informers per namespace
// puts twenty near the apiserver's default per-connection stream cap. The
// numbers behind it live in internal/k8s/statusinformers.go's header.
const staticScopeWarnNamespaces = 20

// warnStaticFanOut tells the operator when a static namespace list opens
// enough watch streams to matter (#96). Nothing breaks; a label or "*" scope
// watches the cluster once instead, at the cost of caching every namespace.
func warnStaticFanOut(scope nsscope.Scope) {
	if !scope.IsStatic() {
		return
	}
	n := len(scope.Namespaces())
	if n < staticScopeWarnNamespaces {
		return
	}
	logger.Logger.Warn("Static namespace scope opens many watch streams",
		zap.Int("namespaces", n),
		zap.String("hint", "about ten informers per namespace, one long-lived watch each; "+
			"a label or \"*\" scope watches the cluster once"))
}

// statusUnits builds the supervised unit for each enabled status collector.
// Node status is cluster-scoped and so takes no namespace scope; the rest are
// namespaced. resync is the informers' resync period (see collectorPollInterval).
func statusUnits(
	sel *config.CollectorsConfig, c *collectorClients,
	resync time.Duration, scope nsscope.Scope,
) []collector.Unit {
	var units []collector.Unit

	add := func(toggle config.CollectorToggle, name string, build func() collector.Supervised) {
		if !toggle.IsEnabled() {
			logger.Logger.Info("Status collector disabled by config", zap.String("collector", name))
			return
		}
		units = append(units, collector.Unit{Name: name, Worker: build()})
	}

	add(sel.PodStatus, "pod-status", func() collector.Supervised {
		return worker.NewPodStatusWorker(c.k8s, c.db, c.llm, resync, scope)
	})
	add(sel.NodeStatus, "node-status", func() collector.Supervised {
		return worker.NewNodeStatusWorker(c.k8s, c.db, c.llm, resync)
	})
	add(sel.DeploymentStatus, "deployment-status", func() collector.Supervised {
		return worker.NewDeploymentStatusWorker(c.k8s, c.db, c.llm, resync, scope)
	})
	add(sel.HPAStatus, "hpa-status", func() collector.Supervised {
		return worker.NewHPAStatusWorker(c.k8s, c.db, c.llm, resync, scope)
	})
	add(sel.EndpointHealth, "endpoint-health", func() collector.Supervised {
		return worker.NewEndpointHealthWorker(c.k8s, c.db, c.llm, resync, scope)
	})
	add(sel.PVCStatus, "pvc-status", func() collector.Supervised {
		return worker.NewPVCStatusWorker(c.k8s, c.db, c.llm, resync, scope)
	})
	add(sel.ConfigMap, "configmap", func() collector.Supervised {
		return worker.NewConfigMapWorker(c.k8s, c.db, c.llm, resync, scope)
	})
	add(sel.JobStatus, "job-status", func() collector.Supervised {
		return worker.NewJobStatusWorker(c.k8s, c.db, c.llm, resync, scope)
	})

	return units
}

// collectorClients are the shared clients a consolidated collector process
// builds once and lends to every collector it runs. Sharing them is most of the
// point: one database pool and one Kubernetes client instead of one per pod.
type collectorClients struct {
	k8s k8s.Interface
	db  database.Interface
	llm llm.Client
	// queue is set only by the data collector, and only when a queue-driven
	// collector is running. It is owned here rather than by the collectors,
	// which each close their own on shutdown (see queue.Shared).
	queue queue.Client
}

func newCollectorClients(cfg *config.CollectorConfig) (*collectorClients, error) {
	// One client serves every collector in this process, so it carries the
	// whole process's API budget; client-go's 5 QPS default would throttle the
	// consolidated collectors far below what twelve separate pods had.
	qps, burst := cfg.K8s.RateLimits()
	k8sClient, err := k8s.NewClientWithRateLimit(cfg.K8s.ConfigPath, qps, burst)
	if err != nil {
		return nil, fmt.Errorf("failed to create K8s client: %w", err)
	}
	logger.Logger.Info("Kubernetes client rate limits",
		zap.Float32("qps", qps), zap.Int("burst", burst))

	dbClient, err := database.NewClient(&cfg.Database)
	if err != nil {
		return nil, fmt.Errorf("failed to create database client: %w", err)
	}

	llmClient, err := llm.NewClient(&cfg.LLM)
	if err != nil {
		// The database pool is already open at this point, so it has to be
		// closed here rather than left to a defer that never runs.
		if closeErr := dbClient.Close(); closeErr != nil {
			logger.Logger.Error("Failed to close database client", zap.Error(closeErr))
		}
		return nil, fmt.Errorf("failed to create LLM client: %w", err)
	}

	return &collectorClients{k8s: k8sClient, db: dbClient, llm: llmClient}, nil
}

func (c *collectorClients) close() {
	if c.queue != nil {
		if err := c.queue.Close(); err != nil {
			logger.Logger.Error("Failed to close queue client", zap.Error(err))
		}
	}
	if err := c.db.Close(); err != nil {
		logger.Logger.Error("Failed to close database client", zap.Error(err))
	}
}

// mustLoadCollectorConfig loads the collector config and initializes logging and
// telemetry from it, exiting on failure. serviceName names the process in
// traces. The returned func flushes telemetry and must be deferred by the caller.
func mustLoadCollectorConfig(serviceName string) (*config.CollectorConfig, func()) {
	cfg, err := config.LoadCollector()
	if err != nil {
		bootstrapCfg := config.LoggingConfig{Level: bootstrapLogLevel, DisableStacktrace: true}
		if initErr := logger.Initialize(bootstrapCfg); initErr != nil {
			panic(fmt.Sprintf("Failed to initialize logger: %v", initErr))
		}
		logger.Logger.Fatal("Failed to load config", zap.Error(err))
	}

	if logErr := logger.Initialize(cfg.Logging); logErr != nil {
		logger.Logger.Fatal("Failed to initialize logger", zap.Error(logErr))
	}

	cfg.Telemetry.ServiceName = serviceName
	otelShutdown, err := telemetry.Init(cfg.Telemetry)
	if err != nil {
		logger.Logger.Fatal("Failed to initialize telemetry", zap.Error(err))
	}

	return cfg, func() {
		otelShutdown(context.Background()) //nolint:errcheck // best-effort shutdown
	}
}

// collectorPollInterval reads k8s.poll_interval, falling back to the 30s the
// individual workers always defaulted to.
//
// The key does two jobs. It is the poll period of the collectors that still
// poll (the event collector), and since #96 it is the informer resync period of
// the status collectors: at least once per interval every watched object is
// re-delivered from the informer's cache and re-evaluated, which is the
// guarantee a poll gave, while a change between resyncs is written as it
// happens. Mapping the existing key rather than adding one keeps every
// checked-in values file and ACCRETION_K8S_POLL_INTERVAL meaningful.
func collectorPollInterval(cfg *config.CollectorConfig) time.Duration {
	interval, err := time.ParseDuration(cfg.K8s.PollInterval)
	if err != nil {
		logger.Logger.Warn("Failed to parse poll interval, using default 30s",
			zap.String("configured", cfg.K8s.PollInterval), zap.Error(err))
		return 30 * time.Second
	}
	return interval
}

func newNamespaceScope(
	ctx context.Context, cfg *config.CollectorConfig, k8sClient k8s.Interface,
) (nsscope.Scope, error) {
	refreshInterval, err := cfg.NamespaceRefreshInterval()
	if err != nil {
		return nil, fmt.Errorf("invalid namespace refresh interval: %w", err)
	}
	return nsscope.New(ctx, cfg.K8sNamespaceSelector(), k8sClient, refreshInterval)
}

// runSupervised starts the supervisor behind a readiness-aware health server and
// blocks until the process is signaled, then shuts both down.
func runSupervised(
	ctx context.Context, cancel context.CancelFunc,
	supervisor *collector.Supervisor, serviceName string,
) {
	logger.Logger.Info("Supervising collectors", zap.Strings("collectors", supervisor.Names()))

	healthServer := NewHealthServerWithReadiness(8080, serviceName, supervisor.Healthy)
	healthServer.Start()

	supervisor.Start(ctx)

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	<-done

	logger.Logger.Info("Collectors are shutting down...", zap.Any("status", supervisor.Status()))
	cancel()
	supervisor.Wait()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := supervisor.Stop(shutdownCtx); err != nil {
		logger.Logger.Error("Failed to stop collectors gracefully", zap.Error(err))
	}
	if err := healthServer.Stop(); err != nil {
		logger.Logger.Error("Failed to stop health server gracefully", zap.Error(err))
	}

	logger.Logger.Info("Collectors exited")
}

func init() {
	rootCmd.AddCommand(statusCmd)
}
