package worker

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// IngestionStats holds the counts for a specific namespace.
type IngestionStats struct {
	Logs    int64
	Metrics int64
	Events  int64
	// Dropped counts log lines discarded by the ingestion filter before
	// embedding. These are intentional drops, not errors.
	Dropped int64
}

// WorkerStats tracks ingestion statistics per namespace.
type WorkerStats struct {
	stats map[string]*IngestionStats
	mu    sync.RWMutex
}

// NewWorkerStats creates a new WorkerStats instance.
func NewWorkerStats() *WorkerStats {
	return &WorkerStats{
		stats: make(map[string]*IngestionStats),
	}
}

// RecordLogIngestion increments the log count for a namespace.
func (s *WorkerStats) RecordLogIngestion(namespace string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.stats[namespace]; !ok {
		s.stats[namespace] = &IngestionStats{}
	}
	s.stats[namespace].Logs++
}

// RecordLogDrop increments the count of log lines dropped by the ingestion
// filter for a namespace. Drops are expected under an opt-in filter config and
// are tracked separately from errors so their volume is observable.
func (s *WorkerStats) RecordLogDrop(namespace string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.stats[namespace]; !ok {
		s.stats[namespace] = &IngestionStats{}
	}
	s.stats[namespace].Dropped++
}

// RecordMetricIngestion increments the metric count for a namespace.
func (s *WorkerStats) RecordMetricIngestion(namespace string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.stats[namespace]; !ok {
		s.stats[namespace] = &IngestionStats{}
	}
	s.stats[namespace].Metrics++
}

// RecordEventIngestion increments the event count for a namespace.
func (s *WorkerStats) RecordEventIngestion(namespace string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.stats[namespace]; !ok {
		s.stats[namespace] = &IngestionStats{}
	}
	s.stats[namespace].Events++
}

// RetainNamespaces drops every recorded namespace that is not in keep, bounding
// the stats map to the current scope. It is meant to be called with the live
// scoped-namespace set (nsscope.Scope.Namespaces()) so entries for namespaces
// that have dropped OUT of scope — e.g. ephemeral CI/preview namespaces under an
// include: ["*"] config — do not accumulate for the process lifetime.
//
// A namespace that is legitimately in scope but idle stays in keep, so it is
// retained here and continues to read as "missing data" for active
// verification: only namespaces no longer in scope are pruned, never merely
// quiet ones.
func (s *WorkerStats) RetainNamespaces(keep []string) {
	keepSet := make(map[string]struct{}, len(keep))
	for _, ns := range keep {
		keepSet[ns] = struct{}{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for ns := range s.stats {
		if _, ok := keepSet[ns]; !ok {
			delete(s.stats, ns)
		}
	}
}

// GetStats returns a copy of the current statistics.
func (s *WorkerStats) GetStats() map[string]IngestionStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	copyStats := make(map[string]IngestionStats)
	for ns, stat := range s.stats {
		copyStats[ns] = *stat
	}
	return copyStats
}

// String returns a formatted string representation of the stats.
func (s *WorkerStats) String() string {
	stats := s.GetStats()
	if len(stats) == 0 {
		return "No ingestion recorded yet"
	}

	var sb strings.Builder
	sb.WriteString("Ingestion Stats: ")

	// Sort namespaces for consistent output
	namespaces := make([]string, 0, len(stats))
	for ns := range stats {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)

	for i, ns := range namespaces {
		entry := stats[ns]
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(fmt.Sprintf("[%s: Logs=%d, Metrics=%d, Events=%d, Dropped=%d]",
			ns, entry.Logs, entry.Metrics, entry.Events, entry.Dropped))
	}

	return sb.String()
}
