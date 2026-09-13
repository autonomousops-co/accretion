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

type AnthropicClient struct {
	httpClient      *http.Client
	embeddingClient Client
	chatModel       string
	config          config.AnthropicConfig
}

// anthropicThinking configures extended thinking. Sonnet 5+ enables adaptive
// thinking by default, which emits thinking blocks that must be replayed
// verbatim across multi-turn tool-use. This minimal client doesn't round-trip
// those blocks, so we disable thinking outright to avoid a 400 on replay.
type anthropicThinking struct {
	Type string `json:"type"`
}

// disabledThinking is the request value that turns adaptive thinking off.
var disabledThinking = &anthropicThinking{Type: "disabled"}

type anthropicRequest struct {
	Thinking    *anthropicThinking `json:"thinking,omitempty"`
	Model       string             `json:"model"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	MaxTokens   int                `json:"max_tokens"`
	Temperature float32            `json:"temperature,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type anthropicResponse struct {
	ID         string                  `json:"id"`
	Type       string                  `json:"type"`
	Role       string                  `json:"role"`
	Model      string                  `json:"model"`
	StopReason string                  `json:"stop_reason"`
	Content    []anthropicContentBlock `json:"content"`
	Usage      anthropicUsage          `json:"usage"`
}

type anthropicErrorResponse struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

//nolint:lll // function signature
func NewAnthropicClient(
	cfg config.AnthropicConfig, ollamaCfg config.OllamaConfig, embeddingModel, chatModel string,
) (*AnthropicClient, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("Anthropic API key is required")
	}

	// Anthropic doesn't provide embeddings, so delegate to the Ollama client.
	embeddingClient, err := NewOllamaClient(ollamaCfg, embeddingModel, "")
	if err != nil {
		return nil, fmt.Errorf("failed to create embedding client for Anthropic provider: %w", err)
	}

	return &AnthropicClient{
		config:          cfg,
		chatModel:       chatModel,
		httpClient:      &http.Client{},
		embeddingClient: embeddingClient,
	}, nil
}

func (c *AnthropicClient) Chat(ctx context.Context, messages []Message) (*Response, error) {
	// Anthropic uses a separate "system" parameter rather than a system message.
	var systemPrompt string
	anthropicMsgs := make([]anthropicMessage, 0, len(messages))

	for _, m := range messages {
		if m.Role == RoleSystem {
			systemPrompt = m.Content
			continue
		}
		anthropicMsgs = append(anthropicMsgs, anthropicMessage(m))
	}

	req := anthropicRequest{
		Model:     c.chatModel,
		Messages:  anthropicMsgs,
		System:    systemPrompt,
		MaxTokens: 2000,
		Thinking:  disabledThinking,
		// Temperature intentionally omitted: current Claude models (Sonnet 5+)
		// reject non-default sampling parameters with a 400.
	}

	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	baseURL := "https://api.anthropic.com"
	if c.config.BaseURL != "" {
		baseURL = c.config.BaseURL
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/v1/messages", bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.config.APIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return nil, fmt.Errorf("Anthropic API returned status %d", resp.StatusCode)
		}
		var errResp anthropicErrorResponse
		if json.Unmarshal(body, &errResp) == nil && errResp.Error.Message != "" {
			return nil, fmt.Errorf("Anthropic API error (%d): %s", resp.StatusCode, errResp.Error.Message)
		}
		return nil, fmt.Errorf("Anthropic API returned status %d: %s", resp.StatusCode, string(body))
	}

	var anthropicResp anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&anthropicResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if len(anthropicResp.Content) == 0 {
		return nil, fmt.Errorf("no content returned from Anthropic")
	}

	var sb strings.Builder
	for _, block := range anthropicResp.Content {
		if block.Type == BlockText {
			sb.WriteString(block.Text)
		}
	}

	return &Response{
		Content: sb.String(),
		Usage: Usage{
			PromptTokens:     anthropicResp.Usage.InputTokens,
			CompletionTokens: anthropicResp.Usage.OutputTokens,
			TotalTokens:      anthropicResp.Usage.InputTokens + anthropicResp.Usage.OutputTokens,
		},
	}, nil
}

func (c *AnthropicClient) GenerateEmbedding(ctx context.Context, text string) ([]float32, error) {
	return c.embeddingClient.GenerateEmbedding(ctx, text)
}

// anthropicToolRequest extends the base request with tools support.
type anthropicToolRequest struct {
	Thinking    *anthropicThinking `json:"thinking,omitempty"`
	Model       string             `json:"model"`
	System      string             `json:"system,omitempty"`
	Messages    []ToolMessage      `json:"messages"`
	Tools       []Tool             `json:"tools,omitempty"`
	MaxTokens   int                `json:"max_tokens"`
	Temperature float32            `json:"temperature,omitempty"`
}

// anthropicToolResponse handles both text and tool_use content blocks.
type anthropicToolResponse struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Role       string         `json:"role"`
	Model      string         `json:"model"`
	StopReason string         `json:"stop_reason"`
	Content    []ContentBlock `json:"content"`
	Usage      anthropicUsage `json:"usage"`
}

// ChatWithTools sends a request with tool definitions and returns structured content blocks.
func (c *AnthropicClient) ChatWithTools(
	ctx context.Context,
	system string,
	messages []ToolMessage,
	tools []Tool,
) (*ToolResponse, error) {
	req := anthropicToolRequest{
		Model:     c.chatModel,
		System:    system,
		Messages:  messages,
		Tools:     tools,
		MaxTokens: 4096,
		Thinking:  disabledThinking,
		// Temperature intentionally omitted: current Claude models (Sonnet 5+)
		// reject non-default sampling parameters with a 400.
	}

	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	baseURL := "https://api.anthropic.com"
	if c.config.BaseURL != "" {
		baseURL = c.config.BaseURL
	}

	httpReq, err := http.NewRequestWithContext(
		ctx, "POST", baseURL+"/v1/messages", bytes.NewBuffer(reqBody),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.config.APIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return nil, fmt.Errorf("Anthropic API returned status %d", resp.StatusCode)
		}
		var errResp anthropicErrorResponse
		if json.Unmarshal(body, &errResp) == nil && errResp.Error.Message != "" {
			return nil, fmt.Errorf("Anthropic API error (%d): %s",
				resp.StatusCode, errResp.Error.Message)
		}
		return nil, fmt.Errorf("Anthropic API returned status %d: %s",
			resp.StatusCode, string(body))
	}

	var anthropicResp anthropicToolResponse
	if err := json.NewDecoder(resp.Body).Decode(&anthropicResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &ToolResponse{
		StopReason: anthropicResp.StopReason,
		Content:    anthropicResp.Content,
		Usage: Usage{
			PromptTokens:     anthropicResp.Usage.InputTokens,
			CompletionTokens: anthropicResp.Usage.OutputTokens,
			TotalTokens:      anthropicResp.Usage.InputTokens + anthropicResp.Usage.OutputTokens,
		},
	}, nil
}

// Compile-time check that AnthropicClient implements ToolUser.
var _ ToolUser = (*AnthropicClient)(nil)
