package llm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// scriptedClient is a Chat-only mock Client that returns queued responses in
// order and records the messages it was last asked to complete. The final
// response is reused once the queue is exhausted.
type scriptedClient struct {
	responses    []string
	calls        int
	lastMessages []Message
}

func (s *scriptedClient) Chat(_ context.Context, messages []Message) (*Response, error) {
	s.lastMessages = messages
	idx := s.calls
	if idx >= len(s.responses) {
		idx = len(s.responses) - 1
	}
	s.calls++
	return &Response{Content: s.responses[idx], Usage: Usage{TotalTokens: 1}}, nil
}

func (s *scriptedClient) GenerateEmbedding(_ context.Context, _ string) ([]float32, error) {
	return nil, nil
}

func searchTool() Tool {
	return Tool{
		Name:        "search",
		Description: "search the knowledge base",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`),
	}
}

func userMessage(text string) ToolMessage {
	return ToolMessage{Role: "user", Content: []ContentBlock{{Type: "text", Text: text}}}
}

func TestReactToolUser_JSONActionParsesAsToolCall(t *testing.T) {
	client := &scriptedClient{responses: []string{`{"tool": "search", "args": {"query": "golang"}}`}}
	r := newReactToolUser(client)

	resp, err := r.ChatWithTools(
		context.Background(),
		"be a helpful assistant",
		[]ToolMessage{userMessage("find golang docs")},
		[]Tool{searchTool()},
	)
	if err != nil {
		t.Fatalf("ChatWithTools returned error: %v", err)
	}

	if resp.StopReason != "tool_use" {
		t.Fatalf("StopReason = %q, want %q", resp.StopReason, "tool_use")
	}

	calls := resp.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1", len(calls))
	}
	if calls[0].Name != "search" {
		t.Errorf("tool name = %q, want %q", calls[0].Name, "search")
	}
	if calls[0].ID == "" {
		t.Error("tool_use block is missing an ID")
	}
	if !strings.Contains(string(calls[0].Input), "golang") {
		t.Errorf("tool input = %q, want it to contain query", string(calls[0].Input))
	}

	// The tool schema and response contract must be injected into the system prompt.
	if len(client.lastMessages) == 0 || client.lastMessages[0].Role != "system" {
		t.Fatalf("expected a leading system message, got %+v", client.lastMessages)
	}
	sys := client.lastMessages[0].Content
	if !strings.Contains(sys, "search") {
		t.Error("system prompt does not mention the tool name")
	}
	if !strings.Contains(sys, "EXACTLY ONE JSON") {
		t.Error("system prompt is missing the strict action instruction")
	}
	if !strings.Contains(sys, "be a helpful assistant") {
		t.Error("system prompt dropped the caller-provided system text")
	}
}

func TestReactToolUser_PlainAnswerParsesAsFinal(t *testing.T) {
	client := &scriptedClient{responses: []string{"The answer is 42."}}
	r := newReactToolUser(client)

	resp, err := r.ChatWithTools(
		context.Background(),
		"",
		[]ToolMessage{userMessage("what is the answer?")},
		[]Tool{searchTool()},
	)
	if err != nil {
		t.Fatalf("ChatWithTools returned error: %v", err)
	}

	if resp.StopReason != "end_turn" {
		t.Fatalf("StopReason = %q, want %q", resp.StopReason, "end_turn")
	}
	if len(resp.ToolCalls()) != 0 {
		t.Errorf("expected no tool calls, got %d", len(resp.ToolCalls()))
	}
	if got := resp.TextContent(); got != "The answer is 42." {
		t.Errorf("TextContent = %q, want %q", got, "The answer is 42.")
	}
}

func TestReactToolUser_FencedJSONWithProseParsesAsToolCall(t *testing.T) {
	reply := "Sure, let me look that up for you.\n\n```json\n{\"tool\": \"search\", \"args\": {\"query\": \"go\"}}\n```"
	client := &scriptedClient{responses: []string{reply}}
	r := newReactToolUser(client)

	resp, err := r.ChatWithTools(
		context.Background(),
		"",
		[]ToolMessage{userMessage("look it up")},
		[]Tool{searchTool()},
	)
	if err != nil {
		t.Fatalf("ChatWithTools returned error: %v", err)
	}

	if resp.StopReason != "tool_use" {
		t.Fatalf("StopReason = %q, want %q", resp.StopReason, "tool_use")
	}
	calls := resp.ToolCalls()
	if len(calls) != 1 || calls[0].Name != "search" {
		t.Fatalf("expected one search tool call, got %+v", calls)
	}
}

func TestReactToolUser_MalformedJSONFallsBackToFinal(t *testing.T) {
	// An unterminated JSON object must not error; it is treated as a final answer.
	reply := `{"tool": "search", "args": {"query":`
	client := &scriptedClient{responses: []string{reply}}
	r := newReactToolUser(client)

	resp, err := r.ChatWithTools(
		context.Background(),
		"",
		[]ToolMessage{userMessage("hi")},
		[]Tool{searchTool()},
	)
	if err != nil {
		t.Fatalf("ChatWithTools returned error: %v", err)
	}

	if resp.StopReason != "end_turn" {
		t.Fatalf("StopReason = %q, want %q", resp.StopReason, "end_turn")
	}
	if got := resp.TextContent(); got != reply {
		t.Errorf("TextContent = %q, want the raw reply %q", got, reply)
	}
}

// TestReactToolUser_DrivesToolRoundThenFinal is the acceptance scenario: given a
// Chat-only mock, the adapter drives one tool round and then a final answer,
// with the intervening tool_result rendered back into the conversation.
func TestReactToolUser_DrivesToolRoundThenFinal(t *testing.T) {
	client := &scriptedClient{responses: []string{
		`{"tool": "search", "args": {"query": "golang origin"}}`,
		"Go was created at Google.",
	}}
	r := newReactToolUser(client)
	tools := []Tool{searchTool()}

	messages := []ToolMessage{userMessage("who created go?")}

	// Round 1: expect a tool call.
	round1, err := r.ChatWithTools(context.Background(), "", messages, tools)
	if err != nil {
		t.Fatalf("round 1 error: %v", err)
	}
	if !round1.NeedsToolUse() {
		t.Fatalf("round 1 StopReason = %q, want tool_use", round1.StopReason)
	}
	call := round1.ToolCalls()[0]

	// Simulate the agent loop: append the assistant action and the tool result.
	const observation = "Go is a programming language designed at Google."
	messages = append(messages,
		ToolMessage{Role: "assistant", Content: round1.Content},
		ToolMessage{Role: "user", Content: []ContentBlock{{
			Type:      "tool_result",
			ToolUseID: call.ID,
			Content:   observation,
		}}},
	)

	// Round 2: expect a final answer.
	round2, err := r.ChatWithTools(context.Background(), "", messages, tools)
	if err != nil {
		t.Fatalf("round 2 error: %v", err)
	}
	if round2.StopReason != "end_turn" {
		t.Fatalf("round 2 StopReason = %q, want end_turn", round2.StopReason)
	}
	if got := round2.TextContent(); got != "Go was created at Google." {
		t.Errorf("final answer = %q, want %q", got, "Go was created at Google.")
	}

	// The tool result must have been rendered into the conversation sent to Chat.
	var joined strings.Builder
	for _, m := range client.lastMessages {
		joined.WriteString(m.Content)
		joined.WriteString("\n")
	}
	if !strings.Contains(joined.String(), observation) {
		t.Errorf("tool result was not rendered into the conversation: %q", joined.String())
	}
}
