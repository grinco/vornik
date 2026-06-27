package service

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/email"
	"vornik.io/vornik/internal/registry"
)

// TestNewEmailChannelInventory_NoChannels — empty channel slice
// returns nil so the UI page falls through to its "not wired"
// empty state.
func TestNewEmailChannelInventory_NoChannels(t *testing.T) {
	if got := newEmailChannelInventory(nil, nil); got != nil {
		t.Errorf("nil channels: got %v, want nil", got)
	}
	if got := newEmailChannelInventory([]*email.Channel{}, []*registry.Project{}); got != nil {
		t.Errorf("empty channels: got %v, want nil", got)
	}
}

// TestEmailChannelInventory_PerProjectShape — populates per-project
// wiring from registry.ProjectEmail and includes the live (empty
// at boot) session list. The channel built here has no inbound
// since daemon start so Sessions is empty.
func TestEmailChannelInventory_PerProjectShape(t *testing.T) {
	t.Setenv("EMAIL_PASS_INV", "shhh")
	p := &registry.Project{
		ID: "ops",
		Email: registry.ProjectEmail{
			IMAPHost:               "imap.example.com",
			IMAPPort:               993,
			IMAPUsername:           "bot@example.com",
			IMAPPasswordEnv:        "EMAIL_PASS_INV",
			IMAPMailbox:            "Vornik",
			SMTPHost:               "smtp.example.com",
			SMTPPort:               587,
			SMTPUsername:           "bot@example.com",
			SMTPPasswordEnv:        "EMAIL_PASS_INV",
			FromAddress:            "bot@example.com",
			SenderAllowlist:        []string{"alice@example.com", "example.com"},
			AttachmentSizeCapBytes: 25 * 1024 * 1024,
			VerifyInboundAuth:      true,
			AuthPolicy:             "strict",
		},
	}
	ch, err := buildEmailChannelForProject(p, nil, "", nil)
	if err != nil {
		t.Fatalf("buildEmailChannelForProject: %v", err)
	}

	inv := newEmailChannelInventory([]*email.Channel{ch}, []*registry.Project{p})
	if inv == nil {
		t.Fatal("inventory unexpectedly nil")
	}

	rows := inv.EmailChannels(context.Background())
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.ProjectID != "ops" {
		t.Errorf("ProjectID = %q", r.ProjectID)
	}
	if r.IMAPHost != "imap.example.com" || r.IMAPPort != 993 || r.IMAPMailbox != "Vornik" {
		t.Errorf("IMAP fields wrong: %+v", r)
	}
	if !r.OutboundConfigured || r.SMTPHost != "smtp.example.com" || r.FromAddress != "bot@example.com" {
		t.Errorf("outbound fields wrong: %+v", r)
	}
	if r.AllowlistSize != 2 {
		t.Errorf("AllowlistSize = %d, want 2", r.AllowlistSize)
	}
	if r.AttachmentCapBytes != 25*1024*1024 {
		t.Errorf("AttachmentCapBytes = %d", r.AttachmentCapBytes)
	}
	if !r.VerifyInboundAuth || r.AuthPolicy != "strict" {
		t.Errorf("auth fields wrong: %+v", r)
	}
	if r.SessionsError != "" {
		t.Errorf("unexpected SessionsError: %q", r.SessionsError)
	}
	// Fresh channel — no inbound delivered, so Sessions is empty.
	// Important contract: the bridge does NOT collapse a nil slice
	// into the error path; an empty session list is the common case.
	if len(r.Sessions) != 0 {
		t.Errorf("Sessions = %d, want 0 (fresh channel)", len(r.Sessions))
	}
}

// TestEmailChannelInventory_SkipsNilEntries — defensive guard for
// the boot path where a channel slot could be nil if construction
// raced or short-circuited. Nil rows are skipped, the rest still
// surface.
func TestEmailChannelInventory_SkipsNilEntries(t *testing.T) {
	t.Setenv("EMAIL_PASS_SKIP", "shhh")
	p := buildEmailProject("good", "EMAIL_PASS_SKIP")
	ch, err := buildEmailChannelForProject(p, nil, "", nil)
	if err != nil {
		t.Fatalf("buildEmailChannelForProject: %v", err)
	}
	inv := newEmailChannelInventory(
		[]*email.Channel{nil, ch},
		[]*registry.Project{nil, p},
	)
	rows := inv.EmailChannels(context.Background())
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (nil entries should be skipped)", len(rows))
	}
	if rows[0].ProjectID != "good" {
		t.Errorf("ProjectID = %q, want good", rows[0].ProjectID)
	}
}
