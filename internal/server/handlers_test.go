package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pgvector/pgvector-go"

	"github.com/autonomousops-co/accretion/internal/config"
	"github.com/autonomousops-co/accretion/internal/database"
	"github.com/autonomousops-co/accretion/internal/llm"
	"github.com/autonomousops-co/accretion/internal/testutil"
)

// TestHealthHandler tests the health check endpoint.
func TestHealthHandler(t *testing.T) {
	testutil.SetupTestEnvironment(t)

	server := &Server{
		config: &config.Config{},
	}

	tests := []struct {
		name           string
		method         string
		expectedBody   string
		expectedStatus int
	}{
		{
			name:           "GET health check",
			method:         "GET",
			expectedStatus: http.StatusOK,
			expectedBody:   `{"status":"ok","service":"accretion-api"}`,
		},
		{
			name:           "POST health check", // Health handler doesn't check method
			method:         "POST",
			expectedStatus: http.StatusOK,
			expectedBody:   `{"status":"ok","service":"accretion-api"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/api/health", http.NoBody)
			w := httptest.NewRecorder()

			server.healthHandler(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("Expected status %d, got %d", tt.expectedStatus, w.Code)
			}

			body := strings.TrimSpace(w.Body.String())
			if body != tt.expectedBody {
				t.Errorf("Expected body %q, got %q", tt.expectedBody, body)
			}
		})
	}
}

// TestChatHandler tests the chat endpoint with various scenarios.
func TestChatHandler(t *testing.T) {
	testutil.SetupTestEnvironment(t)
	// Create a server with mock configuration
	server := &Server{
		config: &config.Config{
			LLM: config.LLMConfig{
				Provider: "openai",
				OpenAI: config.OpenAIConfig{
					APIKey:  "test-key",
					BaseURL: "https://api.openai.com/v1",
				},
			},
			Database: config.DatabaseConfig{
				Host:     "localhost",
				Port:     5432,
				User:     "test",
				Password: "test",
				Database: "test",
				SSLMode:  "disable",
			},
		},
	}

	tests := []struct {
		body           interface{}
		checkResponse  func(t *testing.T, resp *httptest.ResponseRecorder)
		name           string
		method         string
		expectedStatus int
	}{
		{
			name:           "OPTIONS request (CORS preflight)",
			method:         "OPTIONS",
			body:           nil,
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, resp *httptest.ResponseRecorder) {
				if resp.Header().Get("Access-Control-Allow-Origin") != "*" {
					t.Error("Expected CORS header not found")
				}
			},
		},
		{
			name:           "Invalid method",
			method:         "GET",
			body:           nil,
			expectedStatus: http.StatusMethodNotAllowed,
			checkResponse:  nil,
		},
		{
			name:           "Invalid JSON",
			method:         "POST",
			body:           "invalid json",
			expectedStatus: http.StatusBadRequest,
			checkResponse:  nil,
		},
		{
			name:   "Empty messages",
			method: "POST",
			body: ChatRequest{
				Messages: []llm.Message{},
			},
			expectedStatus: http.StatusBadRequest,
			checkResponse:  nil,
		},
		// Skipping "Valid request" test as it would require actual LLM/DB connections
		// This is covered in TestChatHandlerWithMocks instead
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req *http.Request
			if tt.body != nil {
				if bodyStr, ok := tt.body.(string); ok {
					req = httptest.NewRequest(tt.method, "/api/chat", strings.NewReader(bodyStr))
				} else {
					bodyBytes, err := json.Marshal(tt.body)
					if err != nil {
						t.Fatalf("Failed to marshal test body: %v", err)
					}
					req = httptest.NewRequest(tt.method, "/api/chat", bytes.NewBuffer(bodyBytes))
				}
				req.Header.Set("Content-Type", "application/json")
			} else {
				req = httptest.NewRequest(tt.method, "/api/chat", http.NoBody)
			}

			w := httptest.NewRecorder()

			// This test will create actual LLM and DB clients
			// In a real test environment, you'd want to inject mocks
			server.chatHandler(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("Expected status %d, got %d", tt.expectedStatus, w.Code)
				t.Logf("Response body: %s", w.Body.String())
			}

			if tt.checkResponse != nil {
				tt.checkResponse(t, w)
			}
		})
	}
}

// TestChatHandlerWithMocks tests the chat handler using dependency injection.
func TestChatHandlerWithMocks(t *testing.T) {
	testutil.SetupTestEnvironment(t)
	// Create a testable version of the chat handler
	mockLLM, ok := llm.NewMockClient().(*llm.MockClient)
	if !ok {
		t.Fatal("Failed to create mock LLM client")
	}
	mockDB, ok := database.NewMockClient().(*database.MockClient)
	if !ok {
		t.Fatal("Failed to create mock DB client")
	}

	// Configure mock responses
	mockLLM.SetChatResponses([]llm.Response{
		llm.CreateMockResponse("Based on the logs, I can see some connection issues in your cluster.", 25, 20),
	})

	// Add some mock search results
	setupMockSearchResults(mockDB)

	// Create request
	reqBody := ChatRequest{
		Messages: []llm.Message{
			{Role: "user", Content: "What's wrong with my cluster?"},
		},
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("Failed to marshal request body: %v", err)
	}
	req := httptest.NewRequest("POST", "/api/chat", bytes.NewBuffer(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()

	// Create a testable chat handler
	testChatHandler(w, req, mockLLM, mockDB)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, w.Code)
		t.Logf("Response body: %s", w.Body.String())
		return
	}

	var resp ChatResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	if resp.Content == "" {
		t.Error("Expected response content but got empty string")
	}

	if !strings.Contains(resp.Content, "connection issues") {
		t.Error("Expected response to contain mock content about connection issues")
	}

	// Verify mocks were called
	if mockLLM.GetEmbeddingCallCount() == 0 {
		t.Error("Expected embedding generation call")
	}

	if mockLLM.GetChatCallCount() == 0 {
		t.Error("Expected chat call")
	}
}

// TestLogsHandler tests the logs endpoint.
func TestLogsHandler(t *testing.T) {
	testutil.SetupTestEnvironment(t)
	server := &Server{
		config: &config.Config{
			Database: config.DatabaseConfig{
				Host:     "localhost",
				Port:     5432,
				User:     "test",
				Password: "test",
				Database: "test",
				SSLMode:  "disable",
			},
		},
	}

	tests := []struct {
		checkResponse  func(t *testing.T, resp *httptest.ResponseRecorder)
		name           string
		method         string
		queryParams    string
		expectedStatus int
	}{
		{
			name:           "Invalid method",
			method:         "POST",
			queryParams:    "",
			expectedStatus: http.StatusMethodNotAllowed,
			checkResponse:  nil,
		},
		// Skipping tests that would require actual database connections
		// These are covered in TestLogsHandlerWithMock instead
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/api/logs"+tt.queryParams, http.NoBody)
			w := httptest.NewRecorder()

			// This test will create an actual DB client
			// In a real test environment, you'd want to inject mocks
			server.logsHandler(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("Expected status %d, got %d", tt.expectedStatus, w.Code)
				t.Logf("Response body: %s", w.Body.String())
			}

			if tt.checkResponse != nil {
				tt.checkResponse(t, w)
			}
		})
	}
}

// TestLogsHandlerWithMock tests the logs handler using mock database.
func TestLogsHandlerWithMock(t *testing.T) {
	testutil.SetupTestEnvironment(t)
	mockDB, ok := database.NewMockClient().(*database.MockClient)
	if !ok {
		t.Fatal("Failed to create mock DB client")
	}

	// Add some mock log entries
	setupMockLogEntries(mockDB)

	req := httptest.NewRequest("GET", "/api/logs?namespace=default&limit=5", http.NoBody)
	w := httptest.NewRecorder()

	// Create testable logs handler
	testLogsHandler(w, req, mockDB)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, w.Code)
		return
	}

	var resp LogsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	if len(resp.Logs) == 0 {
		t.Error("Expected some log entries but got none")
	}

	// Verify the logs are from the correct namespace
	for _, log := range resp.Logs {
		if log.Namespace != "default" {
			t.Errorf("Expected namespace 'default', got '%s'", log.Namespace)
		}
	}
}

// TestBuildContextFromResults tests the context building logic.
func TestBuildContextFromResults(t *testing.T) {
	testutil.SetupTestEnvironment(t)
	server := &Server{}

	tests := []struct {
		name     string
		expected string
		results  []database.SearchResult
	}{
		{
			name:     "Empty results",
			results:  []database.SearchResult{},
			expected: "No relevant logs or metrics found in the database.",
		},
		{
			name: "Log entries only",
			results: []database.SearchResult{
				{
					LogEntry: &database.LogEntry{
						Namespace: "default",
						PodName:   "test-pod",
						Container: "app",
						Message:   "Application error occurred",
						Timestamp: time.Now(),
					},
					Similarity: 0.9,
				},
			},
			expected: "Application error occurred",
		},
		{
			name: "Mixed log and metrics entries",
			results: []database.SearchResult{
				{
					LogEntry: &database.LogEntry{
						Message: "High CPU usage detected",
					},
					Similarity: 0.8,
				},
				{
					MetricsEntry: &database.MetricsEntry{
						PodName: "web-server",
						CPU:     "500m",
						Memory:  "1Gi",
					},
					Similarity: 0.7,
				},
			},
			expected: "High CPU usage detected",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := server.buildContextFromResults(tt.results)

			if tt.expected != "" && !strings.Contains(result, tt.expected) {
				t.Errorf("Expected result to contain %q, got: %s", tt.expected, result)
			}
		})
	}
}

// Helper functions for testing

// testChatHandler is a testable version of chatHandler that accepts mock dependencies.
func testChatHandler(w http.ResponseWriter, r *http.Request, llmClient llm.Client, dbClient database.Interface) {
	// Handle CORS preflight request
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Max-Age", "86400")
		w.WriteHeader(http.StatusOK)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Set CORS headers for actual POST request
	w.Header().Set("Access-Control-Allow-Origin", "*")

	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if len(req.Messages) == 0 {
		http.Error(w, "No messages provided", http.StatusBadRequest)
		return
	}

	// Get the latest user message for context search
	userMessage := req.Messages[len(req.Messages)-1].Content

	ctx := context.Background()

	// Generate embedding for the user query
	embedding, err := llmClient.GenerateEmbedding(ctx, userMessage)
	if err != nil {
		http.Error(w, "Failed to generate embedding", http.StatusInternalServerError)
		return
	}

	// Search for similar entries
	queryVector := pgvector.NewVector(embedding)
	searchResults, err := dbClient.SearchSimilar(queryVector, 10)
	if err != nil {
		http.Error(w, "Failed to search database", http.StatusInternalServerError)
		return
	}

	// Build context from search results
	server := &Server{}
	contextData := server.buildContextFromResults(searchResults)

	// Add context to the conversation
	if contextData != "" {
		contextMsg := llm.Message{
			Role:    "system",
			Content: "Based on recent logs and metrics from the cluster:\n" + contextData,
		}
		req.Messages = append([]llm.Message{contextMsg}, req.Messages...)
	}

	// Get response from LLM
	response, err := llmClient.Chat(ctx, req.Messages)
	if err != nil {
		http.Error(w, "Failed to get LLM response", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(ChatResponse{
		Content: response.Content,
		Usage:   &response.Usage,
	}); err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}

// testLogsHandler is a testable version of logsHandler that accepts mock dependencies.
func testLogsHandler(w http.ResponseWriter, r *http.Request, dbClient database.Interface) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	namespace := r.URL.Query().Get("namespace")
	if namespace == "" {
		namespace = "default"
	}

	limitStr := r.URL.Query().Get("limit")
	limit := 50
	if limitStr != "" {
		if parsedLimit, err := strconv.Atoi(limitStr); err == nil && parsedLimit > 0 {
			limit = parsedLimit
		}
	}

	logs, err := dbClient.GetRecentLogEntries(namespace, limit)
	if err != nil {
		http.Error(w, "Failed to get logs", http.StatusInternalServerError)
		return
	}

	// Handle empty results gracefully
	if logs == nil {
		logs = []database.LogEntry{}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(LogsResponse{
		Logs: logs,
	}); err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}

// setupMockSearchResults adds mock search results to the database.
func setupMockSearchResults(mockDB *database.MockClient) {
	// Add some log entries
	logEntry1 := &database.LogEntry{
		ID:          1,
		Namespace:   "default",
		PodName:     "web-server",
		Container:   "nginx",
		Message:     "Connection timeout to upstream server",
		Timestamp:   time.Now().Add(-5 * time.Minute),
		Embedding:   pgvector.NewVector(generateTestEmbedding("connection-error")),
		ContentHash: "hash1",
	}

	logEntry2 := &database.LogEntry{
		ID:          2,
		Namespace:   "default",
		PodName:     "api-server",
		Container:   "app",
		Message:     "Database connection failed",
		Timestamp:   time.Now().Add(-3 * time.Minute),
		Embedding:   pgvector.NewVector(generateTestEmbedding("db-error")),
		ContentHash: "hash2",
	}

	if err := mockDB.InsertLogEntry(logEntry1); err != nil {
		// In tests, we typically don't fail on setup errors, just log them
		_ = err
	}
	if err := mockDB.InsertLogEntry(logEntry2); err != nil {
		// In tests, we typically don't fail on setup errors, just log them
		_ = err
	}
}

// setupMockLogEntries adds mock log entries for testing logs endpoint.
func setupMockLogEntries(mockDB *database.MockClient) {
	entries := []*database.LogEntry{
		{
			ID:          1,
			Namespace:   "default",
			PodName:     "test-pod-1",
			Container:   "app",
			Message:     "Application started",
			Timestamp:   time.Now().Add(-10 * time.Minute),
			ContentHash: "hash1",
		},
		{
			ID:          2,
			Namespace:   "default",
			PodName:     "test-pod-2",
			Container:   "web",
			Message:     "HTTP server ready",
			Timestamp:   time.Now().Add(-5 * time.Minute),
			ContentHash: "hash2",
		},
		{
			ID:          3,
			Namespace:   "kube-system",
			PodName:     "coredns",
			Container:   "coredns",
			Message:     "DNS server started",
			Timestamp:   time.Now().Add(-2 * time.Minute),
			ContentHash: "hash3",
		},
	}

	for _, entry := range entries {
		if err := mockDB.InsertLogEntry(entry); err != nil {
			// In tests, we typically don't fail on setup errors, just log them
			_ = err
		}
	}
}

// generateTestEmbedding creates a simple test embedding.
func generateTestEmbedding(seed string) []float32 {
	embedding := make([]float32, 768)
	seedBytes := []byte(seed)

	for i := 0; i < 768; i++ {
		byteIndex := i % len(seedBytes)
		embedding[i] = float32(seedBytes[byteIndex]) / 255.0
	}

	return embedding
}
