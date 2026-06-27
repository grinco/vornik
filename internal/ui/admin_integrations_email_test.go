// Tests for /ui/admin/integrations/email — the per-project email
// channel inventory page. Pins the empty-state, the populated-render
// happy path, and the SessionsError surfacing contract.
package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubEmailChannelInventory drives the handler under test. The
// rows are the same shape the service-container adapter produces;
// the test asserts the handler+template renders them faithfully
// without exercising email.Channel itself.
type stubEmailChannelInventory struct {
	rows []AdminEmailChannelRow
}

func (s *stubEmailChannelInventory) EmailChannels(_ context.Context) []AdminEmailChannelRow {
	return s.rows
}

// TestAdminIntegrationsEmail_NotWired — empty Server option set
// renders the "not wired" empty state, not a 500. Operators must
// see the route is alive even when no email is configured.
func TestAdminIntegrationsEmail_NotWired(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/integrations/email", nil)
	rec := httptest.NewRecorder()
	srv.AdminIntegrationsEmail(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Email-channel inventory not wired") {
		t.Errorf("missing 'not wired' empty-state copy; body fragment %q", firstN(body, 400))
	}
}

// TestAdminIntegrationsEmail_NoChannels — inventory wired but
// reporting zero rows renders the "no projects declare an email
// block" empty state. Distinct copy from the "not wired" path so
// operators can tell the difference between misconfiguration and
// "all your projects use Telegram instead".
func TestAdminIntegrationsEmail_NoChannels(t *testing.T) {
	srv := NewServer(WithEmailChannelInventory(&stubEmailChannelInventory{}))
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/integrations/email", nil)
	rec := httptest.NewRecorder()
	srv.AdminIntegrationsEmail(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "No projects declare an") {
		t.Errorf("missing 'no projects' empty-state copy; body fragment %q", firstN(body, 400))
	}
}

// TestAdminIntegrationsEmail_HappyPath — populated row renders
// project ID, IMAP wiring, outbound badge, session row.
func TestAdminIntegrationsEmail_HappyPath(t *testing.T) {
	rows := []AdminEmailChannelRow{
		{
			ProjectID:          "ops",
			IMAPHost:           "imap.example.com",
			IMAPPort:           993,
			IMAPMailbox:        "INBOX",
			OutboundConfigured: true,
			SMTPHost:           "smtp.example.com",
			FromAddress:        "bot@example.com",
			AllowlistSize:      2,
			AttachmentCapBytes: 25 * 1024 * 1024,
			VerifyInboundAuth:  true,
			AuthPolicy:         "strict",
			Sessions: []AdminEmailSessionRow{
				{
					ID:               "<thread-1@example.com>",
					Title:            "Quarterly review",
					LastActivity:     time.Date(2026, 5, 20, 9, 30, 0, 0, time.UTC),
					ParticipantCount: 3,
				},
			},
		},
	}
	srv := NewServer(WithEmailChannelInventory(&stubEmailChannelInventory{rows: rows}))
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/integrations/email", nil)
	rec := httptest.NewRecorder()
	srv.AdminIntegrationsEmail(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%q", rec.Code, firstN(rec.Body.String(), 300))
	}
	body := rec.Body.String()
	for _, want := range []string{
		"ops",
		"imap.example.com",
		"smtp.example.com",
		"bot@example.com",
		"outbound on",
		"spf/dkim strict",
		"Quarterly review",
		"2026-05-20 09:30",
		"3p",
		"2 entries",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing fragment %q; body excerpt: %q", want, firstN(body, 800))
		}
	}
}

// TestAdminIntegrationsEmail_OutboundOffBadge — outbound-not-configured
// path renders the dimmed badge variant so operators can spot
// inbound-only channels at a glance.
func TestAdminIntegrationsEmail_OutboundOffBadge(t *testing.T) {
	rows := []AdminEmailChannelRow{{
		ProjectID:          "inbound-only",
		IMAPHost:           "imap.example.com",
		OutboundConfigured: false,
	}}
	srv := NewServer(WithEmailChannelInventory(&stubEmailChannelInventory{rows: rows}))
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/integrations/email", nil)
	rec := httptest.NewRecorder()
	srv.AdminIntegrationsEmail(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "outbound off") {
		t.Errorf("missing 'outbound off' badge; body fragment %q", firstN(body, 400))
	}
}

// TestAdminIntegrationsEmail_SessionsError — ListSessions errors
// surface inline rather than dropping the whole row. Today the
// channel never errors but the contract permits it and the UI
// should not hide infrastructure problems.
func TestAdminIntegrationsEmail_SessionsError(t *testing.T) {
	rows := []AdminEmailChannelRow{{
		ProjectID:     "broken",
		IMAPHost:      "imap.example.com",
		SessionsError: "imap session map unavailable: backend wedged",
	}}
	srv := NewServer(WithEmailChannelInventory(&stubEmailChannelInventory{rows: rows}))
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/integrations/email", nil)
	rec := httptest.NewRecorder()
	srv.AdminIntegrationsEmail(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "imap session map unavailable") {
		t.Errorf("missing SessionsError text; body fragment %q", firstN(body, 400))
	}
}

// TestAdminRouter_IntegrationsEmail — the adminRouter dispatch
// table must route /admin/integrations/email to the new handler.
// Without this entry the route 404s even when the inventory is
// wired.
func TestAdminRouter_IntegrationsEmail(t *testing.T) {
	srv := NewServer(WithEmailChannelInventory(&stubEmailChannelInventory{
		rows: []AdminEmailChannelRow{{ProjectID: "router-probe", IMAPHost: "h"}},
	}))
	req := httptest.NewRequest(http.MethodGet, "/admin/integrations/email", nil)
	rec := httptest.NewRecorder()
	srv.adminRouter(rec, withAdminUI(req))
	if rec.Code != http.StatusOK {
		t.Fatalf("router: got %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "router-probe") {
		t.Errorf("router did not reach the email handler; body fragment %q",
			firstN(rec.Body.String(), 300))
	}
}
