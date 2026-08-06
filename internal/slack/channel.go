package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
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
	// Drain in-flight event turns. Since deliveries are dispatched asynchronously
	// (Slack's ack budget is 3s, a turn takes much longer), a Stop that returned
	// immediately would abandon a turn between "answer generated" and "answer
	// posted" — the same silent-loss shape the async dispatch was introduced to fix.
	//
	// Disarm the pending "working on it…" timers first, so a shutdown between arming
	// and firing cannot post a placeholder into a channel the daemon has stopped
	// answering in.
	c.disarmProgressSignals()
	c.waitInFlight()
	return nil
}

// waitInFlight blocks until every dispatched event turn has finished. Used by Stop
// for a graceful drain, and by tests as a deterministic synchronisation point instead
// of a sleep.
func (c *Channel) waitInFlight() {
	c.inFlight.Wait()
}

// ResolveSpeaker maps a Slack user_id (U…) to a conversation.Speaker.
// Returns ErrSpeakerUnknown when no installation's SenderAllowlist
// admits the speaker. An empty allowlist denies since 2026-08-05 unless
// AllowUnlistedSenders opts back in — see anyInstallationAllowsSpeaker.
// Mirrors the GitHub channel's "any installation accepts ⇒ admit"
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
// installation's SenderAllowlist permits the user_id.
//
// An EMPTY allowlist denies (2026-08-05). It used to admit everyone, which
// meant an unconfigured installation silently let any member of the workspace
// drive the dispatcher. Set AllowUnlistedSenders to opt back in deliberately.
func (c *Channel) anyInstallationAllowsSpeaker(userID string) bool {
	for _, inst := range c.installations {
		if len(inst.senders) == 0 {
			if inst.allowUnlisted {
				return true
			}
			continue
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
	// Empty allowlist denies unless explicitly opened — see
	// anyInstallationAllowsSpeaker for the 2026-08-05 default flip.
	if len(inst.senders) == 0 {
		if !inst.allowUnlisted {
			return conversation.Speaker{}, conversation.ErrSpeakerUnknown
		}
	} else if _, ok := inst.senders[userID]; !ok {
		return conversation.Speaker{}, conversation.ErrSpeakerUnknown
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
	// Authorizations names the installed identities the delivery was
	// sent on behalf of. The bot entry carries OUR OWN user id, which
	// is how a thread rooted on one of our messages is recognised
	// without a config entry or an extra auth.test call.
	Authorizations []eventAuthorization `json:"authorizations,omitempty"`
}

// eventAuthorization is one entry of the Events API `authorizations`
// array.
type eventAuthorization struct {
	TeamID string `json:"team_id,omitempty"`
	UserID string `json:"user_id,omitempty"`
	IsBot  bool   `json:"is_bot,omitempty"`
}

// botUserID returns the bot identity this delivery was authorised for,
// or "" when Slack omitted the array (older payload shapes, and the
// synthetic payloads in some tests).
func (p eventPayload) botUserID() string {
	for _, a := range p.Authorizations {
		if a.IsBot && a.UserID != "" {
			return a.UserID
		}
	}
	return ""
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
	// Tab distinguishes which App Home tab was opened on an
	// app_home_opened event: "home" or "messages".
	Tab string `json:"tab,omitempty"`
	// ParentUserID is the author of the thread's ROOT message, present
	// on thread replies. When it equals our own bot user id the thread
	// hangs off something we said, which makes a mention-less reply an
	// unambiguous continuation of our conversation.
	ParentUserID string `json:"parent_user_id,omitempty"`
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

// acceptDelivery atomically claims an upstream delivery ID for the replay
// window. Slack retries an Events API request when the handler spends more
// than a few seconds in the dispatcher; retries carry the same event_id.
func (c *Channel) acceptDelivery(id string, now time.Time) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return true
	}
	c.deliveriesMu.Lock()
	defer c.deliveriesMu.Unlock()
	cutoff := now.Add(-maxReplayWindow)
	for seenID, seenAt := range c.seenDeliveries {
		if seenAt.Before(cutoff) {
			delete(c.seenDeliveries, seenID)
		}
	}
	if _, exists := c.seenDeliveries[id]; exists {
		return false
	}
	if len(c.seenDeliveries) >= maxSeenDeliveries {
		var (
			oldestID string
			oldestAt time.Time
		)
		for seenID, seenAt := range c.seenDeliveries {
			if oldestID == "" || seenAt.Before(oldestAt) {
				oldestID, oldestAt = seenID, seenAt
			}
		}
		delete(c.seenDeliveries, oldestID)
	}
	c.seenDeliveries[id] = now
	return true
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
	// Slack marks retry attempts explicitly. Drop them even when a load
	// balancer sends the retry to another daemon whose in-memory event_id
	// cache has never seen the original delivery.
	if retryNum := strings.TrimSpace(r.Header.Get("X-Slack-Retry-Num")); retryNum != "" {
		c.logger.Info().
			Str("retry_num", retryNum).
			Str("retry_reason", r.Header.Get("X-Slack-Retry-Reason")).
			Msg("slack: retry delivery acknowledged without redispatch")
		w.WriteHeader(http.StatusOK)
		return
	}

	if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/x-www-form-urlencoded") {
		c.handleSlashCommandWebhook(w, r, body, now)
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
	//
	// The challenge is request-derived (attacker-controllable on an
	// unsigned probe — this branch runs before the event_callback
	// routing but the signature IS verified above). It is HTML-escaped
	// before it reaches the response body so a crafted challenge can
	// never be reflected as live markup (go/reflected-xss); real Slack
	// challenges are opaque alphanumeric nonces, so escaping is a no-op
	// on the genuine handshake. The text/plain content type is kept as
	// defence in depth.
	if payload.Type == "url_verification" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(html.EscapeString(payload.Challenge)))
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
	if !c.acceptDelivery("event:"+payload.EventID, now) {
		c.logger.Info().
			Str("team_id", teamID).
			Str("event_id", payload.EventID).
			Msg("slack: duplicate event delivery acknowledged")
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

	// DETACH THE DISPATCH FROM THE REQUEST. Slack allows THREE SECONDS to ack an
	// event delivery; a real turn (memory_search, a Workspace tool, a 60 KB prompt)
	// takes far longer. Handling an event on r.Context() therefore fails twice over:
	// the handler blocks past the ack budget so Slack retries, and when Slack closes
	// the original connection the cancellation kills the in-flight turn — including
	// the outbound reply.
	//
	// Observed in production 2026-07-30: a thread reply ran its tools, produced a
	// 2.9 KB answer, and the user saw nothing. The slash-command path below already
	// had this right; the event paths did not.
	//
	// WithoutCancel keeps the values (trace ids, logger) while dropping the
	// cancellation; WithTimeout re-bounds it so a hung turn cannot leak a goroutine
	// and an LLM call forever. The ack is written before dispatching, so Slack sees a
	// 200 immediately and does not retry.
	dispatchCtx, cancel := context.WithTimeout(
		context.WithoutCancel(r.Context()), eventDispatchTimeout)
	dispatch := func(fn func(context.Context)) {
		w.WriteHeader(http.StatusOK)
		c.inFlight.Add(1)
		go func() {
			defer c.inFlight.Done()
			defer cancel()
			fn(dispatchCtx)
		}()
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
		dispatch(func(ctx context.Context) { c.handleFileSharedEvent(ctx, payload, inst) })
	case "app_home_opened":
		// Publishing the home view keeps Slack from rendering its own "still a work in
		// progress" placeholder in the first surface a new user opens.
		dispatch(func(ctx context.Context) { c.handleAppHomeOpened(ctx, payload, inst) })
	case "app_mention":
		dispatch(func(ctx context.Context) { c.handleMessageEvent(ctx, payload, inst, false) })
	case "message":
		// message.im → always dispatch. message.channels / .groups → only when the
		// message is addressed to us, which handleMessageEvent decides.
		//
		// That decision USED TO LIVE HERE, gated purely on the message text. It now
		// runs inside the dispatched turn because establishing "is this a thread we
		// are already in" can reach the database, and Slack's ack budget is three
		// seconds. The cost of deciding late is one short-lived goroutine per channel
		// message; no LLM spend happens before the gate.
		switch payload.Event.ChannelType {
		case "im":
			dispatch(func(ctx context.Context) { c.handleMessageEvent(ctx, payload, inst, false) })
		case "channel", "group":
			dispatch(func(ctx context.Context) { c.handleMessageEvent(ctx, payload, inst, true) })
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

// handleSlashCommandWebhook handles Slack's signed
// application/x-www-form-urlencoded command payload. Vornik exposes a single
// command, /vornik <prompt>, which enters the same project-scoped dispatcher
// session as ordinary Slack messages. It acknowledges before dispatch so Slack
// never shows its three-second timeout banner.
func (c *Channel) handleSlashCommandWebhook(w http.ResponseWriter, r *http.Request, body []byte, now time.Time) {
	form, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	teamID := strings.TrimSpace(form.Get("team_id"))
	inst, ok := c.installationsByID[teamID]
	if !ok {
		c.logger.Warn().Str("team_id", teamID).Msg("slack: slash command for unknown team")
		w.WriteHeader(http.StatusOK)
		return
	}
	expected := NormaliseSlashCommand(c.cfg.SlashCommand)
	if command := strings.TrimSpace(form.Get("command")); command != expected {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("Unsupported command. Use " + expected + " <prompt>."))
		return
	}
	channelID := strings.TrimSpace(form.Get("channel_id"))
	userID := strings.TrimSpace(form.Get("user_id"))
	if !c.slashCommandActorAllowed(inst, channelID, userID) {
		w.WriteHeader(http.StatusOK)
		return
	}
	text := strings.TrimSpace(form.Get("text"))
	if text == "" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("Usage: " + expected + " <prompt>"))
		return
	}
	triggerID := strings.TrimSpace(form.Get("trigger_id"))
	if !c.acceptDelivery("slash:"+triggerID, now) {
		w.WriteHeader(http.StatusOK)
		return
	}

	c.recvMu.RLock()
	recvAny := c.recv
	c.recvMu.RUnlock()
	recv, ok := recvAny.(conversation.Receiver)
	if !ok || recv == nil {
		c.logger.Warn().Str("trigger_id", triggerID).Msg("slack: slash command received but no Receiver bound")
		w.WriteHeader(http.StatusOK)
		return
	}
	sessionID := fmt.Sprintf("%s/%s#slash:%s", teamID, channelID, userID)
	msg := conversation.ChannelMessage{
		Source:    channelName,
		ID:        triggerID,
		SessionID: sessionID,
		SpeakerID: userID,
		Text:      text,
		ThreadID:  "slash:" + userID,
		Timestamp: now,
		ChannelSpecific: map[string]string{
			"team_id":      teamID,
			"channel_id":   channelID,
			"event_id":     triggerID,
			"event_type":   "slash_command",
			"command":      expected,
			"project_id":   inst.projectID,
			"response_url": form.Get("response_url"),
		},
	}
	c.recordSession(sessionID, "Slack "+channelID, userID, now, inst)

	dispatchCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), slashDispatchTimeout)
	w.WriteHeader(http.StatusOK)
	go func() {
		defer cancel()
		c.beginProgressSignal(dispatchCtx, sessionID)
		if err := recv.Receive(dispatchCtx, msg); err != nil {
			c.logger.Warn().Err(err).Str("trigger_id", triggerID).Msg("slack: slash command Receiver.Receive returned error")
		}
	}()
}

// slashCommandActorAllowed gates a /vornik invocation on the same two allowlists as an
// inbound message, DMs included — the slash payload carries no channel_type, so the
// `D…` id prefix is the only marker available. See channelAllowed for why a channel
// allowlist cannot be applied to a DM at all.
func (c *Channel) slashCommandActorAllowed(inst *installation, channelID, userID string) bool {
	if !channelAllowed(inst, "", channelID) {
		c.logger.Warn().
			Str("channel", channelID).
			Str("project_id", inst.projectID).
			Msg("slack: slash command channel not on installation allowlist; dropping")
		return false
	}
	_, err := c.resolveSpeakerForInstallation(inst, userID)
	return err == nil
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
	if !channelAllowed(inst, ev.ChannelType, ev.Channel) {
		{
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
	// requireMention is true on message.channels / message.groups — every message in a
	// channel the bot belongs to, most of which are not for us.
	msg := c.buildMessageChannelMessage(p, inst)
	if requireMention && !c.addressedToUs(ctx, p, msg.SessionID) {
		return
	}

	// One user message can reach us as TWO deliveries with different event_ids
	// (app_mention plus message.channels), so the event_id cache cannot collapse them.
	// Claim the message itself, and only once every gate above has passed, so a
	// delivery we dropped never suppresses the sibling that would have been answered.
	if !c.acceptDelivery("msg:"+ev.Channel+":"+ev.Ts, c.clock()) {
		c.logger.Debug().
			Str("event_id", p.EventID).
			Str("ts", ev.Ts).
			Msg("slack: message already answered via a sibling delivery; dropping")
		return
	}

	c.recordSession(msg.SessionID, channelTitleFromPayload(p), ev.User, msg.Timestamp, inst)
	// From here the turn is ours and will take as long as it takes. Arm the stand-in
	// for the typing indicator Slack does not give bots.
	c.beginProgressSignal(ctx, msg.SessionID)
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

// addressedToUs decides whether a SHARED-channel message that did not arrive as an
// app_mention should still start a turn.
//
// INCIDENT 2026-07-30. This used to be `mentionsVornik(text)` alone, and three
// consecutive follow-ups inside a thread the bot was actively holding went unanswered
// with nothing in the journal above Debug. Two compounding reasons:
//
//  1. mentionsVornik matches the LITERAL string "@vornik". A real Slack mention is
//     delivered encoded — `<@U0BLPMBQXDL>` — so the gate never recognises a genuine
//     mention. Tagged messages work only because app_mention is a separate delivery.
//  2. Replies inside a thread carry no mention at all. That is how people converse in
//     threads: you tag the bot once to open the thread and then just talk.
//
// So a mention-less message counts as ours when it is a reply inside a thread we are
// already part of. Top-level channel chatter still needs an explicit mention — that is
// what keeps the bot out of conversations between colleagues, and what keeps LLM spend
// bounded in a busy channel.
func (c *Channel) addressedToUs(ctx context.Context, p eventPayload, sessionID string) bool {
	ev := p.Event
	// Someone typing "@vornik" as plain text rather than picking the autocomplete. No
	// app_mention is delivered for that, so this branch is the only thing that catches
	// it.
	if mentionsVornik(ev.Text) {
		return true
	}
	if ev.ThreadTs == "" {
		return false
	}
	// The thread hangs off a message WE posted. Stateless, needs no lookup, and
	// survives a restart — which matters because the whole point is continuity.
	if bot := p.botUserID(); bot != "" && ev.ParentUserID == bot {
		return true
	}
	// Otherwise: do we hold conversation history for this thread? True when a human
	// opened the thread and tagged us into it. Backed by the persisted session row, so
	// this also survives a restart.
	return c.threadEngaged(ctx, sessionID)
}

// channelAllowed applies the channel allowlist, EXEMPTING direct messages.
//
// channel_allowlist exists to bound which SHARED channels the bot may act in — it is
// what keeps it out of a channel someone added it to by mistake. Applying it to DMs is
// unimplementable: a DM's channel id is a per-user `D…` that Slack creates lazily on
// first contact, so an operator cannot list it in advance. Three users means three ids
// that do not exist until each person messages the bot.
//
// Doing so silently killed every direct message on this deployment (2026-07-30):
// "slack: channel not on installation allowlist; dropping channel=D0BLA9ZRDFH", with
// only the team's C… channel allow-listed.
//
// A DM's meaningful control is the SENDER allowlist, which is applied separately and
// unchanged. Note the consequence, stated rather than hidden: with an EMPTY
// sender_allowlist, any workspace member can DM the bot. That was already true for
// every other channel type and is why production deployments set it.
//
// Both signals are consulted because the two inbound shapes differ: message events
// carry channel_type, while file_shared often does not — there, the `D` prefix is the
// only marker available.
// The DM exemption is checked FIRST, before the empty-allowlist deny. Ordering it the
// other way round (2026-08-05 – 2026-08-06) silently reinstated the 2026-07-30 defect
// for the one config a DM bot must use — sender allowlist set, channel allowlist empty —
// dropping every direct message with no reply and no error the Slack user could see. A
// channel allowlist has no bearing on a DM whether it is populated or not, so the
// exemption cannot live behind a branch on its length.
func channelAllowed(inst *installation, channelType, channelID string) bool {
	if isDirectMessageChannel(channelType, channelID) {
		return true
	}
	if len(inst.allowedChannels) == 0 {
		// Empty denies as of 2026-08-05 unless opted open — the sender gate
		// above is the primary control, but an unconfigured channel allowlist
		// used to admit every channel in the workspace, which is the same
		// fail-open shape.
		return inst.allowUnlisted
	}
	_, ok := inst.allowedChannels[channelID]
	return ok
}

// isDirectMessageChannel reports whether an inbound event came from a DM.
func isDirectMessageChannel(channelType, channelID string) bool {
	if channelType == "im" {
		return true
	}
	// Slack channel-id convention: D… direct, C… public, G… private/group. Used only
	// when channel_type is absent, which is the file_shared case.
	return channelType == "" && strings.HasPrefix(channelID, "D")
}

// buildMessageChannelMessage translates a message-shaped event into
// the generic ChannelMessage envelope. SessionID encoding:
// `<team_id>/<channel_id>#<thread_root_ts>` — thread_ts when present
// (a reply), otherwise the message's own ts (a new thread). This
// collapses sibling replies on the same thread into one session,
// matching how Slack's UI displays threads.
func (c *Channel) buildMessageChannelMessage(p eventPayload, inst *installation) conversation.ChannelMessage {
	ev := p.Event
	// Session scope: a message inside a thread belongs to that thread; a
	// top-level channel message belongs to the CHANNEL.
	//
	// Keying a top-level message on its own ts (the original behaviour) gave
	// every channel message a fresh empty session, so a correspondent who
	// follows up in the channel — the normal thing to do, because Slack threads
	// are unfindable after a few days — got a bot with no memory of the earlier
	// threads OR of her own previous channel message (operator report
	// 2026-07-28).
	threadRoot := ev.ThreadTs
	if threadRoot == "" {
		threadRoot = ChannelSessionThreadRoot
	}
	sessionID := fmt.Sprintf("%s/%s#%s", p.TeamID, ev.Channel, threadRoot)
	cs := map[string]string{
		"team_id":      p.TeamID,
		"channel_id":   ev.Channel,
		"channel_type": ev.ChannelType,
		// The REAL thread_ts, or empty at channel level. Never the synthesised
		// session root: voiceTracker keys on this and must not be told a thread
		// exists when it does not.
		"thread_ts":  ev.ThreadTs,
		"event_id":   p.EventID,
		"event_type": ev.Type,
		"project_id": inst.projectID,
	}
	if ev.ThreadTs != "" && ev.ThreadTs != ev.Ts {
		cs["in_reply_to_ts"] = ev.Ts
	}
	// A thread rooted on a message WE posted is a continuation of whatever conversation
	// that message belonged to — most often the CHANNEL-level one, which lives in a
	// different session. The session store uses this to seed an otherwise empty thread,
	// so a follow-up under our own answer is not met with "I have no context to anchor
	// on" (operator report 2026-07-30).
	if bot := p.botUserID(); bot != "" && ev.ThreadTs != "" && ev.ParentUserID == bot {
		cs["thread_parent_is_bot"] = "true"
	}
	ts := slackTsToTime(ev.Ts, c.clock)
	return conversation.ChannelMessage{
		Source:          channelName,
		ID:              ev.Ts,
		SessionID:       sessionID,
		SpeakerID:       ev.User,
		Text:            ev.Text,
		InReplyTo:       "",          // Slack uses thread_ts as the threading primitive — captured in ChannelSpecific
		ThreadID:        ev.ThreadTs, // empty at channel level; ThreadID names a thread, and there isn't one
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
	// A real message is landing, so the "working on it…" placeholder has done its job.
	// Cleared for the disclosure notice too — on a session's first turn that is what
	// arrives first, and a placeholder left behind it would never be collected.
	defer c.clearProgressSignal(ctx, msg.SessionID)

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
		Channel: channelID,
		// Same sentinel mapping as the text path — otherwise a channel-scoped
		// session would upload with a literal thread_ts of "main".
		ThreadTs: resolveThreadTs(threadRoot),
		Filename: "reply.m4a",
	}, msg.Text)
}

// Compile-time guarantee: *Channel satisfies the conversation.Channel
// contract. Does NOT satisfy StreamingChannel — Slack would support
// chat.update edits, but v1 ships one-shot replies only (matching
// the GitHub App scope). See conversation-channel-design.md.
var _ conversation.Channel = (*Channel)(nil)
