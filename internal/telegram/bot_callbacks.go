// Telegram CallbackQuery dispatch — 2026.6.0 SaaS-readiness
// feature 3. Splits inline-keyboard click handling out of bot.go
// so the main file doesn't bloat further. The dispatcher reads
// the (namespace, action, payload) tuple from the callback data
// and routes to a per-namespace handler.
//
// Adding a new flow:
//  1. Add a case to callbackHandlers below.
//  2. Implement the handler signature
//     (ctx, b, chatID, userID, callbackID, payload) error.
//  3. Build the keyboard via EncodeCallback + Keyboard* so the
//     round-trip is guaranteed valid.
//
// The dispatcher MUST acknowledge every CallbackQuery via
// answerCallbackQuery (Telegram dims the button until ack arrives
// or 15s pass). On routing failure / unknown namespace we still
// ack with an error toast so the button doesn't stay loading.

package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// The current dispatcher uses a Go `switch` over namespace in
// handleCallbackQuery below; an earlier draft factored that into
// a callbackHandler function-type table but the switch is small
// enough that the indirection paid for itself in obscurity, not
// extensibility. The table is reinstated only when a third
// namespace shows up — until then, switch wins.

// handleCallbackQuery is the entry point invoked by HandleUpdate
// when an Update carries a CallbackQuery instead of a Message.
// Decodes the callback data, ack's the click, and routes to the
// per-namespace handler.
func (b *Bot) handleCallbackQuery(ctx context.Context, cq *struct {
	ID   string `json:"id"`
	From struct {
		ID       int64  `json:"id"`
		Username string `json:"username,omitempty"`
	} `json:"from"`
	Message *struct {
		ID   int64 `json:"message_id"`
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	} `json:"message"`
	Data string `json:"data"`
}) error {
	if cq == nil {
		return nil
	}
	ns, action, payload, ok := DecodeCallback(cq.Data)
	if !ok {
		b.logger.Warn().
			Str("callback_id", cq.ID).
			Str("raw_data", cq.Data).
			Msg("telegram: malformed callback_data — ignoring with stale-button toast")
		return b.answerCallbackQuery(ctx, cq.ID, "This button is from an older version of the bot. Please start a fresh chat.", false)
	}

	var chatID int64
	if cq.Message != nil {
		chatID = cq.Message.Chat.ID
	}
	userID := cq.From.ID

	b.logger.Debug().
		Str("namespace", ns).
		Str("action", action).
		Str("payload", payload).
		Int64("chat_id", chatID).
		Int64("user_id", userID).
		Msg("telegram: dispatching callback")

	// Authorization + rate limit. The inline-keyboard callback surface
	// is a full input channel just like text messages, so it must pass
	// the SAME gate HandleMessage applies — otherwise a non-allowlisted
	// user (or a flood) reaches the action handlers (e.g. switching the
	// active project) unthrottled and unauthenticated.
	if !b.IsAllowed(userID) {
		return b.answerCallbackQuery(ctx, cq.ID, "You are not authorized to use this bot.", true)
	}
	if !b.CheckRateLimit(userID) {
		if b.metrics != nil {
			b.metrics.RateLimitsHit.Inc()
		}
		return b.answerCallbackQuery(ctx, cq.ID, "Rate limit exceeded. Please try again later.", true)
	}

	// Switch by namespace. Each namespace owns its own action
	// vocabulary — keeps the dispatcher's surface area small.
	var err error
	switch ns {
	case "project":
		err = b.handleProjectCallback(ctx, chatID, userID, cq.ID, action, payload)
	default:
		b.logger.Warn().Str("namespace", ns).Msg("telegram: unknown callback namespace")
		return b.answerCallbackQuery(ctx, cq.ID, "This action isn't recognised — the bot may have been updated since this button was rendered.", false)
	}
	if err != nil {
		b.logger.Warn().Err(err).Str("namespace", ns).Str("action", action).
			Msg("telegram: callback handler returned error; surfacing as toast")
		// Best-effort: try to ack with the error. If THAT fails
		// the button stays loading until Telegram's 15s timeout —
		// no further recovery we can do.
		_ = b.answerCallbackQuery(ctx, cq.ID, "Something went wrong handling that button. Check the daemon log.", true)
	}
	return err
}

// handleProjectCallback owns the `project:*` namespace.
//
// Actions:
//   - select  — payload is a project ID; switch the user's
//     active project to it.
//   - menu    — payload is a sub-action of the persistent project
//     keyboard (status / recent / new).
//
// Unknown actions ack with a stale-button toast.
func (b *Bot) handleProjectCallback(ctx context.Context, chatID, userID int64, callbackID, action, payload string) error {
	switch action {
	case "select":
		projectID := strings.TrimSpace(payload)
		if projectID == "" {
			return b.answerCallbackQuery(ctx, callbackID, "Missing project ID in button payload.", false)
		}
		// Mirror the /project text-command checks: the project must exist
		// AND the user must be cleared for it. Without this a crafted (or
		// stale) callback could pin an arbitrary project the user has no
		// access to — an IDOR divergence from the text path.
		if b.registry != nil && b.registry.GetProject(projectID) == nil {
			return b.answerCallbackQuery(ctx, callbackID, fmt.Sprintf("Unknown project %q.", projectID), false)
		}
		if !b.UserCanAccessProject(userID, projectID) {
			return b.answerCallbackQuery(ctx, callbackID, fmt.Sprintf("You are not authorized for project %q.", projectID), false)
		}
		b.setActiveProject(chatID, projectID)
		// Ack with a confirming toast + send a follow-up message.
		_ = b.answerCallbackQuery(ctx, callbackID, fmt.Sprintf("Switched to project %s", projectID), false)
		_ = b.sendMessage(ctx, chatID, fmt.Sprintf("Active project: %s", projectID))
		return nil
	default:
		b.logger.Warn().Str("action", action).Msg("telegram: unknown project action")
		return b.answerCallbackQuery(ctx, callbackID, "Unknown project action.", false)
	}
}

// answerCallbackQuery POSTs to Telegram's answerCallbackQuery
// endpoint. Every CallbackQuery MUST be answered within ~15s
// or the user's Telegram client shows the button as "loading"
// indefinitely; the dispatcher ack's on every code path including
// validation failures.
//
// ShowAlert=true renders the text as a modal the user has to
// dismiss; ShowAlert=false shows it as a transient toast. Default
// (false) is the right pick for confirmations; alerts are for
// errors the user must read before retrying.
func (b *Bot) answerCallbackQuery(ctx context.Context, callbackID, text string, showAlert bool) error {
	if b.config.Token == "" {
		// Bot not fully wired (test path). Silently no-op —
		// the bot's other write paths follow the same pattern.
		return nil
	}
	body := AnswerCallbackQueryRequest{
		CallbackQueryID: callbackID,
		Text:            text,
		ShowAlert:       showAlert,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal answerCallbackQuery: %w", err)
	}
	// b.baseURL points at the real Telegram API in production
	// (set by NewBot from the Token) and at httptest.Server in
	// tests. Reusing it here keeps callback ack on the same
	// transport as sendMessage / forum-topic operations.
	url := fmt.Sprintf("%s/answerCallbackQuery", b.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		buf, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("answerCallbackQuery HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))
	}
	return nil
}
