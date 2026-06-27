package service

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// buildEmailProject constructs a minimally configured project with
// an inbound-only email block; tests mutate the returned pointer to
// cover specific paths.
func buildEmailProject(id, passwordEnv string) *registry.Project {
	return &registry.Project{
		ID:                id,
		SwarmID:           "s",
		DefaultWorkflowID: "w",
		Email: registry.ProjectEmail{
			IMAPHost: "imap.test",
			// Per-id mailbox: two distinct projects poll distinct
			// inbound mailboxes (the realistic config + what the
			// duplicate-mailbox guard in buildEmailChannels requires).
			IMAPUsername:    id + "@test",
			IMAPPasswordEnv: passwordEnv,
		},
	}
}

func TestBuildEmailChannel_NoProjectsEnabled(t *testing.T) {
	ch, p, err := buildEmailChannel(nil, nil, "")
	if err != nil {
		t.Fatalf("buildEmailChannel(nil): %v", err)
	}
	if ch != nil || p != nil {
		t.Errorf("expected (nil, nil), got (%v, %v)", ch, p)
	}
}

func TestBuildEmailChannel_AllDisabled(t *testing.T) {
	// Projects without an email block at all.
	ps := []*registry.Project{
		{ID: "a", SwarmID: "s", DefaultWorkflowID: "w"},
	}
	ch, p, err := buildEmailChannel(ps, nil, "")
	if err != nil {
		t.Fatalf("buildEmailChannel: %v", err)
	}
	if ch != nil || p != nil {
		t.Errorf("expected (nil, nil), got (%v, %v)", ch, p)
	}
}

func TestBuildEmailChannel_InboundOnly_Constructs(t *testing.T) {
	t.Setenv("EMAIL_PASS_BUILD_INBOUND", "shhh")
	p := buildEmailProject("test", "EMAIL_PASS_BUILD_INBOUND")
	ch, picked, err := buildEmailChannel([]*registry.Project{p}, nil, "")
	if err != nil {
		t.Fatalf("buildEmailChannel: %v", err)
	}
	if ch == nil {
		t.Fatal("expected channel, got nil")
	}
	if picked == nil || picked.ID != "test" {
		t.Errorf("picked = %v", picked)
	}
}

func TestBuildEmailChannel_MissingPasswordEnv(t *testing.T) {
	p := buildEmailProject("test", "EMAIL_PASS_DEFINITELY_UNSET")
	_, _, err := buildEmailChannel([]*registry.Project{p}, nil, "")
	if err == nil {
		t.Fatal("expected error for missing password env, got nil")
	}
	if !strings.Contains(err.Error(), "EMAIL_PASS_DEFINITELY_UNSET") {
		t.Errorf("error doesn't reference env var: %v", err)
	}
}

func TestBuildEmailChannel_MissingSMTPPasswordEnv(t *testing.T) {
	t.Setenv("EMAIL_PASS_FOR_INBOUND", "shhh")
	p := buildEmailProject("test", "EMAIL_PASS_FOR_INBOUND")
	p.Email.SMTPHost = "smtp.test"
	p.Email.SMTPUsername = "u@test"
	p.Email.SMTPPasswordEnv = "SMTP_PASS_DEFINITELY_UNSET"
	p.Email.FromAddress = "u@test"
	_, _, err := buildEmailChannel([]*registry.Project{p}, nil, "")
	if err == nil {
		t.Fatal("expected error for missing SMTP password env")
	}
	if !strings.Contains(err.Error(), "SMTP_PASS_DEFINITELY_UNSET") {
		t.Errorf("error doesn't reference env var: %v", err)
	}
}

func TestBuildEmailChannel_BadPollInterval(t *testing.T) {
	t.Setenv("EMAIL_PASS_BAD_POLL", "shhh")
	p := buildEmailProject("test", "EMAIL_PASS_BAD_POLL")
	p.Email.PollInterval = "not-a-duration"
	_, _, err := buildEmailChannel([]*registry.Project{p}, nil, "")
	if err == nil {
		t.Fatal("expected error for malformed poll_interval")
	}
}

func TestBuildEmailChannel_MultipleEnabled_FirstViaLegacyShim(t *testing.T) {
	// Legacy buildEmailChannel wrapper kept for back-compat; it
	// returns the first channel/project pair out of the slice.
	t.Setenv("EMAIL_PASS_MULTI_A", "shhh")
	t.Setenv("EMAIL_PASS_MULTI_B", "shhh")
	p1 := buildEmailProject("a", "EMAIL_PASS_MULTI_A")
	p2 := buildEmailProject("b", "EMAIL_PASS_MULTI_B")
	ch, picked, err := buildEmailChannel([]*registry.Project{p1, p2}, nil, "")
	if err != nil {
		t.Fatalf("buildEmailChannel: %v", err)
	}
	if ch == nil {
		t.Fatal("expected channel")
	}
	if picked.ID != "a" {
		t.Errorf("legacy shim returned project %q, want a", picked.ID)
	}
}

func TestBuildEmailChannels_PerProject(t *testing.T) {
	// Slice 3 per-project routing: every enabled project gets its
	// own channel; both are returned with matching indices.
	t.Setenv("EMAIL_PASS_PER_A", "shhh")
	t.Setenv("EMAIL_PASS_PER_B", "shhh")
	p1 := buildEmailProject("alpha", "EMAIL_PASS_PER_A")
	p2 := buildEmailProject("beta", "EMAIL_PASS_PER_B")
	channels, picked, err := buildEmailChannels([]*registry.Project{p1, p2}, nil, "", nil)
	if err != nil {
		t.Fatalf("buildEmailChannels: %v", err)
	}
	if len(channels) != 2 || len(picked) != 2 {
		t.Fatalf("got %d channels / %d projects, want 2 each", len(channels), len(picked))
	}
	if picked[0].ID != "alpha" || picked[1].ID != "beta" {
		t.Errorf("projects in unexpected order: %q,%q", picked[0].ID, picked[1].ID)
	}
	for i, c := range channels {
		if c == nil {
			t.Errorf("channels[%d] is nil", i)
		}
	}
}

func TestBuildEmailChannels_DuplicateMailboxRejected(t *testing.T) {
	// Two projects polling the SAME inbound mailbox must abort the build:
	// both channels would poll it and double-process every message.
	// Mirrors the slack team_id guard.
	t.Setenv("EMAIL_PASS_DUP_A", "shhh")
	t.Setenv("EMAIL_PASS_DUP_B", "shhh")
	p1 := buildEmailProject("alpha", "EMAIL_PASS_DUP_A")
	p2 := buildEmailProject("beta", "EMAIL_PASS_DUP_B")
	// Force the same inbound identity (host+user+mailbox) despite distinct IDs.
	p2.Email.IMAPHost = p1.Email.IMAPHost
	p2.Email.IMAPUsername = p1.Email.IMAPUsername
	channels, picked, err := buildEmailChannels([]*registry.Project{p1, p2}, nil, "", nil)
	if err == nil {
		t.Fatal("expected error for two projects sharing one inbound mailbox")
	}
	if channels != nil || picked != nil {
		t.Errorf("duplicate mailbox must abort entire build; got channels=%v picked=%v", channels, picked)
	}
}

// TestBuildEmailChannels_SameUserDifferentMailboxOK ensures the guard keys
// on the FULL identity: one login polling two distinct folders is allowed.
func TestBuildEmailChannels_SameUserDifferentMailboxOK(t *testing.T) {
	t.Setenv("EMAIL_PASS_MB_A", "shhh")
	t.Setenv("EMAIL_PASS_MB_B", "shhh")
	p1 := buildEmailProject("alpha", "EMAIL_PASS_MB_A")
	p2 := buildEmailProject("beta", "EMAIL_PASS_MB_B")
	p2.Email.IMAPHost = p1.Email.IMAPHost
	p2.Email.IMAPUsername = p1.Email.IMAPUsername
	p1.Email.IMAPMailbox = "INBOX"
	p2.Email.IMAPMailbox = "Vornik"
	channels, _, err := buildEmailChannels([]*registry.Project{p1, p2}, nil, "", nil)
	if err != nil {
		t.Fatalf("distinct mailboxes on one login should be allowed: %v", err)
	}
	if len(channels) != 2 {
		t.Fatalf("want 2 channels, got %d", len(channels))
	}
}

func TestBuildEmailChannels_OneBrokenAbortsAll(t *testing.T) {
	// One project misconfigured (no password env) aborts the whole
	// build so the operator sees the misconfig at boot. We don't
	// silently boot some channels and skip the broken one.
	t.Setenv("EMAIL_PASS_PARTIAL_OK", "shhh")
	p1 := buildEmailProject("ok", "EMAIL_PASS_PARTIAL_OK")
	p2 := buildEmailProject("broken", "EMAIL_PASS_PARTIAL_DEFINITELY_UNSET")
	channels, picked, err := buildEmailChannels([]*registry.Project{p1, p2}, nil, "", nil)
	if err == nil {
		t.Fatal("expected error from misconfigured project")
	}
	if channels != nil || picked != nil {
		t.Errorf("misconfig must abort entire build; got channels=%v picked=%v", channels, picked)
	}
}

func TestBuildEmailChannels_AttachmentDirIsPerProject(t *testing.T) {
	// Two projects sharing the daemon-wide default directory must
	// each get a per-project subdir so their attachments stay
	// separable on disk.
	t.Setenv("EMAIL_PASS_DIR_A", "shhh")
	t.Setenv("EMAIL_PASS_DIR_B", "shhh")
	p1 := buildEmailProject("alpha", "EMAIL_PASS_DIR_A")
	p2 := buildEmailProject("beta", "EMAIL_PASS_DIR_B")
	channels, picked, err := buildEmailChannels(
		[]*registry.Project{p1, p2}, nil, "/var/lib/vornik/email", nil,
	)
	if err != nil {
		t.Fatalf("buildEmailChannels: %v", err)
	}
	if len(channels) != 2 {
		t.Fatalf("got %d channels, want 2", len(channels))
	}
	// We can't reach into the unexported Config field but smoke-
	// testing successful construction with two projects + a default
	// dir is enough — the channel-layer tests cover the per-project
	// path-joining semantics directly.
	if picked[0].ID == picked[1].ID {
		t.Errorf("project IDs collided: %q", picked[0].ID)
	}
}

func TestResolveEmailConfig_PollIntervalParses(t *testing.T) {
	t.Setenv("EMAIL_PASS_POLL_OK", "shhh")
	p := registry.ProjectEmail{
		IMAPHost:        "imap.test",
		IMAPUsername:    "u",
		IMAPPasswordEnv: "EMAIL_PASS_POLL_OK",
		PollInterval:    "45s",
	}
	cfg, err := resolveEmailConfig(p)
	if err != nil {
		t.Fatalf("resolveEmailConfig: %v", err)
	}
	if cfg.PollInterval.Seconds() != 45 {
		t.Errorf("PollInterval = %v", cfg.PollInterval)
	}
}

func TestBuildEmailChannel_WiresAttachmentDir(t *testing.T) {
	t.Setenv("EMAIL_PASS_ATTACH_DIR", "shhh")
	p := buildEmailProject("test-attach", "EMAIL_PASS_ATTACH_DIR")
	p.Email.AttachmentSizeCapBytes = 5 * 1024 * 1024
	p.Email.AttachmentStoreDir = "/var/tmp/vornik-test"
	_, picked, err := buildEmailChannel([]*registry.Project{p}, nil, "")
	if err != nil {
		t.Fatalf("buildEmailChannel: %v", err)
	}
	if picked == nil {
		t.Fatal("picked nil")
	}
	if picked.Email.AttachmentSizeCapBytes != 5*1024*1024 {
		t.Errorf("AttachmentSizeCapBytes lost in round-trip: %d", picked.Email.AttachmentSizeCapBytes)
	}
}

func TestResolveEmailConfig_AuthPolicyToggleWiresVerifier(t *testing.T) {
	t.Setenv("EMAIL_PASS_AUTH_VERIFIER", "shhh")
	p := registry.ProjectEmail{
		IMAPHost:          "imap.test",
		IMAPUsername:      "u",
		IMAPPasswordEnv:   "EMAIL_PASS_AUTH_VERIFIER",
		VerifyInboundAuth: true,
	}
	cfg, err := resolveEmailConfig(p)
	if err != nil {
		t.Fatalf("resolveEmailConfig: %v", err)
	}
	if cfg.SignatureVerifier == nil {
		t.Fatal("VerifyInboundAuth=true must wire SignatureVerifier")
	}
}

func TestResolveEmailConfig_AuthPolicyStrict(t *testing.T) {
	t.Setenv("EMAIL_PASS_AUTH_STRICT", "shhh")
	p := registry.ProjectEmail{
		IMAPHost:          "imap.test",
		IMAPUsername:      "u",
		IMAPPasswordEnv:   "EMAIL_PASS_AUTH_STRICT",
		VerifyInboundAuth: true,
		AuthPolicy:        "strict",
	}
	if _, err := resolveEmailConfig(p); err != nil {
		t.Errorf("resolveEmailConfig (strict): %v", err)
	}
}

func TestResolveEmailConfig_AuthPolicyUnknownErrors(t *testing.T) {
	t.Setenv("EMAIL_PASS_AUTH_BAD", "shhh")
	p := registry.ProjectEmail{
		IMAPHost:          "imap.test",
		IMAPUsername:      "u",
		IMAPPasswordEnv:   "EMAIL_PASS_AUTH_BAD",
		VerifyInboundAuth: true,
		AuthPolicy:        "paranoid-extra-strict",
	}
	_, err := resolveEmailConfig(p)
	if err == nil {
		t.Fatal("unknown auth_policy must error")
	}
	if !strings.Contains(err.Error(), "paranoid-extra-strict") {
		t.Errorf("err = %v, want it to surface the bad value", err)
	}
}

func TestResolveEmailConfig_AuthVerifierOffSkips(t *testing.T) {
	t.Setenv("EMAIL_PASS_AUTH_OFF", "shhh")
	p := registry.ProjectEmail{
		IMAPHost:        "imap.test",
		IMAPUsername:    "u",
		IMAPPasswordEnv: "EMAIL_PASS_AUTH_OFF",
		// VerifyInboundAuth default false.
	}
	cfg, err := resolveEmailConfig(p)
	if err != nil {
		t.Fatalf("resolveEmailConfig: %v", err)
	}
	if cfg.SignatureVerifier != nil {
		t.Error("VerifyInboundAuth=false must leave SignatureVerifier nil (channel falls back to no-op)")
	}
}

func TestBuildEmailChannel_DefaultsAttachmentDirFromBase(t *testing.T) {
	t.Setenv("EMAIL_PASS_ATTACH_DEFAULT", "shhh")
	p := buildEmailProject("project-X", "EMAIL_PASS_ATTACH_DEFAULT")
	// AttachmentStoreDir empty in YAML → fall back to base + project ID
	ch, _, err := buildEmailChannel([]*registry.Project{p}, nil, "/var/lib/vornik/email")
	if err != nil {
		t.Fatalf("buildEmailChannel: %v", err)
	}
	if ch == nil {
		t.Fatal("expected channel")
	}
	// The internal field isn't directly exposed; the smoke test is
	// that construction with a non-empty default succeeds — the
	// channel-level attachment tests cover the actual path-joining
	// semantics.
}
