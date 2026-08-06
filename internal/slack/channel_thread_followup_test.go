package slack

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

// engagementStub answers ThreadEngaged from a fixed set of session ids.
type engagementStub struct {
	engaged map[string]bool
	asked   []string
}

func (e *engagementStub) ThreadEngaged(_ context.Context, sessionID string) bool {
	e.asked = append(e.asked, sessionID)
	return e.engaged[sessionID]
}

// INCIDENT 2026-07-30 — "vornik does not answer in threads".
//
// Three consecutive follow-ups in a thread the bot was actively holding
// (C03HTMUL2S1, thread root 1785367141.211839: "do you know if there was any
// followup?", "did i fix it?", "So?") produced no reply and no warning in the
// journal. The bot had answered the thread's opening question moments earlier.
//
// ROOT CAUSE. message.channels was gated on mentionsVornik(text), which matches only
// the LITERAL string "@vornik". A real Slack mention arrives encoded as
// `<@U0BLPMBQXDL>`, so that gate never matches a genuine mention at all — tagged
// messages only worked because app_mention is a separate delivery. A reply inside a
// thread carries no mention, which is how people actually converse in threads, so
// every follow-up was dropped at Debug level: invisible.
//
// FIX. A mention-less message inside a thread the bot is already engaged in starts a
// turn. Engagement is established two ways, so it survives a daemon restart:
// parent_user_id equal to our own bot user id (the thread is rooted on a message we
// posted), or stored conversation history for that thread's session.
func TestHandleWebhook_UntaggedThreadFollowUpStartsATurn(t *testing.T) {
	const (
		botUser    = "U0BLPMBQXDL"
		threadRoot = "1785367141.211839"
		sessionID  = "T123/C_general#" + threadRoot
	)

	for _, tc := range []struct {
		name         string
		threadTs     string
		parentUserID string
		engaged      map[string]bool
		text         string
		wantDispatch bool
	}{
		{
			name:         "follow-up in a thread rooted on the bot's own answer",
			threadTs:     threadRoot,
			parentUserID: botUser,
			text:         "do you know if there was any followup? are there any relevant docs?",
			wantDispatch: true,
		},
		{
			name:         "follow-up in a thread we hold history for survives a restart",
			threadTs:     threadRoot,
			parentUserID: "U_alice", // a human opened the thread; we joined when tagged
			engaged:      map[string]bool{sessionID: true},
			text:         "So?",
			wantDispatch: true,
		},
		{
			name:         "thread we have never spoken in stays out of scope",
			threadTs:     threadRoot,
			parentUserID: "U_alice",
			text:         "two colleagues talking to each other",
			wantDispatch: false,
		},
		{
			name:         "top-level channel chatter still requires a mention",
			threadTs:     "",
			text:         "just chatting",
			wantDispatch: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.ChannelAllowlist = []string{"C_general"}
			ch := makeChannel(t, cfg, time.Now())
			rec := &recordingReceiver{}
			bindReceiver(ch, rec)
			ch.SetThreadEngagementChecker(&engagementStub{engaged: tc.engaged})

			payload := map[string]any{
				"type":     "event_callback",
				"team_id":  "T123",
				"event_id": "Ev_followup",
				"authorizations": []any{
					map[string]any{"team_id": "T123", "user_id": botUser, "is_bot": true},
				},
				"event": map[string]any{
					"type":           "message",
					"user":           "U_alice",
					"text":           tc.text,
					"channel":        "C_general",
					"channel_type":   "channel",
					"ts":             "1785367200.000100",
					"thread_ts":      tc.threadTs,
					"parent_user_id": tc.parentUserID,
				},
			}
			postSignedJSON(t, ch, cfg.SigningSecret, time.Now(), payload)

			got := len(rec.snapshot())
			if tc.wantDispatch && got != 1 {
				t.Fatalf("dispatch count = %d, want 1 — a mention-less reply inside a thread "+
					"the bot is holding must start a turn", got)
			}
			if !tc.wantDispatch && got != 0 {
				t.Fatalf("dispatch count = %d, want 0", got)
			}
		})
	}
}

// A tagged message in a channel is delivered TWICE by Slack — once as app_mention and
// once as message.channels — with different event_ids, so the event_id cache cannot
// collapse them. Before the thread fix the pair was harmless: the message.channels
// copy always failed the literal-"@vornik" gate. Now that a thread follow-up can
// dispatch without a mention, the copy would produce a SECOND answer to one message.
//
// Dedupe is therefore keyed on the message itself (channel + ts), recorded only when a
// turn actually starts, so whichever delivery clears the gates first wins and the
// other becomes a no-op.
func TestHandleWebhook_TaggedThreadReplyAnswersExactlyOnce(t *testing.T) {
	const (
		botUser    = "U0BLPMBQXDL"
		threadRoot = "1785367141.211839"
		msgTs      = "1785367300.000100"
	)
	cfg := validConfig()
	cfg.ChannelAllowlist = []string{"C_general"}
	ch := makeChannel(t, cfg, time.Now())
	rec := &recordingReceiver{}
	bindReceiver(ch, rec)

	event := func(eventType, eventID string) map[string]any {
		return map[string]any{
			"type":     "event_callback",
			"team_id":  "T123",
			"event_id": eventID,
			"authorizations": []any{
				map[string]any{"team_id": "T123", "user_id": botUser, "is_bot": true},
			},
			"event": map[string]any{
				"type":           eventType,
				"user":           "U_alice",
				"text":           "<@" + botUser + "> and one more thing",
				"channel":        "C_general",
				"channel_type":   "channel",
				"ts":             msgTs,
				"thread_ts":      threadRoot,
				"parent_user_id": botUser,
			},
		}
	}

	// Slack does not guarantee ordering, so assert on both interleavings.
	postSignedJSON(t, ch, cfg.SigningSecret, time.Now(), event("app_mention", "Ev_mention"))
	postSignedJSON(t, ch, cfg.SigningSecret, time.Now(), event("message", "Ev_message"))

	if got := len(rec.snapshot()); got != 1 {
		t.Fatalf("dispatch count = %d, want 1 — app_mention and message.channels are two "+
			"deliveries of ONE user message and must produce one answer", got)
	}
}

// app_mention carries no channel_type, so the DM exemption in channelAllowed must not
// be reachable through it for a shared channel — otherwise the allowlist could be
// bypassed by an event shape rather than by a real DM.
func TestChannelAllowed_AppMentionShapeDoesNotBypassAllowlist(t *testing.T) {
	inst := &installation{allowedChannels: map[string]struct{}{"C_allowed": {}}}
	if channelAllowed(inst, "", "C_other") {
		t.Error("a C… channel with no channel_type must not be treated as a DM")
	}
	if !channelAllowed(inst, "", "D0BLA9ZRDFH") {
		t.Error("a D… channel with no channel_type is a DM and must be exempt")
	}
	if channelAllowed(inst, "", "G_private") {
		t.Error("a G… private channel must still be gated")
	}
}

// Regression (reported 2026-08-06): the 2026-08-05 fail-closed flip (62a7ad82) put the
// empty-allowlist check BEFORE the DM exemption, so an installation with NO
// channel_allowlist dropped every direct message — "slack: channel not on installation
// allowlist; dropping channel=D0BKS77EVN1" — and the bot went silent. An empty channel
// allowlist is the only workable shape for a DM bot, since Slack mints a D… id lazily on
// first contact and it cannot be pre-listed. The sender allowlist (fail-closed since the
// same release) is a DM's real gate; the channel allowlist never applies to one.
func TestChannelAllowed_EmptyChannelAllowlistStillExemptsDirectMessages(t *testing.T) {
	inst := &installation{projectID: "p"} // no channel_allowlist, NOT opted open
	if !channelAllowed(inst, "im", "D0BKS77EVN1") {
		t.Error("an empty channel_allowlist must not gate a DM (channel_type=im)")
	}
	if !channelAllowed(inst, "", "D0BKS77EVN1") {
		t.Error("an empty channel_allowlist must not gate a DM carrying no channel_type " +
			"(the file_shared / slash-command shape)")
	}
	if channelAllowed(inst, "channel", "C_ANY") {
		t.Error("an empty channel_allowlist must still deny a real channel")
	}
	if channelAllowed(inst, "", "G_PRIVATE") {
		t.Error("an empty channel_allowlist must still deny a private group")
	}
}

// The engagement lookup runs on the DETACHED dispatch context, not the request
// context: Slack's ack budget is three seconds and the lookup can reach the database.
// Asserting the checker is consulted at all guards against a refactor that reinstates
// the pre-ack check.
func TestHandleWebhook_EngagementCheckedOffTheAckPath(t *testing.T) {
	cfg := validConfig()
	cfg.ChannelAllowlist = []string{"C_general"}
	ch := makeChannel(t, cfg, time.Now())
	bindReceiver(ch, &recordingReceiver{})
	stub := &engagementStub{}
	ch.SetThreadEngagementChecker(stub)

	body := []byte(`{"type":"event_callback","team_id":"T123","event_id":"Ev_ack",
		"event":{"type":"message","channel_type":"channel","user":"U_alice",
		"channel":"C_general","text":"no mention here","ts":"1.1","thread_ts":"0.9"}}`)
	req := signedRequest(t, cfg.SigningSecret, time.Now().Unix(), body)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, req)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200 within Slack's 3s ack budget", w.Code)
	}
	ch.waitInFlight()

	if len(stub.asked) != 1 || stub.asked[0] != "T123/C_general#0.9" {
		t.Fatalf("engagement lookups = %v, want one for T123/C_general#0.9", stub.asked)
	}
}

// The session store seeds an empty thread from the channel conversation only when told
// the thread hangs off OUR message, so the inbound has to carry that fact. Without the
// flag a follow-up under the bot's channel-level answer starts from nothing and the bot
// says it has no context to anchor on (operator report 2026-07-30).
func TestBuildMessageChannelMessage_FlagsAThreadRootedOnOurOwnMessage(t *testing.T) {
	const botUser = "U0BLPMBQXDL"
	cfg := validConfig()
	ch := makeChannel(t, cfg, time.Now())
	inst := ch.installations[0]

	build := func(threadTs, parentUser string, withAuthz bool) map[string]string {
		p := eventPayload{
			TeamID: "T123",
			Event: &eventInner{
				Type: "message", User: "U_alice", Channel: "C_general",
				ChannelType: "channel", Ts: "1785367200.000100",
				ThreadTs: threadTs, ParentUserID: parentUser,
			},
		}
		if withAuthz {
			p.Authorizations = []eventAuthorization{{TeamID: "T123", UserID: botUser, IsBot: true}}
		}
		return ch.buildMessageChannelMessage(p, inst).ChannelSpecific
	}

	if got := build("1785367141.211839", botUser, true)["thread_parent_is_bot"]; got != "true" {
		t.Errorf("thread rooted on our own message: flag = %q, want \"true\"", got)
	}
	if got := build("1785367141.211839", "U_alice", true)["thread_parent_is_bot"]; got != "" {
		t.Errorf("thread rooted on a human: flag = %q, want empty", got)
	}
	if got := build("", botUser, true)["thread_parent_is_bot"]; got != "" {
		t.Errorf("channel-level message: flag = %q, want empty — there is no thread", got)
	}
	// Without the authorizations array we cannot know our own id, so we must not guess.
	if got := build("1785367141.211839", botUser, false)["thread_parent_is_bot"]; got != "" {
		t.Errorf("no authorizations: flag = %q, want empty", got)
	}
}
