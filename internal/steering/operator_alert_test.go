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
	return newOperatorAlertR(t, ch, nil, cfg, enabled)
}

// newOperatorAlertR builds a notifier with an explicit per-project recipient
// resolver (nil = fall back to cfg.Session).
func newOperatorAlertR(t *testing.T, ch *fakeChannel, recipients ProjectRecipients, cfg OperatorAlertConfig, enabled bool) (*OperatorAlertNotifier, *fakeChannel) {
	t.Helper()
	res := fakeResolver{byName: map[string]conversation.Channel{}}
	if ch != nil {
		res.byName[ch.name] = ch
	}
	return NewOperatorAlert(res, recipients, nil, "https://vornik.example", cfg, enabled, zerolog.Nop()), ch
}

// fakeRecipients maps projectID → session IDs for the per-project fan-out test.
type fakeRecipients map[string][]string

func (f fakeRecipients) RecipientsForProject(_, projectID string) []string { return f[projectID] }

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
	// Deep link must be the canonical /ui/tasks/{id} route; the nested
	// /ui/projects/{p}/tasks/{id} form 404s in the browser (operator-reported
	// 2026-07-08).
	if !strings.Contains(m.Text, "https://vornik.example/ui/tasks/task_auto") {
		t.Errorf("alert missing canonical deep link /ui/tasks/task_auto: %q", m.Text)
	}
	if strings.Contains(m.Text, "/ui/projects/") {
		t.Errorf("alert deep link must not use the 404-ing nested form: %q", m.Text)
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

// TestOperatorAlert_NotifiesRoutedSubtask is the reported gap: an autonomy
// task's routed CHILD (source=route, no ChatTurnID) checkpoints, and the chat
// notifier can't reach it. The operator-alert must now catch it.
func TestOperatorAlert_NotifiesRoutedSubtask(t *testing.T) {
	ch := &fakeChannel{name: "telegram"}
	n, tg := newOperatorAlert(t, ch, OperatorAlertConfig{Channel: "telegram", Session: "555"}, true)

	task := autonomyTask()
	task.ID = "task_routed_child"
	task.CreationSource = persistence.TaskCreationSourceRoute
	n.NotifySteeringRequired(context.Background(), task, string(persistence.TaskStatusAwaitingInput))

	if len(tg.sent) != 1 {
		t.Fatalf("routed ownerless sub-task checkpoint must alert the operator, got %d", len(tg.sent))
	}
}

// TestOperatorAlert_SkipsChatOriginatedViaAncestor is the 2026-07-08 fix: a
// ROUTE/CHECKPOINT child of a task that WAS scheduled from a chat carries only
// ParentTaskID. With the ancestry walk wired, the operator-alert must treat it
// as chat-owned and SKIP (the chat Notifier routes it back to the chat),
// instead of firing the generic "No chat originated it" operator alert.
func TestOperatorAlert_SkipsChatOriginatedViaAncestor(t *testing.T) {
	ch := &fakeChannel{name: "telegram"}
	res := fakeResolver{byName: map[string]conversation.Channel{"telegram": ch}}

	parentID := "parent-chat"
	parent := &persistence.Task{ID: parentID, ProjectID: "p1", ChatTurnID: turnID("chat_1")}
	child := autonomyTask()
	child.ID = "task_routed_child"
	child.CreationSource = persistence.TaskCreationSourceRoute
	child.ParentTaskID = &parentID // no own ChatTurnID
	getter := fakeTaskGetter{byID: map[string]*persistence.Task{parentID: parent}}

	n := NewOperatorAlert(res, nil, getter, "https://vornik.example",
		OperatorAlertConfig{Channel: "telegram", Session: "555"}, true, zerolog.Nop())
	n.NotifySteeringRequired(context.Background(), child, string(persistence.TaskStatusAwaitingInput))

	if len(ch.sent) != 0 {
		t.Fatalf("child of a chat-scheduled task must NOT get the generic operator alert (chat notifier owns it); got %d", len(ch.sent))
	}
}

// TestOperatorAlert_PerProjectFanOut: alerts route to the operators with
// access to the task's project (assistant → wildcard only; janka → wildcard +
// janka-scoped), not a single fixed session.
func TestOperatorAlert_PerProjectFanOut(t *testing.T) {
	ch := &fakeChannel{name: "telegram"}
	recips := fakeRecipients{
		"assistant": {"559741208"},
		"janka":     {"559741208", "8019099642"},
	}
	n, tg := newOperatorAlertR(t, ch, recips, OperatorAlertConfig{Channel: "telegram", Session: "fallback"}, true)

	assistantTask := autonomyTask()
	assistantTask.ID, assistantTask.ProjectID = "task_a", "assistant"
	n.NotifySteeringRequired(context.Background(), assistantTask, string(persistence.TaskStatusAwaitingInput))

	jankaTask := autonomyTask()
	jankaTask.ID, jankaTask.ProjectID = "task_j", "janka"
	n.NotifySteeringRequired(context.Background(), jankaTask, string(persistence.TaskStatusAwaitingInput))

	got := map[string]int{}
	for _, m := range tg.sent {
		got[m.SessionID]++
	}
	// assistant → 1 (wildcard only); janka → 2 (wildcard + scoped).
	if got["559741208"] != 2 { // wildcard operator gets both
		t.Errorf("wildcard operator should get both projects' alerts, got %d", got["559741208"])
	}
	if got["8019099642"] != 1 { // janka-scoped only for janka
		t.Errorf("janka-scoped operator should get exactly janka's alert, got %d", got["8019099642"])
	}
	if got["fallback"] != 0 {
		t.Errorf("fallback session must not be used when per-project recipients resolve")
	}
}

// TestOperatorAlert_FallbackWhenNoProjectRecipients: with a resolver that
// returns nobody for the project, the configured fallback session is used.
func TestOperatorAlert_FallbackWhenNoProjectRecipients(t *testing.T) {
	ch := &fakeChannel{name: "telegram"}
	n, tg := newOperatorAlertR(t, ch, fakeRecipients{}, OperatorAlertConfig{Channel: "telegram", Session: "fallback"}, true)

	n.NotifySteeringRequired(context.Background(), autonomyTask(), string(persistence.TaskStatusAwaitingInput))

	if len(tg.sent) != 1 || tg.sent[0].SessionID != "fallback" {
		t.Fatalf("must fall back to the configured session when no project recipients resolve: %+v", tg.sent)
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
