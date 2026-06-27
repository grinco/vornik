// Package email implements vornik's email conversation channel —
// IMAP polling for inbound, SMTP for outbound (slice 1 of the
// email-ingress rollout; see https://docs.vornik.io §Inbound integrations
// and https://docs.vornik.io).
//
// Slice 1 scope (today):
//
//   - IMAP poller dials INBOX on a configurable interval, fetches
//     unseen messages, translates each into a
//     conversation.ChannelMessage and forwards via Receiver.Receive.
//   - SMTP outbound Send formats an RFC 5322 reply (subject prefixed
//     "Re: ", In-Reply-To + References headers preserve threading)
//     and posts via the configured SMTP host.
//   - SenderAllowlist filters inbound senders by either full address
//     (alice@example.com) or bare domain (example.com). Empty list
//     allows every sender — dev-mode pass-through matching the
//     GitHub channel's semantics.
//   - ListSessions returns an in-memory snapshot of every thread
//     that has produced at least one inbound message since daemon
//     start. Daemon restart wipes the set; the persisted truth is
//     the IMAP mailbox itself.
//
// Deferred to later slices:
//   - Attachment ingestion (today: dropped with a TODO log line).
//   - DKIM / SPF verification (the SignatureVerifier interface is
//     wired through Start so a real verifier can plug in without
//     re-shaping the channel; the default is a no-op).
//   - HTML body parsing (slice 1 takes text/plain when present;
//     HTML-only messages fall through a naive strip-tags pass).
//   - Per-project routing (slice 1 is one address, one project —
//     same as the GitHub channel's slice 1).
package email

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/leaderelection"
	"vornik.io/vornik/internal/persistence"
)

// channelName is the stable identifier surfaced as
// ChannelMessage.Source. Downstream consumers (the dispatcher,
// the operator UI) branch on this string without type-asserting.
const channelName = "email"

// defaultPollInterval is the IMAP poll cadence when the operator
// hasn't set Config.PollInterval. Sixty seconds is a deliberate
// floor — most IMAP servers rate-limit aggressive polling and
// the operator workflow doesn't need sub-minute latency for
// email-driven tasks. Operators who want faster turnaround can
// drop it; production trials at 30s have been safe.
const defaultPollInterval = 60 * time.Second

// defaultReconnectLimit / defaultReconnectWindow cap how
// aggressively the channel retries reconnect-on-drop. Three per
// minute matches the runtime/warmpool's posture: a misbehaving
// IMAP server gets one batch of attempts and then we back off to
// the regular poll cadence rather than hammering it. Operators
// who want a different ceiling can override via Config at
// construction (slice-3 wiring).
const (
	defaultReconnectLimit  = 3
	defaultReconnectWindow = time.Minute
)

// Config carries the operator-provided email-channel settings.
// Populated from project config at boot; never mutated after
// Channel construction. Secrets are resolved upstream (the service
// container reads the env var named by ProjectEmail.PasswordEnv
// and passes the resolved string in here) so this struct can stay
// pure data.
type Config struct {
	// IMAPHost is the IMAP server hostname (e.g. imap.gmail.com).
	// Required.
	IMAPHost string

	// IMAPPort is the IMAP server port. Zero defaults to 993 (TLS).
	IMAPPort int

	// IMAPUsername is the IMAP login username (commonly the same as
	// FromAddress, but some providers split). Required.
	IMAPUsername string

	// IMAPPassword is the resolved IMAP password. The service
	// container reads it from the env var named by the project
	// YAML's `imap_password_env` and supplies it here. Required.
	IMAPPassword string

	// IMAPMailbox is the IMAP folder to poll. Empty defaults to
	// "INBOX". Operators routing vornik mail to a sub-folder (e.g.
	// "Vornik") set this to that folder name.
	IMAPMailbox string

	// SMTPHost is the SMTP server hostname (e.g. smtp.gmail.com).
	// Required for outbound Send; zero disables outbound and
	// Send returns ErrOutboundNotConfigured.
	SMTPHost string

	// SMTPPort is the SMTP server port. Zero defaults to 587 (STARTTLS).
	SMTPPort int

	// SMTPUsername is the SMTP login username. Required for outbound.
	SMTPUsername string

	// SMTPPassword is the resolved SMTP password (read from env at
	// the service-container layer). Required for outbound.
	SMTPPassword string

	// FromAddress is the From: header on outbound mail. Should be
	// the address operators want replies to land on — usually the
	// same as IMAPUsername. Required for outbound.
	FromAddress string

	// SenderAllowlist filters inbound. Entries may be a full address
	// ("alice@example.com") or a bare domain ("example.com"); a
	// message is admitted when its From: address matches a full entry
	// or its domain matches a bare entry. Empty list allows every
	// sender (dev-mode pass-through).
	SenderAllowlist []string

	// PollInterval is how often the IMAP poller wakes. Zero defaults
	// to 60s. Sub-second values are accepted but discouraged — IMAP
	// providers rate-limit and the operator UX doesn't need it.
	PollInterval time.Duration

	// Logger is the channel's zerolog instance. Zero value is fine
	// but produces no log output.
	Logger zerolog.Logger

	// IMAPClient overrides the IMAP transport. Production wiring
	// supplies an emersion/go-imap-backed adapter (see
	// imap_emersion.go); tests inject an in-memory fake. Nil falls
	// back to the production adapter constructed from the IMAP*
	// fields above.
	IMAPClient IMAPClient

	// SMTPSender overrides the SMTP transport. Production wiring
	// supplies a net/smtp-backed adapter (see smtp.go); tests inject
	// a fake that captures the formatted RFC 5322 payload. Nil falls
	// back to the production adapter constructed from the SMTP*
	// fields above.
	SMTPSender SMTPSender

	// SignatureVerifier optionally verifies SPF/DKIM on every
	// inbound message. Nil falls back to NoopSignatureVerifier
	// (every message passes). Slice 2 wires a real implementation.
	SignatureVerifier SignatureVerifier

	// Clock overrides time.Now in tests so the poll loop's "last
	// poll" bookkeeping is deterministic. Nil falls back to
	// time.Now.
	Clock func() time.Time

	// ArtifactRepo persists inbound attachment bytes as task
	// artifacts. Nil disables attachment persistence — the channel
	// then drops attachment bytes with a log line. Slice-2 wiring
	// supplies the daemon's ArtifactRepository.
	ArtifactRepo persistence.ArtifactRepository

	// AttachmentStoreDir is the on-disk directory under which
	// per-message attachment files land. Empty disables attachment
	// persistence (same effect as nil ArtifactRepo). Slice-2
	// wiring sets this from the daemon's data directory.
	AttachmentStoreDir string

	// AttachmentProjectID scopes attachment Artifact rows. Slice-2
	// wiring sets this to the email channel's pinned project ID
	// (one project per daemon, mirroring GitHubApp); slice-3
	// per-project routing rewires it per inbound.
	AttachmentProjectID string

	// AttachmentSizeCapBytes refuses inbound messages whose total
	// attachment bytes exceed this ceiling. Zero means unlimited.
	// Mirrors the operator-side ProjectEmail.AttachmentSizeCapBytes
	// — the service container reads YAML and passes it through.
	AttachmentSizeCapBytes int64

	// ReconnectLimit / ReconnectWindow tune the reconnect-on-drop
	// rate limiter. Zero values fall back to defaults (3 per
	// minute). Slice-3 per-project tuning can override.
	ReconnectLimit  int
	ReconnectWindow time.Duration

	// AutoExtractor, when non-nil, gets called once per
	// successfully-persisted attachment so the email channel can
	// drive document extraction at attachment-arrival time. Each
	// call returns a summary the channel folds into the outgoing
	// ChannelMessage.Attachments[i].Extraction field so the
	// dispatcher LLM knows the file already landed in project
	// memory. Errors from a single attachment do not block the
	// message — the channel logs and continues. nil disables the
	// path: attachments still persist, just no extraction. See
	// https://docs.vornik.io §8.1.
	AutoExtractor AttachmentAutoExtractor

	// AutoExtractTimeout is the per-attachment ceiling on
	// extraction work. Synchronous extraction lets us enrich the
	// outbound ChannelMessage before recv.Receive, but a runaway
	// extractor would block the poll loop. Zero = no timeout
	// (relies on the extractor's own internal limits); positive
	// values cap each attachment's extraction at this duration.
	// Recommended default is 60s — covers EPUB / small PDFs
	// comfortably, surfaces audio/video as a timeout that the
	// operator can re-trigger via the manual API endpoint.
	AutoExtractTimeout time.Duration
}

// AttachmentAutoExtractor is the seam between the email channel
// and the daemon's document-extraction pipeline. Implementations
// load the artifact's source MIME, dispatch via the extractor
// registry, run extraction + memory ingest, and return a summary.
// Defined here (not as an import from internal/extractor) so the
// email package stays free of an extractor-package dependency —
// the seam matches the AttachmentAutoExtractor's slim consumer
// surface, not the full extractor.Registry shape.
//
// Implementations MUST be concurrency-safe — the channel may call
// AutoExtract from multiple goroutines in future per-attachment
// fan-out work, even though Phase-5 wiring keeps the calls
// serialised behind a single goroutine.
type AttachmentAutoExtractor interface {
	AutoExtract(ctx context.Context, in AutoExtractRequest) (*AttachmentExtraction, error)
}

// AutoExtractRequest is the input to AttachmentAutoExtractor —
// everything the implementation needs to look up the source
// artifact and run extraction, without taking a dependency on the
// persistence.Artifact shape.
type AutoExtractRequest struct {
	ProjectID   string
	ArtifactID  string
	Name        string
	MimeType    string
	StoragePath string
}

// AttachmentExtraction summarises a successful extraction. nil
// return values mean "this MIME type has no registered extractor"
// — the channel treats that as a no-op without logging an error.
type AttachmentExtraction struct {
	ExtractedDocumentID string
	Title               string
	Author              string
	SectionCount        int
	ChunksIngested      int
}

// Channel implements conversation.Channel over IMAP + SMTP. One
// instance is constructed per daemon boot; per-project multi-
// address routing is a slice-2 concern.
type Channel struct {
	cfg     Config
	senders senderAllowlist

	imap     IMAPClient
	smtp     SMTPSender
	verifier SignatureVerifier
	clock    func() time.Time
	logger   zerolog.Logger

	pollInterval time.Duration
	mailbox      string

	// reconnects rate-limits IMAP reconnect attempts so a server
	// stuck in a "drop after auth" loop doesn't get hammered with
	// reconnect storms. Nil-safe: legacy IMAPClient implementations
	// without IMAPReconnector skip the path before reaching this
	// limiter.
	reconnects *reconnectLimiter

	// attachmentDeps is the dependency bundle the channel passes to
	// PersistAttachments for each inbound message that carried any.
	// All-nil means "no attachment persistence wiring" — the channel
	// drops attachment bytes with a log line and continues.
	attachmentDeps persistAttachmentDeps
	// attachmentCap is the per-message attachment-bytes ceiling.
	// Zero means unlimited. Mirrors maxWebhookBodyBytes's posture
	// — we refuse early so the channel never has to babysit a
	// 25-MiB inbound through the rest of the pipeline.
	attachmentCap int64

	recvMu sync.RWMutex
	recv   conversation.Receiver

	sessionsMu sync.Mutex
	sessions   map[string]*sessionEntry

	// followups tracks per-task auto-resume registrations created
	// via RegisterFollowup. The executor's CompletionNotifier fan-
	// out lands on NotifyTaskCompleted; this map is the filter
	// gate so the channel only fires for tasks created via its
	// own sessions.
	followups *followupStore

	// stopCh is closed by Stop to break the poll loop. The Start
	// goroutine selects on it as well as ctx.Done so callers can
	// shut the channel down either by cancelling the context or by
	// calling Stop directly.
	stopOnce sync.Once
	stopCh   chan struct{}

	// leaderGate, when wired, gates each poll cycle on the
	// elected leader so non-leader replicas don't FetchUnseen +
	// MarkSeen a message the leader is already handling. Nil
	// (single-process default or unwired LeaderLocks repo)
	// leaves every cycle running unconditionally — the legacy
	// behaviour. See SetLeaderGate.
	leaderGate LeaderGate
}

// LeaderGate is the narrow contract runPollCycle uses to skip
// IMAP fetches on non-leader daemons. Satisfied by
// *leaderelection.Elector; defined locally so this package
// doesn't pull leaderelection into its import set.
type LeaderGate interface {
	IsLeader() bool
}

// SetLeaderGate attaches the cluster gate. Wiring this turns
// the channel into a "leader-only fetches" worker: non-leader
// replicas keep the IMAP connection warm (so a takeover is
// instant) but skip FetchUnseen + MarkSeen entirely. Idempotent
// — passing nil clears the gate. Safe to call after Start.
func (c *Channel) SetLeaderGate(g LeaderGate) {
	c.leaderGate = g
}

// sessionEntry holds the per-thread metadata ListSessions surfaces.
// participants is a set so re-sending from the same address doesn't
// inflate ParticipantCount.
type sessionEntry struct {
	Title            string
	LastActivity     time.Time
	ParticipantCount int
	participants     map[string]struct{}
}

// New constructs a Channel from the given Config. Returns an error
// when the config is structurally broken (empty IMAP host or
// username, missing password) — defensive defaults catch the
// misconfig at boot rather than the first poll cycle.
func New(cfg Config) (*Channel, error) {
	if strings.TrimSpace(cfg.IMAPHost) == "" {
		return nil, errors.New("email channel: IMAPHost is required")
	}
	if strings.TrimSpace(cfg.IMAPUsername) == "" {
		return nil, errors.New("email channel: IMAPUsername is required")
	}
	if strings.TrimSpace(cfg.IMAPPassword) == "" {
		return nil, errors.New("email channel: IMAPPassword is required")
	}

	pollInterval := cfg.PollInterval
	if pollInterval <= 0 {
		pollInterval = defaultPollInterval
	}
	mailbox := strings.TrimSpace(cfg.IMAPMailbox)
	if mailbox == "" {
		mailbox = "INBOX"
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}

	verifier := cfg.SignatureVerifier
	if verifier == nil {
		verifier = NoopSignatureVerifier{}
	}

	reconnectLimit := cfg.ReconnectLimit
	if reconnectLimit <= 0 {
		reconnectLimit = defaultReconnectLimit
	}
	reconnectWindow := cfg.ReconnectWindow
	if reconnectWindow <= 0 {
		reconnectWindow = defaultReconnectWindow
	}

	c := &Channel{
		cfg:          cfg,
		senders:      newSenderAllowlist(cfg.SenderAllowlist),
		imap:         cfg.IMAPClient,
		smtp:         cfg.SMTPSender,
		verifier:     verifier,
		clock:        clock,
		logger:       cfg.Logger,
		pollInterval: pollInterval,
		mailbox:      mailbox,
		sessions:     make(map[string]*sessionEntry),
		stopCh:       make(chan struct{}),
		reconnects:   newReconnectLimiter(reconnectLimit, reconnectWindow, clock),
		attachmentDeps: persistAttachmentDeps{
			Repo:      cfg.ArtifactRepo,
			StoreDir:  cfg.AttachmentStoreDir,
			ProjectID: cfg.AttachmentProjectID,
			Now:       clock,
		},
		attachmentCap: cfg.AttachmentSizeCapBytes,
		followups:     newFollowupStore(),
	}
	return c, nil
}

// Name implements conversation.Channel.
func (c *Channel) Name() string { return channelName }

// Start binds the Receiver, dials the IMAP server, and enters the
// poll loop. Returns when ctx is cancelled, Stop is called, or the
// IMAP transport reports an unrecoverable error. Implementations
// retry transient errors silently with a per-cycle backoff; only a
// "credentials rejected" style failure escapes.
//
// The poll loop is the channel's only inbound goroutine — Receive
// is called serially per the conversation.Receiver contract, so
// downstream consumers don't need to worry about concurrent
// delivery from this channel.
func (c *Channel) Start(ctx context.Context, recv conversation.Receiver) error {
	if recv == nil {
		return errors.New("email channel: nil Receiver")
	}
	c.recvMu.Lock()
	c.recv = recv
	c.recvMu.Unlock()

	if c.imap == nil {
		return errors.New("email channel: no IMAPClient configured (production wiring must inject one)")
	}
	if err := c.imap.Connect(ctx, IMAPDialConfig{
		Host:     c.cfg.IMAPHost,
		Port:     c.cfg.IMAPPort,
		Username: c.cfg.IMAPUsername,
		Password: c.cfg.IMAPPassword,
		Mailbox:  c.mailbox,
	}); err != nil {
		return fmt.Errorf("email channel: IMAP connect: %w", err)
	}
	defer func() {
		if err := c.imap.Close(); err != nil {
			c.logger.Warn().Err(err).Msg("email: IMAP close failed")
		}
	}()

	// One-time startup confirmation so journald shows the poll loop
	// is alive. Without this the only signal the channel is running
	// is "no warnings appeared" — which is the same as "the
	// goroutine crashed silently." See task #30 origin.
	c.logger.Info().
		Str("mailbox", c.mailbox).
		Dur("poll_interval", c.pollInterval).
		Msg("email: poll loop entering — IMAP connected, first cycle running")

	// Run one poll cycle immediately so a test (or the first
	// post-boot message) doesn't have to wait the full interval
	// for the first delivery.
	c.runPollCycle(ctx)

	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			c.logger.Info().Err(ctx.Err()).Msg("email: poll loop exiting (context cancelled)")
			return ctx.Err()
		case <-c.stopCh:
			c.logger.Info().Msg("email: poll loop exiting (Stop called)")
			return nil
		case <-ticker.C:
			c.runPollCycle(ctx)
		}
	}
}

// Stop signals Start to exit. Idempotent. Safe to call from any
// goroutine. Does NOT block on the in-flight poll cycle — the
// Start goroutine drains its current cycle and then returns.
func (c *Channel) Stop() error {
	c.stopOnce.Do(func() {
		close(c.stopCh)
	})
	c.recvMu.Lock()
	c.recv = nil
	c.recvMu.Unlock()
	return nil
}

// runPollCycle fetches every unseen message in the mailbox,
// translates each, and forwards via Receiver.Receive. Errors at any
// step are logged and swallowed — a failing cycle does not abort
// the poll loop. Per-message errors do not abort the cycle.
//
// Slice-2 hardening: when FetchUnseen returns a transport-level
// error (EOF, broken pipe, closed connection), the channel calls
// Reconnect on the IMAPClient (if it implements IMAPReconnector)
// and retries the fetch once. Rate-limited at 3 per minute to
// avoid hammering a misbehaving server.
func (c *Channel) runPollCycle(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	// Cluster gate: non-leader replicas skip the cycle entirely.
	// Connection stays warm (Start's Connect is per-channel and
	// already happened) so a takeover is instant — when the
	// elected lease flips to this replica, the next cycle runs.
	// IMAP \Seen flags + at-most-one-leader gating gives at-
	// most-once delivery without an application-level UID
	// watermark.
	if c.leaderGate != nil && !c.leaderGate.IsLeader() {
		c.logger.Debug().Str("mailbox", c.mailbox).Msg("email: poll cycle skipped (not leader)")
		return
	}
	c.logger.Debug().Str("mailbox", c.mailbox).Msg("email: poll cycle start")
	msgs, err := c.imap.FetchUnseen(ctx)
	if err != nil {
		// Transport-level errors get one reconnect-and-retry, subject
		// to the rate limiter. Other errors get logged and we wait
		// for the next poll cycle (the slice-1 behaviour).
		if isTransportError(err) {
			if c.tryReconnectAndRetry(ctx, err, &msgs) {
				// retry succeeded — fall through to delivery
			} else {
				return
			}
		} else {
			c.logger.Warn().Err(err).Msg("email: IMAP fetch failed")
			return
		}
	}
	// Observability: log every cycle's outcome so a future "messages
	// aren't being fetched" stall is diagnosable from journald without
	// adding probes. Info-level only when we actually have mail —
	// debug for the empty case keeps the volume bearable on idle
	// mailboxes (one log line per minute per channel).
	if n := len(msgs); n > 0 {
		c.logger.Info().Int("count", n).Msg("email: fetched unread messages")
	} else {
		c.logger.Debug().Msg("email: poll cycle complete — no unread messages")
	}
	// Leader-epoch fence (review B1): a TTL-expired-but-resumed leader can
	// still see IsLeader()=true and fetch the unseen batch above, but a
	// newer leader has already taken over. Processing the batch here (which
	// creates replies/tasks and MarkSeens) would double-reply the user.
	// VerifyEpoch fails closed: a superseded epoch (or an unreadable lock
	// row) skips the whole batch this cycle — the messages stay unseen for
	// the real leader to handle. A plain IsLeader-only gate (no VerifyEpoch)
	// or a nil gate proceeds, preserving pre-fence behaviour.
	if len(msgs) > 0 {
		if proceed, reason := leaderelection.DangerousWriteAllowed(ctx, c.leaderGate); !proceed {
			c.logger.Warn().Str("reason", reason).Msg("email_channel: leader epoch fence — skipping unseen batch")
			leaderelection.LeaderFenceRejected("email_channel")
			return
		}
	}
	for _, raw := range msgs {
		if ctx.Err() != nil {
			return
		}
		c.handleRawMessage(ctx, raw)
	}
}

// tryReconnectAndRetry handles the transport-drop path. Returns
// true when the post-reconnect fetch succeeded and msgs holds the
// fresh batch; false when reconnect was rate-limited, the client
// doesn't implement IMAPReconnector, the reconnect itself failed,
// or the retry fetch still erred.
func (c *Channel) tryReconnectAndRetry(ctx context.Context, originalErr error, msgs *[]RawMessage) bool {
	rec, ok := c.imap.(IMAPReconnector)
	if !ok {
		// Legacy client without Reconnect — log + wait for next
		// cycle, matching the slice-1 posture.
		c.logger.Warn().Err(originalErr).Msg("email: IMAP transport error; client cannot reconnect — waiting for next poll")
		return false
	}
	if !c.reconnects.tryAcquire() {
		c.logger.Warn().Err(originalErr).Msg("email: IMAP transport error; reconnect rate-limited — waiting for next poll")
		return false
	}
	c.logger.Warn().Err(originalErr).Msg("email: IMAP transport error; reconnecting")
	if err := rec.Reconnect(ctx); err != nil {
		c.logger.Warn().Err(err).Msg("email: IMAP reconnect failed")
		return false
	}
	retryMsgs, retryErr := c.imap.FetchUnseen(ctx)
	if retryErr != nil {
		c.logger.Warn().Err(retryErr).Msg("email: IMAP fetch failed after reconnect — waiting for next poll")
		return false
	}
	*msgs = retryMsgs
	return true
}

// handleRawMessage runs the per-message translation pipeline:
// parse RFC 5322 → enforce allowlist → optional signature
// verification → enforce attachment cap → persist attachments →
// record session → forward to the Receiver → mark message seen on
// the IMAP server so the next poll cycle doesn't re-deliver it.
func (c *Channel) handleRawMessage(ctx context.Context, raw RawMessage) {
	parsed, err := ParseRFC5322(raw.Body)
	if err != nil {
		c.logger.Warn().Err(err).Str("uid", raw.UID).Msg("email: RFC 5322 parse failed; dropping")
		// We still mark the message seen — a re-poll would just
		// re-fail. Operators can recover via direct mailbox tools.
		c.markSeenBestEffort(ctx, raw.UID)
		return
	}
	if !c.senders.allows(parsed.From) {
		c.logger.Warn().
			Str("uid", raw.UID).
			Str("from", parsed.From).
			Msg("email: sender not on allowlist; dropping")
		c.markSeenBestEffort(ctx, raw.UID)
		return
	}
	if err := c.verifier.Verify(ctx, parsed); err != nil {
		c.logger.Warn().Err(err).Str("uid", raw.UID).Str("from", parsed.From).Msg("email: signature verification failed; dropping")
		c.markSeenBestEffort(ctx, raw.UID)
		return
	}
	// Attachment cap is the cheapest gate after auth — refuse early
	// so we never persist or forward a message that exceeds the
	// per-message ceiling.
	if err := enforceAttachmentCap(parsed.Attachments, c.attachmentCap); err != nil {
		c.logger.Warn().Err(err).
			Str("uid", raw.UID).
			Str("from", parsed.From).
			Int("attachment_count", len(parsed.Attachments)).
			Msg("email: attachment cap exceeded; dropping")
		c.markSeenBestEffort(ctx, raw.UID)
		return
	}
	persisted, err := PersistAttachments(ctx, persistAttachmentDeps{
		Repo:      c.attachmentDeps.Repo,
		StoreDir:  c.attachmentDeps.StoreDir,
		ProjectID: c.attachmentDeps.ProjectID,
		MessageID: parsed.MessageID,
		Now:       c.clock,
	}, parsed.Attachments)
	if err != nil {
		// Attachment persistence failure is logged + ignored on the
		// happy path: drop the attachment metadata but still deliver
		// the message body so the operator at least sees the text.
		c.logger.Warn().Err(err).
			Str("uid", raw.UID).
			Int("attachment_count", len(parsed.Attachments)).
			Msg("email: attachment persistence failed; delivering body-only")
		persisted = nil
	}

	// Document-extraction auto-trigger. Synchronous per attachment
	// so the enriched message we hand the dispatcher already
	// reflects the extraction outcome ("ingested 18 chapters" rather
	// than "attached file X.epub"). Best-effort: each attachment's
	// extraction failure is logged and the rest of the pipeline
	// continues. See the AutoExtractor doc comment for the
	// timeout / cancellation contract.
	extractions := c.runAutoExtractions(ctx, persisted)

	msg := c.buildChannelMessage(parsed, raw.UID, persisted, extractions)
	title := parsed.Subject
	if title == "" {
		title = "(no subject)"
	}
	c.recordSession(msg.SessionID, title, parsed.From, msg.Timestamp)

	c.recvMu.RLock()
	recv := c.recv
	c.recvMu.RUnlock()
	if recv == nil {
		c.logger.Warn().Str("uid", raw.UID).Msg("email: inbound received but no Receiver bound; dropping")
		c.markSeenBestEffort(ctx, raw.UID)
		return
	}
	if err := recv.Receive(ctx, msg); err != nil {
		c.logger.Warn().Err(err).Str("uid", raw.UID).Msg("email: Receiver.Receive returned error")
		// Do NOT mark seen — let the next poll cycle re-attempt.
		// (Mirrors the GitHub channel's silent-retry posture for
		// transient downstream failures.)
		return
	}
	c.markSeenBestEffort(ctx, raw.UID)
}

// markSeenBestEffort marks an IMAP message as Seen, logging but
// not propagating any error. Used after a successful Receive (or
// after a permanent drop) so the poll loop doesn't re-deliver.
func (c *Channel) markSeenBestEffort(ctx context.Context, uid string) {
	if err := c.imap.MarkSeen(ctx, uid); err != nil {
		c.logger.Warn().Err(err).Str("uid", uid).Msg("email: IMAP MarkSeen failed (will retry on next cycle)")
	}
}

// buildChannelMessage translates a parsed RFC 5322 message into the
// generic ChannelMessage envelope. SessionID resolution:
//
//   - first References: header entry when present (root of the
//     thread)
//   - else In-Reply-To: when set (the message's parent)
//   - else the message's own Message-ID (a new thread)
//
// This makes sibling replies to the same root collapse into one
// session, matching how mail clients display threads.
//
// Slice 2 addition: when persisted is non-empty, populate
// ChannelMessage.Attachments with one entry per persisted file —
// Name = filename, MimeType = Content-Type, SizeBytes = byte count,
// ChannelRef = the on-disk storage path so downstream consumers
// (the dispatcher's read_artifact tool) can pull the bytes back.
// Mirrors the Telegram channel's intended attachment shape — slice-1
// Telegram code didn't actually populate Attachments either; the
// field's been on conversation.ChannelMessage since the schema was
// frozen but only just gets a first producer now.
// runAutoExtractions invokes the configured AutoExtractor for each
// persisted attachment that has the surfaces it needs (artifact ID
// + storage path). Returns one entry per input in the same order
// — nil at index i means "extraction skipped or failed for
// persisted[i]". The caller's responsibility is to fold the
// returned slice into ChannelMessage.Attachments at the matching
// index.
//
// Concurrency: serialised. The email channel's poll loop is
// single-goroutine, and parallel extractions would compete for
// the same CPU + memory budget on the daemon host. We accept the
// wall-clock cost (extractor.Run is sub-second for EPUBs) in
// exchange for backpressure simplicity.
func (c *Channel) runAutoExtractions(ctx context.Context, persisted []PersistedAttachment) []*conversation.ExtractionSummary {
	out := make([]*conversation.ExtractionSummary, len(persisted))
	if c.cfg.AutoExtractor == nil {
		return out
	}
	for i, p := range persisted {
		if p.Artifact == nil || p.StoragePath == "" {
			continue
		}
		mime := ""
		if p.Artifact.MimeType != nil {
			mime = *p.Artifact.MimeType
		}
		req := AutoExtractRequest{
			ProjectID:   p.Artifact.ProjectID,
			ArtifactID:  p.Artifact.ID,
			Name:        p.Artifact.Name,
			MimeType:    mime,
			StoragePath: p.StoragePath,
		}

		// Per-attachment timeout so a runaway extractor can't
		// block the IMAP poll loop indefinitely.
		callCtx := ctx
		var cancel context.CancelFunc
		if c.cfg.AutoExtractTimeout > 0 {
			callCtx, cancel = context.WithTimeout(ctx, c.cfg.AutoExtractTimeout)
		}
		summary, err := c.cfg.AutoExtractor.AutoExtract(callCtx, req)
		if cancel != nil {
			cancel()
		}
		if err != nil {
			c.logger.Warn().Err(err).
				Str("project_id", req.ProjectID).
				Str("artifact_id", req.ArtifactID).
				Str("mime_type", req.MimeType).
				Str("name", req.Name).
				Msg("email: auto-extract failed; delivering attachment without memory ingest")
			continue
		}
		if summary == nil {
			// Implementation signalled "no extractor for this MIME"
			// — not an error, just not actionable. Skip silently;
			// the regular attachment-arrival metadata still flows.
			continue
		}
		out[i] = &conversation.ExtractionSummary{
			ExtractedDocumentID: summary.ExtractedDocumentID,
			Title:               summary.Title,
			Author:              summary.Author,
			SectionCount:        summary.SectionCount,
			ChunksIngested:      summary.ChunksIngested,
		}
		c.logger.Info().
			Str("project_id", req.ProjectID).
			Str("artifact_id", req.ArtifactID).
			Str("extracted_document_id", summary.ExtractedDocumentID).
			Int("section_count", summary.SectionCount).
			Int("chunks_ingested", summary.ChunksIngested).
			Msg("email: attachment auto-extracted into project memory")
	}
	return out
}

func (c *Channel) buildChannelMessage(parsed ParsedMessage, uid string, persisted []PersistedAttachment, extractions []*conversation.ExtractionSummary) conversation.ChannelMessage {
	sessionID := threadSessionID(parsed)
	ts := parsed.Date
	if ts.IsZero() {
		ts = c.clock()
	}
	cs := map[string]string{
		"message_id": parsed.MessageID,
		"subject":    parsed.Subject,
		"uid":        uid,
	}
	if len(parsed.References) > 0 {
		cs["references"] = strings.Join(parsed.References, " ")
	}
	if parsed.InReplyTo != "" {
		cs["in_reply_to"] = parsed.InReplyTo
	}
	// Record the attachment count even when persistence dropped the
	// bytes (operator can correlate with the warn log line).
	if n := len(parsed.Attachments); n > 0 {
		cs["attachment_count"] = formatInt(n)
	}
	var attachments []conversation.Attachment
	for i, p := range persisted {
		mime := ""
		var size int64
		var artifactID string
		if p.Artifact != nil {
			if p.Artifact.MimeType != nil {
				mime = *p.Artifact.MimeType
			}
			if p.Artifact.SizeBytes != nil {
				size = *p.Artifact.SizeBytes
			}
			artifactID = p.Artifact.ID
		}
		var extraction *conversation.ExtractionSummary
		if i < len(extractions) {
			extraction = extractions[i]
		}
		attachments = append(attachments, conversation.Attachment{
			Name:       artifactName(p),
			MimeType:   mime,
			SizeBytes:  size,
			ChannelRef: p.StoragePath,
			ArtifactID: artifactID,
			Extraction: extraction,
		})
	}
	return conversation.ChannelMessage{
		Source:          channelName,
		ID:              parsed.MessageID,
		SessionID:       sessionID,
		SpeakerID:       parsed.From,
		Text:            parsed.Body,
		Attachments:     attachments,
		InReplyTo:       parsed.InReplyTo,
		ThreadID:        "",
		Timestamp:       ts,
		ChannelSpecific: cs,
	}
}

// artifactName extracts the persisted artifact's Name with a
// defensive nil-check (artifact pointer is always non-nil on the
// happy path but a future PersistAttachments refactor could return
// a bare path; the helper keeps buildChannelMessage tidy).
func artifactName(p PersistedAttachment) string {
	if p.Artifact != nil {
		return p.Artifact.Name
	}
	return ""
}

// formatInt is a thin wrapper that avoids dragging strconv into
// every caller. Inlined call-site readability over a one-liner.
func formatInt(n int) string {
	return fmt.Sprintf("%d", n)
}

// threadSessionID picks the SessionID per the rules described in
// buildChannelMessage's docstring. Exported via the package so the
// SMTP outbound path can derive the same SessionID when looking up
// thread state.
func threadSessionID(parsed ParsedMessage) string {
	if len(parsed.References) > 0 {
		return parsed.References[0]
	}
	if parsed.InReplyTo != "" {
		return parsed.InReplyTo
	}
	return parsed.MessageID
}

// Send delivers an outbound email reply via the configured SMTP
// transport. Returns the channel-assigned Message-ID of the sent
// mail so callers can stash it for future InReplyTo threading.
//
// SessionID is the thread root; the channel looks up the most
// recent inbound on that thread (via the in-memory sessions map)
// to compute the To: header and the Subject: line. When the session
// isn't known (caller invoked Send for a thread the channel never
// saw inbound on), Send returns ErrUnknownSession — the dispatcher
// can fall back to a fresh-thread send by populating
// ChannelSpecific["to"] / ["subject"] explicitly.
//
// Threading headers (In-Reply-To, References) are populated from
// ChannelSpecific when the caller supplies them; otherwise the
// thread root from SessionID is used as both.
func (c *Channel) Send(ctx context.Context, msg conversation.ChannelMessage) (string, error) {
	return c.sendOutbound(ctx, msg, c.buildOutboundAttachments(ctx, msg.Attachments))
}

// SendFile sends a one-off threaded email carrying a single attachment with
// raw bytes (no artifact round-trip) — the outbound surface the dispatcher's
// send_artifact tool uses to deliver a file to an email operator. sessionID is
// the thread root; caption becomes the body. Returns the sent Message-ID.
func (c *Channel) SendFile(ctx context.Context, sessionID, fileName string, content []byte, caption string) (string, error) {
	msg := conversation.ChannelMessage{SessionID: sessionID, Text: caption}
	return c.sendOutbound(ctx, msg, []OutboundAttachment{{Filename: fileName, Content: content}})
}

// sendOutbound resolves the reply envelope + threading for msg, renders the
// RFC 5322 message (with the given already-resolved attachments), and posts it
// via SMTP. Shared by Send (reply path) and SendFile (send_artifact path).
func (c *Channel) sendOutbound(ctx context.Context, msg conversation.ChannelMessage, attachments []OutboundAttachment) (string, error) {
	if c.smtp == nil || strings.TrimSpace(c.cfg.FromAddress) == "" {
		return "", ErrOutboundNotConfigured
	}
	to, subject, err := c.resolveOutboundEnvelope(msg)
	if err != nil {
		return "", err
	}
	// Default both threading headers to the SessionID (the thread root per
	// buildChannelMessage's resolution). ChannelSpecific overrides win, then
	// the message's explicit InReplyTo wins for that one header — so a Send
	// against a known session always produces an RFC-5322 reply chain.
	inReplyTo := msg.SessionID
	references := msg.SessionID
	if msg.InReplyTo != "" {
		inReplyTo = msg.InReplyTo
	}
	if cs := msg.ChannelSpecific; cs != nil {
		if v := cs["references"]; v != "" {
			references = v
		}
		if v := cs["in_reply_to"]; v != "" {
			inReplyTo = v
		}
	}

	out := OutboundMessage{
		From:        c.cfg.FromAddress,
		To:          to,
		Subject:     subject,
		Body:        msg.Text,
		InReplyTo:   inReplyTo,
		References:  references,
		Date:        c.clock(),
		Attachments: attachments,
	}
	rendered, msgID, err := RenderRFC5322(out)
	if err != nil {
		return "", fmt.Errorf("email channel: render outbound: %w", err)
	}
	if err := c.smtp.Send(ctx, SMTPSendRequest{
		From:    c.cfg.FromAddress,
		To:      []string{to},
		Payload: rendered,
	}); err != nil {
		return "", fmt.Errorf("email channel: SMTP send: %w", err)
	}
	return msgID, nil
}

// buildOutboundAttachments resolves each outbound ChannelMessage attachment to
// its on-disk bytes for multipart rendering. Only artifact-backed attachments
// (ArtifactID set) are sendable — the bytes live in the artifact store the
// channel already holds for inbound. Best-effort, mirroring inbound: a missing
// repo, lookup miss, read error, or over-cap file is skipped + logged, and the
// reply still goes out (body + any attachments that did resolve). The total is
// bounded by the same per-message cap as inbound (0 = no cap).
func (c *Channel) buildOutboundAttachments(ctx context.Context, atts []conversation.Attachment) []OutboundAttachment {
	if len(atts) == 0 || c.attachmentDeps.Repo == nil {
		return nil
	}
	var out []OutboundAttachment
	var total int64
	for _, a := range atts {
		if a.ArtifactID == "" {
			continue
		}
		art, err := c.attachmentDeps.Repo.Get(ctx, a.ArtifactID)
		if err != nil || art == nil || art.StoragePath == "" {
			c.logger.Warn().Err(err).Str("artifact_id", a.ArtifactID).
				Msg("email: outbound attachment lookup failed; skipping (body still sent)")
			continue
		}
		data, err := os.ReadFile(art.StoragePath)
		if err != nil {
			c.logger.Warn().Err(err).Str("artifact_id", a.ArtifactID).Str("path", art.StoragePath).
				Msg("email: outbound attachment read failed; skipping (body still sent)")
			continue
		}
		size := int64(len(data))
		if c.attachmentCap > 0 && total+size > c.attachmentCap {
			c.logger.Warn().
				Str("artifact_id", a.ArtifactID).
				Int64("size", size).
				Int64("running_total", total).
				Int64("cap", c.attachmentCap).
				Msg("email: outbound attachment would exceed size cap; skipping")
			continue
		}
		total += size
		filename := a.Name
		if filename == "" {
			filename = art.Name
		}
		ct := a.MimeType
		if ct == "" && art.MimeType != nil {
			ct = *art.MimeType
		}
		out = append(out, OutboundAttachment{Filename: filename, ContentType: ct, Content: data})
	}
	return out
}

// resolveOutboundEnvelope figures out the To: and Subject: for an
// outbound Send. Caller-supplied ChannelSpecific["to"] / ["subject"]
// always win; otherwise we look up the session's last inbound to
// recover them. Returns ErrUnknownSession when neither path yields
// a To: address.
func (c *Channel) resolveOutboundEnvelope(msg conversation.ChannelMessage) (to, subject string, err error) {
	if cs := msg.ChannelSpecific; cs != nil {
		to = strings.TrimSpace(cs["to"])
		subject = strings.TrimSpace(cs["subject"])
	}
	if to != "" && subject != "" {
		return to, subject, nil
	}
	c.sessionsMu.Lock()
	entry, ok := c.sessions[msg.SessionID]
	c.sessionsMu.Unlock()
	if ok {
		if subject == "" {
			subject = "Re: " + entry.Title
		}
		if to == "" {
			// participants set is keyed by raw From: address;
			// any one of them is a fine reply target for slice 1
			// (one-on-one threading).
			for p := range entry.participants {
				to = p
				break
			}
		}
	}
	if to == "" {
		return "", "", ErrUnknownSession
	}
	if subject == "" {
		subject = "Re: (no subject)"
	}
	return to, subject, nil
}

// ListSessions returns a snapshot of every thread that has produced
// at least one inbound message since daemon start. Sorted
// newest-first by LastActivity for operator-UI consumption.
// In-memory only; restart clears the set.
func (c *Channel) ListSessions(_ context.Context) ([]conversation.Session, error) {
	c.sessionsMu.Lock()
	defer c.sessionsMu.Unlock()
	out := make([]conversation.Session, 0, len(c.sessions))
	for id, e := range c.sessions {
		out = append(out, conversation.Session{
			ID:               id,
			Title:            e.Title,
			LastActivity:     e.LastActivity,
			ParticipantCount: e.ParticipantCount,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastActivity.After(out[j].LastActivity)
	})
	return out, nil
}

// recordSession upserts the in-memory session map. Mirrors the
// GitHub channel's recordSession so both channels look the same to
// the operator UI.
func (c *Channel) recordSession(sessionID, title, participant string, when time.Time) {
	c.sessionsMu.Lock()
	defer c.sessionsMu.Unlock()
	e, ok := c.sessions[sessionID]
	if !ok {
		e = &sessionEntry{
			participants: map[string]struct{}{},
		}
		c.sessions[sessionID] = e
	}
	if title != "" {
		e.Title = title
	}
	if when.After(e.LastActivity) {
		e.LastActivity = when
	}
	if participant != "" {
		if _, seen := e.participants[participant]; !seen {
			e.participants[participant] = struct{}{}
			e.ParticipantCount = len(e.participants)
		}
	}
}

// ResolveSpeaker maps an email address to a conversation.Speaker.
// Returns ErrSpeakerUnknown when the SenderAllowlist is non-empty
// and the address isn't permitted (matches the GitHub channel's
// allowlist-gated dev-mode pass-through).
func (c *Channel) ResolveSpeaker(_ context.Context, channelSpeakerID string) (conversation.Speaker, error) {
	if !c.senders.allows(channelSpeakerID) {
		return conversation.Speaker{}, conversation.ErrSpeakerUnknown
	}
	return conversation.Speaker{
		ID:            "email:" + strings.ToLower(channelSpeakerID),
		DisplayName:   channelSpeakerID,
		ChannelHandle: channelSpeakerID,
	}, nil
}

// senderAllowlist canonicalises the operator-supplied list once at
// construction so the per-message check is a single map lookup.
type senderAllowlist struct {
	// permitAll is true when the operator-supplied list is empty —
	// dev-mode pass-through.
	permitAll bool
	// addresses holds the full-address entries lowercased.
	addresses map[string]struct{}
	// domains holds the bare-domain entries lowercased.
	domains map[string]struct{}
}

func newSenderAllowlist(in []string) senderAllowlist {
	out := senderAllowlist{
		addresses: map[string]struct{}{},
		domains:   map[string]struct{}{},
	}
	trimmed := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		trimmed = append(trimmed, s)
	}
	if len(trimmed) == 0 {
		out.permitAll = true
		return out
	}
	for _, s := range trimmed {
		if strings.Contains(s, "@") {
			out.addresses[s] = struct{}{}
		} else {
			out.domains[s] = struct{}{}
		}
	}
	return out
}

// allows reports whether the given From: address is admitted. The
// match is case-insensitive on both the local part and the domain
// (RFC 5321 permits case-sensitive local parts, but every mainstream
// provider treats them as case-insensitive; matching that posture
// avoids operator surprises).
func (s senderAllowlist) allows(from string) bool {
	if s.permitAll {
		return true
	}
	addr := strings.ToLower(strings.TrimSpace(from))
	if addr == "" {
		return false
	}
	if _, ok := s.addresses[addr]; ok {
		return true
	}
	at := strings.LastIndex(addr, "@")
	if at < 0 {
		return false
	}
	domain := addr[at+1:]
	_, ok := s.domains[domain]
	return ok
}

// ErrOutboundNotConfigured is returned by Send when SMTP wiring is
// incomplete (no SMTPSender, no FromAddress). Mirrors the GitHub
// channel's sentinel so the dispatcher can errors.Is across
// channels.
var ErrOutboundNotConfigured = errors.New("email channel: outbound credentials not configured (set SMTPHost + SMTPUsername + SMTPPassword + FromAddress)")

// ErrUnknownSession is returned by Send when the channel can't
// resolve a To: address for the supplied SessionID — neither the
// caller's ChannelSpecific["to"] nor an inbound history entry
// provided one.
var ErrUnknownSession = errors.New("email channel: cannot send — no To: address resolvable from SessionID and no ChannelSpecific[to] supplied")

// Compile-time guarantee: *Channel satisfies the conversation
// Channel contract.
var _ conversation.Channel = (*Channel)(nil)
