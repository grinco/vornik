package slack

import (
	"testing"
	"time"
)

// Regression: operator report 2026-07-28. A correspondent kept messaging the
// bot in the main channel, following up on answers the bot had given in earlier
// threads, and the bot had no idea what she meant. Root cause: a top-level
// message keyed its session on its OWN ts, so every channel message opened a
// fresh empty session — she lost not just earlier threads but the message she
// had typed one minute before.
func TestBuildMessageChannelMessage_SessionKeying(t *testing.T) {
	cfg := validConfig()
	ch := makeChannel(t, cfg, time.Unix(1700000000, 0))
	inst := ch.installationsByID["T123"]

	tests := []struct {
		name          string
		ts            string
		threadTs      string
		wantSessionID string
		wantThreadID  string
		wantThreadTS  string
	}{
		{
			name:          "top-level channel message keys on the channel, not its own ts",
			ts:            "1700000010.000100",
			threadTs:      "",
			wantSessionID: "T123/C_general#main",
			wantThreadID:  "",
			wantThreadTS:  "",
		},
		{
			name:          "a second top-level message lands in the SAME session",
			ts:            "1700000999.000100",
			threadTs:      "",
			wantSessionID: "T123/C_general#main",
			wantThreadID:  "",
			wantThreadTS:  "",
		},
		{
			name:          "in-thread message keeps its own thread session",
			ts:            "1700000020.000200",
			threadTs:      "1700000010.000100",
			wantSessionID: "T123/C_general#1700000010.000100",
			wantThreadID:  "1700000010.000100",
			wantThreadTS:  "1700000010.000100",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := eventPayload{
				Type:    "event_callback",
				TeamID:  "T123",
				EventID: "Ev_" + tc.ts,
				Event: &eventInner{
					Type:        "message",
					User:        "U_janka",
					Text:        "what did we decide?",
					Channel:     "C_general",
					Ts:          tc.ts,
					ThreadTs:    tc.threadTs,
					ChannelType: "channel",
				},
			}
			msg := ch.buildMessageChannelMessage(p, inst)

			if msg.SessionID != tc.wantSessionID {
				t.Errorf("SessionID = %q, want %q", msg.SessionID, tc.wantSessionID)
			}
			if msg.ThreadID != tc.wantThreadID {
				t.Errorf("ThreadID = %q, want %q (must not synthesise a thread)", msg.ThreadID, tc.wantThreadID)
			}
			// thread_ts must carry the REAL thread or nothing — voiceTracker keys
			// on it and must not be told a thread exists when it does not.
			if got := msg.ChannelSpecific["thread_ts"]; got != tc.wantThreadTS {
				t.Errorf("ChannelSpecific[thread_ts] = %q, want %q", got, tc.wantThreadTS)
			}
		})
	}
}

// The channel-scoped sentinel must survive the round trip through the outbound
// parser, since SessionID doubles as the reply route.
func TestChannelSessionSentinelRoundTrips(t *testing.T) {
	teamID, channelID, threadRoot, err := parseSlackSessionID("T123/C_general#" + ChannelSessionThreadRoot)
	if err != nil {
		t.Fatalf("parseSlackSessionID: %v", err)
	}
	if teamID != "T123" || channelID != "C_general" {
		t.Errorf("team/channel = %q/%q, want T123/C_general", teamID, channelID)
	}
	if threadRoot != ChannelSessionThreadRoot {
		t.Errorf("threadRoot = %q, want %q", threadRoot, ChannelSessionThreadRoot)
	}
}

// resolveThreadTs decides whether an outbound reply threads or posts at channel
// level. Both non-timestamp sentinels must post at channel level; a real
// timestamp must thread. This is asserted directly because it is shared by the
// text path (sendChatPostMessage) and the voice path (sendVoiceForSession) —
// the voice path also derives ThreadTs from the parsed SessionID and would
// otherwise upload with thread_ts="main".
func TestResolveThreadTs(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{ChannelSessionThreadRoot, ""},
		{"slash:U_alice", ""},
		{"1700000010.000100", "1700000010.000100"},
	}
	for _, tc := range tests {
		if got := resolveThreadTs(tc.in); got != tc.want {
			t.Errorf("resolveThreadTs(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The sentinel must not be a value Slack could ever produce as a ts, or a
// genuine thread would collide with the channel session.
func TestChannelSessionSentinelIsNotATimestampShape(t *testing.T) {
	if _, err := time.Parse("2006", ChannelSessionThreadRoot); err == nil {
		t.Fatal("sentinel parses as a year — too timestamp-like")
	}
	for _, r := range ChannelSessionThreadRoot {
		if r >= '0' && r <= '9' {
			t.Fatalf("sentinel %q contains a digit; Slack ts values are digits and dots, so a non-numeric sentinel cannot collide", ChannelSessionThreadRoot)
		}
	}
}
