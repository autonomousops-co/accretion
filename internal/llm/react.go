package llm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// reactToolUser adapts a plain Chat-only Client into a ToolUser using a
// prompt-based ReAct protocol. It is the portable floor beneath the native
// tool-use adapters: it lets models without native function-calling participate
// in agentic tool-use loops. Tool schemas and a strict response contract are
// injected into the system prompt, prior tool results are rendered as text, and
// the model's free-form reply is parsed back into structured content blocks.
type reactToolUser struct {
	client Client
}

// newReactToolUser wraps the given Client so it satisfies the ToolUser interface
// via prompt-based ReAct. The wrapped client only needs to implement Chat.
func newReactToolUser(c Client) *reactToolUser {
	return &reactToolUser{client: c}
}

// reactInstructions is the strict response contract appended to the system
// prompt. It asks the model to emit a single JSON action to call a tool, or
// plain text to answer.
const reactInstructions = `To use a tool, reply with EXACTLY ONE JSON action and nothing else, in this exact form:
{"tool": "<tool_name>", "args": { ... }}

The "args" object must satisfy the chosen tool's input schema. Do not add any prose,
explanation, or code fences around the JSON when calling a tool.

When you have enough information to respond, reply with your final answer as plain text only (no JSON, no tool call).`

// reactAction is the JSON shape the model is asked to emit to invoke a tool.
type reactAction struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
}

// ChatWithTools implements ToolUser over a Chat-only client. It injects the tool
// schemas and response contract into the system prompt, renders the prior
// conversation (including tool results) as plain text, calls the wrapped
// client's Chat, and parses the reply into tool_use blocks (StopReason
// "tool_use") or a final text block (StopReason "end_turn").
func (r *reactToolUser) ChatWithTools(
	ctx context.Context,
	system string,
	messages []ToolMessage,
	tools []Tool,
) (*ToolResponse, error) {
	prompt := buildReactSystemPrompt(system, tools)

	convo := make([]Message, 0, len(messages)+1)
	if prompt != "" {
		convo = append(convo, Message{Role: RoleSystem, Content: prompt})
	}
	convo = append(convo, renderMessages(messages)...)

	resp, err := r.client.Chat(ctx, convo)
	if err != nil {
		return nil, err
	}

	content := parseReactReply(resp.Content)
	stopReason := StopEndTurn
	for i := range content {
		if content[i].Type == BlockToolUse {
			stopReason = StopToolUse
			break
		}
	}

	return &ToolResponse{
		StopReason: stopReason,
		Content:    content,
		Usage:      resp.Usage,
	}, nil
}

// buildReactSystemPrompt composes the original system prompt, the available tool
// schemas, and the strict ReAct response contract into a single system message.
func buildReactSystemPrompt(system string, tools []Tool) string {
	var sb strings.Builder

	if s := strings.TrimSpace(system); s != "" {
		sb.WriteString(s)
		sb.WriteString("\n\n")
	}

	if len(tools) > 0 {
		sb.WriteString("You have access to the following tools:\n\n")
		for i := range tools {
			t := tools[i]
			sb.WriteString(fmt.Sprintf("- %s: %s\n", t.Name, t.Description))
			if len(t.InputSchema) > 0 {
				sb.WriteString(fmt.Sprintf("  input schema: %s\n", string(t.InputSchema)))
			}
		}
		sb.WriteString("\n")
	}

	sb.WriteString(reactInstructions)
	return sb.String()
}

// renderMessages flattens tool-use content blocks into plain chat messages so a
// Chat-only client can consume the conversation. Empty messages are skipped.
func renderMessages(messages []ToolMessage) []Message {
	out := make([]Message, 0, len(messages))
	for i := range messages {
		text := renderBlocks(messages[i].Content)
		if strings.TrimSpace(text) == "" {
			continue
		}
		role := messages[i].Role
		if role == "" {
			role = "user"
		}
		out = append(out, Message{Role: role, Content: text})
	}
	return out
}

// renderBlocks turns a slice of content blocks into a single text string.
func renderBlocks(blocks []ContentBlock) string {
	parts := make([]string, 0, len(blocks))
	for i := range blocks {
		b := &blocks[i]
		switch b.Type {
		case BlockToolUse:
			parts = append(parts, renderToolUse(b))
		case BlockToolResult:
			parts = append(parts, renderToolResult(b))
		default:
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// renderToolUse renders a prior assistant tool call back into the JSON action
// form the model was asked to produce, keeping the transcript self-consistent.
func renderToolUse(b *ContentBlock) string {
	args := strings.TrimSpace(string(b.Input))
	if args == "" {
		args = "{}"
	}
	return fmt.Sprintf(`{"tool": %q, "args": %s}`, b.Name, args)
}

// renderToolResult renders a tool_result block as a labeled observation that
// the model can read as ordinary text.
func renderToolResult(b *ContentBlock) string {
	label := "Tool result"
	if b.IsError {
		label = "Tool error"
	}
	if b.ToolUseID != "" {
		return fmt.Sprintf("%s for %s:\n%s", label, b.ToolUseID, b.Content)
	}
	return fmt.Sprintf("%s:\n%s", label, b.Content)
}

// parseReactReply interprets the model's free-form reply. It tolerates fenced
// JSON and surrounding prose; a valid tool action becomes a tool_use block,
// while anything else (including malformed JSON) is returned as a final answer.
func parseReactReply(reply string) []ContentBlock {
	trimmed := strings.TrimSpace(reply)

	if candidate := extractJSONCandidate(trimmed); candidate != "" {
		if block, ok := parseAction(candidate); ok {
			return []ContentBlock{block}
		}
	}

	return []ContentBlock{{Type: BlockText, Text: trimmed}}
}

// parseAction attempts to decode a JSON action into a tool_use content block.
// It reports false when the candidate is not a valid action (e.g. no tool name).
func parseAction(candidate string) (ContentBlock, bool) {
	var action reactAction
	if err := json.Unmarshal([]byte(candidate), &action); err != nil {
		return ContentBlock{}, false
	}
	if strings.TrimSpace(action.Tool) == "" {
		return ContentBlock{}, false
	}

	input := json.RawMessage(strings.TrimSpace(string(action.Args)))
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}

	return ContentBlock{
		Type:  BlockToolUse,
		ID:    newToolUseID(),
		Name:  action.Tool,
		Input: input,
	}, true
}

// extractJSONCandidate pulls the most likely JSON object out of a reply,
// preferring the contents of a fenced code block when one is present.
func extractJSONCandidate(s string) string {
	if fenced, ok := extractFencedBlock(s); ok {
		if obj := extractJSONObject(fenced); obj != "" {
			return obj
		}
	}
	return extractJSONObject(s)
}

// extractFencedBlock returns the inner contents of the first ``` fenced code
// block, dropping an optional language tag (such as "json") on the opening line.
func extractFencedBlock(s string) (string, bool) {
	const fence = "```"
	start := strings.Index(s, fence)
	if start < 0 {
		return "", false
	}
	rest := s[start+len(fence):]
	end := strings.Index(rest, fence)
	if end < 0 {
		return "", false
	}
	inner := rest[:end]

	// Drop an optional language hint on the opening fence line.
	if nl := strings.IndexByte(inner, '\n'); nl >= 0 {
		firstLine := strings.TrimSpace(inner[:nl])
		if firstLine != "" && !strings.ContainsAny(firstLine, "{}") {
			inner = inner[nl+1:]
		}
	}
	return strings.TrimSpace(inner), true
}

// extractJSONObject returns the first balanced {...} object found in s, honoring
// quoted strings and escapes so braces inside strings do not confuse matching.
// It returns an empty string when no balanced object is present.
func extractJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}

	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// newToolUseID returns a unique identifier for a synthesized tool_use block so
// callers can correlate it with the tool_result they feed back.
func newToolUseID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "toolu_react"
	}
	return "toolu_react_" + hex.EncodeToString(buf[:])
}

// Compile-time check that reactToolUser implements ToolUser.
var _ ToolUser = (*reactToolUser)(nil)
