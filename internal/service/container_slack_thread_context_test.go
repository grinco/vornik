package service

import (
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/sessionstore"
	"vornik.io/vornik/internal/slack"
)

// OPERATOR REPORT 2026-07-30: "when vornik replies something on the main channel and I
// ask for additional detail in the thread, the original message seems to not be part of
// the actual thread scope — vornik doesn't know what I'm talking about", quoting the bot:
// "Your question is pretty open — 'followup' from what, exactly? I don't have a recent
// thread context to anchor on."
//
// Cause: a top-level channel message is keyed on the CHANNEL session
// (`<team>/<channel>#main`, the 2026-07-28 continuity change) and the bot answers at
// channel level. Opening a thread under that answer produces a session keyed on the
// thread root — a brand new, empty conversation. The exchange the person is visibly
// replying to is in a different session.
//
// A thread hanging off a message WE posted is a continuation of that conversation, so it
// starts from the channel session's history.
func TestSlackSessionStore_ThreadRootedOnOurMessageInheritsChannelHistory(t *testing.T) {
	store := newSlackSessionStore(nil, "project-x")
	const channelSession = "T123/C_general#" + slack.ChannelSessionThreadRoot
	const threadSession = "T123/C_general#1785367141.211839"

	// The channel-level exchange the person is about to follow up on.
	if err := store.Append(context.Background(),
		conversation.ChannelMessage{SessionID: channelSession},
		dispatcher.Result{Messages: []chat.Message{
			{Role: "user", Content: "summarise the Saturday meeting notes"},
			{Role: "assistant", Content: "Here's the summary of the 25 July meeting…"},
		}},
	); err != nil {
		t.Fatalf("Append: %v", err)
	}

	sess, err := store.Load(context.Background(), conversation.ChannelMessage{
		SessionID: threadSession,
		Text:      "do you know if there was any followup?",
		ChannelSpecific: map[string]string{
			// Set by the channel when parent_user_id is our own bot user id.
			"thread_parent_is_bot": "true",
		},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(sess.History) != 2 {
		t.Fatalf("thread history = %d messages, want the 2 from the channel conversation "+
			"it hangs off", len(sess.History))
	}
	if !strings.Contains(sess.History[0].Content, "Saturday meeting notes") {
		t.Errorf("history[0] = %q, want the channel-level question", sess.History[0].Content)
	}
}

// A thread rooted on a HUMAN's message is not ours to inherit into. Two colleagues
// starting a thread under each other's message and tagging the bot get the bot's help
// with that thread, not a replay of an unrelated channel conversation.
func TestSlackSessionStore_ThreadRootedOnAHumanDoesNotInherit(t *testing.T) {
	store := newSlackSessionStore(nil, "project-x")
	const channelSession = "T123/C_general#" + slack.ChannelSessionThreadRoot

	if err := store.Append(context.Background(),
		conversation.ChannelMessage{SessionID: channelSession},
		dispatcher.Result{Messages: []chat.Message{
			{Role: "user", Content: "unrelated channel chatter"},
			{Role: "assistant", Content: "an unrelated answer"},
		}},
	); err != nil {
		t.Fatalf("Append: %v", err)
	}

	sess, err := store.Load(context.Background(), conversation.ChannelMessage{
		SessionID:       "T123/C_general#1785999999.000100",
		ChannelSpecific: map[string]string{}, // no thread_parent_is_bot
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(sess.History) != 0 {
		t.Fatalf("history = %d messages, want 0 — this thread is not a continuation of "+
			"the channel conversation", len(sess.History))
	}
}

// Inheritance seeds an EMPTY thread only. Once the thread has its own exchanges they are
// the truth, and re-seeding would replay the channel conversation on every turn and grow
// the prompt without bound.
func TestSlackSessionStore_InheritanceOnlySeedsAnEmptyThread(t *testing.T) {
	store := newSlackSessionStore(nil, "project-x")
	const channelSession = "T123/C_general#" + slack.ChannelSessionThreadRoot
	const threadSession = "T123/C_general#1785367141.211839"

	if err := store.Append(context.Background(),
		conversation.ChannelMessage{SessionID: channelSession},
		dispatcher.Result{Messages: []chat.Message{
			{Role: "user", Content: "channel question"},
			{Role: "assistant", Content: "channel answer"},
		}},
	); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := store.Append(context.Background(),
		conversation.ChannelMessage{SessionID: threadSession},
		dispatcher.Result{Messages: []chat.Message{
			{Role: "user", Content: "thread question"},
			{Role: "assistant", Content: "thread answer"},
		}},
	); err != nil {
		t.Fatalf("Append: %v", err)
	}

	sess, err := store.Load(context.Background(), conversation.ChannelMessage{
		SessionID:       threadSession,
		ChannelSpecific: map[string]string{"thread_parent_is_bot": "true"},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(sess.History) != 2 || !strings.Contains(sess.History[0].Content, "thread question") {
		t.Fatalf("history = %v, want only the thread's own 2 messages", sess.History)
	}
}

// The channel-scoped session must not inherit from itself — the prefix arithmetic that
// finds "the channel session for this thread" resolves to the same id at channel level,
// and a self-load would either double the history or recurse.
func TestSlackSessionStore_ChannelSessionDoesNotInheritFromItself(t *testing.T) {
	store := newSlackSessionStore(nil, "project-x")
	const channelSession = "T123/C_general#" + slack.ChannelSessionThreadRoot

	if err := store.Append(context.Background(),
		conversation.ChannelMessage{SessionID: channelSession},
		dispatcher.Result{Messages: []chat.Message{
			{Role: "user", Content: "q"},
			{Role: "assistant", Content: "a"},
		}},
	); err != nil {
		t.Fatalf("Append: %v", err)
	}

	sess, err := store.Load(context.Background(), conversation.ChannelMessage{
		SessionID:       channelSession,
		ChannelSpecific: map[string]string{"thread_parent_is_bot": "true"},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(sess.History) != 2 {
		t.Fatalf("history = %d, want exactly the 2 stored messages", len(sess.History))
	}
}

// A malformed session id must not reach across channels. The inherited prefix is derived
// from the caller's own session, so a thread in channel B can never be seeded from
// channel A's conversation.
func TestSlackSessionStore_InheritanceIsScopedToTheSameChannel(t *testing.T) {
	store := newSlackSessionStore(nil, "project-x")

	if err := store.Append(context.Background(),
		conversation.ChannelMessage{SessionID: "T123/C_alpha#" + slack.ChannelSessionThreadRoot},
		dispatcher.Result{Messages: []chat.Message{
			{Role: "user", Content: "alpha secret"},
			{Role: "assistant", Content: "alpha answer"},
		}},
	); err != nil {
		t.Fatalf("Append: %v", err)
	}

	sess, err := store.Load(context.Background(), conversation.ChannelMessage{
		SessionID:       "T123/C_beta#1785367141.211839",
		ChannelSpecific: map[string]string{"thread_parent_is_bot": "true"},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, m := range sess.History {
		if strings.Contains(m.Content, "alpha") {
			t.Fatalf("channel B's thread inherited channel A's history: %q", m.Content)
		}
	}
}

// stubPersistRepo is a channel-session repository that survives "restart": the rows live
// outside the store, so a fresh store reads what the previous one wrote.
type stubPersistRepo struct {
	rows map[string]*persistence.ChannelSession
}

func newStubPersistRepo() *stubPersistRepo {
	return &stubPersistRepo{rows: map[string]*persistence.ChannelSession{}}
}

func (r *stubPersistRepo) Save(_ context.Context, kind, sessionID, activeProject string, historyJSON []byte) error {
	r.rows[kind+"|"+sessionID] = &persistence.ChannelSession{
		Kind:          kind,
		SessionID:     sessionID,
		ActiveProject: activeProject,
		History:       append([]byte(nil), historyJSON...),
	}
	return nil
}

func (r *stubPersistRepo) Load(_ context.Context, kind, sessionID string) (*persistence.ChannelSession, error) {
	row, ok := r.rows[kind+"|"+sessionID]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return row, nil
}

func (r *stubPersistRepo) Delete(_ context.Context, kind, sessionID string) error {
	delete(r.rows, kind+"|"+sessionID)
	return nil
}

func (r *stubPersistRepo) ListByPrefix(_ context.Context, kind, prefix string, limit int) ([]*persistence.ChannelSession, error) {
	out := []*persistence.ChannelSession{}
	for _, row := range r.rows {
		if row.Kind == kind && strings.HasPrefix(row.SessionID, prefix) {
			out = append(out, row)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// CUSTOMER REPORT 2026-07-30: asked about a job it had scheduled, Vornik "didn't know
// anything about it", and the operator said to assume the process had restarted in
// between — the session must survive that.
//
// The store's own doc comment claimed "daemon restart clears the in-memory history",
// which predates the persister and reads as if restart survival were not a property. It
// IS one when a channel-session repo is wired, and this pins it: a SECOND store, holding
// no in-memory state, reads back what the first one wrote.
func TestSlackSessionStore_ConversationSurvivesARestart(t *testing.T) {
	repo := newStubPersistRepo()
	const session = "T123/C_general#main"

	before := newSlackSessionStore(nil, "project-x")
	before.SetPersister(sessionstore.New(repo, "slack", zerolog.Nop()))
	if err := before.Append(context.Background(),
		conversation.ChannelMessage{SessionID: session},
		dispatcher.Result{Messages: []chat.Message{
			{Role: "user", Content: "schedule the weekly report"},
			{Role: "assistant", Content: "Scheduled as task_99."},
		}},
	); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// The restart: a brand-new store, same durable rows, nothing in memory.
	after := newSlackSessionStore(nil, "project-x")
	after.SetPersister(sessionstore.New(repo, "slack", zerolog.Nop()))

	sess, err := after.Load(context.Background(), conversation.ChannelMessage{
		SessionID: session,
		Text:      "what happened to that job?",
	})
	if err != nil {
		t.Fatalf("Load after restart: %v", err)
	}
	if len(sess.History) != 2 {
		t.Fatalf("history after restart = %d messages, want 2 — the session must survive "+
			"a restart or the bot cannot answer questions about work it scheduled",
			len(sess.History))
	}
	var joined string
	for _, m := range sess.History {
		joined += m.Content + "\n"
	}
	if !strings.Contains(joined, "task_99") {
		t.Errorf("restored history lost the task id it needs to answer: %q", joined)
	}
}

// OPERATOR REQUEST 2026-07-30: rich text in chat. Most outbound text is written by the
// MODEL, so the daemon-side link helper only does half the job — the lead has to know
// that Slack is mrkdwn and not Markdown, or it emits **bold** and [label](url), both of
// which render as visible punctuation and read as a bug.
func TestSlackFormattingBlock_TeachesTheMrkdwnTraps(t *testing.T) {
	for _, want := range []string{
		"<https://example.com|", // the link form
		"[label](url)",          // named as wrong
		"*single asterisks*",
		":white_check_mark:",
	} {
		if !strings.Contains(slackFormattingBlock, want) {
			t.Errorf("the formatting guidance never mentions %q", want)
		}
	}
	// The voice caveat matters: without it a model told to use rich text may avoid it
	// for fear of it being dictated.
	if !strings.Contains(slackFormattingBlock, "voice note") {
		t.Error("the guidance does not tell the lead that voice conversion is automatic")
	}
}
