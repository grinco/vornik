package telegram

import (
	"context"
	"regexp"
	"strconv"
	"testing"

	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/dispatcher"
)

// The operator-profile /link flow keyed its rows on the Telegram CHAT id while
// every profile read asks for the USER id. In a DM they are equal and the flow
// works, which is why this survived — the operator's own usage is DMs. In a
// group they differ, so /link wrote a row under telegram:-1001234 that nothing
// ever reads, Get returned ErrNotFound, the resolver fell back to "the speaker
// is its own canonical id", and the merge the operator was TOLD had succeeded
// had no observable effect.
//
// The canonical speaker id is what Channel.ResolveSpeaker builds
// (internal/telegram/channel.go): "telegram:" + the USER id, parsed with
// ParseInt and handed to IsAllowed(userID). internal/dispatcher's
// maybeInjectOperatorProfile documents the same ("the channel-specific speaker
// id (Telegram user_id, ...)").
//
// The chat-id shape the old code mirrored is a FALLBACK in
// tool_set_reminder.go, used only when the context carries no operator id at
// all — not the canonical form. Mirroring the fallback is the whole bug.
//
// Filed 2026-09-15, fixed 2026-09-17.
func TestLink_OTPIsIssuedUnderTheUserIDNotTheChatID(t *testing.T) {
	const (
		groupChatID int64 = -1001234567
		userID      int64 = 42
	)

	rec := newTelegramRecorder(t)
	bot := linkTestBot(t, rec, &stubLinker{err: authz.ErrLinkCodeInvalid})
	bot.operatorProfiles = &noopProfiles{}
	bot.operatorIdentityLinks = &noopLinks{}

	// /link with no argument mints an OTP keyed on the issuing speaker.
	if err := bot.HandleMessage(context.Background(), &Message{
		ChatID: groupChatID, UserID: userID, Text: "/link",
	}); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	msgs := rec.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("messages = %+v, want 1", msgs)
	}

	code := regexp.MustCompile(`[A-Z0-9]{4}-[A-Z0-9]{4}`).FindString(msgs[0].Text)
	if code == "" {
		t.Fatalf("no OTP in the reply: %q", msgs[0].Text)
	}

	// Claiming from a third speaker reports who ISSUED it. That issuer is the
	// speaker id the rest of the system will look the operator up by.
	issuer, ok, _ := dispatcher.DefaultOperatorLinkOTPStore().Claim("telegram:99", code)
	if !ok {
		t.Fatalf("claim of %q failed", code)
	}

	want := "telegram:" + strconv.FormatInt(userID, 10)
	if issuer != want {
		t.Errorf("OTP issued under %q, want %q — a row keyed on the chat id is "+
			"read by nothing, so the link silently does not exist", issuer, want)
	}

	// Pin the invariant rather than the constant: the id /link uses must be
	// the id the channel's own resolver produces for the same speaker.
	ch := &Channel{bot: bot}
	sp, err := ch.ResolveSpeaker(context.Background(), strconv.FormatInt(userID, 10))
	if err != nil {
		t.Fatalf("ResolveSpeaker: %v", err)
	}
	if issuer != sp.ID {
		t.Errorf("/link issued under %q but the resolver produces %q", issuer, sp.ID)
	}
}
