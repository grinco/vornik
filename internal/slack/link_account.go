package slack

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/chatauth"
	"vornik.io/vornik/internal/persistence"
)

// The §5.2 redemption half on Slack (oidc-identity-permissions-design
// §5.0/§5.2/§5.5).
//
// The design says a link code is "redeemed on any chat channel", and Telegram
// got the handler first because it already had a `/link` command to collide
// with. Slack has no per-command branch — one configured slash command whose
// whole text goes to the dispatcher — so redemption here is a prefix on that
// command rather than a command of its own: `/vornik link <code>`.
//
// Leaving it Telegram-only would have made the design's sentence false on
// half the channels it names, which is worse than an absent feature: it stops
// the next person looking.

// AccountLinker redeems a §5.2 link code. Satisfied by *authz.Accounts. The
// Telegram package declares the same shape for the same reason — neither
// channel should depend on how the account service is built.
type AccountLinker interface {
	RedeemLinkCode(ctx context.Context, code, channel, externalID, display string) (*persistence.User, error)
}

// SetAccountLinker wires the §5.2 redemption path. Nil — the default — leaves
// `link` meaning nothing special, i.e. an ordinary prompt.
func (c *Channel) SetAccountLinker(l AccountLinker) {
	c.accountLinker = l
	if c.redemptionLimiter == nil {
		c.redemptionLimiter = chatauth.NewRedemptionLimiter()
	}
}

// linkCodeFromSlashText returns the code when the slash text is a redemption
// and "" when it is an ordinary prompt.
//
// Deliberately strict: exactly "link <code>" with one argument. A prompt that
// merely BEGINS with the word link ("link the two designs and summarise") must
// reach the dispatcher, because silently swallowing prompts into an
// authorization command is a far worse failure than a missed shortcut.
//
// Keyword normalisation was added 2026-09-15, and the incident is worth
// stating because the visible half was not the dangerous half. The "My
// account" panel rendered its instruction inside a <code> element; pasting it
// into Slack kept the formatting. When only the CODE carried it
// ("link `ACDE2345`") the redemption merely failed, which the operator saw and
// worked around. When the WHOLE command did ("`link ACDE2345`"), fields[0] was
// "`link", the recogniser declined, and the text went to the dispatcher —
// putting a live one-time credential into a model prompt and a conversation
// transcript. Nothing reported that; it looked like an ordinary unanswered
// question.
//
// WHAT THIS DOES AND DOES NOT FIX, stated because the first version of this
// comment narrated the leak and then presented a character trim as the answer
// to it (review-20260915-3d5b, finding 1). Normalising the keyword ends the
// class of decorated-COMMAND misses. It does not, by itself, instrument the
// fall-through boundary — see noteUnrecognisedLinkAttempt, which does, for the
// part of that boundary that can be instrumented without disclosure.
//
// The review made TWO proposals and the first version of this comment answered
// only the stronger one, which made the refusal look broader than it was
// (review-20260915-beff F1 — the same claims-more-than-it-performs shape, this
// time in the prose rather than the code):
//
//   - Short-circuit any prompt carrying a code-SHAPED token. DECLINED, and the
//     reason holds: deciding whether a token is a live code means asking the
//     code store about arbitrary input, which is the redemption oracle §5.2
//     exists to deny, and the heuristic alternative (short [A-Z0-9] tokens)
//     swallows ordinary prompts — "summarise ACDE2345" is two fields and eight
//     alphanumerics.
//   - Log the ATTEMPT, keyed on the keyword the recogniser already computed.
//     This queries nothing and discloses nothing. It is BUILT, below.
//
// What remains open after both: a code pasted bare, or introduced by a word
// that is not "link", is indistinguishable from an ordinary prompt without
// consulting the store, so it still reaches the dispatcher unremarked. That
// residual is real, is bounded by the same disclosure rule, and is recorded in
// §5.2 as needing a design — a one-way marker on issued codes, or moving
// redemption off the shared slash command — rather than a heuristic.
//
// The normalisation widens what counts as a redemption, stated rather than
// left to be discovered: "*link* foo" and "link, foo" now redeem where they
// previously reached the dispatcher. §5.2 already accepts this class for a
// bare two-word "link <token>" prompt — though not identically, since that one
// is deliberately typed and these can arise from prose that merely got
// formatted. The exactly-two-fields rule is what bounds the blast radius, and
// it is unchanged.
func linkCodeFromSlashText(text string) string {
	fields := strings.Fields(text)
	if len(fields) != 2 {
		return ""
	}
	if chatauth.KeywordLetters(fields[0]) != "link" {
		return ""
	}
	// Returned as typed. This layer decides WHETHER the store is consulted;
	// authz.hashLinkCode decides what the code means, and it is the
	// authoritative normalisation — a second, different filter here would be
	// the two-implementations shape, and the one that drifts is the one
	// nobody exercises.
	return fields[1]
}

// noteUnrecognisedLinkAttempt logs a text that names the redemption keyword but
// does not have the shape of a redemption, and is therefore about to be
// dispatched to a model.
//
// This is the review's MINIMUM proposal (review-20260915-3d5b finding 1), and
// it is not the oracle its sibling proposal would have been: it keys on the
// keyword this package already computed and never consults the code store, so
// it answers nothing about whether any token is a live code. It changes no
// behaviour — the text still dispatches — it only stops the boundary being
// silent.
//
// The realistic population after the keyword normalisation is a wrong FIELD
// COUNT: "link ACDE2345 please" or a code split across a line break. Those are
// a person attempting to link, whose code is now going to a model.
//
// The text itself is NEVER logged, and neither is any field of it. A log line
// is the second place a one-time credential escapes to after a URL, and the
// whole point of this path is that the text may contain one. Field count and
// the speaker are enough to tell an operator what happened and to whom.
func (c *Channel) noteUnrecognisedLinkAttempt(userID, text string) {
	fields := strings.Fields(text)
	if len(fields) == 0 || chatauth.KeywordLetters(fields[0]) != "link" {
		return
	}
	c.logger.Warn().
		Str("slack_user_id", userID).
		Int("fields", len(fields)).
		Msg("slack: text names the link keyword but is not a redemption; dispatching it as a prompt. " +
			"If it carried a link code, that code has reached the model — issue a fresh one.")
}

// tryAccountLinkCode redeems and writes the ephemeral reply, reporting
// whether it answered. false means "not a redemption" and the caller
// dispatches the text as a prompt.
//
// The identity is keyed by the Slack USER id, which is what
// resolveSpeakerForInstallation and the resolver both use — the two agree by
// construction here rather than by coincidence.
func (c *Channel) tryAccountLinkCode(ctx context.Context, w http.ResponseWriter, userID, text string) bool {
	if c.accountLinker == nil {
		return false
	}
	code := linkCodeFromSlashText(text)
	if code == "" {
		// Declined — the text is about to be dispatched to a model. Say so if
		// it looked like someone trying to link.
		c.noteUnrecognisedLinkAttempt(userID, text)
		return false
	}
	// Reachable before the allowlist gate (§5.1's exception), so bounded —
	// see the Telegram twin and chatauth.RedemptionLimiter.
	if !c.redemptionLimiter.Allow("slack", userID) {
		ephemeral(w, "Too many link attempts. Wait a few minutes and try again — your code has not been used.")
		return true
	}

	user, err := c.accountLinker.RedeemLinkCode(ctx, code, "slack", userID, userID)
	switch {
	case errors.Is(err, authz.ErrIdentityAlreadyLinked):
		// See the Telegram twin: about the speaker, not the code.
		ephemeral(w, "This Slack identity is already linked to another account. Unlink it from "+
			"that account's \"My account\" page first, then redeem this code — it has not been used.")
	case errors.Is(err, authz.ErrLinkCodeInvalid):
		ephemeral(w, "That code isn't valid (expired, mistyped, or already used). "+
			"Generate a new one from the \"My account\" page.")
	case errors.Is(err, authz.ErrLinkCodesUnavailable):
		ephemeral(w, "Account linking isn't configured on this deployment.")
	case err != nil:
		c.logger.Error().Err(err).Str("user_id", userID).
			Msg("slack: account link-code redemption failed")
		ephemeral(w, "Couldn't complete the link just now — that's a fault on this side, not a bad code. "+
			"Your code has not been used; try again in a moment.")
	default:
		ephemeral(w, "Linked to "+user.DisplayName+".\n\n"+
			"This Slack identity now authorizes through that account: its role and project access apply here, "+
			"and disabling the account removes access from Slack too.")
	}
	return true
}

// ephemeral writes a Slack slash-command reply visible only to the invoker.
// A link confirmation names an account and must not be posted to a channel.
func ephemeral(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"response_type": "ephemeral",
		"text":          text,
	})
}

// SlashCommand is the command this channel answers, already normalised. The
// UI panel names it when telling a person how to redeem a link code; it is
// configurable per deployment, so the panel must not hardcode one.
func (c *Channel) SlashCommand() string { return NormaliseSlashCommand(c.cfg.SlashCommand) }
