package telegram

// Telegram-side handler for the `/link` slash command — closes
// the cross-channel-linking flow opened by Phase A's CLI
// `vornikctl operator link`. Without leaving chat, an operator
// can issue a code on one channel + claim it from another so
// their profiles consolidate.
//
// Flow:
//
//   chat A:  /link
//   bot   :  Code: ABCD-EFGH. From the other channel send
//            `/link ABCD-EFGH` within 5 minutes.
//
//   chat B:  /link ABCD-EFGH
//   bot   :  Linked! Both channels now share the same profile
//            (X keys + Y notes merged from telegram:42).
//
// The dispatch site is the message handler in handlers.go; this
// file owns only the per-command logic + the speaker→canonical
// formatting.

import (
	"context"
	"fmt"
	"strconv"

	"vornik.io/vornik/internal/dispatcher"
)

// handleLinkCommand routes /link with or without an OTP arg.
// Returns the b.sendMessage error so the dispatch site can
// surface delivery failures uniformly with the other slash
// commands.
func (b *Bot) handleLinkCommand(ctx context.Context, chatID int64, parts []string) error {
	if b.operatorProfiles == nil || b.operatorIdentityLinks == nil {
		return b.sendMessage(ctx, chatID,
			"/link is not configured on this deployment (operator-profile repositories not wired). "+
				"Run `vornikctl operator link <canonical> <speaker>` from the host instead.")
	}
	speaker := telegramSpeakerID(chatID)
	if len(parts) < 2 {
		return b.handleLinkIssue(ctx, chatID, speaker)
	}
	return b.handleLinkClaim(ctx, chatID, speaker, parts[1])
}

// telegramSpeakerID formats a Telegram chat id into the
// canonical speaker shape (`telegram:<chat_id>`). Mirrors the
// shape internal/dispatcher/tool_set_reminder.go uses, so a
// link issued here resolves through the same resolver chain.
func telegramSpeakerID(chatID int64) string {
	return "telegram:" + strconv.FormatInt(chatID, 10)
}

// handleLinkIssue runs the /link-with-no-arg path: mint an OTP,
// tell the operator what to type on the other channel.
func (b *Bot) handleLinkIssue(ctx context.Context, chatID int64, speaker string) error {
	store := dispatcher.DefaultOperatorLinkOTPStore()
	code := store.Issue(speaker)
	msg := fmt.Sprintf(
		"Link code: %s\n\n"+
			"From the other channel (Telegram chat, webchat, etc.) send `/link %s` within 5 minutes to consolidate the two profiles. "+
			"The side with more accumulated content becomes the canonical row; the other side merges in.",
		code, code,
	)
	return b.sendMessage(ctx, chatID, msg)
}

// handleLinkClaim runs the /link CODE path. Resolves the code,
// performs the link + merge, and reports the outcome.
func (b *Bot) handleLinkClaim(ctx context.Context, chatID int64, speaker, code string) error {
	store := dispatcher.DefaultOperatorLinkOTPStore()
	issuer, ok, locked := store.Claim(speaker, code)
	if locked {
		return b.sendMessage(ctx, chatID,
			"Too many incorrect link codes. For your security, linking is paused for a few minutes — wait, then run `/link` again from the issuing channel to get a fresh code.")
	}
	if !ok {
		return b.sendMessage(ctx, chatID,
			"That code isn't valid (expired, mistyped, or already claimed). Run `/link` again from the issuing channel to get a fresh code.")
	}
	if issuer == speaker {
		return b.sendMessage(ctx, chatID,
			"That code was issued on this same channel — there's nothing to link. Run `/link` from the OTHER channel you want to consolidate.")
	}
	result, err := dispatcher.PerformOperatorLink(ctx, dispatcher.OperatorLinkRepos{
		Profiles: b.operatorProfiles,
		Links:    b.operatorIdentityLinks,
		Audit:    b.profileUseAudit,
	}, issuer, speaker, "self")
	if err != nil {
		b.logger.Error().Err(err).
			Str("issuer", issuer).
			Str("claimant", speaker).
			Msg("telegram /link: PerformOperatorLink failed")
		return b.sendMessage(ctx, chatID, fmt.Sprintf("Link failed: %v", err))
	}

	var summary string
	switch {
	case result.Merged && (len(result.MergedKeys) > 0 || result.MergedNotes):
		bits := []string{}
		if len(result.MergedKeys) > 0 {
			bits = append(bits, fmt.Sprintf("%d preference key(s)", len(result.MergedKeys)))
		}
		if result.MergedNotes {
			bits = append(bits, "free-form notes")
		}
		summary = fmt.Sprintf(
			"Linked! Canonical profile is now %q.\nMerged in: %s.\n%d existing link row(s) repointed.",
			result.Canonical, joinHumanList(bits), result.LinksMoved,
		)
	case result.LinksMoved > 0:
		summary = fmt.Sprintf(
			"Linked! Canonical profile is now %q. %d existing link row(s) repointed (no profile content to merge).",
			result.Canonical, result.LinksMoved,
		)
	default:
		summary = fmt.Sprintf(
			"Linked! Both channels now resolve to %q.",
			result.Canonical,
		)
	}
	return b.sendMessage(ctx, chatID, summary)
}

// joinHumanList renders ["a", "b", "c"] as "a, b, c" with an
// "and" before the last entry — keeps the chat reply readable
// without dragging in a templating library.
func joinHumanList(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	}
	out := ""
	for i, s := range items {
		switch {
		case i == 0:
			out = s
		case i == len(items)-1:
			out += ", and " + s
		default:
			out += ", " + s
		}
	}
	return out
}
