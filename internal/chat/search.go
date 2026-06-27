// 2026.7.0 F13 — Conversation history search.
//
// Operators park threads via /save <name> and resume via
// /load <name>; the gap is "I don't remember what I called
// it". This module walks the per-chat named-save tree and
// scores each save's messages against a free-text query so
// the operator can find an old thread without enumerating.
//
// Pure-ish: walks the filesystem the chat package already
// writes to; no DB, no LLM, no embedding lookup. Cost scales
// with the number of saves × messages-per-save, which is
// human-scale (operators don't park more than a few dozen
// threads). Match scoring mirrors the dispatcher's tool_search
// scorer — cheap term-overlap with a tie-break on most-recent
// touch time.

package chat

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// SavedSearchHit is one entry in a SearchSavedConversations
// result. Snippet is a short excerpt from the first message
// that matched the query, suitable for inline Telegram /
// web-UI rendering.
type SavedSearchHit struct {
	// Name is the operator-visible save name; pair this with
	// /load <name> to resume the thread.
	Name string
	// Score is the raw match count across all messages.
	// Higher = stronger hit. Exposed so consumers can render
	// a confidence indicator.
	Score int
	// Snippet is up to 240 chars of context drawn from the
	// first message that contained any query term. Empty if
	// no individual message hit (the scorer also accepts
	// terms spread across messages).
	Snippet string
	// MessageCount is the total messages in the save —
	// useful as a "size of the parked thread" cue.
	MessageCount int
}

// snippetMaxBytes bounds the rendered preview so Telegram's
// 4096-char message limit can hold a list of several hits.
const snippetMaxBytes = 240

// SearchSavedConversations walks every per-chat save under
// basePath, scores each against the free-text query, and
// returns the top-N hits ranked by score descending,
// alphabetical name as tiebreak (deterministic output for
// tests and for stable rendering).
//
// limit ≤ 0 falls back to 5. An empty query returns nil
// without filesystem access so callers can pass user input
// directly. Missing saves directory returns nil, nil — the
// chat may simply have no saves yet.
func SearchSavedConversations(basePath string, chatID int64, query string, limit int) ([]SavedSearchHit, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 5
	}
	terms := tokeniseSearchTerms(query)
	if len(terms) == 0 {
		return nil, nil
	}
	dir := filepath.Join(filepath.Dir(basePath), "saves", fmt.Sprintf("%d", chatID))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list saved conversations: %w", err)
	}
	hits := make([]SavedSearchHit, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		const suffix = ".json"
		if len(name) <= len(suffix) || name[len(name)-len(suffix):] != suffix {
			continue
		}
		stem := name[:len(name)-len(suffix)]
		// Reuse the existing loader — it already sanitises the
		// name + returns the messages we want to score.
		conv, lerr := LoadNamedConversation(basePath, chatID, stem, 0)
		if lerr != nil {
			// Skip malformed saves rather than aborting the whole
			// search; one corrupt file shouldn't hide the others.
			continue
		}
		hit := scoreSavedConversation(stem, conv.GetMessages(), terms)
		if hit.Score > 0 {
			hits = append(hits, hit)
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Name < hits[j].Name
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// scoreSavedConversation tallies query-term overlap across the
// messages of a single save. Pure helper — exported in
// lower-case for unit tests via the package's own _test.go.
func scoreSavedConversation(name string, msgs []Message, terms []string) SavedSearchHit {
	out := SavedSearchHit{Name: name, MessageCount: len(msgs)}
	for _, m := range msgs {
		content := strings.ToLower(m.Content)
		if content == "" {
			continue
		}
		hitsThisMessage := 0
		for _, term := range terms {
			if strings.Contains(content, term) {
				hitsThisMessage++
			}
		}
		if hitsThisMessage > 0 {
			out.Score += hitsThisMessage
			if out.Snippet == "" {
				out.Snippet = excerptForSnippet(m.Content, terms)
			}
		}
	}
	return out
}

// excerptForSnippet pulls a short window of text around the
// first occurrence of any query term. Falls back to the first
// snippetMaxBytes of the message when no specific term is
// locatable (e.g. content shorter than the window).
func excerptForSnippet(content string, terms []string) string {
	lower := strings.ToLower(content)
	bestIdx := -1
	for _, term := range terms {
		if idx := strings.Index(lower, term); idx >= 0 && (bestIdx == -1 || idx < bestIdx) {
			bestIdx = idx
		}
	}
	if len(content) <= snippetMaxBytes {
		return content
	}
	if bestIdx < 0 {
		return content[:snippetMaxBytes] + "…"
	}
	// Centre the snippet on the match, clamped to the message
	// bounds. Tilde-prefixed when we trim the head, …-suffixed
	// when we trim the tail.
	start := bestIdx - snippetMaxBytes/3
	if start < 0 {
		start = 0
	}
	end := start + snippetMaxBytes
	if end > len(content) {
		end = len(content)
		start = end - snippetMaxBytes
		if start < 0 {
			start = 0
		}
	}
	out := content[start:end]
	if start > 0 {
		out = "…" + out
	}
	if end < len(content) {
		out = out + "…"
	}
	return out
}

// tokeniseSearchTerms splits a free-text query into tokens
// for substring matching. Same shape as the dispatcher's
// tool_search tokeniser — lowercase, alphanumeric runs only,
// drop single-char tokens. Kept duplicated rather than
// imported to avoid the chat package taking a dep on
// dispatcher (and to keep the conversation-search behaviour
// independent of any future tool-search tweak).
func tokeniseSearchTerms(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteRune(' ')
		}
	}
	words := strings.Fields(b.String())
	out := make([]string, 0, len(words))
	for _, w := range words {
		if len(w) < 2 {
			continue
		}
		out = append(out, w)
	}
	return out
}
