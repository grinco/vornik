package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/ratelimit"
)

// ErrOutboundNotConfigured is returned by Send when the resolved
// installation has no bot token configured. Inbound webhook
// reception still works in that mode; only outbound replies fail
// with this sentinel. Mirrors github.ErrOutboundNotConfigured /
// email.ErrOutboundNotConfigured so the dispatcher can errors.Is
// uniformly across channels.
var ErrOutboundNotConfigured = errors.New("slack channel: outbound credentials not configured (set BotToken or InstallationConfig.BotToken)")

// ErrUnknownSession is returned by Send when the SessionID can't be
// parsed or doesn't map to a known installation. Slack outbound
// requires a channel_id; a fresh-thread reply to an
// unrecorded session is a logic error.
var ErrUnknownSession = errors.New("slack channel: cannot send — SessionID does not resolve to a known team/channel")

// maxOutboundResponseBytes caps how much of a Slack response we
// read. chat.postMessage responses are < 8 KiB in practice; the
// cap protects against a misbehaving upstream returning a giant
// body. Mirrors the GitHub channel's posture.
const maxOutboundResponseBytes = 64 * 1024

// errorBodyExcerpt limits how much of an error response body is
// echoed into the returned error message. Long error bodies in
// logs make grep painful.
const errorBodyExcerpt = 512

// chatPostMessageResponse is the relevant subset of Slack's response
// envelope. `ok` is the success flag; `ts` is the new message's
// timestamp (functions as the message id for future threading);
// `error` carries the machine-readable failure code on `ok:false`.
//
// On HTTP 200 with `ok:false`, Slack still embeds the failure code
// in `error`. We surface that as a Go error so callers don't have to
// inspect the response body.
type chatPostMessageResponse struct {
	OK      bool   `json:"ok"`
	Ts      string `json:"ts,omitempty"`
	Channel string `json:"channel,omitempty"`
	Error   string `json:"error,omitempty"`
	Warning string `json:"warning,omitempty"`
}

// chatPostMessageRequest is the body we POST to chat.postMessage.
// Slack accepts a wider surface (blocks, attachments, etc.) but v1
// only sends plain text + thread_ts for in-thread replies.
type chatPostMessageRequest struct {
	Channel  string `json:"channel"`
	Text     string `json:"text"`
	ThreadTs string `json:"thread_ts,omitempty"`
}

// sendChatPostMessage posts a Slack message via the Web API,
// authenticated with the installation's bot token. Honours the
// per-(team, channel) Tier-3 rate limit and reads Retry-After when
// Slack returns 429.
//
// SessionID encoding mirrors the inbound translation:
// `<team_id>/<channel_id>#<thread_root_ts>`. Reply messages set
// thread_ts so the reply lands in-thread; top-of-thread first
// replies use the inbound's own ts as thread_ts so the operator's
// `@vornik hello` and the bot's reply share a thread.
//
// Returns the new message's ts on success; that's the value the
// DispatcherReceiver stashes for future InReplyTo correlation.
func (c *Channel) sendChatPostMessage(ctx context.Context, msg conversation.ChannelMessage) (string, error) {
	if strings.TrimSpace(msg.Text) == "" {
		return "", errors.New("slack channel: cannot send empty message")
	}
	teamID, channelID, threadRoot, err := parseSlackSessionID(msg.SessionID)
	if err != nil {
		return "", err
	}
	inst, ok := c.installationsByID[teamID]
	if !ok {
		return "", fmt.Errorf("%w: team_id %q not configured", ErrUnknownSession, teamID)
	}
	if strings.TrimSpace(inst.botToken) == "" {
		return "", ErrOutboundNotConfigured
	}

	// Rate-limit gate keyed on team+channel — Slack's Tier-3 cap is
	// per-channel. The keybucket primitive is in-memory; multi-daemon
	// SaaS deployments will need durable state (tracked in
	// https://docs.vornik.io alongside the rest of rate-limit hardening),
	// out of scope for this track.
	if err := c.acquireOutboundToken(teamID, channelID, c.clock()); err != nil {
		return "", err
	}

	body, err := json.Marshal(chatPostMessageRequest{
		Channel:  channelID,
		Text:     msg.Text,
		ThreadTs: threadRoot,
	})
	if err != nil {
		return "", fmt.Errorf("slack channel: marshal request: %w", err)
	}
	url := c.apiBaseURL + "/chat.postMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("slack channel: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+inst.botToken)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("slack channel: chat.postMessage request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxOutboundResponseBytes))

	// Slack signals upstream rate-limit with HTTP 429 + Retry-After.
	// Surface as a structured error so the dispatcher's caller can
	// back off precisely. We don't auto-retry here — that's the
	// caller's policy decision.
	if resp.StatusCode == http.StatusTooManyRequests {
		retry := parseRetryAfter(resp.Header.Get("Retry-After"))
		return "", &RateLimitedError{RetryAfter: retry, Body: truncateBody(string(respBody))}
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("slack channel: chat.postMessage HTTP %d: %s",
			resp.StatusCode, truncateBody(string(respBody)))
	}

	var parsed chatPostMessageResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("slack channel: chat.postMessage parse response: %w", err)
	}
	if !parsed.OK {
		// `error` is the machine-readable failure code (e.g.
		// "channel_not_found", "not_in_channel"). Echo it verbatim
		// so the operator can act on it without diving into logs.
		return "", fmt.Errorf("slack channel: chat.postMessage error: %s", parsed.Error)
	}
	if parsed.Ts == "" {
		return "", errors.New("slack channel: chat.postMessage response missing ts")
	}
	return parsed.Ts, nil
}

// acquireOutboundToken consumes one token from the per-(team,
// channel) bucket. Lazily allocates the limiter on first call.
// Returns a RateLimitedError when the bucket is empty so callers
// can branch on the sentinel via errors.As.
//
// The keybucket primitive's "rps=0 ⇒ no limit" semantics mean an
// operator who sets PostMessageRPS=0 explicitly opts out of in-
// process rate limiting (e.g. they trust the upstream relay to
// enforce). The default New() wiring sets the Tier-3 (1/sec)
// values so an unconfigured channel is rate-limited by default.
func (c *Channel) acquireOutboundToken(teamID, channelID string, now time.Time) error {
	limiter := c.outboundLimiter()
	if limiter == nil || c.postMessageRPS <= 0 {
		return nil
	}
	key := teamID + "|" + channelID
	dec := limiter.Allow(key, c.postMessageRPS, c.postMessageBurst, now)
	if dec.Blocked {
		return &RateLimitedError{RetryAfter: dec.RetryAfter, Body: "local outbound bucket empty"}
	}
	return nil
}

// outboundLimiter returns the per-channel APIKeyLimiter, lazily
// allocating on first call. Held as `any` on the struct so types.go
// doesn't import the ratelimit package.
func (c *Channel) outboundLimiter() *ratelimit.APIKeyLimiter {
	if existing, ok := c.rateLimiter.(*ratelimit.APIKeyLimiter); ok {
		return existing
	}
	// types.go's Channel struct doesn't run a sync.Once on rateLimiter
	// — a concurrent Send race here would briefly allocate two limiters
	// and one would be discarded. That's a benign race for the in-
	// memory rate-limit case (worst-case: a single Send slips past the
	// gate during boot). If a future operator deploys at scale enough
	// for the race to matter, swap to a sync.Once construction in
	// New(). For now, the eager-construct-once-per-Send check below
	// gives us the same observable behaviour without the boot-time
	// allocation in inbound-only deployments.
	lim := ratelimit.NewAPIKeyLimiter()
	c.rateLimiter = lim
	return lim
}

// RateLimitedError signals that the request was blocked by either
// the in-process token bucket or the upstream Slack rate-limiter
// (HTTP 429). RetryAfter is the operator-actionable wait time;
// callers (or the dispatcher) decide whether to retry, queue, or
// surface the failure to the user.
//
// errors.As lets the caller pull the structured retry-after value
// out of a wrapped error chain — handy when a higher layer wraps
// our sentinel with additional context.
type RateLimitedError struct {
	RetryAfter time.Duration
	Body       string
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("slack channel: rate-limited (retry after %s): %s",
		e.RetryAfter, e.Body)
}

// parseRetryAfter parses Slack's Retry-After header (always an
// integer seconds value per their docs). Returns 0 on parse failure
// so callers get a "rate-limited, retry asap" signal rather than
// an unbounded sleep.
func parseRetryAfter(in string) time.Duration {
	in = strings.TrimSpace(in)
	if in == "" {
		return 0
	}
	secs, err := strconv.Atoi(in)
	if err != nil || secs < 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// parseSlackSessionID splits a session ID in the form
// `<team_id>/<channel_id>#<thread_root_ts>` into its components.
// Defensive on every separator so a malformed SessionID surfaces a
// routing error rather than a wonky outbound URL.
func parseSlackSessionID(s string) (teamID, channelID, threadRoot string, err error) {
	hash := strings.LastIndex(s, "#")
	if hash < 0 {
		return "", "", "", fmt.Errorf("%w: missing '#' separator in %q", ErrUnknownSession, s)
	}
	pre := s[:hash]
	threadRoot = s[hash+1:]
	if threadRoot == "" {
		return "", "", "", fmt.Errorf("%w: empty thread_ts in %q", ErrUnknownSession, s)
	}
	slash := strings.Index(pre, "/")
	if slash < 0 {
		return "", "", "", fmt.Errorf("%w: missing '/' separator in %q", ErrUnknownSession, s)
	}
	teamID = pre[:slash]
	channelID = pre[slash+1:]
	if teamID == "" || channelID == "" {
		return "", "", "", fmt.Errorf("%w: empty team or channel in %q", ErrUnknownSession, s)
	}
	return teamID, channelID, threadRoot, nil
}

// truncateBody caps Slack API response excerpts in error messages
// so logs stay greppable.
func truncateBody(s string) string {
	if len(s) > errorBodyExcerpt {
		return s[:errorBodyExcerpt] + "..."
	}
	return s
}
