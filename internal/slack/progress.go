package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Slack gives a bot no typing indicator, and a companion turn runs 10-60 seconds
// (memory_search, a Workspace tool, a ~60 KB prompt). For that whole window the channel
// shows nothing, so a working bot looks exactly like a broken one — which is how the
// 2026-07-30 thread-reply bug hid: a 2.9 KB answer was generated and silently lost, and
// the only reason anyone noticed was that a human asked whether the bot was alive.
//
// The signal is a placeholder message, posted only once a turn has run long enough to
// look dead, and removed when the real reply lands. Chosen over the two alternatives on
// the backlog for one reason each:
//
//   - reactions.add on the inbound message is tidier but needs `reactions:write`, so a
//     manifest change and a reinstall. A reinstall is what interrupted event delivery on
//     2026-07-30; this needs nothing beyond the chat:write the bot already has.
//   - assistant.threads.setStatus is the official mechanism and the nicest UX, but it
//     needs the whole Agents/AI-Apps surface plus `assistant:write`. It supersedes this
//     when it lands; until then this covers channels, threads and DMs alike.
const (
	// progressSignalDelay is how long a turn may run before it needs to say something.
	// Long enough that ordinary quick answers never flicker a placeholder in and out,
	// short enough that the silence never reads as a failure.
	progressSignalDelay = 2 * time.Second

	// progressSignalText is deliberately plain. It is a transient artefact that gets
	// deleted, so it must not look like content worth reading or replying to.
	progressSignalText = "_working on it…_"
)

// progressSignal tracks one turn's placeholder through its three possible fates:
// cancelled before it was ever posted (the fast-turn case), posted and later removed,
// or posted while the reply was already in flight and removed immediately.
type progressSignal struct {
	mu       sync.Mutex
	stop     chan struct{} // closed to disarm the pending timer
	ts       string        // placeholder message ts, once posted
	finished bool          // a real message has landed; nothing more may be posted
	// status is the latest line the dispatcher reported for this turn ("searching
	// memory…"). Empty until a tool runs. Read when the placeholder is first
	// posted so it opens on the CURRENT activity rather than the generic text,
	// and again on every update.
	status string
}

// beginProgressSignal arms a delayed placeholder for a turn on sessionID. Safe to call
// with no bot token or a broken API — every failure degrades to "no signal", never to a
// failed turn.
//
// The context is the DETACHED dispatch context, so the timer is bounded by the turn's
// own deadline rather than by Slack's three-second ack budget.
func (c *Channel) beginProgressSignal(ctx context.Context, sessionID string) {
	if sessionID == "" {
		return
	}
	delay := c.progressDelay
	if delay <= 0 { // operator opted out, or the channel was built with signals off
		return
	}

	sig := &progressSignal{stop: make(chan struct{})}
	c.progressMu.Lock()
	if c.progress == nil {
		c.progress = make(map[string]*progressSignal)
	}
	// A second turn on the same session while one is pending: disarm the older signal
	// so the map never holds an orphan whose placeholder nobody will delete.
	if prev, ok := c.progress[sessionID]; ok {
		c.finishProgressSignal(ctx, sessionID, prev)
	}
	c.progress[sessionID] = sig
	c.progressMu.Unlock()

	// Deliberately NOT counted in inFlight. The timer sleeps for the whole progress
	// delay, so counting it would make Stop — and every test's waitInFlight — block for
	// that delay on each turn. Shutdown correctness comes from disarmProgressSignals
	// instead, which closes stop and collects any placeholder already posted.
	go func() {
		select {
		case <-sig.stop:
			return
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		// Open on whatever the turn is actually doing, if a tool has already run.
		sig.mu.Lock()
		opening := sig.status
		sig.mu.Unlock()
		if opening == "" {
			opening = progressSignalText
		}

		ts, err := c.postPlaceholder(ctx, sessionID, opening)
		if err != nil {
			// Best-effort by design: a channel we cannot post a placeholder into is a
			// channel the reply itself will fail in, and that error is the one worth
			// surfacing.
			c.logger.Debug().Err(err).Str("session", sessionID).
				Msg("slack: progress placeholder could not be posted")
			return
		}

		sig.mu.Lock()
		alreadyDone := sig.finished
		sig.ts = ts
		sig.mu.Unlock()
		if alreadyDone {
			// The reply landed while this POST was in flight. Nobody else will clean
			// this up, because clearProgressSignal already took the map entry.
			c.deletePlaceholder(ctx, sessionID, ts)
		}
	}()
}

// clearProgressSignal removes the placeholder for sessionID, if any. Called once a real
// outbound message has been delivered — including the Art 50 disclosure, which on a
// session's first turn is what lands first.
func (c *Channel) clearProgressSignal(ctx context.Context, sessionID string) {
	c.progressMu.Lock()
	sig, ok := c.progress[sessionID]
	if ok {
		delete(c.progress, sessionID)
	}
	c.progressMu.Unlock()
	if !ok {
		return
	}
	c.finishProgressSignal(ctx, sessionID, sig)
}

// finishProgressSignal disarms a signal and deletes its placeholder if one was posted.
// Callers must have already removed it from the map (or be holding progressMu while
// replacing it) so the cleanup runs exactly once.
func (c *Channel) finishProgressSignal(ctx context.Context, sessionID string, sig *progressSignal) {
	sig.mu.Lock()
	if sig.finished {
		sig.mu.Unlock()
		return
	}
	sig.finished = true
	ts := sig.ts
	close(sig.stop)
	sig.mu.Unlock()

	if ts != "" {
		c.deletePlaceholder(ctx, sessionID, ts)
	}
}

// disarmProgressSignals disarms every pending signal. Called from Stop so a shutdown
// cannot leave a timer that posts a placeholder into a channel after the daemon has
// stopped answering.
func (c *Channel) disarmProgressSignals() {
	c.progressMu.Lock()
	pending := c.progress
	c.progress = nil
	c.progressMu.Unlock()

	for sessionID, sig := range pending {
		c.finishProgressSignal(context.WithoutCancel(context.Background()), sessionID, sig)
	}
}

// postPlaceholder posts the placeholder message and returns its ts.
//
// It deliberately does NOT go through sendChatPostMessage, because that consumes a
// token from the per-(team, channel) Tier-3 bucket and returns an error rather than
// waiting when the bucket is empty. A placeholder that took the reply's token would
// make a turn finishing shortly after the delay fail to deliver its answer — turning a
// cosmetic feature into the exact silent-loss bug it exists to expose. The placeholder
// is our own transient artefact; if Slack itself throttles it, it simply does not
// appear.
func (c *Channel) postPlaceholder(ctx context.Context, sessionID, text string) (string, error) {
	teamID, channelID, threadRoot, err := parseSlackSessionID(sessionID)
	if err != nil {
		return "", err
	}
	var parsed chatPostMessageResponse
	if err := c.callSlackAPI(ctx, teamID, "/chat.postMessage", chatPostMessageRequest{
		Channel:  channelID,
		Text:     text,
		ThreadTs: resolveThreadTs(threadRoot),
	}, &parsed); err != nil {
		return "", err
	}
	if !parsed.OK {
		return "", fmt.Errorf("slack channel: placeholder post refused: %s", parsed.Error)
	}
	if parsed.Ts == "" {
		return "", errors.New("slack channel: placeholder response missing ts")
	}
	return parsed.Ts, nil
}

// deletePlaceholder removes a posted placeholder via chat.delete. Needs no scope beyond
// the chat:write already used to post it — a bot may always delete its own message.
//
// Errors are logged at Debug and swallowed: a placeholder that fails to delete is
// cosmetic, and there is no user-visible action to take.
func (c *Channel) deletePlaceholder(ctx context.Context, sessionID, ts string) {
	teamID, channelID, _, err := parseSlackSessionID(sessionID)
	if err != nil {
		return
	}
	var parsed struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	payload := struct {
		Channel string `json:"channel"`
		Ts      string `json:"ts"`
	}{Channel: channelID, Ts: ts}

	if err := c.callSlackAPI(ctx, teamID, "/chat.delete", payload, &parsed); err != nil {
		c.logger.Debug().Err(err).Str("session", sessionID).
			Msg("slack: progress placeholder could not be deleted")
		return
	}
	if !parsed.OK {
		c.logger.Debug().
			Str("session", sessionID).
			Str("slack_error", parsed.Error).
			Msg("slack: chat.delete refused the progress placeholder")
	}
}

// callSlackAPI performs one authenticated Web API call for the installation owning
// teamID and decodes the response into out. No rate-limit gate: this is the path for
// the channel's own housekeeping calls, which must not compete with user-visible
// replies for the outbound bucket.
func (c *Channel) callSlackAPI(ctx context.Context, teamID, method string, payload, out any) error {
	inst, ok := c.installationsByID[teamID]
	if !ok {
		return fmt.Errorf("%w: team_id %q not configured", ErrUnknownSession, teamID)
	}
	if strings.TrimSpace(inst.botToken) == "" {
		return ErrOutboundNotConfigured
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBaseURL+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+inst.botToken)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxOutboundResponseBytes))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack channel: %s HTTP %d: %s",
			method, resp.StatusCode, truncateBody(string(respBody)))
	}
	return json.Unmarshal(respBody, out)
}

// SetTurnStatus implements conversation.TurnStatusChannel: it rewrites the in-flight
// placeholder to say what the turn is doing right now ("searching memory…", "scheduling
// the job…").
//
// This is the operator's "mechanism to display statuses for thinking, tool calling"
// request, met WITHOUT the Agents/AI-Apps surface. assistant.threads.setStatus is the
// native equivalent, but it needs `assistant:write`, a manifest change and a reinstall —
// and it converts DMs into assistant threads, which is the path three people actually
// use. Rewriting a message we already own needs nothing new.
//
// It only ever UPDATES an already-posted placeholder; it never posts one. That preserves
// the property the delay exists for: a turn that answers in under two seconds still
// leaves no trace in the channel. A status arriving before the placeholder is stored on
// the signal instead, so when the timer fires it opens on the current activity rather
// than the generic line.
//
// Display only. The text never enters conversation history and costs no model context,
// which is what lets it be rewritten as often as the turn changes activity.
func (c *Channel) SetTurnStatus(ctx context.Context, sessionID, status string) {
	status = strings.TrimSpace(status)
	if sessionID == "" || status == "" {
		return
	}
	c.progressMu.Lock()
	sig, ok := c.progress[sessionID]
	c.progressMu.Unlock()
	if !ok {
		return
	}

	sig.mu.Lock()
	if sig.finished {
		// The reply already landed. Updating now would resurrect a line the user has
		// no reason to see again.
		sig.mu.Unlock()
		return
	}
	sig.status = status
	ts := sig.ts
	sig.mu.Unlock()

	if ts == "" {
		// Not posted yet — the timer will open on this status if it fires.
		return
	}
	c.updatePlaceholder(ctx, sessionID, ts, "_"+status+"_")
}

// updatePlaceholder rewrites a posted placeholder in place via chat.update.
//
// No rate-limit gate, for the same reason postPlaceholder skips it: this is the
// channel's own housekeeping and must never take the token the user's actual reply
// needs. Failures are logged at Debug and dropped — a status line that did not update is
// cosmetic, and there is nothing for an operator to do about it.
func (c *Channel) updatePlaceholder(ctx context.Context, sessionID, ts, text string) {
	teamID, channelID, _, err := parseSlackSessionID(sessionID)
	if err != nil {
		return
	}
	var parsed struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	payload := struct {
		Channel string `json:"channel"`
		Ts      string `json:"ts"`
		Text    string `json:"text"`
	}{Channel: channelID, Ts: ts, Text: text}

	if err := c.callSlackAPI(ctx, teamID, "/chat.update", payload, &parsed); err != nil {
		c.logger.Debug().Err(err).Str("session", sessionID).
			Msg("slack: progress status could not be updated")
		return
	}
	if !parsed.OK {
		c.logger.Debug().
			Str("session", sessionID).
			Str("slack_error", parsed.Error).
			Msg("slack: chat.update refused the progress status")
	}
}

// placeholderTs returns the ts of the posted placeholder for a session, or "" when none
// has been posted yet.
//
// Exists because "the POST was sent" and "the ts is recorded" are two different moments:
// the ts is assigned only after the response parses. Tests synchronise on this rather
// than on the request count, which would race the assignment.
func (c *Channel) placeholderTs(sessionID string) string {
	c.progressMu.Lock()
	sig, ok := c.progress[sessionID]
	c.progressMu.Unlock()
	if !ok {
		return ""
	}
	sig.mu.Lock()
	defer sig.mu.Unlock()
	return sig.ts
}
