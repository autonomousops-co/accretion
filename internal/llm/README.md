# `llm` — embeddings, providers & RAG recall

Two jobs: turn text into the **embeddings** that make memory searchable (write path), and turn
retrieved memory into **answers** (read path). Both sit behind one small interface so providers are
swappable.

```go
type Client interface {
    Chat(ctx, messages []Message) (*Response, error)      // chat completion
    GenerateEmbedding(ctx, text string) ([]float32, error) // embedding vector
}
```

`NewClient(cfg *config.LLMConfig)` selects the provider from `cfg.Provider`.

## Providers

| Provider (`ACCRETION_LLM_PROVIDER`) | Chat | Embeddings | Notes |
|---|---|---|---|
| `ollama` *(default)* | `POST {base}/api/chat` | `POST {base}/api/embeddings` | fully local; default `base_url` `http://localhost:11434` |
| `openai` | `POST {base}/chat/completions` | `POST {base}/embeddings` | Bearer auth |
| `anthropic` | `POST {base}/v1/messages` | *(delegates to Ollama)* | thinking disabled |

**Anthropic has no embeddings API**, so `AnthropicClient` constructs an internal Ollama client and
routes `GenerateEmbedding` there. This is why embedding is available regardless of chat provider —
and why the embedding model must be reachable (locally, that's Ollama).

**Defaults** (`config.setDefaults`): provider `ollama`, embedding model `nomic-embed-text:latest`
(its vector width must match the pgvector column), chat model `llama3:8b`. Deployments commonly
override these (e.g. the Discord bot runs `anthropic` with a Claude chat model). `mock.go` is a
test-only client (deterministic embeddings, canned chat).

Every real client is wrapped by `tracing.go`'s `TracingDecorator` for OTel spans.

## Embeddings (write path)

Ingest workers call `GenerateEmbedding` on a human-readable summary of each captured signal, then
store the vector in `unified_entries.embedding`. See [../worker](../worker/README.md) and
[../database](../database/README.md). The vector width is set by the embedding model and must match
the pgvector column; swapping models means changing both.

## RAG recall (read path)

The read path that produces a **grounded** answer (defined in the [root README](../../README.md)):
`accretion_ask`, the web chat, and the Discord bot's fallback path all resolve a natural-language
question into an answer via single-shot RAG (implemented in [`../server`](../server/README.md)
`chatHandler`):

1. **Embed** the user's question → query vector.
2. **Extract a time window** — a *separate* `Chat` call using [`prompts/time-extraction.md`](../../prompts/time-extraction.md)
   parses phrases like "in the last hour" into `{start,end}` RFC3339.
3. **Search** — `dbClient.SearchSimilarWithOptions(vec, 10, window)`: cosine search, `similarity ≥ 0.3`.
4. **Build context** — render the top ~5 results per resource-type into text.
5. **Answer** — inject [`prompts/system-prompt.md`](../../prompts/system-prompt.md) (with
   `{{current_utc_time}}` and `{{cluster_context}}` filled) ahead of the conversation and call `Chat`.

The system prompt *asks* the model to answer only from the retrieved records and to say *"I don't
have enough data"* rather than speculate. This biases toward retrieval-grounded answers — but it's a
prompt-level instruction, not an enforcement mechanism, so the usual RAG hallucination caveats still
apply.

## Agentic tool-use

Beyond single-shot RAG, the Discord bot can run an **agentic tool loop** (up to 10 rounds): instead
of one pre-built context blob, the model **fetches its own data** — calling `search_cluster` /
`get_*` tools against the store until it has what it needs, then answers. This runs through the
`ToolUser` interface (`ChatWithTools`, returning structured `tool_use`/text content blocks) and works
with **any provider**:

- **Native** — `AnthropicClient`, `OpenAIClient`, and `OllamaClient` each implement `ChatWithTools`
  against their provider's own function-calling API (`anthropic.go`, `openai.go`, `ollama.go`).
- **ReAct fallback** — `react.go` adapts any plain `Chat`-only client into a `ToolUser` via a
  prompt-based ReAct protocol: tool schemas and a strict response contract are injected into the
  system prompt, prior tool results are rendered as text, and the model's free-form reply is parsed
  back into content blocks.

`AsToolUser` (`toolfactory.go`) picks the right one — the native implementation when the concrete
client satisfies `ToolUser`, else the ReAct adapter — so every provider can drive the loop.
`NewTracingToolUser` (`tracing.go`) then wraps the result in an OTel span. The bot composes them as
`NewTracingToolUser(AsToolUser(base))`, passing the *raw* provider client so native support is
detected statically (a tracing wrapper would hide it). If the loop itself errors, the bot falls back
to the single-shot RAG path above (`/api/chat`).
