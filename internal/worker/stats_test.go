package worker

import (
	"sync"
	"testing"
)

func TestWorkerStats_RecordLogIngestion(t *testing.T) {
	stats := NewWorkerStats()
	ns := "default"

	stats.RecordLogIngestion(ns)
	stats.RecordLogIngestion(ns)

	s := stats.GetStats()
	if s[ns].Logs != 2 {
		t.Errorf("expected 2 logs, got %d", s[ns].Logs)
	}
}

func TestWorkerStats_RecordMetricIngestion(t *testing.T) {
	stats := NewWorkerStats()
	ns := "kube-system"

	stats.RecordMetricIngestion(ns)

	s := stats.GetStats()
	if s[ns].Metrics != 1 {
		t.Errorf("expected 1 metric, got %d", s[ns].Metrics)
	}
}

func TestWorkerStats_ConcurrentAccess(t *testing.T) {
	stats := NewWorkerStats()
	ns := "concurrent-test"
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stats.RecordLogIngestion(ns)
			stats.RecordMetricIngestion(ns)
		}()
	}

	wg.Wait()

	s := stats.GetStats()
	if s[ns].Logs != 100 {
		t.Errorf("expected 100 logs, got %d", s[ns].Logs)
	}
	if s[ns].Metrics != 100 {
		t.Errorf("expected 100 metrics, got %d", s[ns].Metrics)
	}
}

func TestWorkerStats_RetainNamespaces(t *testing.T) {
	stats := NewWorkerStats()
	stats.RecordLogIngestion("keep-active")
	stats.RecordMetricIngestion("drop-me")
	stats.RecordEventIngestion("also-drop")

	// "keep-idle" is in scope but has no recorded ingestion: it must survive the
	// prune so active verification still flags it as missing data.
	stats.RetainNamespaces([]string{"keep-active", "keep-idle"})

	s := stats.GetStats()
	if _, ok := s["keep-active"]; !ok {
		t.Error("keep-active was pruned; in-scope namespaces must be retained")
	}
	if _, ok := s["drop-me"]; ok {
		t.Error("drop-me was retained; out-of-scope namespaces must be pruned")
	}
	if _, ok := s["also-drop"]; ok {
		t.Error("also-drop was retained; out-of-scope namespaces must be pruned")
	}
	// An idle in-scope namespace has no map entry to begin with (nothing recorded
	// it), and RetainNamespaces must not invent one — it only deletes.
	if _, ok := s["keep-idle"]; ok {
		t.Error("keep-idle should not exist as a stats entry (nothing ingested)")
	}
	if len(s) != 1 {
		t.Errorf("expected exactly 1 retained entry, got %d: %v", len(s), s)
	}
}

func TestWorkerStats_RetainNamespaces_EmptyKeepDeletesAll(t *testing.T) {
	stats := NewWorkerStats()
	stats.RecordLogIngestion("ns1")
	stats.RecordMetricIngestion("ns2")

	stats.RetainNamespaces(nil)

	if s := stats.GetStats(); len(s) != 0 {
		t.Errorf("expected all entries deleted for an empty keep set, got %v", s)
	}
}

func TestWorkerStats_RetainNamespaces_Concurrent(t *testing.T) {
	stats := NewWorkerStats()
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stats.RecordLogIngestion("in-scope")
			stats.RecordLogIngestion("ephemeral")
			stats.RetainNamespaces([]string{"in-scope"})
		}()
	}
	wg.Wait()

	// After the last retain, only the in-scope namespace may remain. The race
	// detector guards the concurrent map access under the write lock.
	if _, ok := stats.GetStats()["ephemeral"]; ok {
		t.Error("ephemeral namespace survived; RetainNamespaces should have pruned it")
	}
}

func TestWorkerStats_String(t *testing.T) {
	stats := NewWorkerStats()
	stats.RecordLogIngestion("ns1")
	stats.RecordMetricIngestion("ns2")

	str := stats.String()
	expected := "Ingestion Stats: [ns1: Logs=1, Metrics=0, Events=0, Dropped=0], [ns2: Logs=0, Metrics=1, Events=0, Dropped=0]"
	if str != expected {
		t.Errorf("expected %q, got %q", expected, str)
	}
}
