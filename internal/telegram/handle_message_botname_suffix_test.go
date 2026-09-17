// Telegram appends @botname to a command when several bots share a group
// chat, so a group delivers "/status@vornikbot" where a DM delivers
// "/status". The command router compared the raw first word, so every
// command in the deterministic ops cluster fell through to the dispatcher and
// was answered as free text — a model call, and a paid one, for what is a
// zero-spend ops command.
//
// Filed 2026-09-15 while hoisting link-code redemption above the
// authorization gate. /link and /config normalised the suffix themselves
// because a miss there is unrecoverable; the general case was left, and is
// this. Invisible until now because the operator's own usage is DMs, where
// the suffix never appears.

package telegram

import (
	"strings"
	"testing"
)

// The ops cluster is where this costs money: each of these is a deterministic
// handler that answers without a model.
func TestHandleMessage_BotnameSuffixReachesTheSameHandler(t *testing.T) {
	cases := []struct {
		command string
		want    string
	}{
		{"/help", "Available commands"},
		{"/context", "Session context"},
		{"/tasks", ""},
		{"/status", ""},
	}
	for _, tc := range cases {
		t.Run(tc.command, func(t *testing.T) {
			b, rec := newBotWithRecorder(t, BotConfig{
				Token:        "tok",
				AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
			})
			plain := runCommand(t, b, rec, 100, 42, tc.command)

			b2, rec2 := newBotWithRecorder(t, BotConfig{
				Token:        "tok",
				AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
			})
			suffixed := runCommand(t, b2, rec2, 100, 42, tc.command+"@vornikbot")

			if len(plain) == 0 || len(suffixed) == 0 {
				t.Fatalf("expected a reply to both forms: plain=%d suffixed=%d", len(plain), len(suffixed))
			}
			if plain[0].Text != suffixed[0].Text {
				t.Errorf("group form answered differently:\n plain:    %q\n suffixed: %q",
					plain[0].Text, suffixed[0].Text)
			}
			if tc.want != "" && !strings.Contains(suffixed[0].Text, tc.want) {
				t.Errorf("suffixed %s did not reach its handler: %q", tc.command, suffixed[0].Text)
			}
		})
	}
}

// An argument-taking command keeps its arguments: the suffix is stripped from
// the command WORD only, and parts[1:] must be untouched.
func TestHandleMessage_BotnameSuffixKeepsArguments(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	snap := runCommand(t, b, rec, 100, 42, "/project@vornikbot no-such-project")
	if len(snap) == 0 {
		t.Fatal("expected a reply")
	}
	if !strings.Contains(snap[0].Text, "no-such-project") {
		t.Errorf("the argument was lost or the command did not dispatch: %q", snap[0].Text)
	}
}

// The recogniser must not turn arbitrary slash-prefixed text into a command.
// A path is the shape most likely to arrive by accident.
func TestHandleMessage_SlashPrefixedNonCommandIsNotDispatched(t *testing.T) {
	for _, text := range []string{"/var/log/vornik", "/statusless", "/status-report"} {
		if normalizeCommandWord(text) == "/status" || normalizeCommandWord(text) == "/help" {
			t.Errorf("%q must not normalise onto a real command (got %q)", text, normalizeCommandWord(text))
		}
	}
}

// The router now normalises before the comparisons, and /link and /config
// normalise again inside their own recognisers. That is only safe if the rule
// is idempotent, so assert it rather than assume it.
func TestNormalizeCommandWord_IsIdempotent(t *testing.T) {
	for _, in := range []string{
		"/status", "/status@vornikbot", "*/status*", "/link@bot", "/var/log/x", "", "@", "/",
	} {
		once := normalizeCommandWord(in)
		if twice := normalizeCommandWord(once); twice != once {
			t.Errorf("normalizeCommandWord(%q): %q then %q", in, once, twice)
		}
	}
}
