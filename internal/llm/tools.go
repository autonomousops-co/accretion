package llm

import (
	"context"
	"encoding/json"
	"strings"
)

// Tool defines a tool that can be provided to the LLM for agentic use.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// The content-block and stop-reason vocabulary of the tool-use wire protocol.
// Anthropic defines these strings; the Ollama and OpenAI clients translate their
// own provider's shapes into the same set, so the ReAct loop and the Discord bot
// can read one format regardless of which provider answered.
const (
	BlockText       = "text"
	BlockToolUse    = "tool_use"
	BlockToolResult = "tool_result"

	RoleSystem = "system"

	StopEndTurn = "end_turn"
	StopToolUse = "tool_use"
)

// ContentBlock represents a single block in a message's content array.
// It can be text, tool_use, or tool_result.
type ContentBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Text      string          `json:"text,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// ToolMessage is a message that uses content blocks instead of a plain string.
// This is needed for multi-turn tool-use conversations.
type ToolMessage struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

// ToolResponse is the response from a ChatWithTools call.
type ToolResponse struct {
	// StopReason is "end_turn" for final answers, "tool_use" when tools are requested.
	StopReason string
	Content    []ContentBlock
	Usage      Usage
}

// ToolCalls returns only the tool_use blocks from the response.
func (r *ToolResponse) ToolCalls() []ContentBlock {
	var calls []ContentBlock
	for i := range r.Content {
		if r.Content[i].Type == BlockToolUse {
			calls = append(calls, r.Content[i])
		}
	}
	return calls
}

// TextContent returns the concatenated text from all text blocks.
func (r *ToolResponse) TextContent() string {
	var sb strings.Builder
	for i := range r.Content {
		if r.Content[i].Type == BlockText {
			sb.WriteString(r.Content[i].Text)
		}
	}
	return sb.String()
}

// NeedsToolUse returns true if the model wants to call tools.
func (r *ToolResponse) NeedsToolUse() bool {
	return r.StopReason == BlockToolUse
}

// ToolUser is an optional interface that LLM clients can implement
// to support agentic tool-use conversations.
type ToolUser interface {
	ChatWithTools(
		ctx context.Context,
		system string,
		messages []ToolMessage,
		tools []Tool,
	) (*ToolResponse, error)
}
