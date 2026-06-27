// Package ui: tests for the chat-audit hallucination signals
// drill-down. Decoder + severity mapping pinned here; the handler
// + template integration is covered by the existing admin chat
// audit test surface.
package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// stubChatAuditRepoSignals returns one canned audit row with a
// non-empty HallucinationSignalsJSON so the handler tests can pin
// the badge + drill-down rendering paths. Implements
// persistence.ChatAuditRepository.
type stubChatAuditRepoSignals struct{}

func (stubChatAuditRepoSignals) Insert(_ context.Context, _ *persistence.ChatAuditEntry) error {
	return nil
}
func (stubChatAuditRepoSignals) SavePrompt(_ context.Context, _, _ string) error { return nil }
func (stubChatAuditRepoSignals) GetPrompt(_ context.Context, _ string) (string, error) {
	return "", nil
}
func (stubChatAuditRepoSignals) List(_ context.Context, _ persistence.ChatAuditFilter) ([]*persistence.ChatAuditEntry, error) {
	return []*persistence.ChatAuditEntry{
		{
			ID:        "row-1",
			Timestamp: time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC),
			ChatID:    "chat-1",
			ProjectID: "proj-1",
			RoleUsed:  "dispatcher",
			Model:     "minimax.minimax-m2",
			HallucinationSignalsJSON: `[{
				"detector":"url_not_fetched",
				"severity":"high",
				"claim_type":"url",
				"claim_value":"https://example.invalid/x",
				"sentence":"I fetched the page at https://example.invalid/x",
				"evidence_searched":"tool_audit (0 entries)",
				"detail":"URL claimed to be fetched but no tool call observed",
				"recorded_at":"2026-05-20T12:00:01Z"
			}]`,
		},
	}, nil
}

// TestParseHallucinationSignals_Empty — empty audit row decodes to
// nil so the template hides the "Hallucination signals" section.
// Operators who chat with the bot 100× a day shouldn't see an
// empty header on every uneventful turn.
func TestParseHallucinationSignals_Empty(t *testing.T) {
	if got := parseHallucinationSignals(""); got != nil {
		t.Errorf("empty input: expected nil, got %v", got)
	}
}

// TestParseHallucinationSignals_Malformed — malformed JSON also
// returns nil. The raw column is still queryable via the audit
// table directly; the parsed drill-down just hides the panel
// rather than 500-ing.
func TestParseHallucinationSignals_Malformed(t *testing.T) {
	if got := parseHallucinationSignals("not json at all"); got != nil {
		t.Errorf("malformed input: expected nil, got %v", got)
	}
}

// TestParseHallucinationSignals_HappyPath pins the full field
// shape — Detector, Severity (+ pre-mapped SeverityClass),
// ClaimType, ClaimValue, Sentence, EvidenceSearched, Detail,
// RecordedAt (formatted). One canonical signal covers the
// rendering contract that the chat-audit drill-down template
// consumes.
func TestParseHallucinationSignals_HappyPath(t *testing.T) {
	in := `[{
		"detector":"url_not_fetched",
		"severity":"high",
		"claim_type":"url",
		"claim_value":"https://example.invalid/x",
		"sentence":"I fetched the docs from https://example.invalid/x",
		"evidence_searched":"tool_audit (3 entries), artifacts (0)",
		"detail":"URL claimed to be fetched but no tool call observed",
		"recorded_at":"2026-05-20T15:30:00Z"
	}]`
	got := parseHallucinationSignals(in)
	if len(got) != 1 {
		t.Fatalf("expected 1 signal, got %d", len(got))
	}
	r := got[0]
	if r.Detector != "url_not_fetched" {
		t.Errorf("Detector = %q", r.Detector)
	}
	if r.Severity != "high" || r.SeverityClass != "rose" {
		t.Errorf("severity mapping: %q / %q (want high/rose)", r.Severity, r.SeverityClass)
	}
	if r.ClaimType != "url" || r.ClaimValue != "https://example.invalid/x" {
		t.Errorf("claim fields: %+v", r)
	}
	if !strings.Contains(r.Sentence, "I fetched the docs") {
		t.Errorf("Sentence lost: %q", r.Sentence)
	}
	if !strings.Contains(r.EvidenceSearched, "tool_audit") {
		t.Errorf("EvidenceSearched lost: %q", r.EvidenceSearched)
	}
	if r.RecordedAt != "2026-05-20 15:30:00" {
		t.Errorf("RecordedAt format: %q (want '2026-05-20 15:30:00')", r.RecordedAt)
	}
}

// TestParseHallucinationSignals_AllSeverities — verify the
// rose/amber/gray mapping for high/warn/info plus the fallback
// for unknown severities. Pins the template's badge-tint
// contract.
func TestParseHallucinationSignals_AllSeverities(t *testing.T) {
	in := `[
		{"detector":"a","severity":"high","detail":"x"},
		{"detector":"b","severity":"warn","detail":"y"},
		{"detector":"c","severity":"info","detail":"z"},
		{"detector":"d","severity":"unknown","detail":"w"}
	]`
	got := parseHallucinationSignals(in)
	if len(got) != 4 {
		t.Fatalf("expected 4, got %d", len(got))
	}
	want := []string{"rose", "amber", "gray", "gray"}
	for i, w := range want {
		if got[i].SeverityClass != w {
			t.Errorf("row %d (%s): SeverityClass = %q, want %q", i, got[i].Severity, got[i].SeverityClass, w)
		}
	}
}

// TestHallucinationSeverityBadgeClass_DirectMapping covers the
// helper independently of the parser — defensive against a future
// caller using it from a place other than parseHallucinationSignals.
func TestHallucinationSeverityBadgeClass_DirectMapping(t *testing.T) {
	cases := map[string]string{
		"high":   "rose",
		"warn":   "amber",
		"info":   "gray",
		"":       "gray",
		"medium": "gray",
	}
	for sev, want := range cases {
		if got := hallucinationSeverityBadgeClass(sev); got != want {
			t.Errorf("hallucinationSeverityBadgeClass(%q) = %q, want %q", sev, got, want)
		}
	}
}

// TestAdminChatAudit_DrillDown_RendersSignals — end-to-end render
// pinning that the drill-down panel shows the signals card with
// detector + severity + detail when the selected row has
// non-empty HallucinationSignalsJSON. Uses a stub repo to inject
// the audit row.
func TestAdminChatAudit_DrillDown_RendersSignals(t *testing.T) {
	srv := NewServer(WithAdminChatAuditRepository(&stubChatAuditRepoSignals{}))
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/chat-audit?id=row-1", nil)
	rec := httptest.NewRecorder()
	srv.AdminChatAudit(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%q", rec.Code, firstN(rec.Body.String(), 300))
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Hallucination signals",
		"url_not_fetched",
		"https://example.invalid/x",
		"URL claimed to be fetched",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing fragment %q in drill-down body", want)
		}
	}
}

// TestAdminChatAudit_ListShowsBadge — turn rows on the list page
// must show the ⚠ badge when HallucinationSignalsJSON is
// non-empty. Without this signal in the list column an operator
// has to drill into EVERY row to find the ones with signals — the
// badge is the navigational affordance.
func TestAdminChatAudit_ListShowsBadge(t *testing.T) {
	srv := NewServer(WithAdminChatAuditRepository(&stubChatAuditRepoSignals{}))
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/chat-audit", nil)
	rec := httptest.NewRecorder()
	srv.AdminChatAudit(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "⚠") {
		t.Errorf("expected ⚠ badge in list output; body fragment %q", firstN(body, 400))
	}
}

func (s *stubChatAuditRepoSignals) GetByID(_ context.Context, _ string) (*persistence.ChatAuditEntry, error) {
	return nil, persistence.ErrNotFound
}
