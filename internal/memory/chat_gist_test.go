package memory

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
)

func TestChatGist_TopTermsFromOverflow(t *testing.T) {
	g := NewChatGist(8)
	msgs := []chat.Message{
		{Role: "user", Content: "How do I configure the postgres retention sweep schedule?"},
		{Role: "assistant", Content: "The retention sweep runs on a leader-gated ticker; configure retention days per project."},
		{Role: "user", Content: "And the retention grace period for link codes?"},
	}
	got := g.Gist(msgs)
	if got == "" {
		t.Fatal("expected a non-empty gist")
	}
	if !strings.Contains(got, "3 earlier turns") {
		t.Errorf("gist should report the omitted-turn count, got %q", got)
	}
	if !strings.Contains(strings.ToLower(got), "retention") {
		t.Errorf("gist should surface the dominant term 'retention', got %q", got)
	}
}

func TestChatGist_TextBlocksCounted(t *testing.T) {
	g := NewChatGist(5)
	msgs := []chat.Message{
		{Role: "user", Blocks: []chat.ContentBlock{{Type: "text", Text: "deployment deployment deployment pipeline"}}},
	}
	got := g.Gist(msgs)
	if !strings.Contains(strings.ToLower(got), "deployment") {
		t.Errorf("gist should read text blocks; got %q", got)
	}
}

func TestChatGist_EmptyInput(t *testing.T) {
	g := NewChatGist(8)
	if got := g.Gist(nil); got != "" {
		t.Errorf("nil input must yield empty gist, got %q", got)
	}
	if got := g.Gist([]chat.Message{{Role: "user", Content: ""}}); got != "" {
		t.Errorf("blank content must yield empty gist, got %q", got)
	}
}

func TestChatGist_DefaultMaxTerms(t *testing.T) {
	// Non-positive maxTerms falls back to the default rather than producing
	// an empty term list.
	g := NewChatGist(0)
	got := g.Gist([]chat.Message{{Role: "user", Content: "alpha beta gamma delta epsilon zeta"}})
	if got == "" {
		t.Fatal("default maxTerms should still produce a gist")
	}
}
