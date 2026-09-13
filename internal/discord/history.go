package discord

import (
	"sync"
	"time"

	"github.com/autonomousops-co/accretion/internal/llm"
	apiclient "github.com/autonomousops-co/accretion/internal/mcp/accretion"
)

const (
	// maxHistoryMessages is how many messages (not exchanges) are replayed into
	// a new turn. Each exchange is two entries, so this is five user/assistant
	// pairs. The cap exists because history is prepended to every request: an
	// unbounded conversation grows the prompt until it is refused for exceeding
	// the model's context window, and the failure would arrive mid-conversation
	// rather than at startup.
	maxHistoryMessages = 10

	// maxConversations bounds how many channels are tracked at once. A bot in a
	// busy guild sees unrelated channels forever, and without a ceiling the map
	// is a slow leak. When the limit is hit the least recently used
	// conversation is dropped.
	maxConversations = 500

	// conversationTTL drops a conversation that has been idle this long. Two
	// hours later the user asking "yes, please" almost certainly does not mean
	// the question from before lunch, so expiring is both cheaper and more
	// correct than replaying it.
	conversationTTL = 2 * time.Hour
)

// conversationStore keeps the recent turns of each Discord conversation so a
// follow-up like "yes, please" resolves against what was actually said.
//
// Keyed by channel ID, which gives per-thread isolation for free: Discord
// models a thread as a channel with its own ID, so a message posted in a thread
// carries the thread's ID, not the parent's. Threads therefore keep their own
// context, and a DM (also its own channel) keeps one per user.
//
// What is stored is deliberately *not* the agent loop's working transcript.
// Only the user's text and the assistant's final text are kept — no tool_use or
// tool_result blocks. Providers require a tool_result to immediately follow the
// tool_use it answers, so replaying a partial tool exchange from an earlier
// turn risks a validation error from the API; the blocks are also far larger
// than the answer they produced. The distilled turn is what a follow-up needs.
type conversationStore struct {
	mu            sync.Mutex
	conversations map[string]*conversation
}

type conversation struct {
	messages []llm.ToolMessage
	lastUsed time.Time
}

func newConversationStore() *conversationStore {
	return &conversationStore{conversations: make(map[string]*conversation)}
}

// history returns the messages to replay for a channel, oldest first. The
// result is a deep copy: the agent loop appends tool_use and tool_result blocks
// to the slice it is handed, and a shallow copy would share each message's
// Content backing array with the stored conversation.
func (s *conversationStore) history(channelID string) []llm.ToolMessage {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.conversations[channelID]
	if !ok {
		return nil
	}
	if time.Since(c.lastUsed) > conversationTTL {
		delete(s.conversations, channelID)
		return nil
	}

	out := make([]llm.ToolMessage, len(c.messages))
	for i, m := range c.messages {
		blocks := make([]llm.ContentBlock, len(m.Content))
		copy(blocks, m.Content)
		out[i] = llm.ToolMessage{Role: m.Role, Content: blocks}
	}
	return out
}

// record appends one completed exchange. It is called only after a successful
// reply: a turn that errored is not worth replaying, and recording it would
// leave a user message with no answer after it.
func (s *conversationStore) record(channelID, userMessage, assistantMessage string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.evictExpiredLocked()

	c, ok := s.conversations[channelID]
	if !ok {
		if len(s.conversations) >= maxConversations {
			s.evictOldestLocked()
		}
		c = &conversation{}
		s.conversations[channelID] = c
	}

	c.messages = append(c.messages,
		textMessage("user", userMessage),
		textMessage("assistant", assistantMessage),
	)

	// Trim from the front, keeping the most recent messages. The slice is
	// re-allocated rather than resliced so the dropped messages become
	// unreachable instead of being pinned by the backing array.
	if len(c.messages) > maxHistoryMessages {
		kept := make([]llm.ToolMessage, maxHistoryMessages)
		copy(kept, c.messages[len(c.messages)-maxHistoryMessages:])
		c.messages = kept
	}

	c.lastUsed = time.Now()
}

// forget drops a conversation, so a user can start clean without waiting out
// the TTL.
func (s *conversationStore) forget(channelID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conversations, channelID)
}

func (s *conversationStore) evictExpiredLocked() {
	for id, c := range s.conversations {
		if time.Since(c.lastUsed) > conversationTTL {
			delete(s.conversations, id)
		}
	}
}

func (s *conversationStore) evictOldestLocked() {
	var oldestID string
	var oldest time.Time
	for id, c := range s.conversations {
		if oldestID == "" || c.lastUsed.Before(oldest) {
			oldestID, oldest = id, c.lastUsed
		}
	}
	if oldestID != "" {
		delete(s.conversations, oldestID)
	}
}

func textMessage(role, text string) llm.ToolMessage {
	return llm.ToolMessage{
		Role:    role,
		Content: []llm.ContentBlock{{Type: "text", Text: text}},
	}
}

// toChatTurns flattens stored history into the API client's turn shape for the
// server-side RAG fallback, which speaks plain text rather than content blocks.
// Non-text blocks cannot occur here (record stores only text), so concatenating
// the text blocks is lossless.
func toChatTurns(history []llm.ToolMessage) []apiclient.ChatTurn {
	turns := make([]apiclient.ChatTurn, 0, len(history))
	for _, m := range history {
		var text string
		for i := range m.Content {
			if m.Content[i].Type == "text" {
				text += m.Content[i].Text
			}
		}
		if text == "" {
			continue
		}
		turns = append(turns, apiclient.ChatTurn{Role: m.Role, Content: text})
	}
	return turns
}
