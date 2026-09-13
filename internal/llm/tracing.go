package llm

import (
	"context"

	"go.opentelemetry.io/otel/attribute"

	"github.com/autonomousops-co/accretion/internal/telemetry"
)

// TracingDecorator wraps an llm.Client with OpenTelemetry spans.
type TracingDecorator struct {
	inner Client
}

// NewTracingDecorator wraps client with tracing spans on each method call.
func NewTracingDecorator(client Client) Client {
	return &TracingDecorator{inner: client}
}

func (t *TracingDecorator) Chat(ctx context.Context, messages []Message) (*Response, error) {
	ctx, span := telemetry.StartSpan(ctx, "llm", "llm.Chat")
	defer span.End()
	span.SetAttributes(attribute.Int("llm.message_count", len(messages)))
	resp, err := t.inner.Chat(ctx, messages)
	if err == nil && resp != nil {
		span.SetAttributes(
			attribute.Int("llm.prompt_tokens", resp.Usage.PromptTokens),
			attribute.Int("llm.completion_tokens", resp.Usage.CompletionTokens),
		)
	}
	return resp, err
}

func (t *TracingDecorator) GenerateEmbedding(ctx context.Context, text string) ([]float32, error) {
	ctx, span := telemetry.StartSpan(ctx, "llm", "llm.GenerateEmbedding")
	defer span.End()
	span.SetAttributes(attribute.Int("llm.text_length", len(text)))
	return t.inner.GenerateEmbedding(ctx, text)
}

// tracingToolUser wraps a ToolUser with an OpenTelemetry span around
// ChatWithTools. It is kept separate from TracingDecorator so that only a
// genuine tool-capable client is presented as a ToolUser — a plain Chat client
// wrapped in TracingDecorator must not appear to satisfy ToolUser.
type tracingToolUser struct {
	inner ToolUser
}

// NewTracingToolUser wraps tu with tracing spans on ChatWithTools.
func NewTracingToolUser(tu ToolUser) ToolUser {
	return &tracingToolUser{inner: tu}
}

func (t *tracingToolUser) ChatWithTools(
	ctx context.Context,
	system string,
	messages []ToolMessage,
	tools []Tool,
) (*ToolResponse, error) {
	ctx, span := telemetry.StartSpan(ctx, "llm", "llm.ChatWithTools")
	defer span.End()
	span.SetAttributes(
		attribute.Int("llm.message_count", len(messages)),
		attribute.Int("llm.tool_count", len(tools)),
	)
	resp, err := t.inner.ChatWithTools(ctx, system, messages, tools)
	if err == nil && resp != nil {
		span.SetAttributes(
			attribute.String("llm.stop_reason", resp.StopReason),
			attribute.Int("llm.prompt_tokens", resp.Usage.PromptTokens),
			attribute.Int("llm.completion_tokens", resp.Usage.CompletionTokens),
		)
	}
	return resp, err
}

// Compile-time check that tracingToolUser implements ToolUser.
var _ ToolUser = (*tracingToolUser)(nil)
