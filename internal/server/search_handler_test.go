package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/autonomousops-co/accretion/internal/database"
	"github.com/autonomousops-co/accretion/internal/llm"
	"github.com/autonomousops-co/accretion/internal/testutil"
)

// TestSearchHandler exercises the GET /api/search endpoint using mock LLM and DB clients.
func TestSearchHandler(t *testing.T) {
	testutil.SetupTestEnvironment(t)

	mockLLM, ok := llm.NewMockClient().(*llm.MockClient)
	if !ok {
		t.Fatal("Failed to create mock LLM client")
	}
	mockDB, ok := database.NewMockClient().(*database.MockClient)
	if !ok {
		t.Fatal("Failed to create mock DB client")
	}
	seedSearchEntries(t, mockDB)

	srv := &Server{llmClient: mockLLM, dbClient: mockDB}

	t.Run("missing q returns 400", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/search", http.NoBody)
		w := httptest.NewRecorder()

		srv.searchHandler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected status %d, got %d", http.StatusBadRequest, w.Code)
		}
	})

	t.Run("valid query returns entries", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/search?q=crashloop&limit=5", http.NoBody)
		w := httptest.NewRecorder()

		srv.searchHandler(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected status %d, got %d (body: %s)", http.StatusOK, w.Code, w.Body.String())
		}

		var entries []database.UnifiedEntry
		if err := json.Unmarshal(w.Body.Bytes(), &entries); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if len(entries) != 2 {
			t.Fatalf("expected 2 entries, got %d", len(entries))
		}
		if mockLLM.GetEmbeddingCallCount() == 0 {
			t.Error("expected embedding to be generated for the query")
		}
	})

	t.Run("limit caps the number of results", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/search?q=crashloop&limit=1", http.NoBody)
		w := httptest.NewRecorder()

		srv.searchHandler(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected status %d, got %d", http.StatusOK, w.Code)
		}

		var entries []database.UnifiedEntry
		if err := json.Unmarshal(w.Body.Bytes(), &entries); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if len(entries) != 1 {
			t.Fatalf("expected 1 entry with limit=1, got %d", len(entries))
		}
	})
}

// seedSearchEntries inserts a couple of unified entries into the mock database.
func seedSearchEntries(t *testing.T, mockDB *database.MockClient) {
	t.Helper()

	entries := []*database.UnifiedEntry{
		{
			ResourceType: database.ResourceTypeLog,
			Namespace:    "default",
			ResourceName: "web-server",
			ContentHash:  "search-hash-1",
			Timestamp:    time.Now().Add(-5 * time.Minute),
			Data:         json.RawMessage(`{"message":"CrashLoopBackOff detected"}`),
		},
		{
			ResourceType: database.ResourceTypeEvent,
			Namespace:    "default",
			ResourceName: "api-server",
			ContentHash:  "search-hash-2",
			Timestamp:    time.Now().Add(-3 * time.Minute),
			Data:         json.RawMessage(`{"reason":"BackOff"}`),
		},
	}

	for _, e := range entries {
		if err := mockDB.InsertEntry(e); err != nil {
			t.Fatalf("failed to insert mock entry: %v", err)
		}
	}
}
