// Tests for the 2026.7.0 F13 conversation history search.
// Anchors:
//   - Empty query returns nil without touching the filesystem
//   - Missing saves directory degrades to nil, nil
//   - Multi-save scoring + ranking
//   - Snippet windowing centres on the match
//   - Score tie-break is alphabetical (deterministic output)
//
// The Telegram-side wiring tests live in the telegram package;
// here we keep the search core hermetic.

package chat

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeSave is a one-line builder that wires up a per-chat save
// at the conventional path. Keeps the per-test setup short.
func makeSave(t *testing.T, basePath string, chatID int64, name string, contents ...string) {
	t.Helper()
	conv := NewConversation("telegram-test", 0)
	for i, c := range contents {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		conv.AddMessage(Message{Role: role, Content: c})
	}
	require.NoError(t, SaveNamedConversation(basePath, chatID, name, conv))
}

// TestSearchSavedConversations_EmptyQueryReturnsNil — the
// callable surface is operator-facing; an empty argument must
// degrade silently rather than dumping every save.
func TestSearchSavedConversations_EmptyQueryReturnsNil(t *testing.T) {
	base := filepath.Join(t.TempDir(), "session.json")
	got, err := SearchSavedConversations(base, 1, "   ", 5)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != nil {
		t.Errorf("empty query must return nil, got %+v", got)
	}
}

// TestSearchSavedConversations_MissingSavesDirReturnsNilNil —
// fresh chats have no saves dir yet; the search must NOT
// surface that as an error.
func TestSearchSavedConversations_MissingSavesDirReturnsNilNil(t *testing.T) {
	base := filepath.Join(t.TempDir(), "session.json")
	got, err := SearchSavedConversations(base, 1, "anything", 5)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != nil {
		t.Errorf("missing saves dir must return nil, got %+v", got)
	}
}

// TestSearchSavedConversations_MultiSaveRanking is the
// happy-path: three saves, two contain "NVDA", one doesn't.
// The two hits surface in score order; the third is dropped.
func TestSearchSavedConversations_MultiSaveRanking(t *testing.T) {
	base := filepath.Join(t.TempDir(), "session.json")
	makeSave(t, base, 1, "trade-research", "Bought NVDA at 870.", "NVDA earnings beat consensus.")
	makeSave(t, base, 1, "macro-notes", "Watching NVDA closely today.")
	makeSave(t, base, 1, "irrelevant", "Reviewed AAPL filings.")

	got, err := SearchSavedConversations(base, 1, "NVDA", 5)
	require.NoError(t, err)
	require.Len(t, got, 2)
	// trade-research has NVDA in 2 messages → score 2
	// macro-notes has NVDA in 1 message → score 1
	assert.Equal(t, "trade-research", got[0].Name)
	assert.Equal(t, 2, got[0].Score)
	assert.Equal(t, "macro-notes", got[1].Name)
	assert.Equal(t, 1, got[1].Score)
	// Snippet for the top hit must include the first matching
	// message verbatim (it's short, no truncation needed).
	assert.Contains(t, got[0].Snippet, "NVDA at 870")
}

// TestSearchSavedConversations_AlphaTieBreak — equal scores
// sort alphabetically so the operator's list doesn't shuffle
// on every search.
func TestSearchSavedConversations_AlphaTieBreak(t *testing.T) {
	base := filepath.Join(t.TempDir(), "session.json")
	makeSave(t, base, 1, "zebra", "Discussion about NVDA.")
	makeSave(t, base, 1, "alpha", "Discussion about NVDA.")
	makeSave(t, base, 1, "mango", "Discussion about NVDA.")

	got, err := SearchSavedConversations(base, 1, "NVDA", 5)
	require.NoError(t, err)
	require.Len(t, got, 3)
	wantOrder := []string{"alpha", "mango", "zebra"}
	for i, name := range wantOrder {
		assert.Equal(t, name, got[i].Name, "tie-break must be alphabetical at position %d", i)
	}
}

// TestSearchSavedConversations_LimitClamp — limit ≤ 0 falls
// back to 5; higher than save count returns just what exists.
func TestSearchSavedConversations_LimitClamp(t *testing.T) {
	base := filepath.Join(t.TempDir(), "session.json")
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		makeSave(t, base, 1, name, "matches NVDA")
	}
	got, err := SearchSavedConversations(base, 1, "NVDA", 0)
	require.NoError(t, err)
	assert.Len(t, got, 5, "limit=0 falls back to default 5")
}

// TestSearchSavedConversations_NoMatchReturnsEmpty — when
// nothing scores, the caller gets an empty slice; the Telegram
// handler renders that as "no matches" rather than crashing.
func TestSearchSavedConversations_NoMatchReturnsEmpty(t *testing.T) {
	base := filepath.Join(t.TempDir(), "session.json")
	makeSave(t, base, 1, "save", "Some content about cats")
	got, err := SearchSavedConversations(base, 1, "elephant", 5)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestSearchSavedConversations_MultiTermQuery — multiple
// terms compound: each term hit adds to the score
// independently, so a message mentioning both terms ranks
// higher than one mentioning only one.
func TestSearchSavedConversations_MultiTermQuery(t *testing.T) {
	base := filepath.Join(t.TempDir(), "session.json")
	makeSave(t, base, 1, "both", "NVDA earnings beat — strong outlook.")
	makeSave(t, base, 1, "one", "Just NVDA today.")

	got, err := SearchSavedConversations(base, 1, "NVDA earnings", 5)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "both", got[0].Name, "two-term match must outrank one-term")
	if got[0].Score <= got[1].Score {
		t.Errorf("multi-term hit (%d) must outscore single-term hit (%d)", got[0].Score, got[1].Score)
	}
}

// TestExcerptForSnippet_CentresOnFirstMatch — pin the
// snippet windowing: when content exceeds snippetMaxBytes,
// the excerpt centres on the first matching term and
// trims with ellipsis markers.
func TestExcerptForSnippet_CentresOnFirstMatch(t *testing.T) {
	long := strings.Repeat("a", 200) + " NVDA " + strings.Repeat("b", 200)
	got := excerptForSnippet(long, []string{"nvda"})
	if !strings.Contains(got, "NVDA") {
		t.Errorf("excerpt must include the matched term; got %q", got)
	}
	// The leading + trailing ellipsis are UTF-8 multibyte (3
	// bytes each), so the budget overhead is up to 6 bytes
	// past snippetMaxBytes.
	if len(got) > snippetMaxBytes+6 {
		t.Errorf("excerpt byte-length = %d, want ≤ %d (snippetMaxBytes + 2×3-byte ellipsis)",
			len(got), snippetMaxBytes+6)
	}
}

// TestExcerptForSnippet_ShortContentReturnsAsIs — when the
// message fits inside the window, no trimming or ellipsis.
func TestExcerptForSnippet_ShortContentReturnsAsIs(t *testing.T) {
	got := excerptForSnippet("hello NVDA world", []string{"nvda"})
	if got != "hello NVDA world" {
		t.Errorf("short content should return as-is; got %q", got)
	}
}

// TestExcerptForSnippet_LongContentNoMatchTrimsFromHead — the
// defensive branch when the scorer hands us a long message
// whose own content doesn't contain any term (terms matched
// in a SIBLING message of the same conversation). We trim
// from the head and add a trailing ellipsis.
func TestExcerptForSnippet_LongContentNoMatchTrimsFromHead(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := excerptForSnippet(long, []string{"nope"})
	if !strings.HasSuffix(got, "…") {
		t.Errorf("long no-match excerpt must end with ellipsis; got %q", got[len(got)-5:])
	}
	if len(got) > snippetMaxBytes+3 {
		t.Errorf("byte length %d, want ≤ %d", len(got), snippetMaxBytes+3)
	}
}

// TestExcerptForSnippet_MatchNearTailClampsEnd — when the
// match sits near the end of a long message, the start of
// the window gets pushed back so we don't slice past the
// content. Anchors path D of the windowing function.
func TestExcerptForSnippet_MatchNearTailClampsEnd(t *testing.T) {
	long := strings.Repeat("x", 500) + " NVDA"
	got := excerptForSnippet(long, []string{"nvda"})
	if !strings.Contains(got, "NVDA") {
		t.Error("tail match must still surface in the snippet")
	}
	// Trailing ellipsis must NOT appear — we're already at
	// the end of the content.
	if strings.HasSuffix(got, "…") {
		t.Errorf("end-clamped excerpt must NOT have trailing ellipsis (we reached content end); got tail %q", got[len(got)-5:])
	}
}

// TestTokeniseSearchTerms_DropsShortTokens — pins the same
// "drop 1-char tokens" rule as the dispatcher's
// tool_search tokeniser so a future drift surfaces.
func TestTokeniseSearchTerms_DropsShortTokens(t *testing.T) {
	got := tokeniseSearchTerms("A NVDA b earnings")
	assert.Equal(t, []string{"nvda", "earnings"}, got)
}
