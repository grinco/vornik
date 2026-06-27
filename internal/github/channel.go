// Package github implements vornik's GitHub App conversation
// channel (slice 4 of the ConversationChannel rollout; see
// https://docs.vornik.io and
// https://docs.vornik.io).
//
// This package owns:
//
//   - the webhook HTTP handler that GitHub posts to
//     (`/api/v1/github-app/webhook`)
//   - HMAC-SHA256 signature verification on inbound deliveries
//   - the per-event translation from GitHub's payload shape into
//     conversation.ChannelMessage
//   - the conversation.Channel interface, so vornik's
//     dispatcher / scheduler stay unaware that the source is
//     GitHub
//
// Slice 4A+4B status (2026-05-17): inbound + translation + Receiver
// invocation work end-to-end. Outbound Send returns
// ErrOutboundNotImplemented; slice 4C will land JWT signing,
// installation-token caching, and the issue-comment POST.
package github

import (
	"context"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/conversation"
)

const channelName = "github-app"

// maxWebhookBodyBytes caps inbound payload size. GitHub's webhooks
// are capped at 25 MiB upstream; the daemon cap is conservative so
// a malformed delivery can't exhaust memory. The cap mirrors the
// existing internal/api webhook handler.
const maxWebhookBodyBytes = 8 * 1024 * 1024

// Config carries the operator-provided GitHub App settings the
// channel needs. Populated from project config at boot; never
// mutated after Channel construction. Secrets are kept off the
// struct — the channel stores the resolved secret as a byte slice.
//
// Two modes are supported:
//
//   - Single-installation (back-compat): leave Installations nil and
//     populate the top-level AppID/PrivateKey/InstallationID/
//     RepoAllowlist/TaskLabels/PRReviewLabels/SenderAllowlist/
//     TaskCreator fields directly. The channel routes every inbound
//     delivery through that single installation.
//   - Multi-installation: populate Installations with one entry per
//     project. On every inbound webhook the channel resolves the
//     installation by payload.installation.id and uses that
//     entry's allowlists + TaskCreator. The outbound token cache is
//     keyed per-installation. Top-level RepoAllowlist/TaskLabels/
//     etc. are ignored when Installations is non-empty — every
//     installation owns its own.
type Config struct {
	// AppID is the GitHub App's numeric ID (visible in the GitHub
	// App settings page). Zero disables the outbound Send path
	// (inbound webhook reception still works); operators who only
	// want inbound notifications can leave it unset.
	//
	// Single-installation mode only. Multi-installation mode reads
	// AppID from each InstallationConfig.
	AppID int64

	// PrivateKey is the GitHub App's RSA private key, used to sign
	// the JWT that exchanges for an installation access token.
	// Operators load via LoadPrivateKeyPEM from disk or a secret
	// store. Nil disables outbound.
	//
	// Single-installation mode only. Multi-installation mode reads
	// PrivateKey from each InstallationConfig.
	PrivateKey *rsa.PrivateKey

	// InstallationID is the GitHub App installation ID for the
	// target organisation / user account. Discoverable via
	// `GET /app/installations` with the App JWT. Zero disables
	// outbound.
	//
	// Single-installation mode only. Multi-installation mode reads
	// InstallationID from each InstallationConfig.
	InstallationID int64

	// APIBaseURL overrides the GitHub REST endpoint. Empty defaults
	// to `https://api.github.com`. Tests inject an httptest stub
	// URL; GitHub Enterprise deployments set this to their own
	// `https://github.example.com/api/v3` base.
	//
	// Shared across every installation — GitHub Enterprise
	// deployments host a single API base for the whole instance.
	APIBaseURL string

	// HTTPClient overrides the HTTP transport used for outbound
	// REST calls. Nil falls back to http.DefaultClient. Tests
	// inject a client configured against an httptest.Server.
	HTTPClient *http.Client

	// WebhookSecret is the shared secret GitHub uses to HMAC-sign
	// every delivery. Empty rejects every inbound delivery with an
	// error so a misconfigured operator gets a loud failure rather
	// than silent acceptance of unverified payloads.
	//
	// GitHub Apps sign every delivery for every installation with
	// the single per-App secret, so this is channel-wide rather
	// than per-installation.
	WebhookSecret string

	// RepoAllowlist is the set of "owner/repo" full names this
	// channel accepts events from. Empty means deny-all (defensive
	// default — a fresh install with no repo set must reject every
	// delivery).
	//
	// Single-installation mode only. Multi-installation mode reads
	// RepoAllowlist from each InstallationConfig.
	RepoAllowlist []string

	// TaskLabels lists labels that, when applied to an issue, fire
	// the task-creation path. Empty disables that path.
	//
	// Single-installation mode only.
	TaskLabels []string

	// PRReviewLabels lists labels that, when present on an opened
	// PR, fire the review-task path. Empty means "all opened PRs
	// trigger a review task" (per the design doc).
	//
	// Single-installation mode only.
	PRReviewLabels []string

	// SenderAllowlist lists GitHub login names allowed to trigger
	// the @vornik reply path via issue_comment.created. Empty allows
	// all logins (dev-mode pass-through, matching Telegram's
	// IsAllowed semantics).
	//
	// Single-installation mode only.
	SenderAllowlist []string

	// Logger is the channel's zerolog instance. Zero-value is fine
	// but produces no log output.
	Logger zerolog.Logger

	// TaskCreator is invoked on the two non-conversational event
	// paths (issues.labeled with a matching TaskLabels entry, and
	// pull_request.opened gated by PRReviewLabels) to fire task
	// creation. Nil disables the path — handlers log "task creation
	// not wired" and discard. Slice 4D's service container provides
	// a real implementation; tests supply stubs.
	//
	// Single-installation mode only. Multi-installation mode reads
	// TaskCreator from each InstallationConfig.
	TaskCreator TaskCreator

	// Installations lists one entry per GitHub App installation the
	// channel serves. When non-empty, the channel routes inbound
	// deliveries by matching payload.installation.id against
	// InstallationConfig.InstallationID. Each installation carries
	// its own project ID, allowlists, and outbound credentials.
	//
	// Unknown installation IDs are dropped with an audit log entry
	// + HTTP 200 (matching the rest-of-codebase contract: never
	// 4xx GitHub or it retries indefinitely).
	//
	// Single-installation backwards-compat: leave nil/empty and
	// populate the top-level fields. With exactly one project
	// configured at the boot layer the channel produces identical
	// behaviour to the pre-multi-install code path.
	Installations []InstallationConfig
}

// InstallationConfig describes one GitHub App installation served
// by the channel. Multi-installation mode pivots routing on the
// InstallationID — the channel's HandleWebhook reads
// `payload.installation.id` and looks up the matching entry.
//
// Outbound credentials (AppID + PrivateKey) live on each entry so
// every project can run under its own GitHub App if needed;
// operators with a single App + multiple installations populate
// the same AppID/PrivateKey across entries.
type InstallationConfig struct {
	// ProjectID identifies the vornik project this installation
	// belongs to. Required for routing TaskCreationEvents to the
	// correct project and for session-store project pinning.
	ProjectID string

	// InstallationID is the GitHub App installation ID for this
	// project's target org / user. Required.
	InstallationID int64

	// AppID is the GitHub App's numeric ID for this installation's
	// outbound replies. Zero disables outbound replies for this
	// installation (inbound webhook handling still works).
	AppID int64

	// PrivateKey is the GitHub App's RSA private key for this
	// installation's outbound replies. Nil disables outbound.
	PrivateKey *rsa.PrivateKey

	// RepoAllowlist is the set of "owner/repo" full names this
	// installation accepts events from. Required (defensive
	// deny-all default).
	RepoAllowlist []string

	// TaskLabels lists labels that, when applied to an issue, fire
	// this installation's task-creation path. Empty disables that
	// path for this installation.
	TaskLabels []string

	// PRReviewLabels lists labels that gate the
	// pull_request.opened review-task path. Empty means every
	// opened PR fires.
	PRReviewLabels []string

	// SenderAllowlist lists GitHub login names allowed to trigger
	// the @vornik reply path for this installation. Empty allows
	// every login (dev-mode pass-through).
	SenderAllowlist []string

	// TaskCreator is the per-installation TaskCreator. Nil disables
	// the task-creation paths for this installation; the channel
	// logs "TaskCreator not wired" and discards the event.
	TaskCreator TaskCreator
}

// TaskCreator wires the GitHub channel's non-conversational paths
// (label-driven task creation, opened-PR review tasks) into the
// daemon's task layer without forcing this package to import the
// task / persistence stack. Implementations live in the service
// container.
//
// The Create method runs in the webhook handler's request
// goroutine — implementations that need to do heavy work (LLM
// classification, durable queueing) should hand off to a worker
// rather than blocking the handler. Returning an error logs the
// failure but does not change the HTTP response to GitHub (we
// always 200 on a valid signed delivery to prevent retries —
// duplicate deliveries are de-duped via the IdempotencyKey).
type TaskCreator interface {
	Create(ctx context.Context, ev TaskCreationEvent) error
}

// TaskCreationEvent is the channel-agnostic envelope passed to
// TaskCreator. Mirrors the fields the existing webhook task
// creator already consumes (`createWebhookTask`), so a service-
// container adapter is a thin translation rather than a new
// schema.
type TaskCreationEvent struct {
	// Kind is the trigger: "issues.labeled" or "pull_request.opened".
	// Tracks separately so the implementation can route to different
	// task types / workflows per trigger.
	Kind string

	// SessionID is the channel session this event belongs to (issue
	// or PR), in the form `owner/repo#issues/N` / `owner/repo#pulls/N`.
	SessionID string

	// Title is the issue / PR title — used as the task title or the
	// "subject" line of the LLM prompt.
	Title string

	// Body is the issue / PR body. Trimmed to a reasonable cap by
	// the implementation as needed.
	Body string

	// Labels is the full label set on the issue or PR at event
	// time. For `issues.labeled` the matched label is included; for
	// `pull_request.opened` it's whatever labels the PR was opened
	// with.
	Labels []string

	// SenderLogin is the GitHub user who triggered the event.
	SenderLogin string

	// Repo is the `owner/repo` full name.
	Repo string

	// Number is the issue / PR number.
	Number int

	// DefaultBranch is the repository's default branch
	// (repository.default_branch) — the base for opened change requests and
	// the pre-work rebase target. Empty when the payload omitted it.
	DefaultBranch string

	// InstallationID is the GitHub App installation that received
	// the event.
	InstallationID int64

	// IdempotencyKey is `github-app:<X-GitHub-Delivery>` — stable
	// per-delivery so retries de-dupe. Mirrors the convention from
	// the existing generic webhook handler.
	IdempotencyKey string
}

// Channel is the GitHub App's conversation.Channel implementation.
// Constructed once at boot; the HTTP handler (HandleWebhook) is
// mounted on the daemon's API server by the service container.
type Channel struct {
	cfg         Config
	secretBytes []byte

	apiBaseURL string
	httpClient *http.Client

	logger zerolog.Logger

	recvMu sync.RWMutex
	recv   conversation.Receiver

	// installations holds the resolved set of routes the channel
	// serves. installationsByID indexes the same set by
	// payload.installation.id for O(1) lookup in HandleWebhook.
	//
	// Always non-empty after New: single-installation mode produces
	// a one-element slice synthesised from the top-level Config
	// fields so the inbound dispatch and outbound Send paths share
	// the same routing primitive.
	installations     []*installation
	installationsByID map[int64]*installation

	// sessionsMu guards the in-memory active-session map. Populated
	// on every inbound translation (issue/PR comment, label,
	// PR-opened); read by ListSessions for the operator UI.
	// In-memory only: daemon restart clears it.
	sessionsMu sync.Mutex
	sessions   map[string]*sessionEntry
}

// installation is the resolved internal form of an InstallationConfig
// plus the per-installation outbound token cache. One per
// configured route; the channel never mutates these after New.
//
// The token cache lives here so concurrent Sends targeting
// different installations don't serialise behind a single mutex —
// each installation mints / refreshes its own access token.
type installation struct {
	projectID      string
	installationID int64
	appID          int64
	privateKey     *rsa.PrivateKey

	allowedRepos map[string]struct{}
	taskLabels   map[string]struct{}
	prLabels     map[string]struct{}
	senders      map[string]struct{}

	repoAllowlistRaw []string
	taskLabelsRaw    []string
	prLabelsRaw      []string
	sendersRaw       []string

	taskCreator TaskCreator

	// tokenMu guards the installation-access-token cache. Held
	// across the JWT exchange so two concurrent Sends after expiry
	// mint exactly one new token.
	tokenMu      sync.Mutex
	token        string
	tokenExpires time.Time
}

// sessionEntry holds the per-session metadata ListSessions
// surfaces. Title is best-effort (issue title or "PR #N" if a
// title isn't known); LastActivity drives newest-first sort.
//
// installation pins the session to a specific GitHub App
// installation for the lifetime of the issue/PR. The outbound
// Send path resolves which installation owns a SessionID by
// reading this field — necessary because the SessionID shape
// (`owner/repo#issues/N`) doesn't carry an installation_id.
//
// projectID mirrors installation.projectID for fast read access
// from the session-store layer (which doesn't need the full
// installation pointer).
type sessionEntry struct {
	Title            string
	LastActivity     time.Time
	ParticipantCount int
	participants     map[string]struct{}

	installation *installation
	projectID    string
}

// New constructs a Channel from the given Config. Returns an error
// when the config is structurally broken (empty webhook secret,
// no installations + empty repo allowlist, duplicate
// installation_id across the Installations slice) — defensive
// defaults catch the misconfig at boot rather than the first
// delivery.
func New(cfg Config) (*Channel, error) {
	if cfg.WebhookSecret == "" {
		return nil, errors.New("github-app channel: WebhookSecret is required")
	}
	apiBase := cfg.APIBaseURL
	if apiBase == "" {
		apiBase = defaultAPIBaseURL
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	installs, err := resolveInstallations(cfg)
	if err != nil {
		return nil, err
	}

	c := &Channel{
		cfg:               cfg,
		secretBytes:       []byte(cfg.WebhookSecret),
		apiBaseURL:        apiBase,
		httpClient:        httpClient,
		logger:            cfg.Logger,
		sessions:          make(map[string]*sessionEntry),
		installations:     installs,
		installationsByID: indexInstallations(installs),
	}
	return c, nil
}

// resolveInstallations normalises Config's two routing modes into
// a single internal []*installation. Single-installation mode
// (Installations nil) synthesises one entry from the top-level
// fields; multi-installation mode validates every entry has a
// non-empty repo allowlist + a non-zero InstallationID +
// no-duplicate IDs.
func resolveInstallations(cfg Config) ([]*installation, error) {
	if len(cfg.Installations) == 0 {
		// Back-compat single-installation mode.
		if len(cfg.RepoAllowlist) == 0 {
			return nil, errors.New("github-app channel: RepoAllowlist must contain at least one repo")
		}
		inst := buildInstallation(InstallationConfig{
			ProjectID:       "",
			InstallationID:  cfg.InstallationID,
			AppID:           cfg.AppID,
			PrivateKey:      cfg.PrivateKey,
			RepoAllowlist:   cfg.RepoAllowlist,
			TaskLabels:      cfg.TaskLabels,
			PRReviewLabels:  cfg.PRReviewLabels,
			SenderAllowlist: cfg.SenderAllowlist,
			TaskCreator:     cfg.TaskCreator,
		})
		return []*installation{inst}, nil
	}

	seen := make(map[int64]string, len(cfg.Installations))
	out := make([]*installation, 0, len(cfg.Installations))
	for i, ic := range cfg.Installations {
		if ic.InstallationID == 0 {
			return nil, fmt.Errorf("github-app channel: Installations[%d] missing InstallationID", i)
		}
		if len(ic.RepoAllowlist) == 0 {
			return nil, fmt.Errorf("github-app channel: Installations[%d] (installation_id %d) has empty RepoAllowlist", i, ic.InstallationID)
		}
		if prev, ok := seen[ic.InstallationID]; ok {
			return nil, fmt.Errorf("github-app channel: duplicate installation_id %d (projects %q and %q)",
				ic.InstallationID, prev, ic.ProjectID)
		}
		seen[ic.InstallationID] = ic.ProjectID
		out = append(out, buildInstallation(ic))
	}
	return out, nil
}

// buildInstallation translates an InstallationConfig into its
// resolved internal form (with the lookup-set maps pre-indexed).
func buildInstallation(ic InstallationConfig) *installation {
	return &installation{
		projectID:        ic.ProjectID,
		installationID:   ic.InstallationID,
		appID:            ic.AppID,
		privateKey:       ic.PrivateKey,
		allowedRepos:     indexSet(ic.RepoAllowlist),
		taskLabels:       indexSet(ic.TaskLabels),
		prLabels:         indexSet(ic.PRReviewLabels),
		senders:          indexSet(ic.SenderAllowlist),
		repoAllowlistRaw: append([]string(nil), ic.RepoAllowlist...),
		taskLabelsRaw:    append([]string(nil), ic.TaskLabels...),
		prLabelsRaw:      append([]string(nil), ic.PRReviewLabels...),
		sendersRaw:       append([]string(nil), ic.SenderAllowlist...),
		taskCreator:      ic.TaskCreator,
	}
}

// indexInstallations builds the installation_id → *installation
// lookup map used by HandleWebhook to route inbound deliveries.
//
// In single-installation mode the channel is intentionally
// tolerant of a zero InstallationID: many inbound-only operators
// don't bother setting it on the Config side, and we still want
// payload.installation.id-keyed routing to land on the only
// configured route. The empty-key entry is added below so
// installationsByID always indexes the one route.
func indexInstallations(installs []*installation) map[int64]*installation {
	out := make(map[int64]*installation, len(installs))
	for _, inst := range installs {
		if inst.installationID != 0 {
			out[inst.installationID] = inst
		}
	}
	return out
}

// resolveInstallationForPayload returns the installation that
// should handle the given payload.installation.id. Single-
// installation mode falls through to the only configured route
// regardless of the ID (operators who only run one route don't
// need to set InstallationID on the inbound-only Config).
// Multi-installation mode requires an exact match — unknown IDs
// produce (nil, false) so HandleWebhook can audit-log + drop.
func (c *Channel) resolveInstallationForPayload(installationID int64) (*installation, bool) {
	if inst, ok := c.installationsByID[installationID]; ok {
		return inst, true
	}
	// Single-installation back-compat: if exactly one route is
	// configured and the operator's Config didn't pin an
	// InstallationID, route every event to that one installation.
	// This preserves the pre-multi-installation behaviour where a
	// single-tenant deployment didn't care about the payload's
	// installation_id.
	if len(c.installations) == 1 && len(c.cfg.Installations) == 0 {
		return c.installations[0], true
	}
	return nil, false
}

func indexSet(in []string) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for _, s := range in {
		out[s] = struct{}{}
	}
	return out
}

// Name implements conversation.Channel.
func (c *Channel) Name() string { return channelName }

// Start binds the Receiver and blocks until ctx is cancelled.
// Unlike Telegram, the GitHub App is webhook-driven — there's no
// poll loop to run. Start exists purely to satisfy the Channel
// contract; the HTTP handler is mounted on the daemon's API server
// at boot and runs in its own goroutine pool.
func (c *Channel) Start(ctx context.Context, recv conversation.Receiver) error {
	c.recvMu.Lock()
	c.recv = recv
	c.recvMu.Unlock()
	c.verifyInstallationPermissions(ctx)
	<-ctx.Done()
	return ctx.Err()
}

// verifyInstallationPermissions warns at boot when an installation with outbound
// credentials lacks `contents: write` — the permission forge.PushBranch needs to
// open a change request. Best-effort and non-fatal: a network hiccup or an
// inbound-only installation just logs and moves on, so a transient GitHub blip
// never blocks daemon start.
func (c *Channel) verifyInstallationPermissions(ctx context.Context) {
	for _, inst := range c.installations {
		if inst == nil || inst.appID == 0 || inst.installationID == 0 || inst.privateKey == nil {
			continue // inbound-only installation; nothing to push with
		}
		ok, level, err := CheckContentsWrite(ctx, c.httpClient, c.apiBaseURL, inst.appID, inst.installationID, inst.privateKey)
		if err != nil {
			c.logger.Debug().Err(err).Str("project_id", inst.projectID).
				Msg("github-app: could not verify installation permissions at start (continuing)")
			continue
		}
		if !ok {
			c.logger.Warn().
				Str("project_id", inst.projectID).
				Int64("installation_id", inst.installationID).
				Str("contents_permission", level).
				Msg("github-app: installation lacks 'contents: write' — forge.open_change_request will fail to push branches; grant Contents:Read&write on the App installation")
		}
	}
}

// Stop clears the Receiver binding. Idempotent.
func (c *Channel) Stop() error {
	c.recvMu.Lock()
	c.recv = nil
	c.recvMu.Unlock()
	return nil
}

// Send posts an issue / PR comment via the GitHub REST API,
// authenticated via a cached installation access token (minted
// lazily on first call). When outbound credentials aren't
// configured (AppID / PrivateKey / InstallationID missing), Send
// returns ErrOutboundNotConfigured — inbound webhook reception
// still works in that mode.
//
// Returns the GitHub comment ID as a decimal string so the
// DispatcherReceiver can stash it for future InReplyTo threading.
func (c *Channel) Send(ctx context.Context, msg conversation.ChannelMessage) (string, error) {
	return c.sendIssueComment(ctx, msg.SessionID, msg.Text)
}

// ListSessions returns a snapshot of every GitHub issue / PR that
// has produced at least one inbound event since daemon start.
// Sorted newest-first by LastActivity so the operator UI can show
// "most-recent conversations" without further work. In-memory
// only; restart clears the set.
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
	// Newest first.
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastActivity.After(out[j].LastActivity)
	})
	return out, nil
}

// recordSession upserts the in-memory session map. Called by every
// inbound translation path so ListSessions reflects every issue /
// PR the channel has heard from. Adds participant via the
// per-issue set so subsequent comments from the same user don't
// inflate the count.
//
// inst pins the session to the GitHub App installation that
// produced the first event on this issue/PR. Subsequent events on
// the same session reuse the originally-recorded installation —
// see the per-session installation contract on sessionEntry.
// Passing nil leaves the existing pin untouched (defensive: a
// legacy call site shouldn't blow away the entry's owner).
func (c *Channel) recordSession(sessionID, title, participant string, when time.Time, inst *installation) {
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
	if inst != nil && e.installation == nil {
		// First event on this session pins the installation. We
		// don't re-pin on subsequent events even if a later one
		// arrives via a different installation_id (which would be a
		// configuration bug anyway — one GitHub issue belongs to one
		// installation).
		e.installation = inst
		e.projectID = inst.projectID
	}
}

// ProjectForSession returns the project ID the channel has
// recorded for the given GitHub session (issue or PR). Returns
// "" when the session is unknown to the channel — used by the
// service container's session store to avoid mis-routing a
// dispatcher turn into another project's tools.
//
// The pin is set on the first event that produces a session entry
// and never changes for the life of that issue/PR (daemon-restart
// resets the in-memory map; subsequent events will re-pin via the
// fresh payload's installation_id, which is by design the same
// installation).
func (c *Channel) ProjectForSession(sessionID string) string {
	c.sessionsMu.Lock()
	defer c.sessionsMu.Unlock()
	if e, ok := c.sessions[sessionID]; ok {
		return e.projectID
	}
	return ""
}

// ResolveSpeaker maps a GitHub login to a conversation.Speaker.
// Returns ErrSpeakerUnknown when the channel-wide SenderAllowlist
// is non-empty and the login isn't on it. Empty allowlist passes
// through (dev mode).
//
// Multi-installation mode: this surface still represents the
// channel as a whole — a speaker is admissible when ANY
// installation's SenderAllowlist accepts them (or when at least
// one installation has an empty allowlist = dev-mode pass). The
// per-installation enforcement that matters for the @vornik reply
// path is the gate inside handleIssueComment, which knows which
// installation owns the event.
func (c *Channel) ResolveSpeaker(_ context.Context, channelSpeakerID string) (conversation.Speaker, error) {
	if !c.anyInstallationAllowsSpeaker(channelSpeakerID) {
		return conversation.Speaker{}, conversation.ErrSpeakerUnknown
	}
	return conversation.Speaker{
		ID:            "github:" + channelSpeakerID,
		DisplayName:   channelSpeakerID,
		ChannelHandle: channelSpeakerID,
	}, nil
}

// anyInstallationAllowsSpeaker returns true when at least one
// installation's SenderAllowlist permits the login (or has an
// empty allowlist = dev-mode pass-through). Used by
// ResolveSpeaker to decide whether the channel as a whole knows
// the speaker; per-installation enforcement on the @vornik reply
// path runs separately in handleIssueComment after the
// installation has been resolved from payload.installation.id.
func (c *Channel) anyInstallationAllowsSpeaker(login string) bool {
	for _, inst := range c.installations {
		if len(inst.senders) == 0 {
			return true // dev-mode pass-through on at least one route
		}
		if _, ok := inst.senders[login]; ok {
			return true
		}
	}
	return false
}

// resolveSpeakerForInstallation enforces the per-installation
// SenderAllowlist gate. Mirrors ResolveSpeaker but scopes the
// allowlist check to a single installation — used by the
// @vornik reply path after HandleWebhook has resolved which
// installation owns the delivery.
func (c *Channel) resolveSpeakerForInstallation(inst *installation, login string) (conversation.Speaker, error) {
	if len(inst.senders) > 0 {
		if _, ok := inst.senders[login]; !ok {
			return conversation.Speaker{}, conversation.ErrSpeakerUnknown
		}
	}
	return conversation.Speaker{
		ID:            "github:" + login,
		DisplayName:   login,
		ChannelHandle: login,
	}, nil
}

// HandleWebhook is the HTTP entry point for inbound GitHub
// deliveries. Mount on the daemon's API mux at
// `/api/v1/github-app/webhook`.
//
// Flow:
//  1. Verify HMAC against the configured webhook secret. Reject 401
//     on mismatch.
//  2. Parse minimal payload fields (event type, action, sender,
//     repository).
//  3. Reject if the repository isn't in the allowlist (403 +
//     audit log).
//  4. Branch on event type per the github-app-channel-design.md
//     mapping table.
//  5. Always respond 200 to handled / discarded events — anything
//     non-200 puts the delivery into GitHub's retry backoff.
func (c *Channel) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBodyBytes+1))
	if err != nil {
		http.Error(w, "read body failed", http.StatusBadRequest)
		return
	}
	if int64(len(body)) > maxWebhookBodyBytes {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	if err := c.verifySignature(r, body); err != nil {
		c.logger.Warn().Err(err).Msg("github-app: signature verification failed")
		http.Error(w, "unauthorised", http.StatusUnauthorized)
		return
	}

	event := strings.TrimSpace(r.Header.Get("X-GitHub-Event"))
	delivery := strings.TrimSpace(r.Header.Get("X-GitHub-Delivery"))
	if event == "" {
		http.Error(w, "missing X-GitHub-Event", http.StatusBadRequest)
		return
	}

	var payload eventPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		c.logger.Warn().Err(err).Str("event", event).Msg("github-app: payload parse failed")
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if payload.Repository.FullName == "" {
		// Some event types (e.g. installation, ping) don't carry a
		// repository — ack without further processing.
		c.logger.Debug().Str("event", event).Msg("github-app: event without repository, acking")
		w.WriteHeader(http.StatusOK)
		return
	}

	// Multi-installation routing: resolve which configured project
	// owns this delivery via payload.installation.id. Unknown IDs
	// are audit-logged + dropped with 200 (GitHub retries on
	// non-200; the rest of the codebase's contract is "200 + log +
	// discard" for unrecognised deliveries).
	inst, ok := c.resolveInstallationForPayload(payload.Installation.ID)
	if !ok {
		c.logger.Warn().
			Str("event", event).
			Str("repo", payload.Repository.FullName).
			Int64("installation_id", payload.Installation.ID).
			Str("delivery", delivery).
			Msg("github-app: installation_id not recognised; dropping delivery")
		w.WriteHeader(http.StatusOK)
		return
	}

	if _, repoOK := inst.allowedRepos[payload.Repository.FullName]; !repoOK {
		c.logger.Warn().
			Str("event", event).
			Str("repo", payload.Repository.FullName).
			Str("project_id", inst.projectID).
			Int64("installation_id", payload.Installation.ID).
			Str("delivery", delivery).
			Msg("github-app: repository not in installation allowlist")
		http.Error(w, "repository not allowed", http.StatusForbidden)
		return
	}

	// Branch on (event, action). Anything else is acked + dropped
	// so GitHub doesn't retry.
	switch event {
	case "issues":
		if payload.Action == "labeled" {
			c.handleIssueLabeled(r.Context(), event, delivery, payload, inst)
		}
	case "issue_comment":
		if payload.Action == "created" {
			c.handleIssueComment(r.Context(), event, delivery, payload, inst)
		}
	case "pull_request":
		if payload.Action == "opened" {
			c.handlePullRequestOpened(r.Context(), event, delivery, payload, inst)
		}
	default:
		c.logger.Debug().Str("event", event).Str("action", payload.Action).
			Msg("github-app: event type not handled, acking")
	}

	w.WriteHeader(http.StatusOK)
}

// verifySignature validates the X-Hub-Signature-256 header against
// the configured webhook secret using HMAC-SHA256. Same wire
// protocol as internal/api/webhook_handlers.go's
// verifyWebhookSignature, but parameterised on the channel's
// secret rather than ProjectWebhookSource.
func (c *Channel) verifySignature(r *http.Request, body []byte) error {
	sig := strings.TrimSpace(r.Header.Get("X-Hub-Signature-256"))
	sig = strings.TrimPrefix(sig, "sha256=")
	if sig == "" {
		return errors.New("missing X-Hub-Signature-256")
	}
	want := computeHMAC(c.secretBytes, body)
	gotBytes, err := hex.DecodeString(sig)
	if err != nil {
		return fmt.Errorf("malformed signature: %w", err)
	}
	if !hmac.Equal(gotBytes, want) {
		return errors.New("signature mismatch")
	}
	return nil
}

func computeHMAC(secret, body []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	return mac.Sum(nil)
}

// eventPayload is the minimal subset of GitHub's webhook envelope
// the channel needs. Each event type may populate a different
// subset; we use pointer fields so unmarshalling against a payload
// that lacks them is a no-op rather than a parse error.
type eventPayload struct {
	Action     string `json:"action"`
	Repository struct {
		FullName      string `json:"full_name"`
		Name          string `json:"name"`
		DefaultBranch string `json:"default_branch"`
		Owner         struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
	Sender struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
	} `json:"sender"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`

	// Issue is populated on `issues` and `issue_comment` events.
	Issue *struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
		PullRequest *struct {
			URL string `json:"url"`
		} `json:"pull_request,omitempty"`
	} `json:"issue,omitempty"`

	// Label is populated on `issues.labeled`.
	Label *struct {
		Name string `json:"name"`
	} `json:"label,omitempty"`

	// Comment is populated on `issue_comment`.
	Comment *struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
		User struct {
			Login string `json:"login"`
			ID    int64  `json:"id"`
		} `json:"user"`
	} `json:"comment,omitempty"`

	// PullRequest is populated on `pull_request` events.
	PullRequest *struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
		DiffURL string `json:"diff_url"`
	} `json:"pull_request,omitempty"`
}

// handleIssueLabeled fires when a label matching inst.taskLabels is
// applied to an issue. The installation passed in is already
// resolved + repo-allowlist-checked by HandleWebhook.
func (c *Channel) handleIssueLabeled(ctx context.Context, event, delivery string, p eventPayload, inst *installation) {
	if p.Issue == nil || p.Label == nil {
		return
	}
	if _, ok := inst.taskLabels[p.Label.Name]; !ok {
		return // label not in trigger set; quiet discard
	}
	sessionID := fmt.Sprintf("%s#issues/%d", p.Repository.FullName, p.Issue.Number)
	c.recordSession(sessionID, p.Issue.Title, p.Sender.Login, time.Now(), inst)
	if inst.taskCreator == nil {
		c.logger.Info().
			Str("event", event).
			Str("delivery", delivery).
			Str("repo", p.Repository.FullName).
			Int("issue", p.Issue.Number).
			Str("label", p.Label.Name).
			Str("sender", p.Sender.Login).
			Str("project_id", inst.projectID).
			Msg("github-app: issue labeled — TaskCreator not wired; discarding")
		return
	}
	ev := TaskCreationEvent{
		Kind:           "issues.labeled",
		SessionID:      sessionID,
		Title:          p.Issue.Title,
		Body:           p.Issue.Body,
		Labels:         issueLabels(p.Issue.Labels, p.Label.Name),
		SenderLogin:    p.Sender.Login,
		Repo:           p.Repository.FullName,
		Number:         p.Issue.Number,
		DefaultBranch:  p.Repository.DefaultBranch,
		InstallationID: p.Installation.ID,
		IdempotencyKey: "github-app:" + delivery,
	}
	if err := inst.taskCreator.Create(ctx, ev); err != nil {
		c.logger.Warn().Err(err).
			Str("delivery", delivery).
			Str("repo", p.Repository.FullName).
			Int("issue", p.Issue.Number).
			Str("project_id", inst.projectID).
			Msg("github-app: TaskCreator.Create failed on issues.labeled")
	}
}

func issueLabels(labels []struct {
	Name string `json:"name"`
}, fallback string) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if l.Name != "" {
			out = append(out, l.Name)
		}
	}
	if len(out) == 0 && fallback != "" {
		out = append(out, fallback)
	}
	return out
}

// handleIssueComment fires when a new issue or PR comment arrives.
// Routes to the @vornik reply path when (a) the body contains the
// mention (case-insensitive) and (b) the sender is on the
// installation's SenderAllowlist (or the allowlist is empty).
func (c *Channel) handleIssueComment(ctx context.Context, event, delivery string, p eventPayload, inst *installation) {
	if p.Issue == nil || p.Comment == nil {
		return
	}
	if !mentionsVornik(p.Comment.Body) {
		return // quiet discard; not all comments are for us
	}
	if _, err := c.resolveSpeakerForInstallation(inst, p.Comment.User.Login); err != nil {
		c.logger.Warn().
			Str("event", event).
			Str("delivery", delivery).
			Str("sender", p.Comment.User.Login).
			Str("project_id", inst.projectID).
			Msg("github-app: @vornik mention from sender not on installation allowlist; dropping")
		return
	}

	msg := c.buildCommentChannelMessage(p, delivery)
	title := p.Issue.Title
	if title == "" && p.Issue.PullRequest != nil {
		title = fmt.Sprintf("PR #%d", p.Issue.Number)
	}
	c.recordSession(msg.SessionID, title, p.Comment.User.Login, time.Now(), inst)
	c.recvMu.RLock()
	recv := c.recv
	c.recvMu.RUnlock()
	if recv == nil {
		c.logger.Warn().Str("delivery", delivery).Msg("github-app: @vornik mention received but no Receiver bound; dropping")
		return
	}
	if err := recv.Receive(ctx, msg); err != nil {
		c.logger.Warn().Err(err).Str("delivery", delivery).Msg("github-app: Receiver.Receive returned error")
	}
}

// handlePullRequestOpened fires on `pull_request.opened`. Honours
// the installation's PRReviewLabels — when non-empty, the PR's
// labels must intersect. Empty PRReviewLabels means every opened
// PR fires.
func (c *Channel) handlePullRequestOpened(ctx context.Context, event, delivery string, p eventPayload, inst *installation) {
	if p.PullRequest == nil {
		return
	}
	if len(inst.prLabels) > 0 {
		hit := false
		for _, l := range p.PullRequest.Labels {
			if _, ok := inst.prLabels[l.Name]; ok {
				hit = true
				break
			}
		}
		if !hit {
			return
		}
	}
	sessionID := fmt.Sprintf("%s#pulls/%d", p.Repository.FullName, p.PullRequest.Number)
	title := p.PullRequest.Title
	if title == "" {
		title = fmt.Sprintf("PR #%d", p.PullRequest.Number)
	}
	c.recordSession(sessionID, title, p.Sender.Login, time.Now(), inst)
	if inst.taskCreator == nil {
		c.logger.Info().
			Str("event", event).
			Str("delivery", delivery).
			Str("repo", p.Repository.FullName).
			Int("pr", p.PullRequest.Number).
			Str("sender", p.Sender.Login).
			Str("project_id", inst.projectID).
			Msg("github-app: PR opened — TaskCreator not wired; discarding")
		return
	}
	labels := make([]string, 0, len(p.PullRequest.Labels))
	for _, l := range p.PullRequest.Labels {
		labels = append(labels, l.Name)
	}
	ev := TaskCreationEvent{
		Kind:           "pull_request.opened",
		SessionID:      sessionID,
		Title:          p.PullRequest.Title,
		Body:           p.PullRequest.Body,
		Labels:         labels,
		SenderLogin:    p.Sender.Login,
		Repo:           p.Repository.FullName,
		Number:         p.PullRequest.Number,
		DefaultBranch:  p.Repository.DefaultBranch,
		InstallationID: p.Installation.ID,
		IdempotencyKey: "github-app:" + delivery,
	}
	if err := inst.taskCreator.Create(ctx, ev); err != nil {
		c.logger.Warn().Err(err).
			Str("delivery", delivery).
			Str("repo", p.Repository.FullName).
			Int("pr", p.PullRequest.Number).
			Str("project_id", inst.projectID).
			Msg("github-app: TaskCreator.Create failed on pull_request.opened")
	}
}

// buildCommentChannelMessage translates an issue_comment.created
// event into the generic ChannelMessage envelope. Session ID is
// `{owner}/{repo}#issues/{N}` for plain issues and
// `{owner}/{repo}#pulls/{N}` for PRs (GitHub uses the same
// `issue_comment.created` event for both, distinguished by
// issue.pull_request being non-nil).
func (c *Channel) buildCommentChannelMessage(p eventPayload, delivery string) conversation.ChannelMessage {
	kind := "issues"
	if p.Issue.PullRequest != nil {
		kind = "pulls"
	}
	sessionID := fmt.Sprintf("%s#%s/%d", p.Repository.FullName, kind, p.Issue.Number)
	cs := map[string]string{
		"repo":            p.Repository.FullName,
		"issue_number":    strconv.Itoa(p.Issue.Number),
		"installation_id": strconv.FormatInt(p.Installation.ID, 10),
		"github_event":    "issue_comment",
		"github_delivery": delivery,
		"sender_login":    p.Sender.Login,
	}
	return conversation.ChannelMessage{
		Source:          channelName,
		ID:              strconv.FormatInt(p.Comment.ID, 10),
		SessionID:       sessionID,
		SpeakerID:       p.Comment.User.Login,
		Text:            p.Comment.Body,
		InReplyTo:       "",
		ThreadID:        "",
		Timestamp:       time.Now(),
		ChannelSpecific: cs,
	}
}

// mentionsVornik returns true when the body contains `@vornik`
// case-insensitively. Word-boundary aware so `@vornik-deploy`
// doesn't trigger.
func mentionsVornik(body string) bool {
	lower := strings.ToLower(body)
	idx := 0
	for {
		hit := strings.Index(lower[idx:], "@vornik")
		if hit < 0 {
			return false
		}
		pos := idx + hit
		end := pos + len("@vornik")
		// Must be word-end: end-of-string, whitespace, or punctuation.
		if end >= len(lower) {
			return true
		}
		nextRune := lower[end]
		if !isMentionWordChar(nextRune) {
			return true
		}
		idx = end
	}
}

// isMentionWordChar tests one byte of the lowercased body — the
// caller always passes lower[end], so the function only needs to
// recognise a-z (no A-Z branch). 0-9 and - / _ extend the
// word-boundary check so handles like `@vornik-deploy` don't
// trigger the mention path.
func isMentionWordChar(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z':
		return true
	case b >= '0' && b <= '9':
		return true
	case b == '-' || b == '_':
		return true
	}
	return false
}

// Compile-time guarantees: *Channel satisfies the conversation
// Channel contract. Does NOT yet satisfy StreamingChannel —
// GitHub comments are atomic.
var _ conversation.Channel = (*Channel)(nil)
