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

type OpenAIClient struct {
	httpClient     *http.Client
	embeddingModel string
	chatModel      string
	config         config.OpenAIConfig
}

type openAIRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float32   `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
}

type openAIResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
		Index        int    `json:"index"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Created int64 `json:"created"`
}

type embeddingRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type embeddingResponse struct {
	Object string `json:"object"`
	Model  string `json:"model"`
	Data   []struct {
		Object    string    `json:"object"`
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

func NewOpenAIClient(cfg config.OpenAIConfig, embeddingModel, chatModel string) (*OpenAIClient, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("OpenAI API key is required")
	}

	return &OpenAIClient{
		config:         cfg,
		embeddingModel: embeddingModel,
		chatModel:      chatModel,
		httpClient:     &http.Client{},
	}, nil
}

func (c *OpenAIClient) Chat(ctx context.Context, messages []Message) (*Response, error) {
	req := openAIRequest{
		Model:       c.chatModel,
		Messages:    messages,
		Temperature: 0.2,
		MaxTokens:   2000,
	}

	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.config.BaseURL+"/chat/completions", bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.config.APIKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OpenAI API returned status %d", resp.StatusCode)
	}

	var openAIResp openAIResponse
	if err := json.NewDecoder(resp.Body).Decode(&openAIResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if len(openAIResp.Choices) == 0 {
		return nil, fmt.Errorf("no choices returned from OpenAI")
	}

	return &Response{
		Content: openAIResp.Choices[0].Message.Content,
		Usage: Usage{
			PromptTokens:     openAIResp.Usage.PromptTokens,
			CompletionTokens: openAIResp.Usage.CompletionTokens,
			TotalTokens:      openAIResp.Usage.TotalTokens,
		},
	}, nil
}

func (c *OpenAIClient) GenerateEmbedding(ctx context.Context, text string) ([]float32, error) {
	req := embeddingRequest{
		Model: c.embeddingModel,
		Input: text,
	}

	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.config.BaseURL+"/embeddings", bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.config.APIKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OpenAI API returned status %d", resp.StatusCode)
	}

	var embResp embeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&embResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if len(embResp.Data) == 0 {
		return nil, fmt.Errorf("no embeddings returned from OpenAI")
	}

	return embResp.Data[0].Embedding, nil
}

// openAIToolRequest carries tool definitions and tool-aware messages for the
// chat completions endpoint.
type openAIToolRequest struct {
	Model       string                 `json:"model"`
	Messages    []openAIToolMessage    `json:"messages"`
	Tools       []openAIToolDefinition `json:"tools,omitempty"`
	Temperature float32                `json:"temperature,omitempty"`
	MaxTokens   int                    `json:"max_tokens,omitempty"`
}

// openAIToolMessage is a chat message that can carry assistant tool_calls or a
// tool result, in addition to plain text content.
type openAIToolMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

// openAIToolCall mirrors a single entry in an assistant message's tool_calls.
type openAIToolCall struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	Function openAIToolCallFunction `json:"function"`
}

// openAIToolCallFunction holds the invoked function name and its arguments.
// OpenAI encodes arguments as a JSON string rather than a JSON object.
type openAIToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// openAIToolDefinition describes a tool exposed to the model.
type openAIToolDefinition struct {
	Type     string               `json:"type"`
	Function openAIFunctionSchema `json:"function"`
}

// openAIFunctionSchema is the function schema nested inside a tool definition.
type openAIFunctionSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// openAIToolResponse decodes a chat completion that may contain tool_calls.
type openAIToolResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Role      string           `json:"role"`
			Content   string           `json:"content"`
			ToolCalls []openAIToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
		Index        int    `json:"index"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// ChatWithTools implements ToolUser using OpenAI's native function calling.
func (c *OpenAIClient) ChatWithTools(
	ctx context.Context,
	system string,
	messages []ToolMessage,
	tools []Tool,
) (*ToolResponse, error) {
	req := openAIToolRequest{
		Model:       c.chatModel,
		Messages:    openAIToolMessages(system, messages),
		Tools:       openAIToolDefinitions(tools),
		Temperature: 0.2,
		MaxTokens:   4096,
	}

	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(
		ctx, "POST", c.config.BaseURL+"/chat/completions", bytes.NewBuffer(reqBody),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.config.APIKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("OpenAI API returned status %d: %s", resp.StatusCode, string(body))
	}

	var openAIResp openAIToolResponse
	if err := json.NewDecoder(resp.Body).Decode(&openAIResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if len(openAIResp.Choices) == 0 {
		return nil, fmt.Errorf("no choices returned from OpenAI")
	}

	choice := openAIResp.Choices[0]
	content, stopReason := openAIChoiceToContent(
		choice.Message.Content, choice.Message.ToolCalls, choice.FinishReason,
	)

	return &ToolResponse{
		StopReason: stopReason,
		Content:    content,
		Usage: Usage{
			PromptTokens:     openAIResp.Usage.PromptTokens,
			CompletionTokens: openAIResp.Usage.CompletionTokens,
			TotalTokens:      openAIResp.Usage.TotalTokens,
		},
	}, nil
}

// openAIToolDefinitions translates neutral tool specs into OpenAI tool definitions.
func openAIToolDefinitions(tools []Tool) []openAIToolDefinition {
	if len(tools) == 0 {
		return nil
	}
	defs := make([]openAIToolDefinition, 0, len(tools))
	for _, t := range tools {
		defs = append(defs, openAIToolDefinition{
			Type: "function",
			Function: openAIFunctionSchema{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}
	return defs
}

// openAIToolMessages flattens the neutral tool conversation into OpenAI chat
// messages: the system prompt becomes a leading system message, tool_result
// blocks expand into standalone {role:"tool"} messages, and tool_use blocks
// become assistant tool_calls.
func openAIToolMessages(system string, messages []ToolMessage) []openAIToolMessage {
	out := make([]openAIToolMessage, 0, len(messages)+1)
	if system != "" {
		out = append(out, openAIToolMessage{Role: RoleSystem, Content: system})
	}

	for _, m := range messages {
		var text strings.Builder
		var toolCalls []openAIToolCall

		for i := range m.Content {
			block := &m.Content[i]
			switch block.Type {
			case BlockText:
				text.WriteString(block.Text)
			case BlockToolUse:
				arguments := string(block.Input)
				if arguments == "" {
					arguments = "{}"
				}
				toolCalls = append(toolCalls, openAIToolCall{
					ID:   block.ID,
					Type: "function",
					Function: openAIToolCallFunction{
						Name:      block.Name,
						Arguments: arguments,
					},
				})
			case BlockToolResult:
				out = append(out, openAIToolMessage{
					Role:       "tool",
					ToolCallID: block.ToolUseID,
					Content:    block.Content,
				})
			}
		}

		if text.Len() == 0 && len(toolCalls) == 0 {
			continue
		}
		out = append(out, openAIToolMessage{
			Role:      m.Role,
			Content:   text.String(),
			ToolCalls: toolCalls,
		})
	}

	return out
}

// openAIChoiceToContent maps an OpenAI choice into neutral content blocks and a
// stop reason. OpenAI's tool-call arguments arrive as a JSON string, which is
// already valid JSON for ContentBlock.Input.
func openAIChoiceToContent(
	text string, toolCalls []openAIToolCall, finishReason string,
) ([]ContentBlock, string) {
	if finishReason == "tool_calls" || len(toolCalls) > 0 {
		var content []ContentBlock
		if text != "" {
			content = append(content, ContentBlock{Type: BlockText, Text: text})
		}
		for _, tc := range toolCalls {
			content = append(content, ContentBlock{
				Type:  BlockToolUse,
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: json.RawMessage(tc.Function.Arguments),
			})
		}
		return content, StopToolUse
	}
	return []ContentBlock{{Type: BlockText, Text: text}}, StopEndTurn
}

// Compile-time check that OpenAIClient implements ToolUser.
var _ ToolUser = (*OpenAIClient)(nil)
