package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/slack"
)

// The button value is the only thing tying a tap back to a task. Task ids
// contain no colon, so the index is after the LAST one — the same rule the
// Telegram decoder applies. A malformed value must be refused, never guessed at.
func TestParseSteerAction(t *testing.T) {
	for _, tc := range []struct {
		in       string
		wantTask string
		wantIdx  int
		wantOK   bool
	}{
		{"steer:c:task_20260806212011_77b90a7d:2", "task_20260806212011_77b90a7d", 2, true},
		{"steer:c:task_1:0", "task_1", 0, true},
		{"steer:approve:task_1", "", 0, false}, // different action namespace
		{"steer:c:task_1", "", 0, false},       // no index
		{"steer:c:task_1:", "", 0, false},      // empty index
		{"steer:c:task_1:x", "", 0, false},     // non-numeric index
		{"steer:c::1", "", 0, false},           // empty task id
		{"", "", 0, false},
		{"nonsense", "", 0, false},
	} {
		gotTask, gotIdx, ok := parseSteerAction(tc.in)
		if ok != tc.wantOK || gotTask != tc.wantTask || gotIdx != tc.wantIdx {
			t.Errorf("parseSteerAction(%q) = (%q,%d,%v), want (%q,%d,%v)",
				tc.in, gotTask, gotIdx, ok, tc.wantTask, tc.wantIdx, tc.wantOK)
		}
	}
}

// A negative index cannot be produced by parseSteerAction (the digit loop
// rejects '-'), which is what keeps slackOptionIDAt's bounds check from ever
// seeing one. Pinned because the option lookup trusts it.
func TestParseSteerAction_RejectsNegativeIndex(t *testing.T) {
	if _, _, ok := parseSteerAction("steer:c:task_1:-1"); ok {
		t.Error("a negative index must not parse — the option lookup relies on it")
	}
}

// An unauthorized clicker and an unknown task must be indistinguishable. A
// distinct "not authorized" would confirm the task exists and that somebody
// else owns it — and this button is visible to everyone in the channel.
func TestRefusalText_DoesNotDiscloseExistenceOrOwnership(t *testing.T) {
	for _, leak := range []string{"authoriz", "owner", "permission", "belongs", "denied"} {
		if containsFold(refusalText, leak) {
			t.Errorf("refusal text %q leaks %q — it must read the same as an unknown task",
				refusalText, leak)
		}
	}
}

func containsFold(haystack, needle string) bool {
	h, n := []rune(haystack), []rune(needle)
	if len(n) == 0 || len(n) > len(h) {
		return false
	}
	lower := func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		return r
	}
	for i := 0; i+len(n) <= len(h); i++ {
		match := true
		for j := range n {
			if lower(h[i+j]) != lower(n[j]) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// stubParser stands in for *slack.Channel: it is the SIGNATURE-VERIFIED half,
// so returning an Interaction here means "this payload was authentic".
type stubParser struct {
	got slack.Interaction
	err error
}

func (s stubParser) ParseInteraction(_ *http.Request, _ time.Time) (slack.Interaction, error) {
	return s.got, s.err
}

// The route must be MOUNTED and must ack. Wiring is the part unit tests on the
// handler cannot prove: a handler nobody routes to is the same as no feature.
func TestSlackInteractions_RouteIsMountedAndAcksEmpty200(t *testing.T) {
	s := NewServer(WithSlackInteractionParser(stubParser{
		// No repos wired on this server, so the work goroutine bails
		// immediately after the ack — which is exactly what we are asserting:
		// the ack does not depend on anything downstream succeeding.
		got: slack.Interaction{UserID: "U_alice", ActionValue: "steer:c:task_1:0"},
	}))
	h := SetupRoutes(s, &config.Config{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/slack/interactions", strings.NewReader("payload=%7B%7D"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — Slack shows the operator a timeout otherwise; body=%q",
			w.Code, w.Body.String())
	}
	if body := w.Body.String(); body != "" {
		t.Errorf("ack body = %q, want empty — the outcome rides response_url", body)
	}
}

// A payload that fails verification must never reach the answering half.
func TestSlackInteractions_RefusesUnverifiedPayload(t *testing.T) {
	s := NewServer(WithSlackInteractionParser(stubParser{err: slack.ErrNotAnInteraction}))
	h := SetupRoutes(s, &config.Config{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/slack/interactions", strings.NewReader("payload=%7B%7D"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for an unverified interaction", w.Code)
	}
}

// Unwired Slack → the route must not exist at all, rather than answering with
// an endpoint that cannot verify anything.
func TestSlackInteractions_RouteAbsentWhenSlackNotConfigured(t *testing.T) {
	h := SetupRoutes(NewServer(), &config.Config{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/slack/interactions", strings.NewReader(""))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Error("the interactions route must not be mounted when no Slack parser is wired")
	}
}

// go/request-forgery (critical) on the public CE repo, 2026-08-15. response_url
// arrives inside the interaction payload — attacker-chosen data the daemon then
// POSTs the reply text to. HMAC verification proves WHO sent the payload; it
// says nothing about where the URL points, so the destination needs its own
// check.
func TestIsSlackResponseURL(t *testing.T) {
	allowed := []string{
		"https://hooks.slack.com/actions/T1/2/abc",
		"https://HOOKS.SLACK.COM/actions/T1/2/abc", // host compare is case-insensitive
	}
	for _, u := range allowed {
		if !isSlackResponseURL(u) {
			t.Errorf("isSlackResponseURL(%q) = false, want true", u)
		}
	}

	refused := map[string]string{
		"http://hooks.slack.com/actions/x":    "plaintext would put the reply on the wire in clear",
		"https://evil.tld/collect":            "unrelated host",
		"https://slack.com.evil.tld/collect":  "suffix-match bypass — the classic way this check is written wrong",
		"https://hooks.slack.com.evil.tld/x":  "same, one label deeper",
		"https://hooks.slack.com@evil.tld/x":  "userinfo before the real host",
		"https://evil.tld/?x=hooks.slack.com": "allowed host only in the query",
		"https://evil.tld#hooks.slack.com":    "allowed host only in the fragment",
		"//hooks.slack.com/actions/x":         "scheme-relative, so no https guarantee",
		"":                                    "empty",
		"   ":                                 "blank",
		"::not a url::":                       "unparseable",
	}
	for u, why := range refused {
		if isSlackResponseURL(u) {
			t.Errorf("isSlackResponseURL(%q) = true, want false (%s)", u, why)
		}
	}
}
