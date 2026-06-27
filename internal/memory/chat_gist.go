package memory

import (
	"fmt"
	"strings"

	"vornik.io/vornik/internal/chat"
)

// ChatGist is a deterministic, LLM-free chat.Compactor: it condenses the
// overflow turns of a conversation into a short topic residue using the same
// term-frequency machinery as the project consolidator (Tokenize /
// FrequencyMap / TopTerms). It performs no network call, so it is safe on the
// interactive chat hot path (review-20260623-df8c findings #3, #6 — no
// llmCompactor on the automatic path).
//
// It lives here (not in internal/chat) because internal/memory already imports
// internal/chat; the reverse would be an import cycle.
type ChatGist struct {
	maxTerms       int
	minTokenLength int
}

// chatGistDefaultMaxTerms is the fallback topic count when a non-positive
// maxTerms is supplied.
const chatGistDefaultMaxTerms = 24

// NewChatGist returns a deterministic compactor surfacing up to maxTerms
// topics. A non-positive maxTerms falls back to the default.
func NewChatGist(maxTerms int) *ChatGist {
	if maxTerms <= 0 {
		maxTerms = chatGistDefaultMaxTerms
	}
	return &ChatGist{maxTerms: maxTerms, minTokenLength: 3}
}

// Gist implements chat.Compactor. Returns "" when there is no usable text.
func (g *ChatGist) Gist(msgs []chat.Message) string {
	contents := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if m.Content != "" {
			contents = append(contents, m.Content)
		}
		for _, b := range m.Blocks {
			if b.Type == "text" && b.Text != "" {
				contents = append(contents, b.Text)
			}
		}
	}
	if len(contents) == 0 {
		return ""
	}
	top := TopTerms(FrequencyMap(contents, g.minTokenLength), g.maxTerms)
	if len(top) == 0 {
		return ""
	}
	terms := make([]string, len(top))
	for i, t := range top {
		terms[i] = t.Term
	}
	return fmt.Sprintf("%d earlier turns omitted; topics: %s", len(msgs), strings.Join(terms, ", "))
}
