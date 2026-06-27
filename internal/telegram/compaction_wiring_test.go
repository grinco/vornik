package telegram

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
)

type stubGist struct{}

func (stubGist) Gist(_ []chat.Message) string { return "topics: stub" }

// With a compactor wired, conversations created by the bot retain overflow
// turns (no token-trim) and the read-path payload carries a gist.
func TestWithCompactor_TunesNewConversations(t *testing.T) {
	b, err := NewBot(
		BotConfig{Token: "t", MaxHistory: 100, MaxHistoryTokens: 40},
		nil,
		WithCompactor(stubGist{}),
	)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	conv := b.getConversation(999)
	for i := 0; i < 12; i++ {
		conv.AddMessage(chat.Message{Role: "user", Content: strings.Repeat("x", 80)})
	}
	if conv.Len() != 12 {
		t.Fatalf("compactor wired → no token-trim expected; kept %d/12", conv.Len())
	}
	payload := conv.MessagesForLLM()
	if len(payload) == 0 || !strings.HasPrefix(payload[0].Content, "[earlier conversation summarized]") {
		t.Fatalf("expected a leading gist in the read-path payload, got %d msgs", len(payload))
	}
}

// Without a compactor, conversations keep legacy truncation behavior.
func TestNoCompactor_LegacyTruncation(t *testing.T) {
	b, err := NewBot(
		BotConfig{Token: "t", MaxHistory: 100, MaxHistoryTokens: 40},
		nil,
	)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	conv := b.getConversation(998)
	for i := 0; i < 12; i++ {
		conv.AddMessage(chat.Message{Role: "user", Content: strings.Repeat("x", 80)})
	}
	if conv.Len() >= 12 {
		t.Fatalf("no compactor → token-trim expected; kept %d", conv.Len())
	}
	// And the read-path payload equals GetMessages (no gist).
	for _, m := range conv.MessagesForLLM() {
		if strings.HasPrefix(m.Content, "[earlier conversation summarized]") {
			t.Fatal("no gist should appear without a compactor")
		}
	}
}
