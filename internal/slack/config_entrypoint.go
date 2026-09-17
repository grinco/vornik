package slack

import (
	"context"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/chatauth"
	"vornik.io/vornik/internal/conversation"
)

// The configuration assistant's CHAT ENTRYPOINT on Slack — config-assistant
// design §6.3.3, plan §9 (WP8).
//
// Slack forwards one slash command's whole text to the dispatcher, so this is
// a keyword prefix on that command rather than a command of its own:
// `<slack.slash_command> config <request>`. §6.3.3 records why the prefix's
// prompt-swallowing residual is accepted here and refused for `link`: the two
// point in opposite directions. Swallowing a redemption spends a one-time
// credential and loses the question; swallowing a prompt into this entrypoint
// files a proposal that never auto-applies, and the person can discard it.
//
// THIS ENTRYPOINT RAISES ITS CALLER'S REACH, which is the whole reason it is
// gated the way it is. A chat sender cannot edit the config tree; through here
// they can propose changes to it. So it carries classes A and B2 only
// (enforced in the engine, GateEntrypointCeiling) and authorises on a RESOLVED
// IDENTITY — never on the legacy allowlist, even during the migration window
// when that list still grants ordinary chat.

// ConfigAssistant is the engine as this package needs it. Declared here rather
// than imported as a concrete type so the channel does not depend on how the
// assistant is built — the same reason AccountLinker is an interface.
type ConfigAssistant interface {
	// ProposeFromChat runs one assistant request and returns the message to
	// send back. It never returns a proposal the caller must render: the
	// engine owns the vocabulary of refusals and outcomes, and a second
	// renderer here would drift from the console's.
	ProposeFromChat(ctx context.Context, projectID, intent, principal string) string
}

// SetConfigAssistant wires the chat entrypoint. Nil — the default — leaves
// `config …` meaning nothing special, i.e. an ordinary prompt.
func (c *Channel) SetConfigAssistant(a ConfigAssistant) { c.configAssistant = a }

// configRequestFromSlashText returns the request when the slash text opens the
// configuration assistant, and "" when it is an ordinary prompt.
//
// The keyword is matched through chatauth.KeywordLetters for the reason §5.2
// of the identity design records: a decorated keyword that fails to match does
// not fail safely, it dispatches the text onward. Unlike `link <code>` there is
// no field-count rule to lean on — the request is free text — so the
// normalisation is the whole recogniser, and near-misses must still miss:
// "configure" reduces to "configure", not "config".
func configRequestFromSlashText(text string) string {
	fields := strings.Fields(text)
	if len(fields) < 2 {
		// One field or none: either not this entrypoint at all, or the bare
		// keyword with no request. The bare-keyword case is answered by the
		// caller, which can tell a person what to type; returning "" here
		// would send "config" to a model instead.
		return ""
	}
	if chatauth.KeywordLetters(fields[0]) != "config" {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), fields[0]))
}

// isBareConfigKeyword reports whether the text is the keyword alone, which
// earns an explanation rather than a model call.
func isBareConfigKeyword(text string) bool {
	fields := strings.Fields(text)
	return len(fields) == 1 && chatauth.KeywordLetters(fields[0]) == "config"
}

// tryConfigAssistant answers a configuration-assistant request, reporting
// whether it answered. false means "not for this entrypoint" and the caller
// dispatches the text as a prompt.
//
// It runs AFTER the ordinary allowlist gate, and applies a STRICTER rule than
// that gate did. `slashCommandActorAllowed` uses the shim's OR-matrix, so a
// sender admitted only by the legacy allowlist passes it. Such a sender must
// not reach this entrypoint: the shim is a grace period for people who have
// not linked yet, on surfaces that read and ask, and this surface edits the
// deployment (identity design §5.3). The discriminator is the one the shim's
// own contract names — a Decision is IDENTIFIED when it carries a Principal,
// not merely when it is Allowed.
func (c *Channel) tryConfigAssistant(w http.ResponseWriter, inst *installation, userID, text string) bool {
	if c.configAssistant == nil {
		return false
	}
	if isBareConfigKeyword(text) {
		ephemeral(w, "Say what you want changed — for example: `config raise the retry budget for the nightly digest`.")
		return true
	}
	intent := configRequestFromSlashText(text)
	if intent == "" {
		return false
	}

	// Authorisation: a RESOLVED identity, never the allowlist.
	//
	// BOUNDED, because this runs before the acknowledgement and Slack allows
	// three seconds. The ResolverUnavailable refusal below handles a resolver
	// that FAILS; without a deadline it does nothing for one that HANGS, and a
	// partitioned database hangs where a stopped one fails fast — so the
	// outage net had its hole exactly where outages live
	// (review-20260915-8717 F3). The deadline turns a hang into the same
	// prompt refusal an error already produced.
	authCtx, cancelAuth := context.WithTimeout(context.Background(), authorizeBudget)
	defer cancelAuth()
	d := c.authorizeCtx(authCtx, userID, c.legacyForInstallation(inst, userID))
	switch {
	case d.ResolverUnavailable:
		// Test 23. Refuses to serve rather than falling back, and says which
		// of the two reasons it is, because they have different remedies.
		ephemeral(w, "Can't verify who you are right now — the identity service isn't answering. "+
			"This isn't about your account, and linking again won't help. Try shortly, or tell an operator.")
		return true
	case d.Principal == nil:
		// Either unlinked, or admitted only by the legacy allowlist. Both get
		// the same answer, and the answer is the remedy: link.
		ephemeral(w, "The configuration assistant needs your Slack account linked to a Vornik account. "+
			"Generate a code on the \"My account\" page and redeem it here with `link <code>`.")
		return true
	}

	projectID := strings.TrimSpace(inst.projectID)
	if projectID == "" {
		// The symmetric case to Telegram's missing active project. The remedy
		// names a config key rather than a chat command, because only an
		// operator can fix it and no amount of typing here will.
		ephemeral(w, "This Slack workspace isn't bound to a project, so there's nothing to configure. "+
			"An operator sets `project` on the Slack installation.")
		return true
	}

	// Acknowledge INSIDE the three seconds Slack allows, then run the engine
	// on the delivery goroutine (§6.3.3). A synchronous call here works
	// against a stub and times out against every real request.
	// One in-flight run per sender (review-20260915-8717 F5). A reach-raising
	// surface that spawns an unbounded number of ten-minute model loops from
	// one person is a blast radius the ordinary read-and-ask dispatch does not
	// have, and two identical requests racing in the filing path produce two
	// proposals for one intent.
	if !c.beginConfigRun(userID) {
		ephemeral(w, "You already have a configuration request running. I'll post that one when it's done.")
		return true
	}
	ephemeral(w, "Working on it — I'll post what I find in a moment.")
	c.runConfigAssistant(projectID, intent, d.Principal.UserID, userID)
	return true
}

// authorizeBudget bounds the pre-acknowledgement identity lookup. Well inside
// Slack's three seconds, so a hanging resolver still leaves room to answer.
const authorizeBudget = 2 * time.Second

// beginConfigRun claims the single in-flight slot for a sender, reporting
// whether it got it. endConfigRun releases it.
func (c *Channel) beginConfigRun(userID string) bool {
	c.configRunsMu.Lock()
	defer c.configRunsMu.Unlock()
	if c.configRuns == nil {
		c.configRuns = map[string]struct{}{}
	}
	if _, running := c.configRuns[userID]; running {
		return false
	}
	c.configRuns[userID] = struct{}{}
	return true
}

func (c *Channel) endConfigRun(userID string) {
	c.configRunsMu.Lock()
	defer c.configRunsMu.Unlock()
	delete(c.configRuns, userID)
}

// runConfigAssistant executes the request off the HTTP request's lifetime and
// posts the outcome back to the channel.
//
// Detached deliberately, and bounded: `context.WithoutCancel` keeps the trace
// ids and logger while dropping the three-second deadline Slack imposes on the
// acknowledgement, and slashDispatchTimeout is the same bound the ordinary
// slash dispatch already runs under — this is the same kind of turn, and a
// person cannot tell from the outside which one they asked for.
func (c *Channel) runConfigAssistant(projectID, intent, accountID, userID string) {
	sessionID := configAssistSessionID(userID)
	go func() {
		defer c.endConfigRun(userID)
		// A panic on a DETACHED goroutine takes the daemon down — there is no
		// request to fail and no caller to return it to. This one runs the
		// whole assistant: a model call, tool dispatch, the judge, the apply
		// journal. The blast radius of a bug anywhere in that path must not be
		// the process, and it is not enough to say the path is tested, because
		// the reason to detach in the first place is that it is slow and
		// external.
		//
		// The person is told, because a chat that goes quiet is
		// indistinguishable from a daemon that died, and this recovery makes
		// the two look the same from the outside unless it says otherwise.
		defer func() {
			if r := recover(); r != nil {
				c.logger.Error().Interface("panic", r).
					Str("project_id", projectID).
					Str("slack_user_id", userID).
					Msg("slack: config assistant panicked; nothing was applied")
				c.notifyAfterPanic(sessionID, userID)
			}
		}()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), slashDispatchTimeout)
		defer cancel()
		reply := c.configAssistant.ProposeFromChat(ctx, projectID, intent, accountID)
		if strings.TrimSpace(reply) == "" {
			// The engine always has something to say — an outcome or a
			// refusal. An empty string means a path returned nothing, and
			// silence in a chat is indistinguishable from the daemon being
			// dead, so say that rather than nothing.
			reply = "The assistant finished without saying anything, which is a fault on this side. Nothing was applied."
		}
		c.sendConfigAssistReply(ctx, sessionID, userID, reply)
	}()
}

// notifyAfterPanic tells the person the run died, under its OWN recover.
//
// The notification runs on the panic path, where the channel is already in a
// state nobody reasoned about. If THIS call panics too, the re-panic escapes
// the deferred recover and kills the daemon — the outcome the recover exists
// to prevent. A safety net that can tear is not one.
func (c *Channel) notifyAfterPanic(sessionID, userID string) {
	defer func() {
		if r := recover(); r != nil {
			c.logger.Error().Interface("panic", r).
				Msg("slack: could not tell the person the assistant panicked")
		}
	}()
	// BOUNDED. The inner recover catches a panic; a HANG in the channel API on
	// the panic path is neither a panic nor bounded, and the goroutine would
	// live until the transport gave up while the person got nothing — the
	// "chat goes quiet" outcome this notification exists to prevent
	// (review-20260915-8717 F4).
	ctx, cancel := context.WithTimeout(context.Background(), panicNotifyBudget)
	defer cancel()
	c.sendConfigAssistReply(ctx, sessionID, userID,
		"The assistant hit an internal fault and stopped. Nothing was applied, "+
			"and this is a fault on this side rather than anything about your request.")
}

// panicNotifyBudget bounds the notification sent from the panic path.
const panicNotifyBudget = 15 * time.Second

// sendConfigAssistReply delivers one outcome message, logging rather than
// returning a delivery failure: nobody is waiting on the return, and the
// alternative to logging is losing it.
func (c *Channel) sendConfigAssistReply(ctx context.Context, sessionID, userID, text string) {
	now := time.Now()
	if c.clock != nil {
		now = c.clock()
	}
	if _, err := c.Send(ctx, conversation.ChannelMessage{
		Source:    channelName,
		SessionID: sessionID,
		ThreadID:  "slash:" + userID,
		Text:      text,
		Timestamp: now,
	}); err != nil {
		c.logger.Error().Err(err).
			Str("slack_user_id", userID).
			Msg("slack: config assistant produced a result that could not be delivered")
	}
}

// configAssistSessionID keys the reply to the same per-user slash session the
// ordinary dispatch uses, so the answer lands where the person asked.
func configAssistSessionID(userID string) string { return "config-assist:" + userID }
