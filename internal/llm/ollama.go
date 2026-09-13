package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/autonomousops-co/accretion/internal/config"
)

type OllamaClient struct {
	httpClient     *http.Client
	embeddingModel string
	chatModel      string
	config         config.OllamaConfig
}

type ollamaChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream"`
	Temperature float32   `json:"temperature,omitempty"`
}

type ollamaChatResponse struct {
	Message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
	Done bool `json:"done"`
}

type ollamaEmbeddingRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type ollamaEmbeddingResponse struct {
	Embedding []float32 `json:"embedding"`
}

func NewOllamaClient(cfg config.OllamaConfig, embeddingModel, chatModel string) (*OllamaClient, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("Ollama base URL is required")
	}

	return &OllamaClient{
		config:         cfg,
		embeddingModel: embeddingModel,
		chatModel:      chatModel,
		httpClient:     &http.Client{},
	}, nil
}

func (c *OllamaClient) Chat(ctx context.Context, messages []Message) (*Response, error) {
	req := ollamaChatRequest{
		Model:       c.chatModel,
		Messages:    messages,
		Stream:      false,
		Temperature: 0.2,
	}

	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.config.BaseURL+"/api/chat", bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Ollama API returned status %d", resp.StatusCode)
	}

	var ollamaResp ollamaChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&ollamaResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &Response{
		Content: ollamaResp.Message.Content,
	}, nil
}

func (c *OllamaClient) GenerateEmbedding(ctx context.Context, text string) ([]float32, error) {
	req := ollamaEmbeddingRequest{
		Model:  c.embeddingModel,
		Prompt: text,
	}

	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.config.BaseURL+"/api/embeddings", bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Ollama API returned status %d", resp.StatusCode)
	}

	var embResp ollamaEmbeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&embResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return embResp.Embedding, nil
}

// ollamaToolChatRequest carries tool definitions and tool-aware messages for
// the /api/chat endpoint.
type ollamaToolChatRequest struct {
	Model       string                 `json:"model"`
	Messages    []ollamaToolMessage    `json:"messages"`
	Tools       []ollamaToolDefinition `json:"tools,omitempty"`
	Stream      bool                   `json:"stream"`
	Temperature float32                `json:"temperature,omitempty"`
}

// ollamaToolMessage is a chat message that can carry assistant tool_calls or a
// tool result, in addition to plain text content.
type ollamaToolMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
}

// ollamaToolCall mirrors a single tool call. Unlike OpenAI, Ollama encodes
// arguments as a JSON object and does not assign call IDs.
type ollamaToolCall struct {
	Function ollamaToolCallFunction `json:"function"`
}

// ollamaToolCallFunction holds the invoked function name and its JSON-object args.
type ollamaToolCallFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ollamaToolDefinition describes a tool exposed to the model.
type ollamaToolDefinition struct {
	Type     string               `json:"type"`
	Function ollamaFunctionSchema `json:"function"`
}

// ollamaFunctionSchema is the function schema nested inside a tool definition.
type ollamaFunctionSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ollamaToolChatResponse decodes an /api/chat response that may include tool_calls.
type ollamaToolChatResponse struct {
	Message struct {
		Role      string           `json:"role"`
		Content   string           `json:"content"`
		ToolCalls []ollamaToolCall `json:"tool_calls"`
	} `json:"message"`
	Done            bool `json:"done"`
	PromptEvalCount int  `json:"prompt_eval_count"`
	EvalCount       int  `json:"eval_count"`
}

// ChatWithTools implements ToolUser using Ollama's native function calling.
func (c *OllamaClient) ChatWithTools(
	ctx context.Context,
	system string,
	messages []ToolMessage,
	tools []Tool,
) (*ToolResponse, error) {
	req := ollamaToolChatRequest{
		Model:       c.chatModel,
		Messages:    ollamaToolMessages(system, messages),
		Tools:       ollamaToolDefinitions(tools),
		Stream:      false,
		Temperature: 0.2,
	}

	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(
		ctx, "POST", c.config.BaseURL+"/api/chat", bytes.NewBuffer(reqBody),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Ollama API returned status %d: %s", resp.StatusCode, string(body))
	}

	var ollamaResp ollamaToolChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&ollamaResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	content, stopReason := ollamaMessageToContent(
		ollamaResp.Message.Content, ollamaResp.Message.ToolCalls,
	)

	return &ToolResponse{
		StopReason: stopReason,
		Content:    content,
		Usage: Usage{
			PromptTokens:     ollamaResp.PromptEvalCount,
			CompletionTokens: ollamaResp.EvalCount,
			TotalTokens:      ollamaResp.PromptEvalCount + ollamaResp.EvalCount,
		},
	}, nil
}

// ollamaToolDefinitions translates neutral tool specs into Ollama tool definitions.
func ollamaToolDefinitions(tools []Tool) []ollamaToolDefinition {
	if len(tools) == 0 {
		return nil
	}
	defs := make([]ollamaToolDefinition, 0, len(tools))
	for _, t := range tools {
		defs = append(defs, ollamaToolDefinition{
			Type: "function",
			Function: ollamaFunctionSchema{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}
	return defs
}

// ollamaToolMessages flattens the neutral tool conversation into Ollama chat
// messages: the system prompt becomes a leading system message, tool_result
// blocks expand into standalone {role:"tool"} messages, and tool_use blocks
// become assistant tool_calls.
func ollamaToolMessages(system string, messages []ToolMessage) []ollamaToolMessage {
	out := make([]ollamaToolMessage, 0, len(messages)+1)
	if system != "" {
		out = append(out, ollamaToolMessage{Role: RoleSystem, Content: system})
	}

	for _, m := range messages {
		var text strings.Builder
		var toolCalls []ollamaToolCall

		for i := range m.Content {
			block := &m.Content[i]
			switch block.Type {
			case BlockText:
				text.WriteString(block.Text)
			case BlockToolUse:
				arguments := block.Input
				if len(arguments) == 0 {
					arguments = json.RawMessage("{}")
				}
				toolCalls = append(toolCalls, ollamaToolCall{
					Function: ollamaToolCallFunction{
						Name:      block.Name,
						Arguments: arguments,
					},
				})
			case BlockToolResult:
				out = append(out, ollamaToolMessage{
					Role:    "tool",
					Content: block.Content,
				})
			}
		}

		if text.Len() == 0 && len(toolCalls) == 0 {
			continue
		}
		out = append(out, ollamaToolMessage{
			Role:      m.Role,
			Content:   text.String(),
			ToolCalls: toolCalls,
		})
	}

	return out
}

// ollamaMessageToContent maps an Ollama chat message into neutral content blocks
// and a stop reason. Ollama's tool-call arguments arrive as a JSON object, which
// maps directly onto ContentBlock.Input. Ollama does not return tool-call IDs,
// so a stable index-based ID is synthesized to keep the tool_use/tool_result
// correlation well-formed for the agent loop.
func ollamaMessageToContent(text string, toolCalls []ollamaToolCall) ([]ContentBlock, string) {
	if len(toolCalls) > 0 {
		var content []ContentBlock
		if text != "" {
			content = append(content, ContentBlock{Type: BlockText, Text: text})
		}
		for i, tc := range toolCalls {
			content = append(content, ContentBlock{
				Type:  BlockToolUse,
				ID:    fmt.Sprintf("call_%d", i),
				Name:  tc.Function.Name,
				Input: tc.Function.Arguments,
			})
		}
		return content, StopToolUse
	}
	return []ContentBlock{{Type: BlockText, Text: text}}, StopEndTurn
}

// Compile-time check that OllamaClient implements ToolUser.
var _ ToolUser = (*OllamaClient)(nil)
