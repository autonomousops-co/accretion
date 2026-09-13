package llm

// AsToolUser returns a ToolUser for the given client. If the concrete client
// natively implements ToolUser (e.g. the Anthropic, OpenAI, or Ollama clients),
// that native implementation is returned directly. Otherwise the client is
// wrapped in a prompt-based ReAct adapter so any Chat-only client can still
// participate in agentic tool-use loops.
//
// The decision is made on the concrete client c, so callers must pass the raw
// provider client rather than a tracing wrapper — otherwise native support
// cannot be detected statically.
func AsToolUser(c Client) ToolUser {
	if tu, ok := c.(ToolUser); ok {
		return tu
	}
	return newReactToolUser(c)
}
