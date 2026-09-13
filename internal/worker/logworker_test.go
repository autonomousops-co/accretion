package worker

import (
	"fmt"
	"testing"
	"time"

	"github.com/autonomousops-co/accretion/internal/config"
	"github.com/autonomousops-co/accretion/internal/database"
	"github.com/autonomousops-co/accretion/internal/k8s"
	"github.com/autonomousops-co/accretion/internal/llm"
	"github.com/autonomousops-co/accretion/internal/nsscope"
	"github.com/autonomousops-co/accretion/internal/queue"
	"github.com/autonomousops-co/accretion/internal/testutil"
)

// newTestLogWorker wires a log worker to mock LLM/DB clients and the given
// filter config, returning the concrete worker plus the mocks so tests can
// assert on embedding calls and stored rows.
func newTestLogWorker(t *testing.T, filterCfg config.LogFilterConfig) (*logWorker, *llm.MockClient, *database.MockClient) {
	t.Helper()
	// Initializes the global logger; the drop path logs at debug level.
	testutil.SetupTestEnvironment(t)

	dbClient := database.NewMockClient()
	mockDB, ok := dbClient.(*database.MockClient)
	if !ok {
		t.Fatal("failed to cast to *database.MockClient")
	}

	llmClient := llm.NewMockClient()
	mockLLM, ok := llmClient.(*llm.MockClient)
	if !ok {
		t.Fatal("failed to cast to *llm.MockClient")
	}

	filter, err := NewLogFilter(filterCfg)
	if err != nil {
		t.Fatalf("NewLogFilter failed: %v", err)
	}

	w, ok := NewLogWorker(nil, nil, dbClient, llmClient, nsscope.Static(config.NamespaceSelector{Include: []string{"default"}}), filter).(*logWorker)
	if !ok {
		t.Fatal("failed to cast to *logWorker")
	}
	return w, mockLLM, mockDB
}

// newTestLogWorkerWithK8s wires a log worker to a real k8s MockClient (so
// StreamPodLogs invocations can be observed) scoped to the given namespaces.
func newTestLogWorkerWithK8s(t *testing.T, namespaces []string) (*logWorker, *k8s.MockClient) {
	t.Helper()
	testutil.SetupTestEnvironment(t)

	k8sClient := k8s.NewMockClient()
	mockK8s, ok := k8sClient.(*k8s.MockClient)
	if !ok {
		t.Fatal("failed to cast to *k8s.MockClient")
	}

	filter, err := NewLogFilter(config.LogFilterConfig{})
	if err != nil {
		t.Fatalf("NewLogFilter failed: %v", err)
	}

	w, ok := NewLogWorker(
		nil, k8sClient, database.NewMockClient(), llm.NewMockClient(),
		nsscope.Static(config.NamespaceSelector{Include: namespaces}), filter,
	).(*logWorker)
	if !ok {
		t.Fatal("failed to cast to *logWorker")
	}
	return w, mockK8s
}

// TestHandleWork_ExcludedNamespaceNeverStreams is the defense-in-depth backstop
// for #145: a log worker scoped to {default} must never start streaming for a
// pod in accretion's own namespace, even if the controller mistakenly dispatched log
// work for it. Pre-fix, handleWork called collectLogs unconditionally and
// StreamPodLogs would fire; the gate makes the excluded-namespace call a no-op.
func TestHandleWork_ExcludedNamespaceNeverStreams(t *testing.T) {
	w, mockK8s := newTestLogWorkerWithK8s(t, []string{"default"})

	excluded := &queue.LogCollectionWork{
		BaseWork:   queue.BaseWork{Namespace: "accretion-core", PodName: "accretion-collector-abc"},
		Containers: []string{"collector"},
	}
	if err := w.handleWork(excluded); err != nil {
		t.Fatalf("handleWork returned error: %v", err)
	}

	if got := mockK8s.GetStreamCallCount(); got != 0 {
		t.Errorf("expected 0 StreamPodLogs calls for excluded namespace, got %d", got)
	}
	if pods := w.GetAssignedPods(); len(pods) != 0 {
		t.Errorf("expected no pod assignment for excluded namespace, got %v", pods)
	}

	// Positive control: an in-scope pod does start streaming, so the assertion
	// above is not vacuously true. Streaming happens in a goroutine, so poll.
	inScope := &queue.LogCollectionWork{
		BaseWork:   queue.BaseWork{Namespace: "default", PodName: "app-xyz"},
		Containers: []string{"app"},
	}
	if err := w.handleWork(inScope); err != nil {
		t.Fatalf("handleWork returned error for in-scope pod: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for mockK8s.GetStreamCallCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := mockK8s.GetStreamCallCount(); got != 1 {
		t.Errorf("expected 1 StreamPodLogs call for in-scope namespace, got %d", got)
	}
	if pods := w.GetAssignedPods(); len(pods) != 1 {
		t.Errorf("expected in-scope pod to be assigned, got %v", pods)
	}
}

func logEntry(message string) *k8s.LogEntry {
	return &k8s.LogEntry{
		Timestamp: time.Now(),
		Namespace: "default",
		PodName:   "pod-a",
		Container: "app",
		Message:   message,
	}
}

func TestProcessLog_DroppedLinesSkipEmbeddingAndInsert(t *testing.T) {
	cases := []struct {
		name    string
		message string
		filter  config.LogFilterConfig
	}{
		{
			name:    "denylist health line",
			filter:  config.LogFilterConfig{Denylist: []string{`/health`}},
			message: `10.0.0.1 - - "GET /health HTTP/1.1" 200 2 "-"`,
		},
		{
			name:    "below level threshold",
			filter:  config.LogFilterConfig{MinLevel: "warn"},
			message: "INFO request handled in 3ms",
		},
		{
			// A genuine structured info level must still be dropped below the
			// warn threshold even after the word-boundary detection fix.
			name:    "structured info below threshold",
			filter:  config.LogFilterConfig{MinLevel: "warn"},
			message: `ts=2026-07-15 level=info msg="request handled"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, mockLLM, mockDB := newTestLogWorker(t, tc.filter)

			if err := w.processLog(logEntry(tc.message)); err != nil {
				t.Fatalf("processLog returned error: %v", err)
			}

			if got := mockLLM.GetEmbeddingCallCount(); got != 0 {
				t.Errorf("expected 0 embedding calls for dropped line, got %d", got)
			}
			if entries := mockDB.GetAllLogEntries(); len(entries) != 0 {
				t.Errorf("expected 0 stored entries for dropped line, got %d", len(entries))
			}
			if dropped := w.stats.GetStats()["default"].Dropped; dropped != 1 {
				t.Errorf("expected 1 recorded drop, got %d", dropped)
			}
		})
	}
}

func TestProcessLog_PassThroughLinesAreEmbeddedAndStored(t *testing.T) {
	cases := []struct {
		name    string
		message string
	}{
		{name: "warn line at threshold", message: "WARN disk usage high"},
		{name: "error line above threshold", message: "ERROR failed to connect"},
		{name: "unparseable level fails open", message: "request completed successfully"},
		// Word-boundary fix: "info" embedded in a larger word is not a level, so
		// these unleveled lines must fail open and pass rather than be dropped.
		{name: "info substring in word is not a level", message: "user information updated"},
		{name: "info substring in reinforcement is not a level", message: "reinforcement scaling triggered"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Threshold set to WARN so the "unparseable" case proves fail-open.
			w, mockLLM, mockDB := newTestLogWorker(t, config.LogFilterConfig{MinLevel: "warn"})

			if err := w.processLog(logEntry(tc.message)); err != nil {
				t.Fatalf("processLog returned error: %v", err)
			}

			if got := mockLLM.GetEmbeddingCallCount(); got != 1 {
				t.Errorf("expected 1 embedding call for passed line, got %d", got)
			}
			if entries := mockDB.GetAllLogEntries(); len(entries) != 1 {
				t.Errorf("expected 1 stored entry for passed line, got %d", len(entries))
			}
			if dropped := w.stats.GetStats()["default"].Dropped; dropped != 0 {
				t.Errorf("expected 0 recorded drops, got %d", dropped)
			}
		})
	}
}

func TestProcessLog_DefaultConfigIngestsEverything(t *testing.T) {
	w, mockLLM, mockDB := newTestLogWorker(t, config.LogFilterConfig{})

	messages := []string{
		`GET /health 200 OK`,
		"INFO starting up",
		"DEBUG verbose detail",
		"some line without a level",
	}
	for i, msg := range messages {
		e := logEntry(msg)
		// Distinct timestamps so dedup never collapses these lines.
		e.Timestamp = e.Timestamp.Add(time.Duration(i) * time.Second)
		if err := w.processLog(e); err != nil {
			t.Fatalf("processLog(%q) returned error: %v", msg, err)
		}
	}

	if got := mockLLM.GetEmbeddingCallCount(); got != len(messages) {
		t.Errorf("expected %d embedding calls with default config, got %d", len(messages), got)
	}
	if entries := mockDB.GetAllLogEntries(); len(entries) != len(messages) {
		t.Errorf("expected %d stored entries with default config, got %d", len(messages), len(entries))
	}
	if dropped := w.stats.GetStats()["default"].Dropped; dropped != 0 {
		t.Errorf("expected 0 drops with default config, got %d", dropped)
	}
}

func TestLogFilter_Allow(t *testing.T) {
	cases := []struct {
		name    string
		message string
		cfg     config.LogFilterConfig
		want    bool
	}{
		{name: "empty config allows all", cfg: config.LogFilterConfig{}, message: "GET /health 200", want: true},
		{
			name:    "denylist substring drops",
			cfg:     config.LogFilterConfig{Denylist: []string{`/health`, `/readyz`}},
			message: "GET /readyz 200",
			want:    false,
		},
		{
			name:    "denylist non-match passes",
			cfg:     config.LogFilterConfig{Denylist: []string{`/health`}},
			message: "GET /api/orders 500",
			want:    true,
		},
		{
			name:    "denylist regex anchors",
			cfg:     config.LogFilterConfig{Denylist: []string{`^probe: `}},
			message: "probe: readiness ok",
			want:    false,
		},
		{name: "info below warn dropped", cfg: config.LogFilterConfig{MinLevel: "warn"}, message: "INFO ok", want: false},
		{name: "warn at warn kept", cfg: config.LogFilterConfig{MinLevel: "warn"}, message: "WARN slow", want: true},
		{name: "error above warn kept", cfg: config.LogFilterConfig{MinLevel: "warn"}, message: "ERROR boom", want: true},
		{
			name:    "unparseable level fails open",
			cfg:     config.LogFilterConfig{MinLevel: "warn"},
			message: "plain message no level",
			want:    true,
		},
		{name: "debug below info dropped", cfg: config.LogFilterConfig{MinLevel: "info"}, message: "DEBUG trace", want: false},
		// Word-boundary detection: "info" inside a larger word is not a level, so
		// the line has no detectable level and fails open (kept).
		{
			name:    "info inside information not a level fails open",
			cfg:     config.LogFilterConfig{MinLevel: "warn"},
			message: "user information updated",
			want:    true,
		},
		{
			name:    "info inside reinforcement not a level fails open",
			cfg:     config.LogFilterConfig{MinLevel: "warn"},
			message: "reinforcement scaling triggered",
			want:    true,
		},
		// Real structured level indicators are still detected at word boundaries
		// and dropped below the threshold.
		{
			name:    "structured level=info dropped below warn",
			cfg:     config.LogFilterConfig{MinLevel: "warn"},
			message: `level=info msg="starting up"`,
			want:    false,
		},
		{
			name:    "json level info dropped below warn",
			cfg:     config.LogFilterConfig{MinLevel: "warn"},
			message: `{"level":"info","msg":"ok"}`,
			want:    false,
		},
		// Non-Go loggers: full spellings and higher severity tiers.
		{name: "python WARNING kept at warn", cfg: config.LogFilterConfig{MinLevel: "warn"}, message: "WARNING: disk almost full", want: true},
		{name: "python CRITICAL kept above warn", cfg: config.LogFilterConfig{MinLevel: "warn"}, message: "CRITICAL: database down", want: true},
		{name: "log4j FATAL kept at error threshold", cfg: config.LogFilterConfig{MinLevel: "error"}, message: "FATAL cannot bind port", want: true},
		// High-severity keyword false positives only ever KEEP (safe direction).
		{name: "critical in prose is kept, not dropped", cfg: config.LogFilterConfig{MinLevel: "warn"}, message: "optimizing the critical path", want: true},
		{name: "panic in prose is kept, not dropped", cfg: config.LogFilterConfig{MinLevel: "warn"}, message: "do not panic, retrying", want: true},
		// Low-severity tiers are NOT keyword-detected, so they fail open (kept) —
		// never wrongly dropped from common prose words.
		{name: "NOTICE line fails open (not keyword-detected)", cfg: config.LogFilterConfig{MinLevel: "warn"}, message: "NOTICE: config reloaded", want: true},
		{name: "'trace' word fails open, not dropped", cfg: config.LogFilterConfig{MinLevel: "info"}, message: "captured stack trace for request", want: true},
		// min_level synonyms and higher tiers as thresholds.
		{name: "min_level synonym 'warning' drops info", cfg: config.LogFilterConfig{MinLevel: "warning"}, message: "INFO ok", want: false},
		{name: "min_level 'fatal' (=critical) drops error", cfg: config.LogFilterConfig{MinLevel: "fatal"}, message: "ERROR boom", want: false},
		{name: "min_level 'fatal' keeps critical", cfg: config.LogFilterConfig{MinLevel: "fatal"}, message: "CRITICAL meltdown", want: true},
		{name: "min_level 'notice' drops info", cfg: config.LogFilterConfig{MinLevel: "notice"}, message: "INFO ok", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := NewLogFilter(tc.cfg)
			if err != nil {
				t.Fatalf("NewLogFilter failed: %v", err)
			}
			if got := f.Allow(tc.message, ""); got != tc.want {
				t.Errorf("Allow(%q) = %v, want %v", tc.message, got, tc.want)
			}
		})
	}
}

func TestNewLogFilter_InvalidConfig(t *testing.T) {
	if _, err := NewLogFilter(config.LogFilterConfig{MinLevel: "bogus"}); err == nil {
		t.Error("expected error for unknown min_level, got nil")
	}
	if _, err := NewLogFilter(config.LogFilterConfig{Denylist: []string{`[unclosed`}}); err == nil {
		t.Error("expected error for invalid denylist regex, got nil")
	}
}

func TestNilLogFilterAllowsEverything(t *testing.T) {
	var f *LogFilter
	if !f.Allow("anything", "key") {
		t.Error("nil LogFilter should allow all lines")
	}
}

func TestLogFilter_SampleRate(t *testing.T) {
	f, err := NewLogFilter(config.LogFilterConfig{SampleRate: 0.5})
	if err != nil {
		t.Fatalf("NewLogFilter failed: %v", err)
	}

	// Deterministic per key, and roughly half of a large keyspace is kept.
	kept := 0
	const n = 10000
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("occurrence-%d", i)
		// Capture the first decision, then assert a repeat call matches it so
		// this genuinely exercises determinism rather than comparing two live
		// calls against each other (a tautology).
		first := f.Allow("line", key)
		if first {
			kept++
		}
		if second := f.Allow("line", key); second != first {
			t.Fatalf("sampling not deterministic for key %q: first=%v second=%v", key, first, second)
		}
	}
	if kept < n*4/10 || kept > n*6/10 {
		t.Errorf("expected ~50%% kept with sample_rate 0.5, kept %d of %d", kept, n)
	}
}
