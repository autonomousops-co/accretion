package cmd

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/autonomousops-co/accretion/internal/collector"
	"github.com/autonomousops-co/accretion/internal/config"
	"github.com/autonomousops-co/accretion/internal/logger"
	"github.com/autonomousops-co/accretion/internal/nsscope"
	"github.com/autonomousops-co/accretion/internal/queue"
	"github.com/autonomousops-co/accretion/internal/worker"
)

// Collector names accepted by --only and by collectors.<name>.enabled.
const (
	collectorLog    = "log"
	collectorMetric = "metric"
	collectorEvent  = "event"
	collectorTrace  = "trace"
)

// dataCollectorOnly is the --only flag: the escape hatch that splits one
// collector into its own deployment.
var dataCollectorOnly []string

// dataCollectorCmd runs the four high-volume collectors -- log, metric, event
// and trace -- as supervised goroutines in one process.
//
// Unlike the status kinds (#91) these carry real volume, so they are the ones
// an operator may genuinely want to scale. That is an argument for keeping them
// separable, not for keeping them separate by default: a minimal install runs
// one replica of each with no HPA, and so pays four Go runtimes (~150Mi) for
// work one process interleaves as goroutines.
//
// The escape hatch is --only. Passing --only log runs nothing but the log
// collector, so the chart can render a dedicated, separately scaled Deployment
// for one collector while the shared process drops it -- no code change, and
// splitting one out leaves the others untouched.
var dataCollectorCmd = &cobra.Command{
	Use:   "data-collector",
	Short: "Start the Accretion log, metric, event and trace collectors",
	Long: `Start every enabled data collector (log, metric, event and trace) as
supervised goroutines in a single process.

Individual collectors are switched off with collectors.<name>.enabled. Passing
--only <name> (repeatable) runs exactly those collectors and ignores the config
toggles, which is how one collector is split into its own deployment.`,
	Run: func(cmd *cobra.Command, args []string) {
		cfg, shutdownTelemetry := mustLoadCollectorConfig(dataCollectorServiceName(dataCollectorOnly))
		defer shutdownTelemetry()

		selected, err := selectedDataCollectors(&cfg.Collectors, dataCollectorOnly)
		if err != nil {
			logger.Logger.Fatal("Invalid --only selection", zap.Error(err))
		}
		if len(selected) == 0 {
			logger.Logger.Fatal("Every data collector is disabled; nothing to run. " +
				"Disable the Deployment itself rather than running a process that collects nothing.")
		}

		logger.Logger.Info("Starting Accretion data collectors",
			zap.String("log_level", cfg.Logging.Level),
			zap.Strings("collectors", selected))

		// The shutdown context is created before the namespace scopes, which hang
		// their background re-resolution goroutines off it (#152).
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clients, err := newCollectorClients(cfg)
		if err != nil {
			logger.Logger.Fatal("Failed to create collector clients", zap.Error(err))
		}
		defer clients.close()

		units, err := dataUnits(ctx, cfg, clients, selected)
		if err != nil {
			logger.Logger.Fatal("Failed to build collectors", zap.Error(err))
		}

		supervisor, err := collector.NewSupervisor(collector.DefaultSupervisorConfig(), units...)
		if err != nil {
			logger.Logger.Fatal("Failed to create collector supervisor", zap.Error(err))
		}

		runSupervised(ctx, cancel, supervisor, "accretion-api-data-collector")
	},
}

// dataUnits builds the supervised unit for each selected collector.
//
// Each collector gets its own namespace scope rather than a shared one, because
// the scopes are deliberately different: the log collector uses
// LogNamespaceSelector, which excludes accretion's own namespace so it never
// ingests its own log lines (the amplification loop, #145), while metric and
// event use K8sNamespaceSelector and trace uses TraceNamespaceSelector.
// Collapsing them into one scope here would quietly reopen that loop.
func dataUnits(
	ctx context.Context, cfg *config.CollectorConfig,
	clients *collectorClients, selected []string,
) ([]collector.Unit, error) {
	refreshInterval, err := cfg.NamespaceRefreshInterval()
	if err != nil {
		return nil, fmt.Errorf("invalid namespace refresh interval: %w", err)
	}

	if queueErr := cfg.Queue.Validate(); queueErr != nil {
		return nil, queueErr
	}

	queueClient, embeddedController, err := buildQueue(ctx, cfg, clients, selected)
	if err != nil {
		return nil, err
	}

	scopeFor := func(sel config.NamespaceSelector, lister config.NamespaceLister) (nsscope.Scope, error) {
		return nsscope.New(ctx, sel, lister, refreshInterval)
	}

	var units []collector.Unit
	for _, name := range selected {
		w, buildErr := buildCollector(name, cfg, clients, queueClient, scopeFor)
		if buildErr != nil {
			return nil, buildErr
		}
		units = append(units, collector.Unit{Name: name, Worker: w})
	}

	// The embedded controller is supervised alongside the collectors it feeds:
	// if it dies the collectors go idle, so it must be restarted and its failure
	// must reach /ready like any other.
	if embeddedController != nil {
		units = append(units, collector.Unit{Name: "controller", Worker: embeddedController.Supervised()})
	}

	return units, nil
}

// selectedDataCollectors resolves which collectors to run. --only wins outright
// over the config toggles: a split-out deployment is told exactly what it is
// for, and must not silently pick up a second collector because the shared
// ConfigMap still has it enabled.
// buildQueue creates the work queue the log and metric collectors need, and the
// embedded controller that feeds it on the single-replica path.
//
// Split out of dataUnits to keep that function's branching readable: this half
// is about how work ARRIVES, the remainder is about which collectors are built.
//
// The log and metric collectors are queue-driven; event and trace are not. A
// queue is built only if something needs one, so a process running only the
// event or trace collector has no queue and no Redis dependency at all.
// buildCollector constructs one collector by name. Split out of dataUnits so
// that function reads as "resolve shared inputs, then build each selected
// collector" rather than carrying every collector's construction inline.
// Each case owns its own configuration errors, which is why they are wrapped
// with the collector's name.
func buildCollector(
	name string, cfg *config.CollectorConfig, clients *collectorClients,
	queueClient queue.Client,
	scopeFor func(config.NamespaceSelector, config.NamespaceLister) (nsscope.Scope, error),
) (collector.Supervised, error) {
	var w collector.Supervised

	switch name {
	case collectorLog:
		logFilter, filterErr := worker.NewLogFilter(cfg.LogFilter)
		if filterErr != nil {
			return nil, fmt.Errorf("invalid log filter configuration: %w", filterErr)
		}
		scope, scopeErr := scopeFor(cfg.LogNamespaceSelector(), clients.k8s)
		if scopeErr != nil {
			return nil, fmt.Errorf("log namespace scope: %w", scopeErr)
		}
		w = worker.NewLogWorker(queueClient, clients.k8s, clients.db, clients.llm, scope, logFilter)

	case collectorMetric:
		pollInterval, pollErr := cfg.GetMetricsPollInterval()
		if pollErr != nil {
			return nil, fmt.Errorf("metrics poll interval: %w", pollErr)
		}
		scope, scopeErr := scopeFor(cfg.K8sNamespaceSelector(), clients.k8s)
		if scopeErr != nil {
			return nil, fmt.Errorf("metric namespace scope: %w", scopeErr)
		}
		w = worker.NewMetricWorker(queueClient, clients.k8s, clients.db, clients.llm, pollInterval, scope)

	case collectorEvent:
		scope, scopeErr := scopeFor(cfg.K8sNamespaceSelector(), clients.k8s)
		if scopeErr != nil {
			return nil, fmt.Errorf("event namespace scope: %w", scopeErr)
		}
		w = worker.NewEventWorker(clients.k8s, clients.db, clients.llm, collectorPollInterval(cfg), scope)

	case collectorTrace:
		listenAddr := cfg.TraceWorker.ListenAddr
		if listenAddr == "" {
			listenAddr = ":4318"
		}
		// A purely name-based trace selector needs no expansion and so needs
		// no lister; pass nil in that case exactly as the standalone trace
		// worker did, so a non-cluster OTLP deployment keeps working.
		var lister config.NamespaceLister
		selector := cfg.TraceNamespaceSelector()
		if selector.NeedsExpansion() {
			lister = clients.k8s
		}
		scope, scopeErr := scopeFor(selector, lister)
		if scopeErr != nil {
			return nil, fmt.Errorf("trace namespace scope: %w", scopeErr)
		}
		w = worker.NewTraceWorker(clients.db, clients.llm, listenAddr, scope)

	default:
		return nil, fmt.Errorf("unknown collector %q", name)
	}

	return w, nil
}

func buildQueue(
	ctx context.Context, cfg *config.CollectorConfig,
	clients *collectorClients, selected []string,
) (queue.Client, *collector.Manager, error) {
	if !contains(selected, collectorLog) && !contains(selected, collectorMetric) {
		return nil, nil, nil
	}

	var (
		queueClient        queue.Client
		embeddedController *collector.Manager
		err                error
	)

	if cfg.Queue.UsesRedis() {
		queueClient, err = queue.NewRedisClient(cfg.Queue.RedisAddr, cfg.Queue.RedisPassword, cfg.Queue.RedisDB)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create queue client: %w", err)
		}
	} else {
		// The default single-replica path (#93). With one collector of each kind
		// and no HPA there is nothing to shard, so the dispatcher runs here and
		// hands this process's collectors their own work over channels rather
		// than round-tripping through Redis to assign work to itself. The
		// controller joins the supervisor as a unit, so it gets the same restart
		// and health treatment as a collector.
		inProcess := queue.NewInProcessClient()
		queueClient = inProcess

		embeddedController, err = collector.NewManagerWithQueue(ctx, cfg, clients.k8s, inProcess)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create embedded controller: %w", err)
		}
	}

	// Both collectors hold this client and each closes it from its own Cleanup;
	// hand them a view whose Close is a no-op and close it once.
	clients.queue = queueClient
	return queue.Shared(queueClient), embeddedController, nil
}

func selectedDataCollectors(sel *config.CollectorsConfig, only []string) ([]string, error) {
	all := []string{collectorLog, collectorMetric, collectorEvent, collectorTrace}

	if len(only) > 0 {
		chosen := make([]string, 0, len(only))
		seen := make(map[string]bool, len(only))
		for _, name := range only {
			name = strings.TrimSpace(name)
			if !contains(all, name) {
				return nil, fmt.Errorf("unknown collector %q; valid values are %s",
					name, strings.Join(all, ", "))
			}
			if !seen[name] {
				seen[name] = true
				chosen = append(chosen, name)
			}
		}
		return chosen, nil
	}

	toggles := map[string]config.CollectorToggle{
		collectorLog:    sel.Log,
		collectorMetric: sel.Metric,
		collectorEvent:  sel.Event,
		collectorTrace:  sel.Trace,
	}

	chosen := make([]string, 0, len(all))
	for _, name := range all {
		if toggles[name].IsEnabled() {
			chosen = append(chosen, name)
		} else {
			logger.Logger.Info("Data collector disabled by config", zap.String("collector", name))
		}
	}
	return chosen, nil
}

// dataCollectorServiceName names the process in traces. A split-out deployment
// gets its collector's name so its spans stay distinguishable from the shared
// process's, which is what makes a split visible in the trace data.
func dataCollectorServiceName(only []string) string {
	if len(only) == 0 {
		return "accretion-data-collector"
	}
	names := append([]string(nil), only...)
	sort.Strings(names)
	return "accretion-" + strings.Join(names, "-") + "-collector"
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func init() {
	dataCollectorCmd.Flags().StringSliceVar(&dataCollectorOnly, "only", nil,
		"run only these collectors (log, metric, event, trace), ignoring the config toggles")
	rootCmd.AddCommand(dataCollectorCmd)
}
