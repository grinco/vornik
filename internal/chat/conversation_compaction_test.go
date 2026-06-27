package chat

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// fakeCompactor produces a deterministic, inspectable gist so the
// Conversation read-path logic can be tested without the real
// (memory-package) deterministic compactor.
type fakeCompactor struct{}

func (fakeCompactor) Gist(msgs []Message) string {
	return fmt.Sprintf("dropped=%d", len(msgs))
}

func userMsg(s string) Message      { return Message{Role: "user", Content: s} }
func assistantMsg(s string) Message { return Message{Role: "assistant", Content: s} }

// roleSeq is a readable shorthand for asserting the role order of a payload.
func roleSeq(msgs []Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Role
	}
	return out
}

func TestMessagesForLLM_NoCompactor_EqualsGetMessages(t *testing.T) {
	c := NewConversation("c1", 100)
	c.SetMaxTokens(10) // tiny budget
	for i := 0; i < 6; i++ {
		c.AddMessage(userMsg(strings.Repeat("x", 100)))
		c.AddMessage(assistantMsg(strings.Repeat("y", 100)))
	}
	// With no compactor wired, the read-path payload is byte-for-byte
	// the legacy GetMessages() output.
	got := c.MessagesForLLM()
	want := c.GetMessages()
	if len(got) != len(want) {
		t.Fatalf("len(MessagesForLLM)=%d, len(GetMessages)=%d — must match without a compactor", len(got), len(want))
	}
	for i := range got {
		if got[i].Content != want[i].Content || got[i].Role != want[i].Role {
			t.Fatalf("message %d differs: %+v vs %+v", i, got[i], want[i])
		}
	}
}

func TestAddMessage_NoTokenTrimWhenCompactorSet(t *testing.T) {
	// Without a compactor: token budget evicts old turns (legacy).
	legacy := NewConversation("legacy", 100)
	legacy.SetMaxTokens(50) // 200 chars
	for i := 0; i < 10; i++ {
		legacy.AddMessage(userMsg(strings.Repeat("a", 100)))
	}
	if legacy.Len() >= 10 {
		t.Fatalf("legacy path should have token-trimmed; kept %d messages", legacy.Len())
	}

	// With a compactor: AddMessage no longer token-trims — history is
	// retained (bounded only by the message-count cap) so the read path
	// has older turns to compact.
	compacted := NewConversation("compacted", 100)
	compacted.SetMaxTokens(50)
	compacted.SetCompactor(fakeCompactor{})
	for i := 0; i < 10; i++ {
		compacted.AddMessage(userMsg(strings.Repeat("a", 100)))
	}
	if compacted.Len() != 10 {
		t.Fatalf("compactor path must retain all 10 messages (count cap 100); kept %d", compacted.Len())
	}
}

func TestAddMessage_CountCapStillAppliesWithCompactor(t *testing.T) {
	c := NewConversation("c", 4) // tiny count cap
	c.SetMaxTokens(1_000_000)
	c.SetCompactor(fakeCompactor{})
	for i := 0; i < 10; i++ {
		c.AddMessage(userMsg("m"))
	}
	if c.Len() > 4 {
		t.Fatalf("count cap must still bound storage even with a compactor; kept %d (cap 4)", c.Len())
	}
}

func TestMessagesForLLM_OverBudget_GistsOlderRetainsRecent(t *testing.T) {
	c := NewConversation("c", 100)
	c.SetMaxTokens(60) // 240 chars budget
	c.SetCompactor(fakeCompactor{})
	// 8 turns of ~100 chars each → ~800 chars, well over the 240-char budget.
	for i := 0; i < 8; i++ {
		c.AddMessage(userMsg(fmt.Sprintf("question-%d %s", i, strings.Repeat("q", 90))))
		c.AddMessage(assistantMsg(fmt.Sprintf("answer-%d %s", i, strings.Repeat("a", 90))))
	}
	got := c.MessagesForLLM()

	// First message must be the honest-marker gist.
	if len(got) == 0 {
		t.Fatal("empty payload")
	}
	if !strings.HasPrefix(got[0].Content, compactionMarkerPrefix) {
		t.Fatalf("first message must be the gist marker, got role=%q content=%q", got[0].Role, got[0].Content)
	}
	if !strings.Contains(got[0].Content, "dropped=") {
		t.Fatalf("gist must embed the compactor output, got %q", got[0].Content)
	}

	// The most-recent turn must survive.
	last := got[len(got)-1]
	if !strings.Contains(last.Content, "answer-7") {
		t.Fatalf("newest turn must be retained; last message = %q", last.Content)
	}

	// The payload (excluding the single gist message) must fit the budget.
	total := 0
	for _, m := range got {
		total += messageTokenChars(m)
	}
	// Budget is 240 chars + a bounded gist reserve; assert we did materially
	// shrink vs the raw ~1600 chars.
	if total > 240+compactionGistReserveChars+200 {
		t.Fatalf("payload not compacted enough: %d chars", total)
	}

	// Exactly one gist message.
	gists := 0
	for _, m := range got {
		if strings.HasPrefix(m.Content, compactionMarkerPrefix) {
			gists++
		}
	}
	if gists != 1 {
		t.Fatalf("expected exactly one gist message, got %d", gists)
	}
}

func TestMessagesForLLM_UnderBudget_NoGist(t *testing.T) {
	c := NewConversation("c", 100)
	c.SetMaxTokens(10_000)
	c.SetCompactor(fakeCompactor{})
	c.AddMessage(userMsg("hi"))
	c.AddMessage(assistantMsg("hello"))
	got := c.MessagesForLLM()
	for _, m := range got {
		if strings.HasPrefix(m.Content, compactionMarkerPrefix) {
			t.Fatalf("under budget must produce no gist; got %q", m.Content)
		}
	}
	if want := roleSeq([]Message{userMsg(""), assistantMsg("")}); len(got) != len(want) {
		t.Fatalf("under budget should return all %d messages, got %d", len(want), len(got))
	}
}

func TestMessagesForLLM_RaceClean(t *testing.T) {
	c := NewConversation("c", 100)
	c.SetMaxTokens(40)
	c.SetCompactor(fakeCompactor{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); c.AddMessage(userMsg(strings.Repeat("z", 50))) }()
		go func() { defer wg.Done(); _ = c.MessagesForLLM() }()
	}
	wg.Wait()
	if c.Len() == 0 {
		t.Fatal("expected messages after concurrent appends")
	}
}
