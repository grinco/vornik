package steering

import (
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
)

// --- fakes ----------------------------------------------------------------

type fakeAudit struct {
	row *persistence.ChatAuditEntry
	err error
}

func (f fakeAudit) GetByID(_ context.Context, _ string) (*persistence.ChatAuditEntry, error) {
	return f.row, f.err
}

type fakeChannel struct {
	name    string
	sent    []conversation.ChannelMessage
	sendErr error
}

func (f *fakeChannel) Name() string                                       { return f.name }
func (f *fakeChannel) Start(context.Context, conversation.Receiver) error { return nil }
func (f *fakeChannel) Stop() error                                        { return nil }
func (f *fakeChannel) Send(_ context.Context, m conversation.ChannelMessage) (string, error) {
	f.sent = append(f.sent, m)
	if f.sendErr != nil {
		return "", f.sendErr
	}
	return "sent-1", nil
}
func (f *fakeChannel) ListSessions(context.Context) ([]conversation.Session, error) {
	return nil, nil
}
func (f *fakeChannel) ResolveSpeaker(context.Context, string) (conversation.Speaker, error) {
	return conversation.Speaker{}, nil
}

type fakeResolver struct {
	byName map[string]conversation.Channel
}

func (f fakeResolver) ResolveChannel(name string) conversation.Channel {
	if f.byName == nil {
		return nil
	}
	return f.byName[name] // nil when absent
}

func turnID(s string) *string { return &s }

// fakeTaskGetter serves tasks by ID so the ancestry walk can be exercised.
type fakeTaskGetter struct {
	byID map[string]*persistence.Task
	err  error
}

func (f fakeTaskGetter) Get(_ context.Context, id string) (*persistence.Task, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byID[id], nil
}

func newNotifier(t *testing.T, row *persistence.ChatAuditEntry, ch *fakeChannel, enabled bool) (*Notifier, *fakeChannel) {
	t.Helper()
	res := fakeResolver{byName: map[string]conversation.Channel{}}
	if ch != nil {
		res.byName[ch.name] = ch
	}
	// nil task-getter + nil checkpoints: existing tests exercise the
	// immediate-task, no-button path only.
	return New(fakeAudit{row: row}, res, nil, nil, "https://vornik.example", enabled, zerolog.Nop()), ch
}

// TestNotify_ChatOriginatedViaAncestor is the 2026-07-08 fix: a child task
// (checkpoint/route/delegation) carries only ParentTaskID, not ChatTurnID.
// The notifier must walk the ancestry, find the chat-scheduled parent's
// ChatTurnID, and route the DM to that chat — not silently skip.
func TestNotify_ChatOriginatedViaAncestor(t *testing.T) {
	row := &persistence.ChatAuditEntry{ID: "chat_1", ChatID: "555", UserID: "telegram:555", ProjectID: "p1"}
	ch := &fakeChannel{name: "telegram"}
	res := fakeResolver{byName: map[string]conversation.Channel{"telegram": ch}}

	parentID := "parent-chat"
	parent := &persistence.Task{ID: parentID, ProjectID: "p1", ChatTurnID: turnID("chat_1")}
	child := &persistence.Task{ID: "child-1", ProjectID: "p1", ParentTaskID: &parentID} // no ChatTurnID
	getter := fakeTaskGetter{byID: map[string]*persistence.Task{parentID: parent}}

	n := New(fakeAudit{row: row}, res, getter, nil, "https://vornik.example", true, zerolog.Nop())
	n.NotifySteeringRequired(context.Background(), child, string(persistence.TaskStatusAwaitingInput))

	if len(ch.sent) != 1 {
		t.Fatalf("want 1 DM to the ancestor's chat, got %d", len(ch.sent))
	}
	if ch.sent[0].SessionID != "555" {
		t.Errorf("SessionID = %q, want the ancestor chat's 555", ch.sent[0].SessionID)
	}
}

// --- decodeChatID ---------------------------------------------------------

func TestDecodeChatID(t *testing.T) {
	cases := []struct{ in, wantCh, wantSess string }{
		{"123456789", "telegram", "123456789"},
		{"slack:T1/C2#169.42", "slack", "T1/C2#169.42"},
		{"email:<abc@x.com>", "email", "<abc@x.com>"},
		{"web-chat:cookie-uuid", "web-chat", "cookie-uuid"},
		{"", "", ""},
		{"nocolon-nonnumeric", "", ""},
	}
	for _, c := range cases {
		ch, sess := decodeChatID(c.in)
		if ch != c.wantCh || sess != c.wantSess {
			t.Errorf("decodeChatID(%q) = (%q,%q), want (%q,%q)", c.in, ch, sess, c.wantCh, c.wantSess)
		}
	}
}

// --- NotifySteeringRequired ----------------------------------------------

func TestNotify_TelegramInput(t *testing.T) {
	row := &persistence.ChatAuditEntry{ID: "chat_1", ChatID: "555", UserID: "telegram:555", ProjectID: "p1"}
	ch := &fakeChannel{name: "telegram"}
	n, tg := newNotifier(t, row, ch, true)

	task := &persistence.Task{ID: "task_a", ProjectID: "p1", ChatTurnID: turnID("chat_1")}
	n.NotifySteeringRequired(context.Background(), task, string(persistence.TaskStatusAwaitingInput))

	if len(tg.sent) != 1 {
		t.Fatalf("want 1 send, got %d", len(tg.sent))
	}
	m := tg.sent[0]
	if m.SessionID != "555" {
		t.Errorf("SessionID = %q, want 555", m.SessionID)
	}
	if !strings.Contains(m.Text, "task_a") || !strings.Contains(m.Text, "input") {
		t.Errorf("text missing task id / intent: %q", m.Text)
	}
	// Deep link must use the canonical UI task-detail route /ui/tasks/{id};
	// the nested /ui/projects/{p}/tasks/{id} form is the API path and 404s in
	// the browser (operator-reported 2026-07-08 regression).
	if !strings.Contains(m.Text, "https://vornik.example/ui/tasks/task_a") {
		t.Errorf("text missing canonical deep link /ui/tasks/task_a: %q", m.Text)
	}
	if strings.Contains(m.Text, "/ui/projects/") {
		t.Errorf("deep link must not use the 404-ing nested /ui/projects/.../tasks form: %q", m.Text)
	}
}

func TestNotify_EmailSetsToAndSubject(t *testing.T) {
	row := &persistence.ChatAuditEntry{ID: "chat_2", ChatID: "email:<thread@x.com>", UserID: "email:ops@x.com", ProjectID: "p1"}
	ch := &fakeChannel{name: "email"}
	n, em := newNotifier(t, row, ch, true)

	task := &persistence.Task{ID: "task_b", ProjectID: "p1", ChatTurnID: turnID("chat_2")}
	n.NotifySteeringRequired(context.Background(), task, string(persistence.TaskStatusAwaitingApproval))

	if len(em.sent) != 1 {
		t.Fatalf("want 1 send, got %d", len(em.sent))
	}
	m := em.sent[0]
	if m.SessionID != "<thread@x.com>" {
		t.Errorf("SessionID = %q", m.SessionID)
	}
	if m.ChannelSpecific["to"] != "ops@x.com" {
		t.Errorf("email To = %q, want ops@x.com", m.ChannelSpecific["to"])
	}
	if m.ChannelSpecific["subject"] == "" {
		t.Errorf("email subject must be set")
	}
	if !strings.Contains(m.Text, "approval") {
		t.Errorf("approval text expected: %q", m.Text)
	}
}

func TestNotify_NoChatTurnID_NoSend(t *testing.T) {
	ch := &fakeChannel{name: "telegram"}
	n, tg := newNotifier(t, &persistence.ChatAuditEntry{}, ch, true)
	n.NotifySteeringRequired(context.Background(), &persistence.Task{ID: "t", ProjectID: "p"}, string(persistence.TaskStatusAwaitingInput))
	if len(tg.sent) != 0 {
		t.Fatalf("task without ChatTurnID must not notify")
	}
}

func TestNotify_Disabled_NoSend(t *testing.T) {
	row := &persistence.ChatAuditEntry{ID: "c", ChatID: "555", ProjectID: "p"}
	ch := &fakeChannel{name: "telegram"}
	n, tg := newNotifier(t, row, ch, false) // disabled
	n.NotifySteeringRequired(context.Background(), &persistence.Task{ID: "t", ProjectID: "p", ChatTurnID: turnID("c")}, string(persistence.TaskStatusAwaitingInput))
	if len(tg.sent) != 0 {
		t.Fatalf("disabled notifier must not send")
	}
}

func TestNotify_UnresolvableChannel_NoSend(t *testing.T) {
	// web-chat has no outbound channel registered → graceful no-op.
	row := &persistence.ChatAuditEntry{ID: "c", ChatID: "web-chat:uuid", ProjectID: "p"}
	n, _ := newNotifier(t, row, nil, true) // resolver has no channels
	// Should not panic; nothing to assert beyond no crash.
	n.NotifySteeringRequired(context.Background(), &persistence.Task{ID: "t", ProjectID: "p", ChatTurnID: turnID("c")}, string(persistence.TaskStatusAwaitingInput))
}

func TestNotify_Dedup(t *testing.T) {
	row := &persistence.ChatAuditEntry{ID: "c", ChatID: "555", ProjectID: "p"}
	ch := &fakeChannel{name: "telegram"}
	n, tg := newNotifier(t, row, ch, true)
	task := &persistence.Task{ID: "t", ProjectID: "p", ChatTurnID: turnID("c")}
	n.NotifySteeringRequired(context.Background(), task, string(persistence.TaskStatusAwaitingInput))
	n.NotifySteeringRequired(context.Background(), task, string(persistence.TaskStatusAwaitingInput))
	if len(tg.sent) != 1 {
		t.Fatalf("second notify within dedup window must be suppressed; got %d sends", len(tg.sent))
	}
}

func TestNotify_NilReceiverSafe(t *testing.T) {
	var n *Notifier
	// Must not panic.
	n.NotifySteeringRequired(context.Background(), &persistence.Task{ID: "t"}, "AWAITING_INPUT")
}
