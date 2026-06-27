package dispatcher

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/hallucination"
	"vornik.io/vornik/internal/intentjudge"
	"vornik.io/vornik/internal/persistence"
)

// fivehundred-core branch, wave 3: high-value unit tests for
// still-under-covered PURE helpers in package dispatcher. These
// avoid the surface already pinned by twohundred_core_test.go
// (resolveReminderFireAt, operator-link code formatting,
// scalarToString, tokeniseSearchQuery) and by the existing
// per-feature *_test.go files. TESTS ONLY — no production change.

// --- scoreTools ------------------------------------------------------------
//
// Existing tests pin name>desc weighting, zero-score drop and empty
// query. These pin the additive multi-term accumulation and the
// alphabetic tiebreak on equal scores (the deterministic-output
// guarantee the doc comment promises).

// TestW3DispScoreTools_RepeatedTermAccumulates: a term appearing in
// both name and description on the same tool contributes both the
// 3.0 (name) and 1.0 (desc) weights, and a second distinct query
// term stacks on top. Verifies the score is additive across terms,
// not max-of.
func TestW3DispScoreTools_RepeatedTermAccumulates(t *testing.T) {
	// "order" hits name(3)+desc(1)=4; "stock" hits desc(1) only.
	// Total = 5.
	cat := []chat.Tool{
		makeMCPTool("mcp__broker__place_order", "Place a stock order on the market."),
	}
	hits := scoreTools(cat, "order stock")
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(hits))
	}
	if hits[0].score != 5.0 {
		t.Errorf("score = %v, want 5.0 (name+desc for 'order' = 4, desc for 'stock' = 1)", hits[0].score)
	}
}

// TestW3DispScoreTools_EqualScoreAlphabeticTiebreak: two tools that
// score identically must come back in ascending name order so the
// returned list (and thus tool_search output) is stable run-to-run.
func TestW3DispScoreTools_EqualScoreAlphabeticTiebreak(t *testing.T) {
	cat := []chat.Tool{
		// Insertion order deliberately reversed vs. desired output.
		makeMCPTool("mcp__zeta__gmail", "x"),
		makeMCPTool("mcp__alpha__gmail", "x"),
		makeMCPTool("mcp__mid__gmail", "x"),
	}
	hits := scoreTools(cat, "gmail") // each scores 3.0 (name only)
	if len(hits) != 3 {
		t.Fatalf("expected 3 hits, got %d", len(hits))
	}
	want := []string{"mcp__alpha__gmail", "mcp__mid__gmail", "mcp__zeta__gmail"}
	for i, w := range want {
		if got := hits[i].tool.Function.Name; got != w {
			t.Errorf("hit[%d] = %q, want %q (alphabetic tiebreak broken)", i, got, w)
		}
	}
}

// TestW3DispScoreTools_CaseInsensitiveMatch: scoring lower-cases both
// the tool fields and (via the tokeniser) the query, so an
// upper-case catalog entry still matches a lower-case query term.
func TestW3DispScoreTools_CaseInsensitiveMatch(t *testing.T) {
	cat := []chat.Tool{makeMCPTool("mcp__X__SEND_EMAIL", "Sends EMAIL")}
	hits := scoreTools(cat, "email")
	if len(hits) != 1 {
		t.Fatalf("expected case-insensitive match, got %d hits", len(hits))
	}
	if hits[0].score != 4.0 {
		t.Errorf("score = %v, want 4.0 (name+desc match, case-folded)", hits[0].score)
	}
}

// --- applyDeferredLoading --------------------------------------------------
//
// Existing tests cover below/above-threshold, expanded surfacing,
// chatID==0, and degraded-tier. This pins the nil-store early return
// in the above-threshold branch: builtins + tool_search only, with
// NO MCP tools (a nil store can answer no `contains` query).

func TestW3DispApplyDeferredLoading_NilStoreAboveThresholdYieldsSearchOnly(t *testing.T) {
	builtin := []chat.Tool{makeMCPTool("create_task", "b")}
	mcp := make([]chat.Tool, 0, 25)
	for i := 0; i < 25; i++ {
		mcp = append(mcp, makeMCPTool("mcp__svc__t", "d"))
	}
	// threshold 20, len(mcp)=25 > 20, chatID!=0 → above-threshold path;
	// store nil → must return builtin + tool_search only.
	got := applyDeferredLoading(builtin, mcp, nil, 7, 20)
	if len(got) != 2 {
		t.Fatalf("nil store above threshold: len=%d, want 2 (builtin + tool_search)", len(got))
	}
	if !containsToolByName(got, "create_task") {
		t.Error("builtin tool must remain visible")
	}
	if !containsToolByName(got, ToolSearchName) {
		t.Errorf("tool_search helper must be surfaced; got %+v", toolNames(got))
	}
}

// TestW3DispApplyDeferredLoading_ZeroThresholdDefaults: a threshold of
// 0 (or negative) falls back to DefaultDeferredToolThreshold rather
// than treating "everything > 0" as above-threshold. With 5 MCP
// tools (< default 20) and a 0 threshold, every tool stays visible.
func TestW3DispApplyDeferredLoading_ZeroThresholdDefaults(t *testing.T) {
	builtin := []chat.Tool{makeMCPTool("create_task", "b")}
	mcp := []chat.Tool{
		makeMCPTool("mcp__a__1", "d"), makeMCPTool("mcp__a__2", "d"),
		makeMCPTool("mcp__a__3", "d"), makeMCPTool("mcp__a__4", "d"),
		makeMCPTool("mcp__a__5", "d"),
	}
	store := newExpandedToolStore()
	got := applyDeferredLoading(builtin, mcp, store, 9, 0)
	if len(got) != len(builtin)+len(mcp) {
		t.Fatalf("zero threshold should default to %d and keep all visible; len=%d want %d",
			DefaultDeferredToolThreshold, len(got), len(builtin)+len(mcp))
	}
	if containsToolByName(got, ToolSearchName) {
		t.Error("tool_search must NOT be injected below the (defaulted) threshold")
	}
}

// --- extractURLsFromText ---------------------------------------------------
//
// 87.5% before. Pins JSON-embedded extraction, trailing-punctuation
// stripping, multi-URL, and the empty-input path.

func TestW3DispExtractURLsFromText_JSONEmbeddedAndTrailingPunct(t *testing.T) {
	text := `See {"url":"https://api.example.com/v1/x"} and visit https://docs.vornik.io/guide.`
	got := extractURLsFromText(text)
	want := []string{
		"https://api.example.com/v1/x", // closing quote+brace stripped by regex class
		"https://docs.vornik.io/guide", // trailing period stripped
	}
	if len(got) != len(want) {
		t.Fatalf("got %d URLs %v, want %d %v", len(got), got, len(want), want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("url[%d] = %q, want %q", i, got[i], w)
		}
	}
}

// TestW3DispExtractURLsFromText_NoURLsReturnsEmptyNonNil: text with no
// URL returns a zero-length (non-nil) slice so callers can range
// without a nil check.
func TestW3DispExtractURLsFromText_NoURLsReturnsEmpty(t *testing.T) {
	got := extractURLsFromText("plain text, no links here at all")
	if got == nil {
		t.Fatal("want non-nil empty slice, got nil")
	}
	if len(got) != 0 {
		t.Errorf("want 0 URLs, got %v", got)
	}
}

// --- formatHallucinationRetryPrompt ----------------------------------------
//
// 83.3% before. Pins the cap-at-5 behaviour, the warn-severity skip,
// and that each blocking claim's type/value/detail land in the prose.

func TestW3DispFormatHallucinationRetryPrompt_CapsAtFiveAndSkipsNonBlocking(t *testing.T) {
	var sigs []hallucination.Signal
	// One warn (must be skipped) ...
	sigs = append(sigs, hallucination.Signal{
		Severity: hallucination.SeverityWarn, ClaimType: "numeric",
		ClaimValue: "42", Detail: "soft mismatch",
	})
	// ... then 6 high-severity (only the first 5 should render).
	for i := 0; i < 6; i++ {
		sigs = append(sigs, hallucination.Signal{
			Severity:   hallucination.SeverityHigh,
			ClaimType:  "url",
			ClaimValue: "https://h" + string(rune('0'+i)) + ".example",
			Detail:     "not fetched",
		})
	}
	out := formatHallucinationRetryPrompt(sigs)

	if strings.Contains(out, "soft mismatch") {
		t.Error("warn-severity signal must be skipped from the retry prompt")
	}
	// Exactly 5 bullet lines rendered.
	if n := strings.Count(out, "\n  - "); n != 5 {
		t.Errorf("rendered bullet count = %d, want 5 (cap)", n)
	}
	// 6th high signal (index 5) must be cut.
	if strings.Contains(out, "https://h5.example") {
		t.Error("6th blocking claim should be dropped by the cap")
	}
	if !strings.Contains(out, "https://h0.example") || !strings.Contains(out, "(not fetched)") {
		t.Error("first blocking claim's value + detail must be present")
	}
	if !strings.Contains(out, "Please retry") {
		t.Error("trailer instruction missing")
	}
}

// --- retainBlockingSignals / formatUserWarningBanner -----------------------
//
// Pin the interplay: retainBlockingSignals drops warn/info; the
// banner singular/plural form keys off the retained count.

func TestW3DispRetainBlockingSignalsThenBanner(t *testing.T) {
	all := []hallucination.Signal{
		{Severity: hallucination.SeverityInfo},
		{Severity: hallucination.SeverityHigh},
		{Severity: hallucination.SeverityWarn},
		{Severity: hallucination.SeverityHigh},
	}
	blocking := retainBlockingSignals(all)
	if len(blocking) != 2 {
		t.Fatalf("retained %d, want 2 high-severity", len(blocking))
	}

	plural := formatUserWarningBanner(blocking)
	if !strings.Contains(plural, "2 unsupported claims") {
		t.Errorf("plural banner missing count phrasing: %q", plural)
	}
	singular := formatUserWarningBanner(blocking[:1])
	if !strings.Contains(singular, "an unsupported claim") || strings.Contains(singular, "claims") {
		t.Errorf("singular banner wrong: %q", singular)
	}
}

// --- originating-channel context round-trip --------------------------------
//
// originatingChannelFromContext was 83.3%; the empty-context and
// wrong-type-value branches were the gap.

func TestW3DispOriginatingChannel_RoundTripAndEmptyPaths(t *testing.T) {
	base := context.Background()

	// Both empty → no key set; reader returns empties.
	if ctx := withOriginatingChannel(base, "", ""); ctx != base {
		t.Error("empty channel+session must return the parent context unchanged")
	}

	// Channel only (sessionID empty) still sets the key.
	ctx := withOriginatingChannel(base, "email", "thread-123")
	ch, sid := originatingChannelFromContext(ctx)
	if ch != "email" || sid != "thread-123" {
		t.Errorf("round-trip = (%q,%q), want (email, thread-123)", ch, sid)
	}

	// Reader on a bare context returns empties, not a panic.
	if ch, sid := originatingChannelFromContext(base); ch != "" || sid != "" {
		t.Errorf("bare context = (%q,%q), want empties", ch, sid)
	}
	// nil context tolerated (held in a var so staticcheck doesn't
	// flag a literal nil-context argument; the guard is real).
	var nilCtx context.Context
	if ch, sid := originatingChannelFromContext(nilCtx); ch != "" || sid != "" {
		t.Errorf("nil context = (%q,%q), want empties", ch, sid)
	}
}

// --- operatorIDFromContext / WithOperatorID --------------------------------

func TestW3DispOperatorIDContext_RoundTripAndMisses(t *testing.T) {
	base := context.Background()

	// Empty id → no key; reader misses.
	if ctx := WithOperatorID(base, ""); ctx != base {
		t.Error("empty operator id must not derive a new context")
	}
	if _, ok := operatorIDFromContext(base); ok {
		t.Error("bare context must report no operator id")
	}
	var nilCtx context.Context
	if _, ok := operatorIDFromContext(nilCtx); ok {
		t.Error("nil context must report no operator id")
	}

	ctx := WithOperatorID(base, "op-42")
	got, ok := operatorIDFromContext(ctx)
	if !ok || got != "op-42" {
		t.Errorf("round-trip = (%q,%v), want (op-42,true)", got, ok)
	}
}

// --- reminderBelongsToCaller -----------------------------------------------
//
// 62.5% before. Pins the two accept signals (chatID match, ctx
// operator match), the nil-row guard, and the cross-operator reject.

func TestW3DispReminderBelongsToCaller(t *testing.T) {
	// nil row never belongs to anyone.
	if reminderBelongsToCaller(nil, 5, context.Background()) {
		t.Error("nil row must not belong to caller")
	}

	// chatID matches stored ChannelRef (stringified int64).
	row := &persistence.Reminder{ChannelRef: "123", OperatorID: "op-A"}
	if !reminderBelongsToCaller(row, 123, context.Background()) {
		t.Error("matching chatID must grant ownership")
	}

	// chatID mismatch AND no operator ctx → reject.
	if reminderBelongsToCaller(row, 999, context.Background()) {
		t.Error("mismatched chatID with no operator ctx must reject")
	}

	// Operator id from context matches row.OperatorID (chatID 0).
	ctx := WithOperatorID(context.Background(), "op-A")
	if !reminderBelongsToCaller(row, 0, ctx) {
		t.Error("matching operator id from context must grant ownership")
	}

	// Operator id present but DIFFERENT → cross-operator reject.
	other := WithOperatorID(context.Background(), "op-B")
	if reminderBelongsToCaller(row, 0, other) {
		t.Error("different operator id must reject (cross-operator guard)")
	}
}

// --- formatChatID ----------------------------------------------------------
//
// 93.3% before. The negative-id branch (Telegram group/channel IDs
// are negative) and zero were the gap.

func TestW3DispFormatChatID(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, ""},
		{42, "42"},
		{-1001234567890, "-1001234567890"}, // Telegram supergroup id
		{-7, "-7"},
		{9223372036854775807, "9223372036854775807"}, // max int64
	}
	for _, c := range cases {
		if got := formatChatID(c.in); got != c.want {
			t.Errorf("formatChatID(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- resolveChatID ---------------------------------------------------------
//
// Pins the three-bucket precedence: numeric ChatID > channel:session
// > empty.

func TestW3DispResolveChatID_Precedence(t *testing.T) {
	// Bucket 1: numeric wins even when channel/session also present.
	got := resolveChatID(Request{ChatID: -55, OriginatingChannel: "email", OriginatingSessionID: "x"})
	if got != "-55" {
		t.Errorf("numeric ChatID should win: got %q want -55", got)
	}

	// Bucket 2: channel:session when no numeric id.
	got = resolveChatID(Request{OriginatingChannel: "web-chat", OriginatingSessionID: "uuid-1"})
	if got != "web-chat:uuid-1" {
		t.Errorf("channel:session form wrong: %q", got)
	}

	// Bucket 2 requires BOTH parts; channel-only falls through to empty.
	if got := resolveChatID(Request{OriginatingChannel: "email"}); got != "" {
		t.Errorf("channel without session must be empty, got %q", got)
	}

	// Bucket 3: fully empty.
	if got := resolveChatID(Request{}); got != "" {
		t.Errorf("empty request must yield empty chat id, got %q", got)
	}
}

// --- safeUTF8Prefix --------------------------------------------------------
//
// 92.3% before. Pins the multi-byte boundary (won't split a rune),
// the n>=len passthrough, the n<=0 guard, and exact-fit.

func TestW3DispSafeUTF8Prefix_RuneBoundaries(t *testing.T) {
	// "héllo": h(1) é(2) l(1) l(1) o(1) = 6 bytes.
	const s = "héllo"

	// n in the MIDDLE of the 2-byte 'é' must not split it: returns "h".
	if got := safeUTF8Prefix(s, 2); got != "h" {
		t.Errorf("safeUTF8Prefix(%q, 2) = %q, want \"h\" (no split of é)", s, got)
	}
	// n exactly covering "hé" (3 bytes).
	if got := safeUTF8Prefix(s, 3); got != "hé" {
		t.Errorf("safeUTF8Prefix(%q, 3) = %q, want \"hé\"", s, got)
	}
	// n >= len → whole string.
	if got := safeUTF8Prefix(s, 100); got != s {
		t.Errorf("n>=len should return whole string, got %q", got)
	}
	// n<=0 and empty input → "".
	if got := safeUTF8Prefix(s, 0); got != "" {
		t.Errorf("n<=0 must return empty, got %q", got)
	}
	if got := safeUTF8Prefix("", 5); got != "" {
		t.Errorf("empty input must return empty, got %q", got)
	}
}

// --- renderKnownKeys -------------------------------------------------------
//
// 92.3% before. Pins allow-list filtering (unknown keys dropped),
// empty-value skip, sorted output, and the empty-map nil return.

func TestW3DispRenderKnownKeys(t *testing.T) {
	// nil/empty map → nil.
	if got := renderKnownKeys(nil); got != nil {
		t.Errorf("nil map should return nil, got %v", got)
	}

	structured := map[string]any{
		"tone":              "warm",
		"verbosity":         "  terse  ", // trimmed by scalarToString
		"time_zone":         "",          // empty → skipped
		"unknown_key":       "ignored",   // not in allow-list → dropped
		"preferred_channel": "telegram",
	}
	got := renderKnownKeys(structured)

	// Sorted; only known + non-empty keys.
	want := []string{
		"preferred_channel: telegram",
		"tone: warm",
		"verbosity: terse",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// --- toolStatusEmoji -------------------------------------------------------
//
// 40% before — only a couple of arms were hit. Pins the mcp prefix,
// a representative spread of named arms, and the default fallback.

func TestW3DispToolStatusEmoji(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"mcp__gmail__send", "🔌"}, // any mcp__ prefix
		{"memory_search", "🧠"},
		{"memory_correct", "🧠"},
		{"create_task", "📋"},
		{"send_artifact", "📎"},
		{"send_message", "✉️"},
		{"summarize_thread", "🧵"},
		{"get_conversation_window", "💬"},
		{"file_read", "📄"},
		{"file_edit", "📄"},
		{"run_shell", "⌨️"},
		{"grep", "🔎"},
		{"glob", "🗂️"},
		{"current_time", "🕒"},
		{"some_unknown_tool", "⏳"}, // default
		{"", "⏳"},
	}
	for _, c := range cases {
		if got := toolStatusEmoji(c.name); got != c.want {
			t.Errorf("toolStatusEmoji(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestW3DispToolStatusMarker_WrapsEmojiAndVerb: the marker composes
// emoji + humanized verb in "[emoji verb]" form.
func TestW3DispToolStatusMarker_WrapsEmojiAndVerb(t *testing.T) {
	got := toolStatusMarker("create_task")
	if !strings.HasPrefix(got, "[📋 ") || !strings.HasSuffix(got, "]") {
		t.Errorf("marker = %q, want [📋 <verb>] shape", got)
	}
}

// --- modelFromResponse -----------------------------------------------------

func TestW3DispModelFromResponse(t *testing.T) {
	if got := modelFromResponse(nil); got != "" {
		t.Errorf("nil response should yield empty model, got %q", got)
	}
	if got := modelFromResponse(&chat.ChatResponse{Model: "gemma-4-26b"}); got != "gemma-4-26b" {
		t.Errorf("model = %q, want gemma-4-26b", got)
	}
	if got := modelFromResponse(&chat.ChatResponse{}); got != "" {
		t.Errorf("empty model field should yield empty, got %q", got)
	}
}

// --- shouldRefine (additional edge: zero/unknown risk) ---------------------
//
// Existing TestShouldRefine pins the main matrix. This adds the
// unknown-risk arm (rank 0) which the heuristic can emit and which
// must never refine above a real floor.
func TestW3DispShouldRefine_UnknownRiskBelowAnyRealFloor(t *testing.T) {
	var unknown intentjudge.Risk = "garbage"
	if shouldRefine(unknown, intentjudge.RiskLow) {
		t.Error("unknown risk (rank 0) must not clear a Low floor (rank 1)")
	}
	// Unknown floor also ranks 0, so a Low verdict clears it.
	if !shouldRefine(intentjudge.RiskLow, unknown) {
		t.Error("Low verdict (rank 1) must clear an unknown floor (rank 0)")
	}
}

// toolNames is a tiny test helper for diagnostics.
func toolNames(tools []chat.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Function.Name)
	}
	return out
}
