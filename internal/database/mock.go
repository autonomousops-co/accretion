package database

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pgvector/pgvector-go"
)

// timestampInRange checks whether t falls within the time range specified by opts.
func timestampInRange(t time.Time, opts SearchOptions) bool {
	if !opts.StartTime.IsZero() && t.Before(opts.StartTime) {
		return false
	}
	if !opts.EndTime.IsZero() && t.After(opts.EndTime) {
		return false
	}
	return true
}

// MockClient is a mock implementation of the Interface for testing purposes.
type MockClient struct {
	entries []UnifiedEntry
	mu      sync.RWMutex
	nextID  int
	closed  bool
}

// NewMockClient creates a new mock database client for testing.
func NewMockClient() Interface {
	return &MockClient{
		entries: make([]UnifiedEntry, 0),
		nextID:  1,
	}
}

// InsertEntry inserts a unified entry into the mock storage.
func (m *MockClient) InsertEntry(entry *UnifiedEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return fmt.Errorf("mock client is closed")
	}

	// Check for duplicate content hash (if non-empty)
	if entry.ContentHash != "" {
		for i := range m.entries {
			if m.entries[i].ContentHash == entry.ContentHash {
				return nil // ON CONFLICT DO NOTHING
			}
		}
	}

	entry.ID = m.nextID
	entry.CreatedAt = time.Now()
	m.nextID++

	newEntry := *entry
	// Deep copy Data
	if entry.Data != nil {
		dataCopy := make(json.RawMessage, len(entry.Data))
		copy(dataCopy, entry.Data)
		newEntry.Data = dataCopy
	}
	m.entries = append(m.entries, newEntry)
	return nil
}

// SearchEntries performs a mock similarity search on unified entries.
func (m *MockClient) SearchEntries(
	queryEmbedding pgvector.Vector, limit int, opts SearchOptions,
) ([]UnifiedEntry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.closed {
		return nil, fmt.Errorf("mock client is closed")
	}

	results := make([]UnifiedEntry, 0, len(m.entries))
	for i := range m.entries {
		if !timestampInRange(m.entries[i].Timestamp, opts) {
			continue
		}
		entry := m.entries[i]
		// Assign a mock similarity based on position
		entry.Similarity = 1.0 - (float64(i) / float64(len(m.entries)+1))
		results = append(results, entry)
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Similarity > results[j].Similarity
	})

	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}

	return results, nil
}

// SearchSimilar performs a mock similarity search (legacy compatibility).
func (m *MockClient) SearchSimilar(queryEmbedding pgvector.Vector, limit int) ([]SearchResult, error) {
	return m.SearchSimilarWithOptions(queryEmbedding, limit, SearchOptions{})
}

// SearchSimilarWithOptions performs a mock similarity search with optional time filtering.
func (m *MockClient) SearchSimilarWithOptions(
	queryEmbedding pgvector.Vector, limit int, opts SearchOptions,
) ([]SearchResult, error) {
	entries, err := m.SearchEntries(queryEmbedding, limit, opts)
	if err != nil {
		return nil, err
	}

	return unifiedEntriesToSearchResults(entries), nil
}

// GetRecentLogEntries retrieves recent log entries from mock storage.
func (m *MockClient) GetRecentLogEntries(namespace string, limit int) ([]LogEntry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.closed {
		return nil, fmt.Errorf("mock client is closed")
	}

	filtered := make([]LogEntry, 0, len(m.entries))
	for i := range m.entries {
		if m.entries[i].ResourceType != ResourceTypeLog || m.entries[i].Namespace != namespace {
			continue
		}
		entry := LogEntry{
			ID:          m.entries[i].ID,
			Namespace:   m.entries[i].Namespace,
			PodName:     m.entries[i].ResourceName,
			Timestamp:   m.entries[i].Timestamp,
			Embedding:   m.entries[i].Embedding,
			CreatedAt:   m.entries[i].CreatedAt,
			ContentHash: m.entries[i].ContentHash,
		}
		var d struct {
			Container string `json:"container"`
			Message   string `json:"message"`
		}
		if err := json.Unmarshal(m.entries[i].Data, &d); err == nil {
			entry.Container = d.Container
			entry.Message = d.Message
		}
		filtered = append(filtered, entry)
	}

	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].CreatedAt.After(filtered[j].CreatedAt)
	})

	if limit > 0 && len(filtered) > limit {
		filtered = filtered[:limit]
	}

	return filtered, nil
}

// GetLogStats returns statistics about the mock storage.
func (m *MockClient) GetLogStats() (map[string]int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.closed {
		return nil, fmt.Errorf("mock client is closed")
	}

	stats := make(map[string]int)
	totalLogs := 0
	logsWithEmbeddings := 0
	for i := range m.entries {
		if m.entries[i].ResourceType == ResourceTypeLog {
			totalLogs++
			if len(m.entries[i].Embedding.Slice()) > 0 {
				logsWithEmbeddings++
			}
		}
	}
	stats["total_entries"] = totalLogs
	stats["entries_with_embeddings"] = logsWithEmbeddings

	return stats, nil
}

// Close marks the mock client as closed.
func (m *MockClient) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.closed = true
	return nil
}

// QueryResources retrieves entries matching the given structured query.
//
//nolint:cyclop,gocyclo // mock mirrors real query builder with many optional filters
func (m *MockClient) QueryResources(query *ResourceQuery) ([]UnifiedEntry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.closed {
		return nil, fmt.Errorf("mock client is closed")
	}

	typeSet := make(map[string]bool, len(query.ResourceTypes))
	for _, rt := range query.ResourceTypes {
		typeSet[rt] = true
	}

	results := make([]UnifiedEntry, 0)
	for i := range m.entries {
		e := &m.entries[i]
		if len(typeSet) > 0 && !typeSet[e.ResourceType] {
			continue
		}
		if query.Namespace != "" && e.Namespace != query.Namespace {
			continue
		}
		if query.ResourceName != "" && !strings.Contains(
			strings.ToLower(e.ResourceName), strings.ToLower(query.ResourceName),
		) {
			continue
		}
		if !query.StartTime.IsZero() && e.Timestamp.Before(query.StartTime) {
			continue
		}
		if !query.EndTime.IsZero() && e.Timestamp.After(query.EndTime) {
			continue
		}
		// Check DataFilters (exact match on JSONB fields)
		if len(query.DataFilters) > 0 || len(query.DataContains) > 0 {
			var dataMap map[string]interface{}
			if err := json.Unmarshal(e.Data, &dataMap); err != nil {
				continue
			}
			match := true
			for k, v := range query.DataFilters {
				if fmt.Sprintf("%v", dataMap[k]) != v {
					match = false
					break
				}
			}
			if !match {
				continue
			}
			for k, v := range query.DataContains {
				val := fmt.Sprintf("%v", dataMap[k])
				if !strings.Contains(strings.ToLower(val), strings.ToLower(v)) {
					match = false
					break
				}
			}
			if !match {
				continue
			}
		}
		results = append(results, *e)
	}

	// Sort by timestamp descending
	sort.Slice(results, func(i, j int) bool {
		return results[i].Timestamp.After(results[j].Timestamp)
	})

	limit := query.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	if len(results) > limit {
		results = results[:limit]
	}

	return results, nil
}

// GetResourceStats returns entry counts grouped by resource type and namespace.
func (m *MockClient) GetResourceStats() ([]ResourceStat, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.closed {
		return nil, fmt.Errorf("mock client is closed")
	}

	counts := make(map[string]int)
	for i := range m.entries {
		key := m.entries[i].ResourceType + "|" + m.entries[i].Namespace
		counts[key]++
	}

	stats := make([]ResourceStat, 0, len(counts))
	for key, count := range counts {
		parts := strings.SplitN(key, "|", 2)
		stats = append(stats, ResourceStat{
			ResourceType: parts[0],
			Namespace:    parts[1],
			Count:        count,
		})
	}

	sort.Slice(stats, func(i, j int) bool {
		if stats[i].ResourceType != stats[j].ResourceType {
			return stats[i].ResourceType < stats[j].ResourceType
		}
		return stats[i].Namespace < stats[j].Namespace
	})

	return stats, nil
}

// Reset clears all data from the mock client (useful for tests).
func (m *MockClient) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.entries = make([]UnifiedEntry, 0)
	m.nextID = 1
	m.closed = false
}

// GetAllLogEntries returns all log entries (useful for testing).
func (m *MockClient) GetAllLogEntries() []LogEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entries := make([]LogEntry, 0, len(m.entries))
	for i := range m.entries {
		if m.entries[i].ResourceType != ResourceTypeLog {
			continue
		}
		entry := LogEntry{
			ID:          m.entries[i].ID,
			Namespace:   m.entries[i].Namespace,
			PodName:     m.entries[i].ResourceName,
			Timestamp:   m.entries[i].Timestamp,
			Embedding:   m.entries[i].Embedding,
			CreatedAt:   m.entries[i].CreatedAt,
			ContentHash: m.entries[i].ContentHash,
		}
		var d struct {
			Container string `json:"container"`
			Message   string `json:"message"`
		}
		if err := json.Unmarshal(m.entries[i].Data, &d); err == nil {
			entry.Container = d.Container
			entry.Message = d.Message
		}
		entries = append(entries, entry)
	}
	return entries
}

// GetAllMetricsEntries returns all metrics entries (useful for testing).
func (m *MockClient) GetAllMetricsEntries() []MetricsEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entries := make([]MetricsEntry, 0, len(m.entries))
	for i := range m.entries {
		if m.entries[i].ResourceType != ResourceTypeMetrics {
			continue
		}
		entry := MetricsEntry{
			ID:        m.entries[i].ID,
			Namespace: m.entries[i].Namespace,
			PodName:   m.entries[i].ResourceName,
			Timestamp: m.entries[i].Timestamp,
			Embedding: m.entries[i].Embedding,
			CreatedAt: m.entries[i].CreatedAt,
		}
		var d struct {
			Container string `json:"container"`
			CPU       string `json:"cpu"`
			Memory    string `json:"memory"`
		}
		if err := json.Unmarshal(m.entries[i].Data, &d); err == nil {
			entry.Container = d.Container
			entry.CPU = d.CPU
			entry.Memory = d.Memory
		}
		entries = append(entries, entry)
	}
	return entries
}

// InsertLogEntry is a convenience method that converts a LogEntry to a UnifiedEntry and inserts it.
func (m *MockClient) InsertLogEntry(entry *LogEntry) error {
	data, err := json.Marshal(map[string]interface{}{
		"container": entry.Container,
		"message":   entry.Message,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal log data: %w", err)
	}
	ue := &UnifiedEntry{
		ResourceType: ResourceTypeLog,
		Namespace:    entry.Namespace,
		ResourceName: entry.PodName,
		Data:         data,
		Timestamp:    entry.Timestamp,
		Embedding:    entry.Embedding,
		ContentHash:  entry.ContentHash,
	}
	if err = m.InsertEntry(ue); err == nil {
		entry.ID = ue.ID
		entry.CreatedAt = ue.CreatedAt
	}
	return err
}

// InsertMetricsEntry is a convenience method that converts a MetricsEntry to a UnifiedEntry and inserts it.
func (m *MockClient) InsertMetricsEntry(entry *MetricsEntry) error {
	data, err := json.Marshal(map[string]interface{}{
		"container": entry.Container,
		"cpu":       entry.CPU,
		"memory":    entry.Memory,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal metrics data: %w", err)
	}
	ue := &UnifiedEntry{
		ResourceType: ResourceTypeMetrics,
		Namespace:    entry.Namespace,
		ResourceName: entry.PodName,
		Data:         data,
		Timestamp:    entry.Timestamp,
		Embedding:    entry.Embedding,
	}
	if err = m.InsertEntry(ue); err == nil {
		entry.ID = ue.ID
		entry.CreatedAt = ue.CreatedAt
	}
	return err
}
