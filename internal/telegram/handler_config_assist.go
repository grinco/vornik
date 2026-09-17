package telegram

import (
	"context"
	"strings"
	"time"
)

// The configuration assistant's CHAT ENTRYPOINT on Telegram — config-assistant
// design §6.3.3, plan §9 (WP8).
//
// Telegram dispatches on a leading `/word`, so this is a real command in the
// deterministic ops cluster beside /tasks, /status, /cancel and /retry, rather
// than the keyword prefix Slack is forced into. No prompt can be mistaken for
// it, which is why this half carries none of the swallowing residual §6.3.3
// accepts for Slack.
//
// It raises its caller's reach — a chat sender cannot edit the config tree,
// and through here they can propose changes to it — so it carries classes A
// and B2 only (enforced in the engine) and authorises on a RESOLVED IDENTITY,
// never on the legacy allowlist.

// ConfigAssistant is the engine as this package needs it. The same shape the
// Slack channel declares, and declared separately for the same reason
// AccountLinker is: neither channel should depend on how the assistant is
// built.
type ConfigAssistant interface {
	ProposeFromChat(ctx context.Context, projectID, intent, accountID string) string
}

// SetConfigAssistant wires the chat entrypoint. Nil — the default — leaves
// /config unrecognised, which is the pre-feature behaviour.
func (b *Bot) SetConfigAssistant(a ConfigAssistant) { b.configAssistant = a }

// isConfigCommand matches /config, including Telegram's /config@botname form
// and whatever decoration a composer wrapped it in.
//
// It reuses isLinkCommand's shape rather than its own: the leading slash is
// KEPT, because on Telegram a bare word is not a command and relaxing that
// would make every message beginning "config …" a redemption attempt — the
// prompt-swallowing §5.2 calls the worse failure, and the exact residual
// Telegram's command model lets this entrypoint avoid.
func isConfigCommand(word string) bool { return isSlashCommand(word, "/config") }

// handleConfigCommand serves /config <request>.
//
// Returns the reply to send. An empty string means it has already answered (or
// detached), matching this package's dispatch convention.
// The request context is deliberately NOT taken: every decision here is
// synchronous and local, and the engine run is detached with its own deadline
// precisely so it survives the caller returning. Threading the caller's
// context into the detached run would cancel the assistant the moment the
// dispatch loop moved on, which is the bug this shape exists to avoid.
func (b *Bot) handleConfigCommand(_ context.Context, chatID, userID int64, parts []string) string {
	if b.configAssistant == nil {
		return "The configuration assistant isn't enabled on this deployment."
	}
	intent := strings.TrimSpace(strings.Join(parts[1:], " "))
	if intent == "" {
		return "Say what you want changed — for example:\n/config raise the retry budget for the nightly digest"
	}

	// Authorisation: a RESOLVED identity, never the allowlist. See the Slack
	// twin and identity design §5.3 — the shim is a grace period for surfaces
	// that read and ask, and this one edits the deployment.
	// BOUNDED, for the reason the Slack twin records: the refusal below handles
	// a resolver that FAILS and does nothing for one that HANGS, and a
	// partitioned database hangs where a stopped one fails fast
	// (review-20260915-8717 F3). Telegram has no three-second deadline, but a
	// handler blocked forever is its own failure and the fix is the same.
	authCtx, cancelAuth := context.WithTimeout(context.Background(), authorizeBudget)
	defer cancelAuth()
	d := b.authorizeCtx(authCtx, userID)
	switch {
	case d.ResolverUnavailable:
		// Design test 23: refuse to serve rather than fall back, and say which
		// of the two reasons it is. They have different remedies, and telling
		// someone to link while the identity store is unreachable sends them
		// to do something that cannot work.
		return "I can't verify who you are right now — the identity service isn't answering. " +
			"This isn't about your account, and linking again won't help. Try shortly, or tell an operator."
	case d.Principal == nil:
		return "The configuration assistant needs your Telegram account linked to a Vornik account. " +
			"Generate a code on the \"My account\" page and send it here with /link <code>."
	}

	// Project scope: refused, never guessed. §6.3.3 — a config change proposed
	// against a project the person did not mean is the failure the class
	// ceiling exists to bound, and guessing would reintroduce it underneath
	// the ceiling. Including when exactly one project exists, which is the
	// only case where guessing is tempting.
	projectID := b.getActiveProject(chatID)
	if projectID == "" {
		return "Pick a project first with /project — I won't guess which one you mean for a " +
			"change to the deployment."
	}

	// One in-flight run per sender (review-20260915-8717 F5) — see the Slack
	// twin for why a reach-raising surface must not spawn unbounded model
	// loops from one person.
	if !b.beginConfigRun(userID) {
		return "You already have a configuration request running. I'll post that one when it's done."
	}
	b.runConfigAssistant(chatID, userID, projectID, intent, d.Principal.UserID)
	return "Working on it — I'll post what I find in a moment."
}

// authorizeBudget bounds the identity lookup made before the handler answers.
const authorizeBudget = 2 * time.Second

// panicNotifyBudget bounds the notification sent from the panic path.
const panicNotifyBudget = 15 * time.Second

// beginConfigRun claims the single in-flight slot for a sender.
func (b *Bot) beginConfigRun(userID int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.configRuns == nil {
		b.configRuns = map[int64]struct{}{}
	}
	if _, running := b.configRuns[userID]; running {
		return false
	}
	b.configRuns[userID] = struct{}{}
	return true
}

func (b *Bot) endConfigRun(userID int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.configRuns, userID)
}

// runConfigAssistant executes the request detached and posts the outcome.
//
// Telegram imposes no three-second deadline the way Slack does, so this is not
// forced. It is still detached, for the two reasons §6.3.3 gives: one code path
// for both channels is worth more than saving a goroutine on one of them, and a
// person waiting on a silent chat needs the same "working on it" either way.
func (b *Bot) runConfigAssistant(chatID, userID int64, projectID, intent, accountID string) {
	go func() {
		defer b.endConfigRun(userID)
		// A panic on a detached goroutine takes the daemon down: no request to
		// fail, no caller to return to, and this one runs the whole assistant.
		// The person is told, because a chat that goes quiet is
		// indistinguishable from a daemon that died.
		defer func() {
			if r := recover(); r != nil {
				b.logger.Error().Interface("panic", r).
					Str("project_id", projectID).
					Int64("telegram_chat_id", chatID).
					Msg("telegram: config assistant panicked; nothing was applied")
				b.notifyAfterPanic(chatID)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), configAssistTimeout)
		defer cancel()
		reply := b.configAssistant.ProposeFromChat(ctx, projectID, intent, accountID)
		if strings.TrimSpace(reply) == "" {
			reply = "The assistant finished without saying anything, which is a fault on this side. Nothing was applied."
		}
		if err := b.sendMessage(ctx, chatID, reply); err != nil {
			b.logger.Error().Err(err).
				Str("project_id", projectID).
				Msg("telegram: config assistant produced a result that could not be delivered")
		}
	}()
}

// notifyAfterPanic tells the person the run died, under its OWN recover.
//
// The notification runs on the panic path, where the daemon is already in a
// state nobody reasoned about. If THIS call panics too — a nil transport, a
// half-built bot — the re-panic escapes the deferred recover and kills the
// process, which is precisely the outcome the recover exists to prevent. A
// safety net that can tear is not one. Found by its own test, 2026-09-15.
func (b *Bot) notifyAfterPanic(chatID int64) {
	defer func() {
		if r := recover(); r != nil {
			b.logger.Error().Interface("panic", r).
				Msg("telegram: could not tell the person the assistant panicked")
		}
	}()
	// BOUNDED: the inner recover catches a panic, but a HANG in the channel API
	// on the panic path is neither a panic nor bounded, and the person would
	// get nothing while the goroutine lived on (review-20260915-8717 F4).
	ctx, cancel := context.WithTimeout(context.Background(), panicNotifyBudget)
	defer cancel()
	_ = b.sendMessage(ctx, chatID,
		"The assistant hit an internal fault and stopped. Nothing was applied, "+
			"and this is a fault on this side rather than anything about your request.")
}

// configAssistTimeout bounds a detached assistant run. The same order as the
// Slack twin's slashDispatchTimeout: this is one turn of the same feature, and
// a person cannot tell from the outside which channel they asked from.
const configAssistTimeout = 10 * time.Minute
