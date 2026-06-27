package steering

import (
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
)

func newOperatorAlert(t *testing.T, ch *fakeChannel, cfg OperatorAlertConfig, enabled bool) (*OperatorAlertNotifier, *fakeChannel) {
	t.Helper()
	res := fakeResolver{byName: map[string]conversation.Channel{}}
	if ch != nil {
		res.byName[ch.name] = ch
	}
	return NewOperatorAlert(res, "https://vornik.example", cfg, enabled, zerolog.Nop()), ch
}

func autonomyTask() *persistence.Task {
	return &persistence.Task{
		ID:             "task_auto",
		ProjectID:      "p1",
		CreationSource: persistence.TaskCreationSourceAutonomous,
		// no ChatTurnID — ownerless
	}
}

func TestOperatorAlert_NotifiesOwnerlessAutonomyApproval(t *testing.T) {
	ch := &fakeChannel{name: "telegram"}
	n, tg := newOperatorAlert(t, ch, OperatorAlertConfig{Channel: "telegram", Session: "555"}, true)

	n.NotifySteeringRequired(context.Background(), autonomyTask(), string(persistence.TaskStatusAwaitingApproval))

	if len(tg.sent) != 1 {
		t.Fatalf("want 1 send to the operator recipient, got %d", len(tg.sent))
	}
	m := tg.sent[0]
	if m.SessionID != "555" {
		t.Errorf("SessionID = %q, want the configured operator session 555", m.SessionID)
	}
	if !strings.Contains(m.Text, "task_auto") {
		t.Errorf("alert text missing task id: %q", m.Text)
	}
}

func TestOperatorAlert_SkipsChatOriginatedTask(t *testing.T) {
	ch := &fakeChannel{name: "telegram"}
	n, tg := newOperatorAlert(t, ch, OperatorAlertConfig{Channel: "telegram", Session: "555"}, true)

	task := autonomyTask()
	task.ChatTurnID = turnID("chat_1") // has an originating chat → the chat notifier owns it
	n.NotifySteeringRequired(context.Background(), task, string(persistence.TaskStatusAwaitingApproval))

	if len(tg.sent) != 0 {
		t.Fatalf("chat-originated task must not get the operator fallback alert (double-notify)")
	}
}

func TestOperatorAlert_SkipsNonAutonomyOwnerless(t *testing.T) {
	ch := &fakeChannel{name: "telegram"}
	n, tg := newOperatorAlert(t, ch, OperatorAlertConfig{Channel: "telegram", Session: "555"}, true)

	task := autonomyTask()
	task.CreationSource = persistence.TaskCreationSourceUser // API-created, no chat
	n.NotifySteeringRequired(context.Background(), task, string(persistence.TaskStatusAwaitingApproval))

	if len(tg.sent) != 0 {
		t.Fatalf("non-autonomy ownerless task must not trigger the operator alert")
	}
}

func TestOperatorAlert_SkipsWhenNoRecipientConfigured(t *testing.T) {
	ch := &fakeChannel{name: "telegram"}
	n, tg := newOperatorAlert(t, ch, OperatorAlertConfig{}, true) // no channel/session

	n.NotifySteeringRequired(context.Background(), autonomyTask(), string(persistence.TaskStatusAwaitingApproval))

	if len(tg.sent) != 0 {
		t.Fatalf("no recipient configured must be a no-op, got %d sends", len(tg.sent))
	}
}

func TestOperatorAlert_Disabled(t *testing.T) {
	ch := &fakeChannel{name: "telegram"}
	n, tg := newOperatorAlert(t, ch, OperatorAlertConfig{Channel: "telegram", Session: "555"}, false)

	n.NotifySteeringRequired(context.Background(), autonomyTask(), string(persistence.TaskStatusAwaitingApproval))

	if len(tg.sent) != 0 {
		t.Fatalf("disabled notifier must not send")
	}
}

func TestOperatorAlert_Dedup(t *testing.T) {
	ch := &fakeChannel{name: "telegram"}
	n, tg := newOperatorAlert(t, ch, OperatorAlertConfig{Channel: "telegram", Session: "555"}, true)

	task := autonomyTask()
	n.NotifySteeringRequired(context.Background(), task, string(persistence.TaskStatusAwaitingApproval))
	n.NotifySteeringRequired(context.Background(), task, string(persistence.TaskStatusAwaitingApproval))

	if len(tg.sent) != 1 {
		t.Fatalf("duplicate (task,state) within the dedup window must send once, got %d", len(tg.sent))
	}
}

func TestNotifyOperator_SendsFreeFormAlert(t *testing.T) {
	ch := &fakeChannel{name: "telegram"}
	n, tg := newOperatorAlert(t, ch, OperatorAlertConfig{Channel: "telegram", Session: "555"}, true)
	n.NotifyOperator(context.Background(), "cluster: endpoint down", "public-webhook-ingress unreachable")
	if len(tg.sent) != 1 {
		t.Fatalf("want 1 operator alert, got %d", len(tg.sent))
	}
	if m := tg.sent[0]; m.SessionID != "555" || !strings.Contains(m.Text, "endpoint down") {
		t.Errorf("alert not addressed/worded correctly: %+v", m)
	}
}

func TestNotifyOperator_NoopWhenUnconfigured(t *testing.T) {
	ch := &fakeChannel{name: "telegram"}
	n, tg := newOperatorAlert(t, ch, OperatorAlertConfig{}, true) // no recipient
	n.NotifyOperator(context.Background(), "x", "y")
	if len(tg.sent) != 0 {
		t.Fatalf("unconfigured recipient must be a no-op")
	}
}

func TestOperatorAlert_EmailRecipientGetsToAndSubject(t *testing.T) {
	ch := &fakeChannel{name: "email"}
	n, em := newOperatorAlert(t, ch, OperatorAlertConfig{Channel: "email", Session: "ops-thread", Address: "ops@example.com"}, true)

	n.NotifySteeringRequired(context.Background(), autonomyTask(), string(persistence.TaskStatusAwaitingApproval))

	if len(em.sent) != 1 {
		t.Fatalf("want 1 email send, got %d", len(em.sent))
	}
	cs := em.sent[0].ChannelSpecific
	if cs["to"] != "ops@example.com" {
		t.Errorf("email To = %q, want ops@example.com", cs["to"])
	}
	if cs["subject"] == "" {
		t.Errorf("email alert must carry a subject")
	}
}
