package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/conversation"
)

// Start binds the Receiver and blocks until ctx is cancelled. Slack
// is webhook-driven — there's no poll loop. Start exists purely to
// satisfy the Channel contract; the HTTP handler is mounted on the
// daemon's API server at boot and runs in its own goroutine pool.
// Mirrors internal/github's Start.
func (c *Channel) Start(ctx context.Context, recv conversation.Receiver) error {
	if recv == nil {
		return errors.New("slack channel: nil Receiver")
	}
	c.recvMu.Lock()
	c.recv = recv
	c.recvMu.Unlock()
	<-ctx.Done()
	return ctx.Err()
}

// Stop clears the Receiver binding. Idempotent.
func (c *Channel) Stop() error {
	c.recvMu.Lock()
	c.recv = nil
	c.recvMu.Unlock()
	return nil
}

// ResolveSpeaker maps a Slack user_id (U…) to a conversation.Speaker.
// Returns ErrSpeakerUnknown when no installation's SenderAllowlist
// admits the speaker. Empty allowlists pass through (dev mode);
// mirrors the GitHub channel's "any installation accepts ⇒ admit"
// posture so the channel-wide surface stays uniform while per-
// installation enforcement on the dispatch path runs separately.
func (c *Channel) ResolveSpeaker(_ context.Context, channelSpeakerID string) (conversation.Speaker, error) {
	speakerID := strings.TrimSpace(channelSpeakerID)
	if speakerID == "" {
		return conversation.Speaker{}, conversation.ErrSpeakerUnknown
	}
	if !c.anyInstallationAllowsSpeaker(speakerID) {
		return conversation.Speaker{}, conversation.ErrSpeakerUnknown
	}
	return conversation.Speaker{
		ID:            "slack:" + speakerID,
		DisplayName:   speakerID,
		ChannelHandle: speakerID,
	}, nil
}

// anyInstallationAllowsSpeaker returns true when at least one
// installation's SenderAllowlist permits the user_id (or has an empty
// allowlist = dev-mode pass-through).
func (c *Channel) anyInstallationAllowsSpeaker(userID string) bool {
	for _, inst := range c.installations {
		if len(inst.senders) == 0 {
			return true
		}
		if _, ok := inst.senders[userID]; ok {
			return true
		}
	}
	return false
}

// resolveSpeakerForInstallation enforces the per-installation
// SenderAllowlist gate. Used by the dispatch path after the inbound
// has already been routed to a specific installation.
func (c *Channel) resolveSpeakerForInstallation(inst *installation, userID string) (conversation.Speaker, error) {
	if strings.TrimSpace(userID) == "" {
		return conversation.Speaker{}, conversation.ErrSpeakerUnknown
	}
	if len(inst.senders) > 0 {
		if _, ok := inst.senders[userID]; !ok {
			return conversation.Speaker{}, conversation.ErrSpeakerUnknown
		}
	}
	return conversation.Speaker{
		ID:            "slack:" + userID,
		DisplayName:   userID,
		ChannelHandle: userID,
	}, nil
}

// ListSessions returns a snapshot of every Slack thread that has
// produced at least one inbound event since daemon start. Sorted
// newest-first by LastActivity. In-memory only; restart clears the
// set.
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
// the operator UI. inst pins the session to the workspace that
// produced the first inbound event on this thread; subsequent
// events reuse the original pin (see sessionEntry.installation).
func (c *Channel) recordSession(sessionID, title, participant string, when time.Time, inst *installation) {
	c.sessionsMu.Lock()
	defer c.sessionsMu.Unlock()
	e, ok := c.sessions[sessionID]
	if !ok {
		e = &sessionEntry{participants: map[string]struct{}{}}
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
		e.installation = inst
		e.projectID = inst.projectID
	}
}

// ProjectForSession returns the project ID the channel has recorded
// for the given Slack session (thread). Returns "" when unknown.
// Used by the service container's session store to avoid mis-routing
// a dispatcher turn into another project's tools.
func (c *Channel) ProjectForSession(sessionID string) string {
	c.sessionsMu.Lock()
	defer c.sessionsMu.Unlock()
	if e, ok := c.sessions[sessionID]; ok {
		return e.projectID
	}
	return ""
}

// eventPayload is the minimal Events API envelope the channel needs.
// Slack populates a different subset per event type — we use pointer
// fields so json.Unmarshal silently drops absent ones.
type eventPayload struct {
	Type      string      `json:"type"`
	Challenge string      `json:"challenge,omitempty"`
	TeamID    string      `json:"team_id,omitempty"`
	APIAppID  string      `json:"api_app_id,omitempty"`
	Event     *eventInner `json:"event,omitempty"`
	EventID   string      `json:"event_id,omitempty"`
	EventTime int64       `json:"event_time,omitempty"`
}

// eventInner is the nested object Slack wraps the actual event in
// when Type == "event_callback".
type eventInner struct {
	Type     string `json:"type"`                // app_mention | message | file_shared
	User     string `json:"user,omitempty"`      // U… speaker id
	Text     string `json:"text,omitempty"`      // message body
	Channel  string `json:"channel,omitempty"`   // C… channel id
	Ts       string `json:"ts,omitempty"`        // message timestamp
	ThreadTs string `json:"thread_ts,omitempty"` // present when in a thread
	// ChannelType is "im" for DMs, "channel" for public channels,
	// "group" for private channels. The channel branches on this to
	// distinguish message.im from message.channels.
	ChannelType string `json:"channel_type,omitempty"`
	// BotID is non-empty when Slack relays one bot's message to
	// another via message.channels. We drop those so bots don't talk
	// to themselves.
	BotID string `json:"bot_id,omitempty"`
	// Subtype filters out edits / deletes / etc. — slice 1 only
	// handles plain user messages so any non-empty subtype gets
	// silently dropped.
	Subtype string `json:"subtype,omitempty"`
	// File carries the inline file payload when Slack delivers a
	// file_shared event. Older payload shapes ship file_id on the
	// top-level event; newer shapes embed the full file metadata.
	// The voice handler accepts either by calling files.info when
	// only the id is present. Voice MVP slice 4.
	File *slackFile `json:"file,omitempty"`
	// UserID + ChannelID + EventTs mirror the file_shared payload
	// when the inner event arrives without a user/channel/ts
	// (Slack's file_shared v2 sometimes ships fields one level up).
	// Kept here so the unmarshal stays a single struct rather than
	// a polymorphic decoder.
	UserID    string `json:"user_id,omitempty"`
	ChannelID string `json:"channel_id,omitempty"`
	EventTs   string `json:"event_ts,omitempty"`
}

// HandleWebhook is the HTTP entry point for inbound Slack Events
// API deliveries. Mount on the daemon's API mux at
// `/api/v1/slack/webhook`.
//
// Flow:
//  1. Read body (size-capped).
//  2. Verify HMAC signature against the signing secret. Reject 401
//     on mismatch or replay-window failure.
//  3. Parse minimal payload. If type == "url_verification", echo the
//     challenge back as text/plain and return.
//  4. Route by team_id to the matching installation. Unknown
//     team_ids are 200 + log + drop (Slack retries on non-200).
//  5. Branch on event type per the docstring above. Non-matching
//     events are acked + dropped.
//  6. Always respond 200 on a valid signed delivery so Slack doesn't
//     enter retry backoff.
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
	now := c.clock()
	if err := c.verifySignature(r, body, now); err != nil {
		c.logger.Warn().Err(err).Msg("slack: signature verification failed")
		http.Error(w, "unauthorised", http.StatusUnauthorized)
		return
	}

	var payload eventPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		c.logger.Warn().Err(err).Msg("slack: payload parse failed")
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	// URL-verification handshake: Slack POSTs this once when the
	// endpoint is registered. Echo the challenge back as text/plain
	// so the endpoint-registration UI confirms.
	if payload.Type == "url_verification" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(payload.Challenge))
		return
	}

	if payload.Type != "event_callback" || payload.Event == nil {
		// retry-style "we don't recognise this envelope" deliveries
		// (e.g. a future event type or a legacy "outer_event") are
		// acked silently — Slack would retry indefinitely otherwise.
		c.logger.Debug().Str("type", payload.Type).Msg("slack: unrecognised payload type, acking")
		w.WriteHeader(http.StatusOK)
		return
	}

	teamID := strings.TrimSpace(payload.TeamID)
	if teamID == "" {
		c.logger.Warn().Msg("slack: event without team_id; dropping")
		w.WriteHeader(http.StatusOK)
		return
	}
	inst, ok := c.installationsByID[teamID]
	if !ok {
		c.logger.Warn().
			Str("team_id", teamID).
			Str("event_id", payload.EventID).
			Msg("slack: team_id not recognised; dropping delivery")
		w.WriteHeader(http.StatusOK)
		return
	}

	// Drop bot-echoed messages and edit/delete subtype events. Doing
	// it after installation resolution so the audit log captures
	// which workspace produced the noise.
	if payload.Event.BotID != "" {
		c.logger.Debug().Str("team_id", teamID).Msg("slack: dropping bot-relayed message")
		w.WriteHeader(http.StatusOK)
		return
	}
	if payload.Event.Subtype != "" {
		c.logger.Debug().
			Str("team_id", teamID).
			Str("subtype", payload.Event.Subtype).
			Msg("slack: dropping non-plain message subtype")
		w.WriteHeader(http.StatusOK)
		return
	}

	switch payload.Event.Type {
	case "file_shared":
		// Voice MVP slice 4. Normalise the file_shared variant
		// shape so the handler can rely on User / Channel without
		// having to know which envelope variant Slack used.
		if payload.Event.User == "" {
			payload.Event.User = payload.Event.UserID
		}
		if payload.Event.Channel == "" {
			payload.Event.Channel = payload.Event.ChannelID
		}
		c.handleFileSharedEvent(r.Context(), payload, inst)
	case "app_mention":
		c.handleMessageEvent(r.Context(), payload, inst, false)
	case "message":
		// message.im → always dispatch. message.channels / .groups →
		// only when @vornik is mentioned in the text. Slack delivers
		// message.channels for every message in a channel the bot is
		// a member of; we drop the rest to keep LLM spend bounded.
		switch payload.Event.ChannelType {
		case "im":
			c.handleMessageEvent(r.Context(), payload, inst, false)
		case "channel", "group":
			if mentionsVornik(payload.Event.Text) {
				c.handleMessageEvent(r.Context(), payload, inst, true)
			} else {
				c.logger.Debug().
					Str("team_id", teamID).
					Str("channel", payload.Event.Channel).
					Msg("slack: message without @vornik mention; dropping")
			}
		default:
			c.logger.Debug().
				Str("team_id", teamID).
				Str("channel_type", payload.Event.ChannelType).
				Msg("slack: unhandled channel_type; dropping")
		}
	default:
		c.logger.Debug().
			Str("event_type", payload.Event.Type).
			Msg("slack: event type not handled, acking")
	}

	w.WriteHeader(http.StatusOK)
}

// handleMessageEvent is the shared inbound translation path for the
// three message-shaped event types (app_mention, message.im,
// @vornik-mentioned message.channels). It enforces the per-
// installation ChannelAllowlist + SenderAllowlist, builds a
// ChannelMessage, records the session, and hands off to the bound
// Receiver. requireMention is true on message.channels (which we
// already filtered before getting here, but the explicit param keeps
// the call site's intent loud); false on app_mention + message.im
// (which Slack only sends when our app is the intended recipient).
func (c *Channel) handleMessageEvent(ctx context.Context, p eventPayload, inst *installation, requireMention bool) {
	ev := p.Event
	if ev.User == "" {
		c.logger.Debug().
			Str("team_id", p.TeamID).
			Str("event_id", p.EventID).
			Msg("slack: message event without user; dropping")
		return
	}
	if len(inst.allowedChannels) > 0 {
		if _, ok := inst.allowedChannels[ev.Channel]; !ok {
			c.logger.Warn().
				Str("team_id", p.TeamID).
				Str("channel", ev.Channel).
				Str("project_id", inst.projectID).
				Msg("slack: channel not on installation allowlist; dropping")
			return
		}
	}
	if _, err := c.resolveSpeakerForInstallation(inst, ev.User); err != nil {
		c.logger.Warn().
			Str("team_id", p.TeamID).
			Str("user", ev.User).
			Str("project_id", inst.projectID).
			Msg("slack: sender not on installation allowlist; dropping (no LLM spend)")
		return
	}
	// Defensive: requireMention is true on message.channels. The
	// HandleWebhook caller already checks but we re-check here so a
	// future refactor that adds a new caller can't accidentally bypass
	// the gate.
	if requireMention && !mentionsVornik(ev.Text) {
		return
	}

	msg := c.buildMessageChannelMessage(p, inst)
	c.recordSession(msg.SessionID, channelTitleFromPayload(p), ev.User, msg.Timestamp, inst)
	c.recvMu.RLock()
	recvAny := c.recv
	c.recvMu.RUnlock()
	if recvAny == nil {
		c.logger.Warn().Str("event_id", p.EventID).Msg("slack: inbound received but no Receiver bound; dropping")
		return
	}
	recv, ok := recvAny.(conversation.Receiver)
	if !ok {
		c.logger.Error().Str("event_id", p.EventID).Msg("slack: bound Receiver does not implement conversation.Receiver; dropping")
		return
	}
	if err := recv.Receive(ctx, msg); err != nil {
		c.logger.Warn().Err(err).Str("event_id", p.EventID).Msg("slack: Receiver.Receive returned error")
	}
}

// buildMessageChannelMessage translates a message-shaped event into
// the generic ChannelMessage envelope. SessionID encoding:
// `<team_id>/<channel_id>#<thread_root_ts>` — thread_ts when present
// (a reply), otherwise the message's own ts (a new thread). This
// collapses sibling replies on the same thread into one session,
// matching how Slack's UI displays threads.
func (c *Channel) buildMessageChannelMessage(p eventPayload, inst *installation) conversation.ChannelMessage {
	ev := p.Event
	threadRoot := ev.ThreadTs
	if threadRoot == "" {
		threadRoot = ev.Ts
	}
	sessionID := fmt.Sprintf("%s/%s#%s", p.TeamID, ev.Channel, threadRoot)
	cs := map[string]string{
		"team_id":      p.TeamID,
		"channel_id":   ev.Channel,
		"channel_type": ev.ChannelType,
		"thread_ts":    threadRoot,
		"event_id":     p.EventID,
		"event_type":   ev.Type,
		"project_id":   inst.projectID,
	}
	if ev.ThreadTs != "" && ev.ThreadTs != ev.Ts {
		cs["in_reply_to_ts"] = ev.Ts
	}
	ts := slackTsToTime(ev.Ts, c.clock)
	return conversation.ChannelMessage{
		Source:          channelName,
		ID:              ev.Ts,
		SessionID:       sessionID,
		SpeakerID:       ev.User,
		Text:            ev.Text,
		InReplyTo:       "", // Slack uses thread_ts as the threading primitive — captured in ChannelSpecific
		ThreadID:        threadRoot,
		Timestamp:       ts,
		ChannelSpecific: cs,
	}
}

// channelTitleFromPayload extracts a best-effort Title for the
// session. Slack doesn't ship the channel name in Events API
// payloads (only the channel_id), so we use the channel_id itself —
// the dispatcher's session-list UI shows e.g. "C0123" rather than
// "general". A future enhancement could call conversations.info to
// resolve to a human-readable name; not needed for v1.
func channelTitleFromPayload(p eventPayload) string {
	if p.Event == nil {
		return ""
	}
	if p.Event.ChannelType == "im" {
		return "DM " + p.Event.Channel
	}
	return "Slack " + p.Event.Channel
}

// slackTsToTime parses Slack's `ts` (a "1234567890.123456" string,
// seconds.microseconds since epoch) into a time.Time. Defensive: an
// unparseable ts falls back to the clock so the rest of the pipeline
// never sees a zero Timestamp.
func slackTsToTime(ts string, clock func() time.Time) time.Time {
	if ts == "" {
		return clock()
	}
	dot := strings.IndexByte(ts, '.')
	secStr := ts
	if dot >= 0 {
		secStr = ts[:dot]
	}
	var sec int64
	for _, b := range []byte(secStr) {
		if b < '0' || b > '9' {
			return clock()
		}
		sec = sec*10 + int64(b-'0')
	}
	return time.Unix(sec, 0)
}

// mentionsVornik returns true when the body contains `<@vornik>` or
// any `@vornik` literal (case-insensitive, word-boundary aware). The
// canonical Slack form is `<@U_BOT_ID>` (resolved from the bot user
// id when the App is installed), but operators sometimes type the
// literal "@vornik" in tests; both should trigger. Mirrors the
// GitHub channel's mention parser.
//
// In production wiring the channel would be configured with the
// bot's own user id and match `<@U_BOT_ID>` exactly. For v1 we use
// the loose "@vornik" literal because every operator deployment has
// the bot named `vornik` and the case-insensitive literal check is
// the lowest-friction onboarding path.
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
		if end >= len(lower) {
			return true
		}
		nextByte := lower[end]
		if !isMentionWordChar(nextByte) {
			return true
		}
		idx = end
	}
}

// isMentionWordChar tests one byte of the lowercased body — caller
// always passes lower[end]. Defines the word-boundary alphabet so
// "@vornik-deploy" doesn't trigger.
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

// Send delivers an outbound message via Slack's chat.postMessage Web
// API. Implementation lives in outbound.go (slice 3); this signature
// is here so *Channel satisfies conversation.Channel after slice 2.
//
// Voice MVP slice 4: when the session's most-recent inbound was an
// audio clip AND a TTS provider is wired, Send synthesises the
// reply (mp4-aac) and uploads via files.upload_v2. On TTS failure,
// oversize audio, or upload failure, Send falls back to text via
// chat.postMessage with the same body.
//
// Returns the upstream message ts ("1234567890.123456") on the text
// path; on the voice path returns the new file_id (Slack's file
// surface uses the file_id for InReplyTo correlation since uploaded
// files don't carry a `ts` of their own).
func (c *Channel) Send(ctx context.Context, msg conversation.ChannelMessage) (string, error) {
	if c.shouldReplyAsVoice(msg.SessionID) {
		sentID, used, err := c.sendVoiceForSession(ctx, msg)
		if used {
			return sentID, nil
		}
		if err != nil {
			c.logger.Info().Err(err).Str("session", msg.SessionID).
				Msg("slack: voice reply failed; falling back to text")
		}
		// Fall through to text.
	}
	return c.sendChatPostMessage(ctx, msg)
}

// sendVoiceForSession resolves the installation from the SessionID
// and routes through the voice-reply upload path. Kept on the
// channel here so the Send body stays small and the voice path can
// surface via a single helper for tests.
func (c *Channel) sendVoiceForSession(ctx context.Context, msg conversation.ChannelMessage) (string, bool, error) {
	teamID, channelID, threadRoot, err := parseSlackSessionID(msg.SessionID)
	if err != nil {
		return "", false, err
	}
	inst, ok := c.installationsByID[teamID]
	if !ok {
		return "", false, fmt.Errorf("%w: team_id %q not configured", ErrUnknownSession, teamID)
	}
	return c.sendVoiceReply(ctx, inst, uploadAudioParams{
		Channel:  channelID,
		ThreadTs: threadRoot,
		Filename: "reply.m4a",
	}, msg.Text)
}

// Compile-time guarantee: *Channel satisfies the conversation.Channel
// contract. Does NOT satisfy StreamingChannel — Slack would support
// chat.update edits, but v1 ships one-shot replies only (matching
// the GitHub App scope). See conversation-channel-design.md.
var _ conversation.Channel = (*Channel)(nil)
