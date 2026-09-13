package worker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/pgvector/pgvector-go"
	"go.uber.org/zap"

	"github.com/autonomousops-co/accretion/internal/database"
	"github.com/autonomousops-co/accretion/internal/k8s"
	"github.com/autonomousops-co/accretion/internal/llm"
	"github.com/autonomousops-co/accretion/internal/logger"
	"github.com/autonomousops-co/accretion/internal/nsscope"
	"github.com/autonomousops-co/accretion/internal/queue"
	"github.com/autonomousops-co/accretion/internal/resilience"
)

// logWorker processes log collection work from dedicated queues.
type logWorker struct {
	*BaseWorker
	k8s      k8s.Interface
	db       database.Interface
	llm      llm.Client
	filter   *LogFilter
	seenHash *lruCache
}

// NewLogWorker creates a new log worker. filter is applied to every line before
// embedding; pass a filter built from a zero-value LogFilterConfig (or nil) to
// keep today's behavior of ingesting every non-empty line.
func NewLogWorker(
	queue queue.Consumer, k8s k8s.Interface, db database.Interface, llm llm.Client,
	scope nsscope.Scope, filter *LogFilter,
) LogWorker {
	return &logWorker{
		BaseWorker: NewBaseWorker("logs", queue, scope),
		k8s:        k8s,
		db:         db,
		llm:        llm,
		filter:     filter,
		seenHash:   newLRUCache(10000),
	}
}

func (w *logWorker) Start(ctx context.Context) error {
	logger.Logger.Info("Starting log worker", zap.String("worker_id", w.ID()), zap.String("type", w.Type()))

	if err := w.StartHealthReporting(ctx); err != nil {
		return fmt.Errorf("failed to start health reporting: %w", err)
	}

	w.StartStatsReporting(ctx)

	return w.queue.ConsumeLogWorkForWorker(ctx, w.ID(), w.handleWork)
}

func (w *logWorker) Stop(ctx context.Context) error {
	return w.Cleanup(ctx)
}

func (w *logWorker) handleWork(work *queue.LogCollectionWork) error {
	ctx, span := w.StartSpan(context.Background(), "handleWork")
	defer span.End()

	// Defense-in-depth log self-exclusion (issue #145). The controller is the
	// authoritative gate — it never dispatches log work for a log-excluded
	// namespace (e.g. accretion's own) — but drop anything that slips through here
	// too, so a future controller change can't silently re-open the amplification
	// loop. Skip only when the worker has a concrete namespace scope; an empty
	// scope (opt-out / misconfig) fails open to preserve ingestion.
	//
	// Read the scope live, so a namespace that came into (or dropped out of) the
	// log scope on a background refresh is honored without a restart (#152). This
	// deliberately tests the resolved namespace SET rather than calling
	// scope.Allows: Allows would drop work when the scope resolves to nothing,
	// inverting the fail-open above. IsEmpty+Contains express exactly that, and
	// unlike Namespaces() they don't copy the set on this per-work-item path.
	if !w.scope.IsEmpty() && !w.scope.Contains(work.Namespace) {
		logger.Logger.Debug("Dropping log work for out-of-scope namespace",
			zap.String("worker_id", w.ID()), zap.String("namespace", work.Namespace),
			zap.String("pod", work.PodName))
		return nil
	}

	podKey := fmt.Sprintf("%s/%s", work.Namespace, work.PodName)

	w.MarkPodAssigned(podKey)
	w.StorePodAssignment(podKey)

	_ = ctx // span context available for future use
	return w.collectLogs(work)
}

// Level-detection patterns, compiled once at package load. Each matches its
// level token only at a word boundary so genuine indicators (INFO, [INFO],
// level=info, "level":"info", info:) are detected while a level word embedded in
// a larger word (information, reinforcement, errors-free prose) is not. Patterns
// are case-insensitive and evaluated in the priority order below (most severe
// first, so a line mentioning several levels classifies by the highest).
//
// Non-Go loggers spell levels out (WARNING, ERROR) and add higher tiers
// (CRITICAL, FATAL — Python/log4j; ALERT, EMERG — syslog); those are detected
// here. A false keyword match on one of these high-severity tiers ("critical
// path", "don't panic") only ever KEEPS the line, which is the safe direction.
// Low-severity tiers (notice, trace) are deliberately NOT keyword-matched: those
// words are too common in ordinary prose ("stack trace", "please notice"), and a
// false match there would DROP a real line. Such lines fail open (kept); notice
// and trace still work as min_level thresholds against lines whose level IS
// detected.
var (
	reEmerg    = regexp.MustCompile(`(?i)\b(emerg|emergency)\b`)
	reAlert    = regexp.MustCompile(`(?i)\balert\b`)
	reCritical = regexp.MustCompile(`(?i)\b(crit|critical|fatal|panic|dpanic)\b`)
	reError    = regexp.MustCompile(`(?i)\b(err|error)\b`)
	reWarn     = regexp.MustCompile(`(?i)\b(warn|warning)\b`)
	reDebug    = regexp.MustCompile(`(?i)\bdebug\b`)
	reInfo     = regexp.MustCompile(`(?i)\binfo\b`)
)

// detectLogLevel attempts to extract a log level from a log line. It returns
// the canonical lowercase level and whether one was found; callers that need a
// definite level (e.g. for display) can fall back to a default, while the
// ingestion filter relies on the found flag to fail open on undetectable lines.
// Matching is anchored on word boundaries so substrings inside larger words
// (e.g. "information", "reinforcement") do not misclassify an unleveled line.
func detectLogLevel(line string) (string, bool) {
	switch {
	case reEmerg.MatchString(line):
		return "emerg", true
	case reAlert.MatchString(line):
		return "alert", true
	case reCritical.MatchString(line):
		return "critical", true
	case reError.MatchString(line):
		return "error", true
	case reWarn.MatchString(line):
		return "warn", true
	case reDebug.MatchString(line):
		return "debug", true
	case reInfo.MatchString(line):
		return "info", true
	}

	return "", false
}

// parseLogLevel attempts to extract log level from a log line, defaulting to
// "info" when none can be detected.
func (w *logWorker) parseLogLevel(line string) string {
	if level, found := detectLogLevel(line); found {
		return level
	}
	return "info" // Default to info level
}

// collectLogs starts log collection for a pod
func (w *logWorker) collectLogs(work *queue.LogCollectionWork) error { //nolint:unparam // error always nil by design
	// Start a goroutine for each container to collect logs
	for _, container := range work.Containers {
		logger.Logger.Debug("Starting container log collection",
			zap.String("worker_id", w.ID()), zap.String("pod", work.PodName), zap.String("container", container))
		go w.streamLogs(work, container)
	}

	logger.Logger.Info("Started log collection",
		zap.String("worker_id", w.ID()), zap.String("pod", work.PodName), zap.Int("containers", len(work.Containers)))

	return nil
}

// streamLogs streams logs from a container
func (w *logWorker) streamLogs(work *queue.LogCollectionWork, container string) {
	podKey := fmt.Sprintf("%s/%s", work.Namespace, work.PodName)

	logChan := make(chan k8s.LogEntry, 100)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start streaming logs in a separate goroutine
	go func() {
		defer close(logChan)

		logger.Logger.Debug("Starting log stream",
			zap.String("worker_id", w.ID()), zap.String("pod", work.PodName), zap.String("container", container))

		if err := w.k8s.StreamPodLogs(ctx, work.Namespace, work.PodName, container, logChan); err != nil {
			logger.Logger.Error("Log streaming failed",
				zap.String("worker_id", w.ID()), zap.String("pod", work.PodName), zap.Error(err))
			w.IncrementErrors()
		}
	}()

	// Process incoming log entries
	for {
		select {
		case logEntry, ok := <-logChan:
			if !ok {
				logger.Logger.Debug("Log stream ended", zap.String("worker_id", w.ID()), zap.String("pod", work.PodName))
				w.MarkPodUnassigned(podKey)
				return
			}

			if err := w.processLog(&logEntry); err != nil {
				logger.Logger.Error("Log processing failed",
					zap.String("worker_id", w.ID()), zap.String("pod", work.PodName), zap.Error(err))
				w.IncrementErrors()

				// Check if pod is still running
				if !w.k8s.IsPodRunningByName(context.Background(), work.Namespace, work.PodName) {
					logger.Logger.Info("Pod stopped, ending collection",
						zap.String("worker_id", w.ID()), zap.String("pod", work.PodName))
					w.MarkPodUnassigned(podKey)
					return
				}
			}
		case <-ctx.Done():
			logger.Logger.Info("Log collection canceled", zap.String("worker_id", w.ID()), zap.String("pod", work.PodName))
			w.MarkPodUnassigned(podKey)
			return
		}
	}
}

// processLog processes a single log entry.
func (w *logWorker) processLog(entry *k8s.LogEntry) error {
	ctx, span := w.StartSpan(context.Background(), "processLog")
	defer span.End()

	// Compute content hash and skip if already seen (avoids redundant LLM + DB calls).
	// The event timestamp is part of the hash so that distinct occurrences of an
	// identical message (e.g. repeated "/health 200 OK") remain separate rows,
	// while a re-fetch of the *same* line (same time) collapses to one.
	contentHash := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%s|%s",
		entry.Namespace, entry.PodName, entry.Container, entry.Message,
		entry.Timestamp.Format(time.RFC3339Nano)))))

	// Drop health/probe/access logs and sub-threshold levels before any
	// embedding or DB work. Filtering is opt-in: with the default (empty)
	// config Allow always returns true, so behavior is unchanged. Dropped lines
	// are counted as drops (not as errors) and deliberately skip the seenHash LRU
	// so filtered high-frequency lines don't evict useful dedup entries. The drop
	// check runs before RecordLogIngestion so a dropped line never inflates the
	// Logs (ingested) count.
	if !w.filter.Allow(entry.Message, contentHash) {
		w.stats.RecordLogDrop(entry.Namespace)
		logger.Logger.Debug("Dropping log line before embedding",
			zap.String("worker_id", w.ID()), zap.String("namespace", entry.Namespace),
			zap.String("pod", entry.PodName), zap.String("container", entry.Container))
		return nil
	}

	// Only lines that pass the filter count as ingested.
	w.stats.RecordLogIngestion(entry.Namespace)

	if w.seenHash.contains(contentHash) {
		return nil
	}
	w.seenHash.add(contentHash)

	logLevel := w.parseLogLevel(entry.Message)
	logText := fmt.Sprintf("Pod: %s, Container: %s, Level: %s, Message: %s, Timestamp: %s",
		entry.PodName, entry.Container, logLevel, entry.Message, entry.Timestamp.Format(time.RFC3339))

	var embedding []float32
	var err error

	// Generate embedding with resilience
	err = w.llmBreaker.Execute(ctx, func(ctx context.Context) error {
		return resilience.WithRetry(ctx, resilience.DefaultRetryConfig(), func(ctx context.Context) error {
			embedding, err = w.llm.GenerateEmbedding(ctx, logText)
			return err
		})
	})
	if err != nil {
		return fmt.Errorf("embedding generation failed: %w", err)
	}

	// Create unified database entry
	data, err := json.Marshal(map[string]interface{}{
		"container": entry.Container,
		"message":   entry.Message,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal log data: %w", err)
	}

	dbEntry := &database.UnifiedEntry{
		ResourceType: "log",
		Namespace:    entry.Namespace,
		ResourceName: entry.PodName,
		Data:         data,
		Timestamp:    entry.Timestamp,
		ContentHash:  contentHash,
		Embedding:    pgvector.NewVector(embedding),
	}

	// Insert with resilience
	return w.dbBreaker.Execute(ctx, func(ctx context.Context) error {
		return resilience.WithRetry(ctx, resilience.DefaultRetryConfig(), func(ctx context.Context) error {
			return w.db.InsertEntry(dbEntry)
		})
	})
}
