package discord

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/autonomousops-co/accretion/internal/llm"
)

func textOf(t *testing.T, s *conversationStore, channelID string) []string {
	t.Helper()
	var out []string
	history := s.history(channelID)
	for i := range history {
		for j := range history[i].Content {
			out = append(out, history[i].Role+":"+history[i].Content[j].Text)
		}
	}
	return out
}

func TestHistoryIsEmptyForUnknownChannel(t *testing.T) {
	s := newConversationStore()
	if got := s.history("nope"); got != nil {
		t.Fatalf("want nil history for an unseen channel, got %v", got)
	}
}

func TestRecordReplaysTurnsInOrder(t *testing.T) {
	s := newConversationStore()
	s.record("c1", "what is up?", "a snapshot")
	s.record("c1", "yes, please", "digging in")

	want := []string{
		"user:what is up?",
		"assistant:a snapshot",
		"user:yes, please",
		"assistant:digging in",
	}
	got := textOf(t, s, "c1")
	if len(got) != len(want) {
		t.Fatalf("want %d messages, got %d: %v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("message %d: want %q, got %q", i, want[i], got[i])
		}
	}
}

// Channels must not bleed into each other. Discord models a thread as its own
// channel, so this is also what keeps a thread's context out of its parent.
func TestChannelsAreIsolated(t *testing.T) {
	s := newConversationStore()
	s.record("channel", "in the channel", "ok")
	s.record("thread", "in the thread", "ok")

	if got := textOf(t, s, "channel"); len(got) != 2 || got[0] != "user:in the channel" {
		t.Errorf("channel history leaked or is wrong: %v", got)
	}
	if got := textOf(t, s, "thread"); len(got) != 2 || got[0] != "user:in the thread" {
		t.Errorf("thread history leaked or is wrong: %v", got)
	}
}

func TestHistoryTrimsToTheCap(t *testing.T) {
	s := newConversationStore()
	for i := range 20 {
		s.record("c1", fmt.Sprintf("q%d", i), fmt.Sprintf("a%d", i))
	}

	got := textOf(t, s, "c1")
	if len(got) != maxHistoryMessages {
		t.Fatalf("want history capped at %d, got %d", maxHistoryMessages, len(got))
	}
	// The cap keeps the newest, so the last exchange recorded must survive.
	if got[len(got)-1] != "assistant:a19" {
		t.Errorf("want the newest exchange kept, got last = %q", got[len(got)-1])
	}
	if got[0] != "user:q15" {
		t.Errorf("want the oldest kept to be q15, got %q", got[0])
	}
}

func TestHistoryReturnsACopy(t *testing.T) {
	s := newConversationStore()
	s.record("c1", "q", "a")

	got := s.history("c1")
	got[0].Content[0].Text = "mutated"

	if after := textOf(t, s, "c1"); after[0] != "user:q" {
		t.Errorf("caller mutated stored history: got %q", after[0])
	}
}

func TestExpiredConversationIsDropped(t *testing.T) {
	s := newConversationStore()
	s.record("c1", "q", "a")

	s.mu.Lock()
	s.conversations["c1"].lastUsed = time.Now().Add(-conversationTTL - time.Minute)
	s.mu.Unlock()

	if got := s.history("c1"); got != nil {
		t.Errorf("want an expired conversation dropped, got %v", got)
	}
}

func TestForgetDropsAConversation(t *testing.T) {
	s := newConversationStore()
	s.record("c1", "q", "a")
	s.forget("c1")

	if got := s.history("c1"); got != nil {
		t.Errorf("want forget to clear history, got %v", got)
	}
}

func TestConversationCountIsBounded(t *testing.T) {
	s := newConversationStore()
	for i := range maxConversations + 50 {
		s.record(fmt.Sprintf("c%d", i), "q", "a")
	}

	s.mu.Lock()
	n := len(s.conversations)
	s.mu.Unlock()

	if n > maxConversations {
		t.Errorf("want at most %d conversations tracked, got %d", maxConversations, n)
	}
}

// The store is reached from discordgo's handler goroutines, one per message, so
// concurrent access is the normal case rather than an edge case.
func TestStoreIsSafeForConcurrentUse(t *testing.T) {
	s := newConversationStore()
	var wg sync.WaitGroup

	for i := range 50 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			s.record(fmt.Sprintf("c%d", i%5), "q", "a")
		}()
		go func() {
			defer wg.Done()
			_ = s.history(fmt.Sprintf("c%d", i%5))
		}()
	}
	wg.Wait()
}

func TestToChatTurnsFlattensText(t *testing.T) {
	s := newConversationStore()
	s.record("c1", "q", "a")

	turns := toChatTurns(s.history("c1"))
	if len(turns) != 2 {
		t.Fatalf("want 2 turns, got %d", len(turns))
	}
	if turns[0].Role != "user" || turns[0].Content != "q" {
		t.Errorf("turn 0: got %+v", turns[0])
	}
	if turns[1].Role != "assistant" || turns[1].Content != "a" {
		t.Errorf("turn 1: got %+v", turns[1])
	}
}

// recordingToolUser captures the messages the agent loop sends, so a test can
// assert on what the model was actually shown.
type recordingToolUser struct {
	seen  []llm.ToolMessage
	reply string
}

func (r *recordingToolUser) ChatWithTools(
	_ context.Context,
	_ string,
	messages []llm.ToolMessage,
	_ []llm.Tool,
) (*llm.ToolResponse, error) {
	r.seen = messages
	return &llm.ToolResponse{
		StopReason: llm.StopEndTurn,
		Content:    []llm.ContentBlock{{Type: llm.BlockText, Text: r.reply}},
	}, nil
}

// The regression this whole change exists for: a follow-up must arrive at the
// model with the prior turns ahead of it, not on its own.
func TestAgentLoopPrependsHistory(t *testing.T) {
	store := newConversationStore()
	store.record("c1", "what has my cluster been doing?", "here is a snapshot")

	tu := &recordingToolUser{reply: "digging in"}
	b := &Bot{history: store}

	got, err := b.runAgentLoop(tu, "system", store.history("c1"), "yes, please")
	if err != nil {
		t.Fatalf("runAgentLoop: %v", err)
	}
	if got != "digging in" {
		t.Errorf("want the model's reply, got %q", got)
	}

	if len(tu.seen) != 3 {
		t.Fatalf("want 2 history messages + the new one, got %d", len(tu.seen))
	}
	want := []string{
		"what has my cluster been doing?",
		"here is a snapshot",
		"yes, please",
	}
	for i := range want {
		if tu.seen[i].Content[0].Text != want[i] {
			t.Errorf("message %d: want %q, got %q", i, want[i], tu.seen[i].Content[0].Text)
		}
	}
}

// Without history the loop must still send exactly the one message, so a first
// message is unchanged by this feature.
func TestAgentLoopWithoutHistorySendsOneMessage(t *testing.T) {
	tu := &recordingToolUser{reply: "ok"}
	b := &Bot{history: newConversationStore()}

	if _, err := b.runAgentLoop(tu, "system", nil, "first question"); err != nil {
		t.Fatalf("runAgentLoop: %v", err)
	}
	if len(tu.seen) != 1 {
		t.Fatalf("want exactly 1 message, got %d", len(tu.seen))
	}
	if tu.seen[0].Content[0].Text != "first question" {
		t.Errorf("got %q", tu.seen[0].Content[0].Text)
	}
}
