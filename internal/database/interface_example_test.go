package database_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/pgvector/pgvector-go"

	"github.com/autonomousops-co/accretion/internal/database"
)

// ExampleInterface demonstrates how to use the database interface with different implementations.
func ExampleInterface() {
	// This example shows how the interface allows us to use different implementations
	// interchangeably, which is great for testing and future extensibility.

	// Create a mock client for testing
	mockDB := database.NewMockClient()
	defer mockDB.Close()

	// The interface allows us to use the same code with different implementations
	testDatabaseOperations(mockDB)

	// In production, you would use:
	// realDB, err := database.NewClient(&config.DatabaseConfig{...})
	// if err != nil { ... }
	// defer realDB.Close()
	// testDatabaseOperations(realDB)
}

// testDatabaseOperations demonstrates using the database interface
func testDatabaseOperations(db database.Interface) {
	// Insert a unified entry
	data, _ := json.Marshal(map[string]interface{}{ //nolint:errcheck // example code
		"container": "test-container",
		"message":   "Test log message",
	})
	entry := &database.UnifiedEntry{
		ResourceType: "log",
		Namespace:    "default",
		ResourceName: "test-pod",
		Data:         data,
		Timestamp:    time.Now(),
		Embedding:    pgvector.NewVector([]float32{0.1, 0.2, 0.3}),
		ContentHash:  "test-hash-123",
	}

	if err := db.InsertEntry(entry); err != nil {
		_ = err
	}

	// Get recent entries
	if entries, err := db.GetRecentLogEntries("default", 10); err != nil {
		_ = err
	} else {
		_ = entries
	}

	// Get statistics
	if stats, err := db.GetLogStats(); err != nil {
		_ = err
	} else {
		_ = stats
	}

	// Perform similarity search
	queryVector := pgvector.NewVector([]float32{0.1, 0.2, 0.3})
	if results, err := db.SearchSimilar(queryVector, 5); err != nil {
		_ = err
	} else {
		_ = results
	}
}

// TestInterfaceImplementation verifies that our implementations satisfy the interface
func TestInterfaceImplementation(t *testing.T) {
	// Test that MockClient implements Interface
	var _ database.Interface = database.NewMockClient()

	// This test will fail to compile if the interface contract is broken
	t.Log("Interface implementations verified at compile time")
}

// TestMockClientBasicOperations tests basic operations with the mock client
func TestMockClientBasicOperations(t *testing.T) {
	mockDB := database.NewMockClient()
	defer mockDB.Close()

	// Test inserting a unified entry (log type)
	data, _ := json.Marshal(map[string]interface{}{ //nolint:errcheck // test code
		"container": "test-container",
		"message":   "Test message",
	})
	entry := &database.UnifiedEntry{
		ResourceType: "log",
		Namespace:    "test-namespace",
		ResourceName: "test-pod",
		Data:         data,
		Timestamp:    time.Now(),
		Embedding:    pgvector.NewVector([]float32{1.0, 2.0, 3.0}),
		ContentHash:  "unique-hash-123",
	}

	err := mockDB.InsertEntry(entry)
	if err != nil {
		t.Fatalf("Failed to insert entry: %v", err)
	}

	// Test retrieving entries
	entries, err := mockDB.GetRecentLogEntries("test-namespace", 10)
	if err != nil {
		t.Fatalf("Failed to get recent entries: %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("Expected 1 entry, got %d", len(entries))
	}

	if entries[0].Message != "Test message" {
		t.Errorf("Expected message 'Test message', got '%s'", entries[0].Message)
	}

	// Test duplicate prevention
	err = mockDB.InsertEntry(entry) // Same entry again (same content_hash)
	if err != nil {
		t.Fatalf("Failed to handle duplicate entry: %v", err)
	}

	// Should still have only 1 entry
	entries, err = mockDB.GetRecentLogEntries("test-namespace", 10)
	if err != nil {
		t.Fatalf("Failed to get recent entries after duplicate: %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("Expected 1 entry after duplicate insert, got %d", len(entries))
	}

	// Test stats
	stats, err := mockDB.GetLogStats()
	if err != nil {
		t.Fatalf("Failed to get stats: %v", err)
	}

	if stats["total_entries"] != 1 {
		t.Errorf("Expected 1 total entry, got %d", stats["total_entries"])
	}

	if stats["entries_with_embeddings"] != 1 {
		t.Errorf("Expected 1 entry with embedding, got %d", stats["entries_with_embeddings"])
	}
}
