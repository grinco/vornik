// Package slack implements vornik's Slack bot conversation channel
// (Track A of the 2026.6.0 hardening tracks; see
// https://docs.vornik.io §"Slice 4 —
// GitHub App, email, Slack").
//
// This package owns:
//
//   - the Events API webhook HTTP handler that Slack posts to
//     (`/api/v1/slack/webhook`)
//   - HMAC-SHA256 signature verification of inbound deliveries
//     (Slack's v0:<timestamp>:<body> wire shape, ±5 min replay window)
//   - the URL-verification challenge handshake Slack issues once on
//     endpoint registration
//   - the per-event translation from Slack's payload into
//     conversation.ChannelMessage (app_mention, message.im, and
//     @vornik-mentioned message.channels)
//   - the conversation.Channel interface, so vornik's dispatcher /
//     scheduler stay unaware that the source is Slack
//   - outbound chat.postMessage rate-limiting against Slack's
//     Tier-3 1-msg/sec/channel ceiling, with Retry-After honouring
//     on upstream 429s
//
// The structure mirrors internal/github (multi-installation routing,
// per-installation SenderAllowlist, in-memory session map) and
// internal/email (per-project channel construction via
// buildSlackChannels) so a reviewer reads it as "the same shape."
//
// Slack-specific bits this package owns and the others don't:
//
//   - URL verification handshake (returned in-band by HandleWebhook)
//   - Replay-window enforcement (timestamp skew > 5 min ⇒ reject)
//   - Tier-3 rate-limit honoring (per team_id+channel_id token bucket
//     via internal/ratelimit/keybucket.go)
//
// Out of scope for v1: Socket Mode, StreamingChannel (chat.update),
// file attachment ingestion, interactive components.
package slack

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// channelName is the stable identifier surfaced as
// ChannelMessage.Source. Downstream consumers (the dispatcher, the
// operator UI, the per-channel metrics labels) branch on this string
// without type-asserting.
const channelName = "slack"

// maxWebhookBodyBytes caps inbound payload size. Slack's Events API
// deliveries are typically well under 100 KiB; the cap is conservative
// so a malformed delivery can't exhaust memory. Mirrors the GitHub
// channel's posture.
const maxWebhookBodyBytes = 1 * 1024 * 1024

// maxReplayWindow is Slack's documented replay-defence horizon — any
// signed request whose X-Slack-Request-Timestamp is more than 5
// minutes off from the server clock is rejected even when the HMAC
// itself verifies. Matches the example in Slack's
// "Verifying requests from Slack" docs.
const maxReplayWindow = 5 * time.Minute

// defaultPostMessageRPS / defaultPostMessageBurst tune the per-
// (team, channel) outbound rate limiter. Slack's published Tier-3
// ceiling is ~1 message/second per channel; we honor that with a
// burst of one (no smoothing — a one-shot reply is exactly one POST).
// Operators can override via Config if they're on a higher-tier app.
const (
	defaultPostMessageRPS   = 1
	defaultPostMessageBurst = 1
)

// Config carries the operator-provided Slack settings the channel
// needs. Populated from project config at boot; never mutated after
// Channel construction. Secrets are kept off the struct — the channel
// stores the resolved signing-secret bytes for HMAC equality checks.
//
// Two modes are supported, mirroring internal/github:
//
//   - Single-installation (back-compat / one-workspace deployments):
//     leave Installations nil and populate the top-level
//     BotToken / SigningSecret / TeamID / allowlists fields. The
//     channel routes every inbound delivery through that single
//     workspace.
//   - Multi-installation: populate Installations with one entry per
//     project. On every inbound webhook the channel resolves the
//     installation by payload.team_id and uses that entry's
//     allowlists + outbound bot token. The outbound rate-limit
//     buckets are keyed per-(team, channel).
type Config struct {
	// BotToken is the workspace bot token (xoxb-…) used for outbound
	// chat.postMessage calls. Single-installation only; multi-
	// installation reads BotToken from each InstallationConfig.
	// Empty disables outbound; inbound webhook reception still works.
	BotToken string

	// SigningSecret is the workspace signing secret Slack uses to
	// HMAC-sign every Events API delivery. Channel-wide rather than
	// per-installation because each Slack App has a single signing
	// secret regardless of how many workspaces installed it.
	//
	// Empty rejects every inbound delivery so a misconfigured operator
	// gets a loud failure rather than silent acceptance of unverified
	// payloads. Mirrors the GitHub channel's WebhookSecret posture.
	SigningSecret string

	// TeamID is the Slack workspace ID (T…). Used in single-
	// installation mode for back-compat operator-visible logging and
	// rate-limit keying. Multi-installation mode reads TeamID from
	// each InstallationConfig.
	TeamID string

	// APIBaseURL overrides the Slack Web API endpoint. Empty defaults
	// to `https://slack.com/api`. Tests inject an httptest stub URL.
	APIBaseURL string

	// HTTPClient overrides the HTTP transport used for outbound Web
	// API calls. Nil falls back to http.DefaultClient. Tests inject
	// a client configured against an httptest.Server.
	HTTPClient *http.Client

	// TeamAllowlist is the set of Slack team_ids (T…) this channel
	// accepts events from. Empty means deny-all — a defensive default
	// that catches a misconfigured channel rather than silently
	// processing events from a workspace the operator hasn't claimed.
	//
	// Single-installation mode only; multi-installation mode reads
	// TeamID from each InstallationConfig.
	TeamAllowlist []string

	// ChannelAllowlist is the set of Slack channel_ids (C…) this
	// channel accepts events from. Empty allows every channel in the
	// configured team — useful in dev where the operator doesn't want
	// to pin a specific channel. Production deployments should set
	// this so a misclick in Slack's "install to channel" picker
	// doesn't expose the bot to unrelated channels.
	ChannelAllowlist []string

	// SenderAllowlist is the set of Slack user_ids (U…) allowed to
	// trigger the dispatcher path. Empty allows every user — dev-mode
	// pass-through matching the GitHub channel's SenderAllowlist
	// semantics.
	SenderAllowlist []string

	// Logger is the channel's zerolog instance. Zero-value is fine
	// but produces no log output.
	Logger zerolog.Logger

	// Clock overrides time.Now in tests so the replay-window guard
	// is deterministic. Nil falls back to time.Now.
	Clock func() time.Time

	// PostMessageRPS / PostMessageBurst tune the per-(team, channel)
	// outbound rate limiter. Zero values fall back to Slack's
	// documented Tier-3 cap (1 msg/sec, burst 1). Operators on a
	// higher-tier app can raise these.
	PostMessageRPS   int
	PostMessageBurst int

	// Installations lists one entry per Slack workspace the channel
	// serves. When non-empty, the channel routes inbound deliveries
	// by matching payload.team_id against InstallationConfig.TeamID.
	// Each installation carries its own project ID, allowlists, and
	// outbound bot token.
	//
	// Unknown team_ids are dropped with an audit log entry + HTTP 200
	// (Slack retries on non-200; the rest-of-codebase contract is
	// "200 + log + discard" for unrecognised deliveries).
	//
	// Single-installation backwards-compat: leave nil/empty and
	// populate the top-level fields.
	Installations []InstallationConfig

	// Voice (optional) wires STT + TTS providers for inbound audio
	// transcription and outbound voice synthesis. Zero value keeps
	// the pre-voice behaviour exactly: audio file_shared events are
	// dropped silently, replies are always text. Voice MVP slice 4.
	Voice VoiceProviders
}

// InstallationConfig describes one Slack workspace served by the
// channel. Multi-installation mode pivots routing on the TeamID — the
// channel's HandleWebhook reads `payload.team_id` and looks up the
// matching entry.
type InstallationConfig struct {
	// ProjectID identifies the vornik project this workspace belongs
	// to. Required for routing dispatcher turns to the correct
	// project scope and for session-store project pinning.
	ProjectID string

	// TeamID is the Slack workspace ID (T…). Required for routing
	// inbound deliveries.
	TeamID string

	// BotToken is the workspace bot token (xoxb-…) used for outbound
	// chat.postMessage calls. Empty disables outbound replies for
	// this workspace.
	BotToken string

	// ChannelAllowlist is the set of Slack channel_ids accepted for
	// this workspace. Empty allows every channel in the team.
	ChannelAllowlist []string

	// SenderAllowlist is the set of Slack user_ids allowed to trigger
	// the dispatcher path. Empty allows every user (dev-mode
	// pass-through).
	SenderAllowlist []string
}

// Channel is the Slack bot's conversation.Channel implementation.
// Constructed once per project at boot; the HTTP handler
// (HandleWebhook) is mounted on the daemon's API server by the
// service container.
//
// The struct lives in types.go but its non-trivial methods
// (HandleWebhook, Start, Send, ResolveSpeaker) live in channel.go.
// Splitting keeps slice-1 unit tests on the verifier independent of
// the slice-2 webhook-dispatch pipeline.
type Channel struct {
	cfg                Config
	signingSecretBytes []byte
	apiBaseURL         string
	httpClient         *http.Client
	logger             zerolog.Logger
	clock              func() time.Time

	postMessageRPS   int
	postMessageBurst int

	// installations holds the resolved set of routes the channel
	// serves. Always non-empty after New: single-installation mode
	// produces a one-element slice synthesised from the top-level
	// Config fields so inbound dispatch and outbound Send share the
	// same routing primitive.
	installations     []*installation
	installationsByID map[string]*installation

	recvMu sync.RWMutex
	// recv is the bound conversation.Receiver — wired to "any" here
	// so types.go doesn't pull in the conversation package; channel.go
	// type-asserts on read. Set by Start, cleared by Stop.
	recv any

	// sessionsMu guards the in-memory active-session map. Populated
	// on every inbound translation (app_mention, message.im,
	// @vornik-mentioned message.channels); read by ListSessions for
	// the operator UI. In-memory only: daemon restart clears it.
	sessionsMu sync.Mutex
	sessions   map[string]*sessionEntry

	// rateLimiter is the per-(team, channel) outbound token bucket
	// (internal/ratelimit/keybucket.go). Lazily allocated; wired in
	// channel.go. Held as `any` so types.go doesn't depend on the
	// ratelimit package.
	rateLimiter any

	// voice carries the STT + TTS providers + length-cap settings
	// for the voice-message MVP (slice 4). All fields are nil-safe;
	// when neither STT nor TTS is wired the file_shared handler
	// drops audio inbounds and Channel.Send stays on the text
	// chat.postMessage path. voiceTracker remembers which sessions
	// are currently in voice-reply mode (latest inbound was audio).
	voice        VoiceProviders
	voiceTracker *voiceTracker
}

// New constructs a Channel from the given Config. Returns an error
// when the config is structurally broken (empty signing secret, no
// installations + empty team allowlist, duplicate team_id across
// the Installations slice). Defensive defaults catch the misconfig
// at boot rather than the first delivery — same posture as the
// GitHub channel's New.
func New(cfg Config) (*Channel, error) {
	if strings.TrimSpace(cfg.SigningSecret) == "" {
		return nil, errors.New("slack channel: SigningSecret is required")
	}
	apiBase := cfg.APIBaseURL
	if apiBase == "" {
		apiBase = defaultAPIBaseURL
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}

	installs, err := resolveInstallations(cfg)
	if err != nil {
		return nil, err
	}

	rps := cfg.PostMessageRPS
	if rps <= 0 {
		rps = defaultPostMessageRPS
	}
	burst := cfg.PostMessageBurst
	if burst <= 0 {
		burst = defaultPostMessageBurst
	}

	c := &Channel{
		cfg:                cfg,
		signingSecretBytes: []byte(cfg.SigningSecret),
		apiBaseURL:         apiBase,
		httpClient:         httpClient,
		logger:             cfg.Logger,
		clock:              clock,
		postMessageRPS:     rps,
		postMessageBurst:   burst,
		sessions:           make(map[string]*sessionEntry),
		installations:      installs,
		installationsByID:  indexInstallations(installs),
		voice:              cfg.Voice,
		voiceTracker:       newVoiceTracker(),
	}
	if c.voice.MaxOutboundDuration <= 0 {
		c.voice.MaxOutboundDuration = slackAudioMaxDurationMs
	}
	return c, nil
}

// Name returns the stable channel identifier surfaced as
// ChannelMessage.Source.
func (c *Channel) Name() string { return channelName }

// resolveInstallations normalises Config's two routing modes into a
// single internal []*installation. Single-installation mode (no
// Installations entries) synthesises one entry from the top-level
// fields; multi-installation mode validates every entry has a
// non-empty TeamID and no-duplicate TeamID across the slice.
func resolveInstallations(cfg Config) ([]*installation, error) {
	if len(cfg.Installations) == 0 {
		// Back-compat single-installation mode. Defensive default:
		// require either an explicit TeamAllowlist or a top-level
		// TeamID so the channel doesn't accept inbound from arbitrary
		// workspaces. This mirrors the GitHub channel's
		// "RepoAllowlist non-empty" boot guard.
		if len(cfg.TeamAllowlist) == 0 && strings.TrimSpace(cfg.TeamID) == "" {
			return nil, errors.New("slack channel: TeamAllowlist or TeamID is required")
		}
		teamID := strings.TrimSpace(cfg.TeamID)
		// In single-installation mode the channel synthesises one
		// route per allowed team so HandleWebhook's installation
		// lookup short-circuits without a special case.
		if teamID == "" {
			teamID = strings.TrimSpace(cfg.TeamAllowlist[0])
		}
		inst := buildInstallation(InstallationConfig{
			ProjectID:        "",
			TeamID:           teamID,
			BotToken:         cfg.BotToken,
			ChannelAllowlist: cfg.ChannelAllowlist,
			SenderAllowlist:  cfg.SenderAllowlist,
		})
		// Add any additional allowed teams as no-token installations
		// — inbound passes but outbound from those workspaces hits
		// the empty-token sentinel.
		out := []*installation{inst}
		for _, t := range cfg.TeamAllowlist {
			t = strings.TrimSpace(t)
			if t == "" || t == inst.teamID {
				continue
			}
			out = append(out, buildInstallation(InstallationConfig{
				TeamID:           t,
				ChannelAllowlist: cfg.ChannelAllowlist,
				SenderAllowlist:  cfg.SenderAllowlist,
			}))
		}
		return out, nil
	}

	seen := make(map[string]string, len(cfg.Installations))
	out := make([]*installation, 0, len(cfg.Installations))
	for i, ic := range cfg.Installations {
		teamID := strings.TrimSpace(ic.TeamID)
		if teamID == "" {
			return nil, fmt.Errorf("slack channel: Installations[%d] missing TeamID", i)
		}
		if prev, ok := seen[teamID]; ok {
			return nil, fmt.Errorf("slack channel: duplicate team_id %q (projects %q and %q)",
				teamID, prev, ic.ProjectID)
		}
		seen[teamID] = ic.ProjectID
		out = append(out, buildInstallation(ic))
	}
	return out, nil
}

// buildInstallation translates an InstallationConfig into its
// resolved internal form (with the allowlist-set maps pre-indexed
// for O(1) lookup on the hot path).
func buildInstallation(ic InstallationConfig) *installation {
	return &installation{
		projectID:           ic.ProjectID,
		teamID:              strings.TrimSpace(ic.TeamID),
		botToken:            ic.BotToken,
		allowedChannels:     indexSet(ic.ChannelAllowlist),
		senders:             indexSet(ic.SenderAllowlist),
		channelAllowlistRaw: append([]string(nil), ic.ChannelAllowlist...),
		senderAllowlistRaw:  append([]string(nil), ic.SenderAllowlist...),
	}
}

// indexInstallations builds the team_id → *installation lookup map
// used by HandleWebhook to route inbound deliveries.
func indexInstallations(installs []*installation) map[string]*installation {
	out := make(map[string]*installation, len(installs))
	for _, inst := range installs {
		if inst.teamID != "" {
			out[inst.teamID] = inst
		}
	}
	return out
}

// indexSet folds a string slice into a presence map so the hot-path
// allowlist check is a single map lookup rather than a linear scan.
func indexSet(in []string) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		out[s] = struct{}{}
	}
	return out
}

// defaultAPIBaseURL is Slack's public Web API endpoint. Tests
// override via Config.APIBaseURL.
const defaultAPIBaseURL = "https://slack.com/api"

// installation is the resolved internal form of an InstallationConfig.
// One per configured workspace; the channel never mutates these after
// New.
type installation struct {
	projectID string
	teamID    string
	botToken  string

	allowedChannels map[string]struct{}
	senders         map[string]struct{}

	channelAllowlistRaw []string
	senderAllowlistRaw  []string
}

// sessionEntry holds the per-session metadata ListSessions surfaces.
// Title is best-effort (channel name when Slack supplies one);
// LastActivity drives newest-first sort.
//
// installation pins the session to the workspace that produced the
// first inbound event so outbound Send can look up which bot token
// to use — necessary because the SessionID shape (`Cxxx#ts`) doesn't
// carry a team_id.
//
// projectID mirrors installation.projectID for fast read access from
// the session-store layer.
type sessionEntry struct {
	Title            string
	LastActivity     time.Time
	ParticipantCount int
	participants     map[string]struct{}

	installation *installation
	projectID    string
}
