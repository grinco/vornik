package telegram

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/chatauth"
	"vornik.io/vornik/internal/persistence"
)

// The account half of `/link` (oidc-identity-permissions-design §5.0/§5.2).
//
// §5.0 settled a collision rather than inventing a command: `/link` already
// existed and meant the chat→chat operator-profile OTP. Round-1 finding F2
// established that the two flows cannot merge — different issuers, different
// stores, different things written — so the design specified ONE command with
// TWO code types, "disambiguated at redemption by looking up the hashed code
// in link_codes first and the in-memory OTP store second; a code matches at
// most one, so the operator types one command and never chooses a mode."
//
// Until this file, only the second half of that lookup existed. A code issued
// from the "My account" panel could be typed into chat and would be answered
// "that code isn't valid" — the issue surfaces were reachable and the
// redemption surface was not, which made the whole link-code feature a
// one-way street.

// AccountLinker redeems a §5.2 link code. Satisfied by *authz.Accounts; an
// interface so the bot does not depend on the account service's construction,
// and so the fall-through order is testable without a database.
type AccountLinker interface {
	RedeemLinkCode(ctx context.Context, code, channel, externalID, display string) (*persistence.User, error)
}

// SetAccountLinker wires the §5.2 redemption path. Nil — the default — leaves
// `/link <code>` meaning exactly what it meant before Phase 4.
func (b *Bot) SetAccountLinker(l AccountLinker) {
	b.accountLinker = l
	if b.redemptionLimiter == nil {
		b.redemptionLimiter = chatauth.NewRedemptionLimiter()
	}
}

// tryAccountLinkCode attempts the §5.2 redemption and reports whether it
// answered. false means "not an account link code" — including when no
// account service is wired — and the caller falls through to the OTP store.
//
// The identity is keyed by the Telegram USER id, never the chat id. They are
// equal in a DM and differ in a group, and the resolver looks up the user id
// (middleware.authorize), so binding a chat id would write a row that can
// never authorize anyone and would silently do nothing for group senders.
func (b *Bot) tryAccountLinkCode(ctx context.Context, chatID, userID int64, code string) (bool, error) {
	if b.accountLinker == nil {
		return false, nil
	}
	externalID := strconv.FormatInt(userID, 10)
	// Redemption is reachable BEFORE authorization (§5.1's exception), so it
	// is a surface anyone who can message the bot can reach. The code space
	// makes a blind search hopeless, but that is an arithmetic argument and
	// this is a mechanism — and the OTP flow it shares a command with has
	// carried a lockout all along.
	if !b.redemptionLimiter.Allow("telegram", externalID) {
		return true, b.sendMessage(ctx, chatID,
			"Too many link attempts. Wait a few minutes and try again — your code has not been used.")
	}
	user, err := b.accountLinker.RedeemLinkCode(ctx, code, "telegram", externalID, externalID)
	switch {
	case errors.Is(err, authz.ErrIdentityAlreadyLinked):
		// Distinct from the code refusals on purpose: this is about the
		// speaker's own binding, which they can already see, and the remedy
		// is unreachable if we tell them the code is bad.
		return true, b.sendMessage(ctx, chatID,
			"This chat is already linked to another account. Unlink it from that account's "+
				"\"My account\" page first, then redeem this code — it has not been used.")
	case errors.Is(err, authz.ErrLinkCodeInvalid):
		// Not an account code — or not a good one. Either way §5.0 says try
		// the OTP store next; a code matches at most one of the two.
		return false, nil
	case errors.Is(err, authz.ErrLinkCodesUnavailable):
		// The store is not configured. Not a refusal, so do not report one.
		return false, nil
	case err != nil:
		// A real fault, which redemption stopped flattening into "invalid"
		// precisely so it could be reported here rather than sending the
		// operator to ask for another code during an outage.
		b.logger.Error().Err(err).
			Int64("user_id", userID).
			Msg("account link-code redemption failed")
		return true, b.sendMessage(ctx, chatID,
			"Couldn't complete the link just now — that's a fault on this side, not a bad code. "+
				"Your code has not been used; try again in a moment.")
	}

	// The code is spent and the binding is written. Say which account, so a
	// person who holds codes for two accounts can see which one they joined.
	return true, b.sendMessage(ctx, chatID, fmt.Sprintf(
		"Linked to %s.\n\n"+
			"This Telegram identity now authorizes through that account: its role and project access apply here, "+
			"and disabling the account removes access from this chat too.",
		user.DisplayName))
}

// unauthorizedMessage is the refusal an unlinked speaker sees.
//
// §5.7's ledger requires it to name the `/link` remedy. It does so only when
// redemption is actually wired: naming a command that answers "not
// configured" is worse than not naming it, and is the same class of defect as
// a documented behaviour nothing implements.
func (b *Bot) unauthorizedMessage() string {
	const base = "You are not authorized to use this bot."
	if b.accountLinker == nil {
		return base
	}
	return base + "\n\nIf you have a Vornik account, open \"My account\" in the web UI, " +
		"generate a link code, and send `/link <code>` here to bind this chat to it."
}

// isLinkCommand matches `/link`, including Telegram's `/link@botname` form.
//
// Telegram appends @botname when several bots share a group, and this
// package's general command dispatch does not strip it — a pre-existing
// limitation affecting every command equally, filed separately rather than
// changed for all of them overnight. It is handled HERE because this path is
// where the limitation costs the most: a group chat is precisely where the
// chat id and the user id differ, so it is where linking matters, and the
// failure mode is a person who cannot link at all rather than a command that
// merely does nothing.
// It also tolerates surrounding DECORATION, for the reason the Slack twin
// does (review-20260915-3d5b, and §5.2's normalisation note). A command word
// that declines does not merely fail to link — the text carries on to the
// dispatcher, so a message holding a live one-time code reaches a model and a
// transcript with nothing reporting it. Telegram is less exposed than Slack
// here, because it carries formatting as message entities and hands us clean
// text, so this is the same class arriving by a narrower door rather than an
// equally likely repeat. It is closed anyway: the class is what the last round
// of review was about, and leaving the second channel open would make the
// design's claim true of one of the two channels it names.
//
// The trim keeps the LEADING SLASH, which is the part that must not be
// relaxed. Reducing to letters the way Slack's keyword does would make a bare
// "link ACDE2345" a command here, and on Telegram a bare word is not a command
// — that would swallow ordinary messages, which §5.2 calls the worse failure.
// So the cutset is inverted: trim everything that is not part of a command
// word, and leave the command word's own shape to the comparison below.
func isLinkCommand(word string) bool { return isSlashCommand(word, "/link") }

// isSlashCommand is the rule itself, for any command word. One implementation
// rather than one per command: the configuration assistant's /config needs the
// same tolerance and the same strictness (config-assistant design §6.3.3), and
// a second copy is the shape whose drifting half is always the one nobody
// exercises.
//
// Underscore is deliberately NOT in the keep set even though Telegram command
// words may contain one (/link_account). TrimFunc only touches the ENDS, so an
// interior underscore survives; a leading or trailing one is never part of a
// command name and is markdown italics far more often.
func isSlashCommand(word, want string) bool {
	return normalizeCommandWord(word) == want
}

// normalizeCommandWord is the rule itself, lifted out of isSlashCommand so the
// GENERAL command router can share it (handlers.go) rather than grow a second,
// narrower one beside it. Strips surrounding decoration and Telegram's
// @botname suffix, and returns the bare command word.
//
// Extracted 2026-09-17. Until then the rule reached only /link and /config,
// each of which called it explicitly, while every other command compared the
// raw word — so in a group chat where the bot is not alone, Telegram's
// "/status@vornikbot" fell through to the dispatcher and was answered by a
// MODEL. Invisible in a DM, which is where it was always tested. One
// implementation, because a normalisation with two of them has one that is
// wrong and it is never the one being exercised.
func normalizeCommandWord(word string) string {
	word = strings.TrimFunc(word, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '/' && r != '@'
	})
	if at := strings.IndexByte(word, '@'); at >= 0 {
		word = word[:at]
	}
	return word
}
