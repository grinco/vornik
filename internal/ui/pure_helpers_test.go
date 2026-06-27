// Coverage for the ui package's pure helper functions. The handlers
// they support are HTTP-bound and harder to unit test in isolation,
// but these are data-in / data-out and worth the narrow tests so a
// future refactor that changes (e.g.) the verdict→class mapping
// breaks the suite loudly.

package ui

import (
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// TestClassColour pins the memory-scatter colour palette. The
// legend ↔ scatter pairing on /ui/memory/<project>'s vector
// scatter relies on stable hex values — a regression here would
// silently desync the legend dots from the scatter points.
func TestClassColour(t *testing.T) {
	cases := map[string]string{
		"research":       "#60a5fa",
		"spec":           "#34d399",
		"decision":       "#a78bfa",
		"commit_msg":     "#fbbf24",
		"diagnostic":     "#f87171",
		"external_fetch": "#659157",
		"summary":        "#fb923c",
		"unclassified":   "#94a3b8",
		"weird_new":      "#6b7280", // unknown → gray fallback
		"":               "#6b7280",
	}
	for class, want := range cases {
		if got := classColour(class); got != want {
			t.Errorf("classColour(%q) = %q, want %q", class, got, want)
		}
	}
}

// TestStatusStrokeColour pins the scatter-ring-by-status palette.
// "transparent" for the default path means unverified points
// render without a ring; verified/superseded/refuted/legacy each
// get their own hex.
func TestStatusStrokeColour(t *testing.T) {
	cases := map[string]string{
		"verified":   "#10b981",
		"superseded": "#f59e0b",
		"refuted":    "#ef4444",
		"legacy":     "#64748b",
		"unverified": "transparent",
		"":           "transparent",
		"unknown":    "transparent",
	}
	for status, want := range cases {
		if got := statusStrokeColour(status); got != want {
			t.Errorf("statusStrokeColour(%q) = %q, want %q", status, got, want)
		}
	}
}

// TestJudgeVerdictCSSClass anchors the verdict→pill mapping used
// by the execution detail page. Adding a new verdict literal
// without extending this switch would silently render outcome-
// neutral; the test makes that a visible change.
func TestJudgeVerdictCSSClass(t *testing.T) {
	if got := judgeVerdictCSSClass(persistence.JudgeVerdictPass); got != "outcome-ok" {
		t.Errorf("pass: got %q, want outcome-ok", got)
	}
	if got := judgeVerdictCSSClass(persistence.JudgeVerdictFail); got != "outcome-bad" {
		t.Errorf("fail: got %q, want outcome-bad", got)
	}
	if got := judgeVerdictCSSClass(persistence.JudgeVerdictAbstain); got != "outcome-pending" {
		t.Errorf("abstain: got %q, want outcome-pending", got)
	}
	if got := judgeVerdictCSSClass(""); got != "outcome-neutral" {
		t.Errorf("empty: got %q, want outcome-neutral", got)
	}
	if got := judgeVerdictCSSClass("future_verdict"); got != "outcome-neutral" {
		t.Errorf("unknown: got %q, want outcome-neutral", got)
	}
}

// TestTruncateSummary covers the project-page summary clipper.
// Short strings pass through; long strings clip to n + …
func TestTruncateSummary(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"short pass-through", "hello", 10, "hello"},
		{"exact length pass-through", "12345", 5, "12345"},
		{"long clip", "abcdefghij", 5, "abcde…"},
		{"empty", "", 5, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncateSummary(tc.in, tc.n); got != tc.want {
				t.Errorf("truncateSummary(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
			}
		})
	}
}

// TestHTTPError pins the response shape: status code propagated,
// "vornik ui: " prefix prepended to the message so an operator
// hitting the page directly knows which layer rejected the
// request.
func TestHTTPError(t *testing.T) {
	rr := httptest.NewRecorder()
	httpError(rr, 404, "task not found")
	if rr.Code != 404 {
		t.Errorf("code: got %d, want 404", rr.Code)
	}
	body := rr.Body.String()
	if !contains(body, "vornik ui: task not found") {
		t.Errorf("body: got %q, want it to contain the prefixed message", body)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOfStr(haystack, needle) >= 0
}

func indexOfStr(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
