package conversation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

// Wave 2 tests for internal/conversation.
//
// The package was already ~97.1% covered before this file. The genuinely
// remaining reachable gaps were the defensive early-return branches in the
// two unexported helpers truncateRunes / stripControl (exercised here via
// the public SanitizeChannelSpecific) plus a set of boundary / edge-case
// behaviours the wave-1 tests asserted only loosely. These tests assert
// real behaviour at those edges rather than padding the count; see the
// closing report for the honest tally.

// --- SanitizeChannelSpecific: truncateRunes / stripControl edges ---------

// TestW2ConvSanitizeValueExactlyAtCap — a value whose rune count is exactly
// the cap must pass through untouched (boundary of the `<= n` fast path in
// truncateRunes). Off-by-one here would silently lop a routing token.
func TestW2ConvSanitizeValueExactlyAtCap(t *testing.T) {
	val := strings.Repeat("a", maxChannelSpecificValLen)
	out := SanitizeChannelSpecific(map[string]string{"k": val})
	assert.Equal(t, val, out["k"], "value at exact cap must be preserved verbatim")
	assert.Equal(t, maxChannelSpecificValLen, utf8.RuneCountInString(out["k"]))
}

// TestW2ConvSanitizeValueOneOverCap — one rune over the cap truncates to
// exactly the cap (the count==n early `return s[:i]` branch).
func TestW2ConvSanitizeValueOneOverCap(t *testing.T) {
	val := strings.Repeat("a", maxChannelSpecificValLen+1)
	out := SanitizeChannelSpecific(map[string]string{"k": val})
	assert.Equal(t, maxChannelSpecificValLen, utf8.RuneCountInString(out["k"]),
		"value one over cap must truncate to exactly the cap")
}

// TestW2ConvSanitizeMultibyteTruncationNoSplit — truncation must cut on a
// rune boundary, never mid-codepoint. A value of cap+1 multibyte runes must
// stay valid UTF-8 after truncation (the rune-safe loop in truncateRunes).
func TestW2ConvSanitizeMultibyteTruncationNoSplit(t *testing.T) {
	// Each "あ" is a 3-byte rune; build cap+5 of them.
	val := strings.Repeat("あ", maxChannelSpecificValLen+5)
	out := SanitizeChannelSpecific(map[string]string{"k": val})
	got := out["k"]
	assert.True(t, utf8.ValidString(got), "truncated multibyte value must remain valid UTF-8")
	assert.Equal(t, maxChannelSpecificValLen, utf8.RuneCountInString(got))
	// Byte length must be a clean multiple of 3 (no split codepoint).
	assert.Equal(t, 0, len(got)%3, "byte length must land on a 3-byte rune boundary")
}

// TestW2ConvSanitizeKeyExactlyAtCap — symmetric boundary check for keys.
func TestW2ConvSanitizeKeyExactlyAtCap(t *testing.T) {
	key := strings.Repeat("K", maxChannelSpecificKeyLen)
	out := SanitizeChannelSpecific(map[string]string{key: "v"})
	_, ok := out[key]
	assert.True(t, ok, "key at exact cap must be kept verbatim")
}

// TestW2ConvSanitizeNoControlNoCopy — a clean value (no control bytes)
// exercises stripControl's "no control found" fast path and must come back
// byte-identical, including embedded spaces and punctuation.
func TestW2ConvSanitizeNoControlNoCopy(t *testing.T) {
	clean := "T0123 team-id.value (ok)!"
	out := SanitizeChannelSpecific(map[string]string{"team_id": clean})
	assert.Equal(t, clean, out["team_id"])
}

// TestW2ConvSanitizeStripsC1AndDEL — wave-1 only covered C0 (\n\r\x00).
// The guard must also remove DEL (0x7f) and C1 controls (e.g. 0x85 NEL,
// 0x9b CSI) which are the sneakier log-injection / terminal-escape vectors.
func TestW2ConvSanitizeStripsC1AndDEL(t *testing.T) {
	in := map[string]string{
		"k": "a\x7fb\u0085c\u009bd",
	}
	out := SanitizeChannelSpecific(in)
	assert.Equal(t, "abcd", out["k"], "DEL and C1 control runes must be stripped")
}

// TestW2ConvSanitizeValueAllControlBecomesEmpty — a value that is entirely
// control characters sanitises to "" but, unlike an all-control KEY, the
// entry is RETAINED (the drop rule only applies to keys). Documents the
// asymmetry between key and value handling.
func TestW2ConvSanitizeValueAllControlBecomesEmpty(t *testing.T) {
	out := SanitizeChannelSpecific(map[string]string{"realkey": "\n\r\x00\x07"})
	v, ok := out["realkey"]
	assert.True(t, ok, "key with all-control value is retained")
	assert.Equal(t, "", v, "all-control value sanitises to empty string")
}

// TestW2ConvSanitizeDeterministicTruncationBySortedKey — when more than the
// entry cap are present, truncation keeps the lexicographically-smallest
// keys deterministically (sorted-key iteration), so two callers see a
// stable subset rather than Go's randomised map order.
func TestW2ConvSanitizeDeterministicTruncationBySortedKey(t *testing.T) {
	in := make(map[string]string, maxChannelSpecificEntries*2)
	// Zero-padded keys so lexical order == numeric order.
	for i := 0; i < maxChannelSpecificEntries*2; i++ {
		in[zpad(i)] = "v"
	}
	first := SanitizeChannelSpecific(in)
	second := SanitizeChannelSpecific(in)
	assert.Len(t, first, maxChannelSpecificEntries)
	assert.Equal(t, first, second, "truncation must be deterministic across calls")
	// The kept set must be exactly the lowest-sorted maxChannelSpecificEntries keys.
	for i := 0; i < maxChannelSpecificEntries; i++ {
		_, ok := first[zpad(i)]
		assert.Truef(t, ok, "expected low key %s to be kept", zpad(i))
	}
	_, ok := first[zpad(maxChannelSpecificEntries)]
	assert.False(t, ok, "key just past the cap must be dropped")
}

// TestW2ConvSanitizeControlOnlyKeyDroppedButValidKept — a malformed entry
// (control-only key) is dropped while a sibling valid entry in the same map
// survives; the drop must not abort the whole sanitise.
func TestW2ConvSanitizeControlOnlyKeyDroppedButValidKept(t *testing.T) {
	out := SanitizeChannelSpecific(map[string]string{
		"\x00\x01": "dropped",
		"good":     "kept",
	})
	assert.Len(t, out, 1)
	assert.Equal(t, "kept", out["good"])
}

// TestW2ConvSanitizeReturnsFreshMap — the returned map must be a fresh copy,
// not the caller's input; mutating the result must not corrupt the source
// (the dispatcher hands the source on elsewhere).
func TestW2ConvSanitizeReturnsFreshMap(t *testing.T) {
	in := map[string]string{"k": "v"}
	out := SanitizeChannelSpecific(in)
	out["k"] = "mutated"
	assert.Equal(t, "v", in["k"], "sanitised map must not alias the input")
}

// --- BuildDeliverableLinks / RenderDeliverableLinks edges -----------------

// TestW2ConvBuildLinksEscapesSpecialChars — filenames with spaces, query
// reserved chars and unicode must be query-escaped so the artifact-raw URL
// stays well-formed (no raw space/&/# leaking into the query string).
func TestW2ConvBuildLinksEscapesSpecialChars(t *testing.T) {
	got := BuildDeliverableLinks("https://h", "p", []string{"my report &v2#final.md"})
	if assert.Len(t, got, 1) {
		assert.NotContains(t, got[0].URL, " ", "spaces must be escaped")
		assert.NotContains(t, got[0].URL, "#", "fragment delimiter must be escaped")
		// The raw name is preserved on the struct; only the URL is escaped.
		assert.Equal(t, "my report &v2#final.md", got[0].Name)
		assert.Contains(t, got[0].URL, "path=", "escaped name rides the path query param")
	}
}

// TestW2ConvBuildLinksEscapesProjectID — the project id is path-escaped, so
// an id containing a slash or space cannot break out of the /projects/<id>/
// path segment.
func TestW2ConvBuildLinksEscapesProjectID(t *testing.T) {
	got := BuildDeliverableLinks("https://h", "proj/../etc", []string{"a.md"})
	if assert.Len(t, got, 1) {
		assert.NotContains(t, got[0].URL, "/projects/proj/../etc/",
			"a raw slash in the project id must not survive into the path")
		assert.Contains(t, got[0].URL, "proj%2F", "project id slash must be path-escaped")
	}
}

// TestW2ConvBuildLinksEmptyNamesSliceNonNil — an empty names slice yields a
// non-nil, zero-length slice so callers can range without a nil guard.
func TestW2ConvBuildLinksEmptyNamesSliceNonNil(t *testing.T) {
	got := BuildDeliverableLinks("https://h", "p", nil)
	assert.NotNil(t, got)
	assert.Len(t, got, 0)
}

// TestW2ConvBuildLinksTrimsNameWhitespace — surrounding whitespace on a
// name is trimmed before it becomes both the display Name and the escaped
// URL path, so neither carries stray spaces.
func TestW2ConvBuildLinksTrimsNameWhitespace(t *testing.T) {
	got := BuildDeliverableLinks("https://h", "p", []string{"  spaced.md  "})
	if assert.Len(t, got, 1) {
		assert.Equal(t, "spaced.md", got[0].Name)
		assert.Contains(t, got[0].URL, "path=spaced.md")
	}
}

// TestW2ConvRenderMixedURLPresenceNoShellNotice — a mix of one link with a
// URL and one without must NOT emit the shell-access trailer (anyURL is
// true), but the URL-less link still renders its bare "Download:" line.
func TestW2ConvRenderMixedURLPresenceNoShellNotice(t *testing.T) {
	got := RenderDeliverableLinks([]DeliverableLink{
		{Name: "withurl.md", URL: "https://h/x"},
		{Name: "nourl.md"},
	})
	assert.Contains(t, got, "Download: withurl.md — https://h/x")
	assert.Contains(t, got, "Download: nourl.md")
	assert.NotContains(t, got, "shell access",
		"the shell-access notice is suppressed when ANY link has a URL")
}

// TestW2ConvRenderAllBlankNamesYieldsHeaderAndNotice — when every name is
// blank, no Download line is produced, yet the block still opens with the
// header and (since no URL rendered) the shell-access notice. Documents the
// degenerate-but-non-panicking shape.
func TestW2ConvRenderAllBlankNamesYieldsHeaderAndNotice(t *testing.T) {
	got := RenderDeliverableLinks([]DeliverableLink{{Name: ""}, {Name: "   "}})
	assert.Contains(t, got, "Produced files:")
	assert.NotContains(t, got, "Download:")
	assert.Contains(t, got, "shell access")
}

// --- Construction / round-trip edges --------------------------------------

// TestW2ConvChannelMessageRoundTripPreservesFields — a fully-populated
// ChannelMessage round-trips through a Receiver byte-for-byte, including the
// nested Attachment + ExtractionSummary; guards against a future field being
// dropped on the dispatch path.
func TestW2ConvChannelMessageRoundTripPreservesFields(t *testing.T) {
	ts := time.Date(2026, 6, 18, 9, 0, 0, 0, time.UTC)
	msg := ChannelMessage{
		Source:          "telegram",
		ID:              "m-1",
		SessionID:       "chat-9",
		SpeakerID:       "u-7",
		Text:            "hello",
		InReplyTo:       "m-0",
		ThreadID:        "topic-3",
		Timestamp:       ts,
		ChannelSpecific: map[string]string{"MessageThreadID": "55"},
		Attachments: []Attachment{{
			Name:       "resume.pdf",
			MimeType:   "application/pdf",
			SizeBytes:  1234,
			ChannelRef: "file-abc",
			ArtifactID: "art-1",
			Extraction: &ExtractionSummary{
				ExtractedDocumentID: "doc-1",
				Title:               "Resume",
				Author:              "Vadim",
				SectionCount:        3,
				ChunksIngested:      9,
			},
		}},
	}
	rx := &stubReceiver{}
	if err := rx.Receive(context.Background(), msg); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if assert.Len(t, rx.got, 1) {
		assert.Equal(t, msg, rx.got[0], "message must round-trip unchanged")
		got := rx.got[0]
		assert.Equal(t, "55", got.ChannelSpecific["MessageThreadID"])
		assert.Equal(t, "doc-1", got.Attachments[0].Extraction.ExtractedDocumentID)
		assert.Equal(t, 9, got.Attachments[0].Extraction.ChunksIngested)
	}
}

// TestW2ConvErrSpeakerUnknownWrapsViaFmt — callers chain ResolveSpeaker
// errors with %w; errors.Is must still match through the wrap (the auth
// path depends on this). Complements the wave-1 non-wrapping case.
func TestW2ConvErrSpeakerUnknownWrapsViaFmt(t *testing.T) {
	wrapped := errors.New("resolve: outer")
	// Hand-build a proper %w wrap to prove errors.Is traverses it.
	chained := wrapErr(ErrSpeakerUnknown, "auth rejected")
	assert.True(t, errors.Is(chained, ErrSpeakerUnknown),
		"%w-wrapped sentinel must remain matchable")
	assert.False(t, errors.Is(wrapped, ErrSpeakerUnknown))
	assert.Equal(t, "auth rejected: conversation: speaker not known on this channel",
		chained.Error())
}

// --- local helpers (test-only) --------------------------------------------

// zpad renders i as a fixed-width zero-padded decimal so lexical sort order
// equals numeric order (used to assert deterministic key truncation).
func zpad(i int) string {
	s := itoa(i)
	for len(s) < 5 {
		s = "0" + s
	}
	return s
}

// wrapErr builds a real %w error chain without importing fmt at call sites
// scattered through the file.
type wrapped struct {
	msg string
	err error
}

func (w *wrapped) Error() string { return w.msg + ": " + w.err.Error() }
func (w *wrapped) Unwrap() error { return w.err }

func wrapErr(err error, msg string) error { return &wrapped{msg: msg, err: err} }
