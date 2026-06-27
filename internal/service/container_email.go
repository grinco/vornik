package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"vornik.io/vornik/internal/email"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// buildEmailChannels scans the project registry for every project
// carrying a fully configured `email` block and constructs one
// *email.Channel per project. Returns aligned slices: channels[i]
// corresponds to projects[i]. Empty inputs (or no project with an
// email block) return (nil, nil, nil) — the service container then
// leaves the inbound poll-loop unwired entirely.
//
// Slice 3: per-project routing. The slice-1/2 posture was "first
// enabled project wins, others get warned and dropped." Now every
// enabled project gets its own IMAP session, its own SMTP outbound,
// its own attachment-persistence subdir. Each channel is pinned to
// its own project ID, so dispatcher-level project scoping stays
// inside the project that received the inbound message.
//
// Returned errors are operator-facing — empty IMAP password env
// var, malformed poll interval, all surface as daemon-boot failures
// so the misconfig is loud at startup rather than at the first
// poll cycle. A failure on one project's channel aborts the entire
// build so the operator sees the misconfig immediately; we don't
// silently boot some channels and skip the broken one (that would
// reduce visibility into "why isn't project X receiving mail?").
func buildEmailChannels(projects []*registry.Project, artifactRepo persistence.ArtifactRepository, defaultAttachmentDir string, autoExtractor email.AttachmentAutoExtractor) ([]*email.Channel, []*registry.Project, error) {
	var (
		channels []*email.Channel
		picked   []*registry.Project
	)
	// Guard against two projects polling the SAME inbound mailbox — both
	// channels would poll it on their own cadence and double-process (or
	// race on) every incoming message. Mirrors buildSlackChannels' team_id
	// guard. Identity is (IMAPHost, IMAPUsername, IMAPMailbox); the mailbox
	// default ("INBOX", per email.Channel) is applied so an explicit
	// "INBOX" and an empty value collide as they should.
	seenMailboxes := make(map[string]string)
	for _, p := range projects {
		if !p.Email.Enabled() {
			continue
		}
		mailboxKey := emailInboxIdentity(p.Email)
		if existing, ok := seenMailboxes[mailboxKey]; ok {
			return nil, nil, fmt.Errorf("project %q email: duplicate inbound mailbox %q (already configured by project %q)",
				p.ID, mailboxKey, existing)
		}
		ch, err := buildEmailChannelForProject(p, artifactRepo, defaultAttachmentDir, autoExtractor)
		if err != nil {
			return nil, nil, err
		}
		seenMailboxes[mailboxKey] = p.ID
		channels = append(channels, ch)
		picked = append(picked, p)
	}
	if len(channels) == 0 {
		return nil, nil, nil
	}
	return channels, picked, nil
}

// emailInboxIdentity returns the canonical key identifying the inbound
// mailbox a project polls: host + username (both case-insensitive — mail
// hosts and login names are not case-sensitive in practice) + mailbox
// folder (case-sensitive per IMAP, defaulting to "INBOX" to match
// email.Channel so an explicit "INBOX" and an empty value collide).
// Two projects sharing this key would double-poll the same mailbox.
func emailInboxIdentity(e registry.ProjectEmail) string {
	mailbox := strings.TrimSpace(e.IMAPMailbox)
	if mailbox == "" {
		mailbox = "INBOX"
	}
	host := strings.ToLower(strings.TrimSpace(e.IMAPHost))
	user := strings.ToLower(strings.TrimSpace(e.IMAPUsername))
	return host + "|" + user + "|" + mailbox
}

// buildEmailChannelForProject is the per-project constructor
// buildEmailChannels delegates to. Kept separate for testability —
// callers (tests, future per-project hot-reload paths) can build
// a single channel without iterating a registry.
func buildEmailChannelForProject(p *registry.Project, artifactRepo persistence.ArtifactRepository, defaultAttachmentDir string, autoExtractor email.AttachmentAutoExtractor) (*email.Channel, error) {
	cfg, err := resolveEmailConfig(p.Email)
	if err != nil {
		return nil, fmt.Errorf("project %q email: %w", p.ID, err)
	}
	// Production wiring: real IMAP adapter (emersion/go-imap-backed)
	// + real SMTP adapter (net/smtp-backed). Tests inject fakes via
	// the channel's Config seam.
	cfg.IMAPClient = email.NewIMAPClient()
	if cfg.SMTPHost != "" {
		// SMTP outbound only wires when the operator filled in the
		// full quartet (host/username/password/from).
		cfg.SMTPSender = newSMTPSenderFromConfig(cfg)
	}
	cfg.ArtifactRepo = artifactRepo
	cfg.AttachmentProjectID = p.ID
	cfg.AttachmentStoreDir = strings.TrimSpace(p.Email.AttachmentStoreDir)
	if cfg.AttachmentStoreDir == "" && defaultAttachmentDir != "" {
		// Per-project subdir under the daemon default keeps two
		// projects' attachments separable on disk.
		cfg.AttachmentStoreDir = filepath.Join(defaultAttachmentDir, p.ID)
	}
	cfg.AttachmentSizeCapBytes = p.Email.AttachmentSizeCapBytes
	if autoExtractor != nil {
		cfg.AutoExtractor = autoExtractor
		// 60s per attachment — covers EPUB (sub-second), small PDFs,
		// and short audio transcripts comfortably. Larger media
		// surfaces as a timeout that the operator can re-trigger via
		// the manual /extract endpoint when ready to wait for it.
		cfg.AutoExtractTimeout = 60 * time.Second
	}

	ch, err := email.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("project %q email: %w", p.ID, err)
	}
	return ch, nil
}

// buildEmailChannel is the legacy single-channel constructor kept
// for backwards-compatible test access. It now wraps the multi-
// project path and returns the first channel/project pair. New
// callers should use buildEmailChannels.
//
// Deprecated: use buildEmailChannels.
func buildEmailChannel(projects []*registry.Project, artifactRepo persistence.ArtifactRepository, defaultAttachmentDir string) (*email.Channel, *registry.Project, error) {
	channels, picked, err := buildEmailChannels(projects, artifactRepo, defaultAttachmentDir, nil)
	if err != nil {
		return nil, nil, err
	}
	if len(channels) == 0 {
		return nil, nil, nil
	}
	return channels[0], picked[0], nil
}

// resolveEmailConfig translates a registry.ProjectEmail YAML block
// into the email.Config the channel constructor consumes: env-var
// indirected passwords are resolved here, the poll interval is
// parsed, and string allowlists / hosts / ports pass through
// verbatim.
func resolveEmailConfig(p registry.ProjectEmail) (email.Config, error) {
	imapPass := os.Getenv(p.IMAPPasswordEnv)
	if strings.TrimSpace(imapPass) == "" {
		return email.Config{}, fmt.Errorf("imap_password_env %q is unset or empty", p.IMAPPasswordEnv)
	}
	cfg := email.Config{
		IMAPHost:        p.IMAPHost,
		IMAPPort:        p.IMAPPort,
		IMAPUsername:    p.IMAPUsername,
		IMAPPassword:    imapPass,
		IMAPMailbox:     p.IMAPMailbox,
		SMTPHost:        p.SMTPHost,
		SMTPPort:        p.SMTPPort,
		SMTPUsername:    p.SMTPUsername,
		FromAddress:     p.FromAddress,
		SenderAllowlist: p.SenderAllowlist,
	}
	if p.SMTPHost != "" {
		smtpPass := os.Getenv(p.SMTPPasswordEnv)
		if strings.TrimSpace(smtpPass) == "" {
			return email.Config{}, fmt.Errorf("smtp_password_env %q is unset or empty", p.SMTPPasswordEnv)
		}
		cfg.SMTPPassword = smtpPass
	}
	if v := strings.TrimSpace(p.PollInterval); v != "" {
		dur, err := time.ParseDuration(v)
		if err != nil {
			return email.Config{}, fmt.Errorf("parse poll_interval %q: %w", v, err)
		}
		cfg.PollInterval = dur
	}
	if p.VerifyInboundAuth {
		policy, err := parseAuthPolicy(p.AuthPolicy)
		if err != nil {
			return email.Config{}, err
		}
		cfg.SignatureVerifier = email.HeaderAuthVerifier{
			Policy:           policy,
			TrustedServerIDs: p.TrustedAuthServers,
		}
	}
	return cfg, nil
}

// parseAuthPolicy maps the operator-facing string knob to the
// email.AuthPolicy constant. Empty/"relaxed" → relaxed; "strict" →
// strict; anything else surfaces an explicit error so a typo in
// YAML lands at boot rather than silently picking a permissive
// default.
func parseAuthPolicy(in string) (email.AuthPolicy, error) {
	switch strings.ToLower(strings.TrimSpace(in)) {
	case "", "relaxed":
		return email.AuthPolicyRelaxed, nil
	case "strict":
		return email.AuthPolicyStrict, nil
	default:
		return 0, fmt.Errorf("auth_policy %q is not recognised (use \"relaxed\" or \"strict\")", in)
	}
}

// newSMTPSenderFromConfig wraps email.newNetSMTPSender's
// constructor — kept at the service-container layer so a future
// adapter swap (e.g. provider-specific REST send instead of SMTP)
// changes only this one call site.
func newSMTPSenderFromConfig(cfg email.Config) email.SMTPSender {
	return email.NewNetSMTPSender(cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPUsername, cfg.SMTPPassword)
}

// emailAttachmentDir returns the daemon-wide default directory
// under which inbound-email attachment bytes land. The value is
// derived (in priority order) from:
//
//  1. The operator's Storage.ArtifactsPath setting + "/email-inbound"
//     when set (mirrors how the executor's artifact store roots).
//  2. VORNIK_DATA_DIR + "/email-attachments" when the env var is set.
//  3. The Go-stdlib temp dir + "/vornik-email-attachments" as a
//     last-resort fallback for development.
//
// Returning "" disables attachment persistence entirely; that path
// is reserved for tests / minimal-container builds.
func (c *Container) emailAttachmentDir() string {
	if c == nil || c.Config == nil {
		return ""
	}
	if ap := strings.TrimSpace(c.Config.Storage.ArtifactsPath); ap != "" {
		return filepath.Join(ap, "email-inbound")
	}
	if dataDir := strings.TrimSpace(os.Getenv("VORNIK_DATA_DIR")); dataDir != "" {
		return filepath.Join(dataDir, "email-attachments")
	}
	return filepath.Join(os.TempDir(), "vornik-email-attachments")
}
