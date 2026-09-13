package llm

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// MockClient is a mock implementation of the Client interface for testing purposes.
// It provides predictable responses and can be configured to simulate different scenarios.
type MockClient struct {
	chatError           error
	embeddingError      error
	embeddings          map[string][]float32
	chatResponses       []Response
	chatCallCount       int
	embeddingCallCount  int
	mu                  sync.RWMutex
	shouldFailChat      bool
	shouldFailEmbedding bool
}

// NewMockClient creates a new mock LLM client for testing.
func NewMockClient() Client {
	return &MockClient{
		chatResponses: []Response{},
		embeddings:    make(map[string][]float32),
	}
}

// Chat returns a mock response based on the configured responses.
func (m *MockClient) Chat(ctx context.Context, messages []Message) (*Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.chatCallCount++

	if m.shouldFailChat {
		if m.chatError != nil {
			return nil, m.chatError
		}
		return nil, fmt.Errorf("mock chat error")
	}

	// If no responses are configured, generate a default response
	if len(m.chatResponses) == 0 {
		return &Response{
			Content: fmt.Sprintf("Mock response to: %s", getLastUserMessage(messages)),
			Usage: Usage{
				PromptTokens:     calculateMockTokens(messages),
				CompletionTokens: 50,
				TotalTokens:      calculateMockTokens(messages) + 50,
			},
		}, nil
	}

	// Return responses in order, cycling back to the beginning
	responseIndex := (m.chatCallCount - 1) % len(m.chatResponses)
	response := m.chatResponses[responseIndex]

	// Add some dynamic content based on the input
	if strings.Contains(response.Content, "{USER_MESSAGE}") {
		userMessage := getLastUserMessage(messages)
		response.Content = strings.ReplaceAll(response.Content, "{USER_MESSAGE}", userMessage)
	}

	return &response, nil
}

// GenerateEmbedding returns a mock embedding based on the input text.
func (m *MockClient) GenerateEmbedding(ctx context.Context, text string) ([]float32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.embeddingCallCount++

	if m.shouldFailEmbedding {
		if m.embeddingError != nil {
			return nil, m.embeddingError
		}
		return nil, fmt.Errorf("mock embedding error")
	}

	// Check if we have a pre-configured embedding for this text
	if embedding, exists := m.embeddings[text]; exists {
		// Return a copy to avoid race conditions
		result := make([]float32, len(embedding))
		copy(result, embedding)
		return result, nil
	}

	// Generate a deterministic mock embedding based on text content
	embedding := generateMockEmbedding(text)

	// Store it for consistency
	m.embeddings[text] = embedding

	// Return a copy
	result := make([]float32, len(embedding))
	copy(result, embedding)
	return result, nil
}

// Test helper methods

// SetChatResponses configures the responses that Chat() will return.
func (m *MockClient) SetChatResponses(responses []Response) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.chatResponses = responses
}

// SetEmbedding configures a specific embedding for a given text.
func (m *MockClient) SetEmbedding(text string, embedding []float32) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Store a copy to avoid external modifications
	embeddingCopy := make([]float32, len(embedding))
	copy(embeddingCopy, embedding)
	m.embeddings[text] = embeddingCopy
}

// SetShouldFailChat configures the mock to fail chat requests.
func (m *MockClient) SetShouldFailChat(shouldFail bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.shouldFailChat = shouldFail
	m.chatError = err
}

// SetShouldFailEmbedding configures the mock to fail embedding requests.
func (m *MockClient) SetShouldFailEmbedding(shouldFail bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.shouldFailEmbedding = shouldFail
	m.embeddingError = err
}

// GetChatCallCount returns the number of times Chat() was called.
func (m *MockClient) GetChatCallCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.chatCallCount
}

// GetEmbeddingCallCount returns the number of times GenerateEmbedding() was called.
func (m *MockClient) GetEmbeddingCallCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.embeddingCallCount
}

// Reset clears all state from the mock client (useful for tests).
func (m *MockClient) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.chatResponses = []Response{}
	m.embeddings = make(map[string][]float32)
	m.chatCallCount = 0
	m.embeddingCallCount = 0
	m.shouldFailChat = false
	m.shouldFailEmbedding = false
	m.chatError = nil
	m.embeddingError = nil
}

// Helper functions

// getLastUserMessage extracts the last user message from a conversation.
func getLastUserMessage(messages []Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content
		}
	}
	return "No user message found"
}

// calculateMockTokens estimates token count for mock purposes.
func calculateMockTokens(messages []Message) int {
	totalChars := 0
	for _, msg := range messages {
		totalChars += len(msg.Content)
	}
	// Rough approximation: 4 characters per token
	return totalChars / 4
}

// generateMockEmbedding creates a deterministic embedding based on text content.
func generateMockEmbedding(text string) []float32 {
	// Create a 768-dimensional embedding (common size for many models)
	embedding := make([]float32, 768)

	// Use text characteristics to generate deterministic values
	textBytes := []byte(text)
	textLen := len(textBytes)

	for i := 0; i < 768; i++ {
		// Create a deterministic but varied value based on text content and position
		var value float32
		if textLen > 0 {
			byteIndex := i % textLen
			charValue := float32(textBytes[byteIndex])
			// Normalize to [-1, 1] range and add position-based variation
			value = (charValue/127.5 - 1.0) * (1.0 + float32(i)/1000.0)
		} else {
			value = float32(i) / 768.0 // Fallback for empty text
		}

		embedding[i] = value
	}

	return embedding
}

// CreateMockResponse creates a mock response for testing.
func CreateMockResponse(content string, promptTokens, completionTokens int) Response {
	return Response{
		Content: content,
		Usage: Usage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		},
	}
}

// CreateMockMessage creates a mock message for testing.
func CreateMockMessage(role, content string) Message {
	return Message{
		Role:    role,
		Content: content,
	}
}
