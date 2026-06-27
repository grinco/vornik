package dispatcher

import (
	"strings"
	"testing"
	"time"
)

// twohundred-core branch: high-value unit tests for under-covered
// pure helpers in package dispatcher — reminder time resolution,
// operator-link code formatting, profile scalar coercion, and the
// tool-search query tokeniser. All assert current behaviour.

// --- resolveReminderFireAt -------------------------------------------------
//
// Only the both-missing branch was previously exercised
// (TestResolveReminderFireAt_BothMissing). These cover the two
// happy paths, the precedence rule, and malformed RFC3339.

func TestResolveReminderFireAt_RFC3339Parsed(t *testing.T) {
	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	args := setReminderArgs{FireAtRFC3339: "2026-05-24T09:00:00+02:00"}
	got, err := resolveReminderFireAt(args, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := time.Date(2026, 5, 24, 9, 0, 0, 0, time.FixedZone("", 2*3600))
	if !got.Equal(want) {
		t.Errorf("fire_at = %v, want equal to %v", got, want)
	}
}

func TestResolveReminderFireAt_MalformedRFC3339Errors(t *testing.T) {
	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	got, err := resolveReminderFireAt(setReminderArgs{FireAtRFC3339: "next tuesday"}, now)
	if err == nil {
		t.Fatalf("expected parse error for non-RFC3339 input")
	}
	if !got.IsZero() {
		t.Errorf("error path must return zero time, got %v", got)
	}
	// Error text echoes the bad value so the LLM can self-correct.
	if !strings.Contains(err.Error(), "next tuesday") {
		t.Errorf("error should quote the offending value; got %q", err.Error())
	}
}

func TestResolveReminderFireAt_SecondsOffsetFromNow(t *testing.T) {
	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	got, err := resolveReminderFireAt(setReminderArgs{FireInSeconds: 90}, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := now.Add(90 * time.Second); !got.Equal(want) {
		t.Errorf("fire_in_seconds = %v, want %v", got, want)
	}
}

// TestResolveReminderFireAt_RFC3339TakesPrecedence: when both fields
// are supplied, the absolute timestamp wins over the relative offset.
func TestResolveReminderFireAt_RFC3339TakesPrecedence(t *testing.T) {
	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	args := setReminderArgs{
		FireAtRFC3339: "2026-06-17T15:00:00Z",
		FireInSeconds: 30,
	}
	got, err := resolveReminderFireAt(args, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := time.Date(2026, 6, 17, 15, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("expected RFC3339 to win, got %v want %v", got, want)
	}
}

// TestResolveReminderFireAt_NonPositiveSecondsRejected: zero/negative
// offsets fall through to the validation error (same surface as both
// fields missing).
func TestResolveReminderFireAt_NonPositiveSecondsRejected(t *testing.T) {
	now := time.Now()
	for _, secs := range []int64{0, -5} {
		if _, err := resolveReminderFireAt(setReminderArgs{FireInSeconds: secs}, now); err == nil {
			t.Errorf("fire_in_seconds=%d should be rejected", secs)
		}
	}
}

// --- formatOperatorLinkCode ------------------------------------------------
//
// Untested before. normaliseOperatorLinkCode (its inverse) is
// covered; this pins the display side and the round-trip.

func TestFormatOperatorLinkCode_InsertsDash(t *testing.T) {
	if got := formatOperatorLinkCode("ABCD1234"); got != "ABCD-1234" {
		t.Errorf("formatOperatorLinkCode(ABCD1234) = %q, want ABCD-1234", got)
	}
}

// Codes shorter than 8 chars are returned verbatim (no dash) so the
// formatter never produces a misleading "XXXX-" with an empty tail.
func TestFormatOperatorLinkCode_ShortReturnedVerbatim(t *testing.T) {
	for _, in := range []string{"", "AB", "ABCDEFG"} {
		if got := formatOperatorLinkCode(in); got != in {
			t.Errorf("formatOperatorLinkCode(%q) = %q, want unchanged", in, got)
		}
	}
}

// Round-trip: a freshly generated code, formatted for display, must
// normalise back to the original 8-char value the store matches on.
func TestFormatOperatorLinkCode_RoundTripsThroughNormalise(t *testing.T) {
	raw := generateOperatorLinkCode()
	if len(raw) != 8 {
		t.Fatalf("generateOperatorLinkCode returned %d chars, want 8: %q", len(raw), raw)
	}
	display := formatOperatorLinkCode(raw)
	if !strings.Contains(display, "-") {
		t.Fatalf("display form should carry the cosmetic dash: %q", display)
	}
	if got := normaliseOperatorLinkCode(display); got != raw {
		t.Errorf("normalise(format(%q)) = %q, want round-trip", raw, got)
	}
}

// --- scalarToString --------------------------------------------------------
//
// 40% coverage before — only the string branch was hit indirectly.
// This pins all four arms: string (trimmed), float64, bool, and the
// default (maps/slices/nil → empty).

func TestScalarToString_AllArms(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"string trimmed", "  hi  ", "hi"},
		{"empty string", "", ""},
		{"float integer-valued", float64(42), "42"},
		{"float fractional", float64(3.5), "3.5"},
		{"bool true", true, "true"},
		{"bool false", false, "false"},
		{"map skipped", map[string]any{"k": "v"}, ""},
		{"slice skipped", []any{1, 2}, ""},
		{"nil skipped", nil, ""},
		{"int not float64 skipped", 7, ""}, // JSON numbers arrive as float64
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := scalarToString(c.in); got != c.want {
				t.Errorf("scalarToString(%#v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// --- tokeniseSearchQuery ---------------------------------------------------
//
// The tool_search query tokeniser. Indirectly exercised via
// scoreTools; this pins the documented rules directly: lower-case,
// alnum runs only, 1-char tokens dropped, empty/blank → nil.

func TestTokeniseSearchQuery_LowercasesAndSplitsOnPunctuation(t *testing.T) {
	got := tokeniseSearchQuery("Send Email, please!")
	want := []string{"send", "email", "please"}
	assertTokens(t, got, want)
}

// 1-char tokens match too eagerly and are dropped; surrounding
// multi-char tokens survive. Digits are kept as discriminators.
func TestTokeniseSearchQuery_DropsSingleCharTokensKeepsDigits(t *testing.T) {
	got := tokeniseSearchQuery("a gmail v2 x order99")
	want := []string{"gmail", "v2", "order99"}
	assertTokens(t, got, want)
}

// Truly blank input (empty / whitespace-only) short-circuits to nil
// via the TrimSpace guard. Punctuation-only input is NOT blank, so it
// passes the guard and falls through to an empty (zero-length) token
// list after every run is dropped as <2 chars — pinning the distinct
// nil-vs-empty behaviour of the two paths.
func TestTokeniseSearchQuery_BlankReturnsNil(t *testing.T) {
	for _, in := range []string{"", "   ", "\t\n"} {
		if got := tokeniseSearchQuery(in); got != nil {
			t.Errorf("tokeniseSearchQuery(%q) = %v, want nil", in, got)
		}
	}
}

func TestTokeniseSearchQuery_PunctuationOnlyYieldsNoTokens(t *testing.T) {
	got := tokeniseSearchQuery("-- , . !")
	if len(got) != 0 {
		t.Errorf("tokeniseSearchQuery punct-only = %v, want zero tokens", got)
	}
}

// Unicode letters/digits are retained and lower-cased; only one-char
// runs are pruned, so accented multi-letter words survive.
func TestTokeniseSearchQuery_UnicodeLettersRetained(t *testing.T) {
	got := tokeniseSearchQuery("Café Über 123")
	want := []string{"café", "über", "123"}
	assertTokens(t, got, want)
}

func assertTokens(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("token count = %d (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("token[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}
