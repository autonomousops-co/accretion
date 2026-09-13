package database_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/pgvector/pgvector-go"

	"github.com/autonomousops-co/accretion/internal/database"
)

// TestPopulateTraceResult verifies that a stored trace UnifiedEntry is decoded
// back into a TraceEntry through the search/populate path (exercises the
// ResourceTypeTrace case added to populateSearchResult).
func TestPopulateTraceResult(t *testing.T) {
	db := database.NewMockClient()

	data, err := json.Marshal(map[string]any{
		"trace_id":    "abc123",
		"span_id":     "def456",
		"service":     "dkjr",
		"operation":   "GET /api/things",
		"kind":        "SPAN_KIND_SERVER",
		"status_code": "STATUS_CODE_OK",
		"status_msg":  "",
		"duration_ms": 12.5,
		"summary":     "dkjr: GET /api/things took 12ms, OK",
	})
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}

	entry := &database.UnifiedEntry{
		ResourceType: database.ResourceTypeTrace,
		Namespace:    "dkjr",
		ResourceName: "dkjr",
		ContentHash:  "hash-1",
		Data:         data,
		Timestamp:    time.Now().Add(-time.Minute),
		Embedding:    pgvector.NewVector(make([]float32, 3)),
	}
	if insErr := db.InsertEntry(entry); insErr != nil {
		t.Fatalf("insert: %v", insErr)
	}

	results, err := db.SearchSimilar(pgvector.NewVector(make([]float32, 3)), 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	var got *database.TraceEntry
	for _, r := range results {
		if r.TraceEntry != nil {
			got = r.TraceEntry
			break
		}
	}
	if got == nil {
		t.Fatal("expected a populated TraceEntry in search results, got none")
	}

	if got.TraceID != "abc123" {
		t.Errorf("TraceID = %q, want abc123", got.TraceID)
	}
	if got.SpanID != "def456" {
		t.Errorf("SpanID = %q, want def456", got.SpanID)
	}
	if got.Service != "dkjr" {
		t.Errorf("Service = %q, want dkjr", got.Service)
	}
	if got.Operation != "GET /api/things" {
		t.Errorf("Operation = %q, want GET /api/things", got.Operation)
	}
	if got.StatusCode != "STATUS_CODE_OK" {
		t.Errorf("StatusCode = %q, want STATUS_CODE_OK", got.StatusCode)
	}
	if got.DurationMs != 12.5 {
		t.Errorf("DurationMs = %v, want 12.5", got.DurationMs)
	}
	if got.Namespace != "dkjr" {
		t.Errorf("Namespace = %q, want dkjr", got.Namespace)
	}
	if got.Summary == "" {
		t.Error("expected non-empty Summary")
	}
}
