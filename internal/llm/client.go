package llm

import (
	"context"
	"fmt"

	"github.com/autonomousops-co/accretion/internal/config"
)

type Client interface {
	Chat(ctx context.Context, messages []Message) (*Response, error)
	GenerateEmbedding(ctx context.Context, text string) ([]float32, error)
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Response struct {
	Content string `json:"content"`
	Usage   Usage  `json:"usage,omitempty"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func NewClient(cfg *config.LLMConfig) (Client, error) {
	switch cfg.Provider {
	case "openai":
		return NewOpenAIClient(cfg.OpenAI, cfg.EmbeddingModel, cfg.ChatModel)
	case "ollama":
		return NewOllamaClient(cfg.Ollama, cfg.EmbeddingModel, cfg.ChatModel)
	case "anthropic":
		return NewAnthropicClient(cfg.Anthropic, cfg.Ollama, cfg.EmbeddingModel, cfg.ChatModel)
	default:
		return nil, fmt.Errorf("unsupported LLM provider: %s", cfg.Provider)
	}
}
