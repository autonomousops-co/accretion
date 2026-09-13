package database

import (
	"context"

	"github.com/pgvector/pgvector-go"

	"github.com/autonomousops-co/accretion/internal/telemetry"
)

// TracingDecorator wraps a database.Interface with OpenTelemetry spans.
type TracingDecorator struct {
	inner Interface
}

// NewTracingDecorator wraps db with tracing spans on each method call.
func NewTracingDecorator(db Interface) Interface {
	return &TracingDecorator{inner: db}
}

func (t *TracingDecorator) InsertEntry(entry *UnifiedEntry) error {
	_, span := telemetry.StartSpan(context.Background(), "database", "db.InsertEntry")
	defer span.End()
	return t.inner.InsertEntry(entry)
}

func (t *TracingDecorator) SearchEntries(
	queryEmbedding pgvector.Vector, limit int, opts SearchOptions,
) ([]UnifiedEntry, error) {
	_, span := telemetry.StartSpan(context.Background(), "database", "db.SearchEntries")
	defer span.End()
	return t.inner.SearchEntries(queryEmbedding, limit, opts)
}

func (t *TracingDecorator) GetRecentLogEntries(namespace string, limit int) ([]LogEntry, error) {
	_, span := telemetry.StartSpan(context.Background(), "database", "db.GetRecentLogEntries")
	defer span.End()
	return t.inner.GetRecentLogEntries(namespace, limit)
}

func (t *TracingDecorator) SearchSimilar(queryEmbedding pgvector.Vector, limit int) ([]SearchResult, error) {
	_, span := telemetry.StartSpan(context.Background(), "database", "db.SearchSimilar")
	defer span.End()
	return t.inner.SearchSimilar(queryEmbedding, limit)
}

func (t *TracingDecorator) SearchSimilarWithOptions(
	queryEmbedding pgvector.Vector, limit int, opts SearchOptions,
) ([]SearchResult, error) {
	_, span := telemetry.StartSpan(context.Background(), "database", "db.SearchSimilarWithOptions")
	defer span.End()
	return t.inner.SearchSimilarWithOptions(queryEmbedding, limit, opts)
}

func (t *TracingDecorator) QueryResources(query *ResourceQuery) ([]UnifiedEntry, error) {
	_, span := telemetry.StartSpan(context.Background(), "database", "db.QueryResources")
	defer span.End()
	return t.inner.QueryResources(query)
}

func (t *TracingDecorator) GetResourceStats() ([]ResourceStat, error) {
	_, span := telemetry.StartSpan(context.Background(), "database", "db.GetResourceStats")
	defer span.End()
	return t.inner.GetResourceStats()
}

func (t *TracingDecorator) GetLogStats() (map[string]int, error) {
	_, span := telemetry.StartSpan(context.Background(), "database", "db.GetLogStats")
	defer span.End()
	return t.inner.GetLogStats()
}

func (t *TracingDecorator) Close() error {
	return t.inner.Close()
}
