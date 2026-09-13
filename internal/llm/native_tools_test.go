package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/autonomousops-co/accretion/internal/config"
)

// toolConversation is a representative multi-turn tool-use exchange used to
// exercise the outbound (neutral -> provider) translation: a user question, an
// assistant tool call, and the tool result fed back in.
func toolConversation() ([]ToolMessage, []Tool) {
	messages := []ToolMessage{
		{Role: "user", Content: []ContentBlock{
			{Type: "text", Text: "how many pods are running?"},
		}},
		{Role: "assistant", Content: []ContentBlock{
			{Type: "tool_use", ID: "call_0", Name: "search_docs", Input: json.RawMessage(`{"query":"pods"}`)},
		}},
		{Role: "user", Content: []ContentBlock{
			{Type: "tool_result", ToolUseID: "call_0", Content: "42 pods"},
		}},
	}
	tools := []Tool{
		{
			Name:        "search_docs",
			Description: "Search the knowledge base",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`),
		},
	}
	return messages, tools
}

func TestOpenAIChatWithToolsToolCall(t *testing.T) {
	var reqBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices": [{
				"message": {
					"role": "assistant",
					"content": "",
					"tool_calls": [{
						"id": "call_abc",
						"type": "function",
						"function": {"name": "search_docs", "arguments": "{\"query\":\"pods\"}"}
					}]
				},
				"finish_reason": "tool_calls"
			}],
			"usage": {"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18}
		}`))
	}))
	defer server.Close()

	client, err := NewOpenAIClient(config.OpenAIConfig{APIKey: "test", BaseURL: server.URL}, "embed", "gpt-tool")
	if err != nil {
		t.Fatalf("NewOpenAIClient: %v", err)
	}

	messages, tools := toolConversation()
	resp, err := client.ChatWithTools(context.Background(), "You are helpful", messages, tools)
	if err != nil {
		t.Fatalf("ChatWithTools: %v", err)
	}

	// Inbound: tool_calls -> tool_use blocks with StopReason tool_use.
	if resp.StopReason != "tool_use" {
		t.Errorf("StopReason = %q, want tool_use", resp.StopReason)
	}
	calls := resp.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1", len(calls))
	}
	if calls[0].ID != "call_abc" {
		t.Errorf("tool_use ID = %q, want call_abc", calls[0].ID)
	}
	if calls[0].Name != "search_docs" {
		t.Errorf("tool_use Name = %q, want search_docs", calls[0].Name)
	}
	if string(calls[0].Input) != `{"query":"pods"}` {
		t.Errorf("tool_use Input = %q, want {\"query\":\"pods\"}", string(calls[0].Input))
	}
	if resp.Usage.TotalTokens != 18 {
		t.Errorf("Usage.TotalTokens = %d, want 18", resp.Usage.TotalTokens)
	}

	// Outbound: verify the request translation.
	var sent openAIToolRequest
	if err := json.Unmarshal(reqBody, &sent); err != nil {
		t.Fatalf("unmarshal sent request: %v", err)
	}
	if len(sent.Tools) != 1 || sent.Tools[0].Type != "function" || sent.Tools[0].Function.Name != "search_docs" {
		t.Fatalf("tools not translated: %+v", sent.Tools)
	}
	if sent.Messages[0].Role != "system" || sent.Messages[0].Content != "You are helpful" {
		t.Errorf("leading system message missing: %+v", sent.Messages[0])
	}

	var sawAssistantCall, sawToolResult bool
	for _, m := range sent.Messages {
		if m.Role == "assistant" && len(m.ToolCalls) == 1 {
			sawAssistantCall = true
			if m.ToolCalls[0].Function.Name != "search_docs" {
				t.Errorf("assistant tool_call name = %q", m.ToolCalls[0].Function.Name)
			}
			if m.ToolCalls[0].Function.Arguments != `{"query":"pods"}` {
				t.Errorf("assistant tool_call arguments = %q", m.ToolCalls[0].Function.Arguments)
			}
		}
		if m.Role == "tool" {
			sawToolResult = true
			if m.ToolCallID != "call_0" {
				t.Errorf("tool message tool_call_id = %q, want call_0", m.ToolCallID)
			}
			if m.Content != "42 pods" {
				t.Errorf("tool message content = %q, want 42 pods", m.Content)
			}
		}
	}
	if !sawAssistantCall {
		t.Error("expected an assistant message carrying tool_calls")
	}
	if !sawToolResult {
		t.Error("expected a role=tool message carrying the tool_result")
	}
}

func TestOpenAIChatWithToolsFinalAnswer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices": [{
				"message": {"role": "assistant", "content": "There are 42 pods."},
				"finish_reason": "stop"
			}],
			"usage": {"prompt_tokens": 5, "completion_tokens": 4, "total_tokens": 9}
		}`))
	}))
	defer server.Close()

	client, err := NewOpenAIClient(config.OpenAIConfig{APIKey: "test", BaseURL: server.URL}, "embed", "gpt-tool")
	if err != nil {
		t.Fatalf("NewOpenAIClient: %v", err)
	}

	messages, tools := toolConversation()
	resp, err := client.ChatWithTools(context.Background(), "You are helpful", messages, tools)
	if err != nil {
		t.Fatalf("ChatWithTools: %v", err)
	}

	if resp.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want end_turn", resp.StopReason)
	}
	if len(resp.ToolCalls()) != 0 {
		t.Errorf("expected no tool calls, got %d", len(resp.ToolCalls()))
	}
	if got := resp.TextContent(); got != "There are 42 pods." {
		t.Errorf("TextContent = %q, want There are 42 pods.", got)
	}
}

func TestOllamaChatWithToolsToolCall(t *testing.T) {
	var reqBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"message": {
				"role": "assistant",
				"content": "",
				"tool_calls": [{
					"function": {"name": "search_docs", "arguments": {"query": "pods"}}
				}]
			},
			"done": true,
			"prompt_eval_count": 12,
			"eval_count": 6
		}`))
	}))
	defer server.Close()

	client, err := NewOllamaClient(config.OllamaConfig{BaseURL: server.URL}, "embed", "llama-tool")
	if err != nil {
		t.Fatalf("NewOllamaClient: %v", err)
	}

	messages, tools := toolConversation()
	resp, err := client.ChatWithTools(context.Background(), "You are helpful", messages, tools)
	if err != nil {
		t.Fatalf("ChatWithTools: %v", err)
	}

	// Inbound: tool_calls -> tool_use blocks with StopReason tool_use.
	if resp.StopReason != "tool_use" {
		t.Errorf("StopReason = %q, want tool_use", resp.StopReason)
	}
	calls := resp.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1", len(calls))
	}
	if calls[0].ID != "call_0" {
		t.Errorf("synthesized tool_use ID = %q, want call_0", calls[0].ID)
	}
	if calls[0].Name != "search_docs" {
		t.Errorf("tool_use Name = %q, want search_docs", calls[0].Name)
	}
	if string(calls[0].Input) != `{"query": "pods"}` {
		t.Errorf("tool_use Input = %q, want {\"query\": \"pods\"}", string(calls[0].Input))
	}
	if resp.Usage.TotalTokens != 18 {
		t.Errorf("Usage.TotalTokens = %d, want 18", resp.Usage.TotalTokens)
	}

	// Outbound: verify the request translation.
	var sent ollamaToolChatRequest
	if err := json.Unmarshal(reqBody, &sent); err != nil {
		t.Fatalf("unmarshal sent request: %v", err)
	}
	if sent.Stream {
		t.Error("expected stream=false")
	}
	if len(sent.Tools) != 1 || sent.Tools[0].Type != "function" || sent.Tools[0].Function.Name != "search_docs" {
		t.Fatalf("tools not translated: %+v", sent.Tools)
	}
	if sent.Messages[0].Role != "system" || sent.Messages[0].Content != "You are helpful" {
		t.Errorf("leading system message missing: %+v", sent.Messages[0])
	}

	var sawAssistantCall, sawToolResult bool
	for _, m := range sent.Messages {
		if m.Role == "assistant" && len(m.ToolCalls) == 1 {
			sawAssistantCall = true
			if m.ToolCalls[0].Function.Name != "search_docs" {
				t.Errorf("assistant tool_call name = %q", m.ToolCalls[0].Function.Name)
			}
			// Ollama arguments are a JSON object, not a string.
			if string(m.ToolCalls[0].Function.Arguments) != `{"query":"pods"}` {
				t.Errorf("assistant tool_call arguments = %q", string(m.ToolCalls[0].Function.Arguments))
			}
		}
		if m.Role == "tool" {
			sawToolResult = true
			if m.Content != "42 pods" {
				t.Errorf("tool message content = %q, want 42 pods", m.Content)
			}
		}
	}
	if !sawAssistantCall {
		t.Error("expected an assistant message carrying tool_calls")
	}
	if !sawToolResult {
		t.Error("expected a role=tool message carrying the tool_result")
	}
}

func TestOllamaChatWithToolsFinalAnswer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"message": {"role": "assistant", "content": "There are 42 pods."},
			"done": true,
			"prompt_eval_count": 5,
			"eval_count": 4
		}`))
	}))
	defer server.Close()

	client, err := NewOllamaClient(config.OllamaConfig{BaseURL: server.URL}, "embed", "llama-tool")
	if err != nil {
		t.Fatalf("NewOllamaClient: %v", err)
	}

	messages, tools := toolConversation()
	resp, err := client.ChatWithTools(context.Background(), "You are helpful", messages, tools)
	if err != nil {
		t.Fatalf("ChatWithTools: %v", err)
	}

	if resp.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want end_turn", resp.StopReason)
	}
	if len(resp.ToolCalls()) != 0 {
		t.Errorf("expected no tool calls, got %d", len(resp.ToolCalls()))
	}
	if got := resp.TextContent(); got != "There are 42 pods." {
		t.Errorf("TextContent = %q, want There are 42 pods.", got)
	}
}

// TestNativeClientsSatisfyToolUser is a runtime companion to the compile-time
// assertions in openai.go and ollama.go.
func TestNativeClientsSatisfyToolUser(t *testing.T) {
	openAI, err := NewOpenAIClient(config.OpenAIConfig{APIKey: "test", BaseURL: "http://example.invalid"}, "embed", "chat")
	if err != nil {
		t.Fatalf("NewOpenAIClient: %v", err)
	}
	ollama, err := NewOllamaClient(config.OllamaConfig{BaseURL: "http://example.invalid"}, "embed", "chat")
	if err != nil {
		t.Fatalf("NewOllamaClient: %v", err)
	}
	if _, ok := interface{}(openAI).(ToolUser); !ok {
		t.Error("OpenAIClient does not satisfy ToolUser")
	}
	if _, ok := interface{}(ollama).(ToolUser); !ok {
		t.Error("OllamaClient does not satisfy ToolUser")
	}
}
