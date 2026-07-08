package steering

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
)

type fakeCheckpointReader struct {
	cp  *persistence.TaskMessage
	err error
}

func (f fakeCheckpointReader) GetOpenCheckpoint(_ context.Context, _ string) (*persistence.TaskMessage, error) {
	return f.cp, f.err
}

// notifierWithCheckpoints builds a Notifier wired to a telegram fake channel +
// a checkpoint reader, returning the channel so tests can inspect the sent
// message's Buttons.
func notifierWithButtons(t *testing.T, cp *persistence.TaskMessage) (*Notifier, *fakeChannel) {
	t.Helper()
	row := &persistence.ChatAuditEntry{ID: "chat_1", ChatID: "555", UserID: "telegram:555", ProjectID: "p1"}
	ch := &fakeChannel{name: "telegram"}
	res := fakeResolver{byName: map[string]conversation.Channel{"telegram": ch}}
	n := New(fakeAudit{row: row}, res, nil, fakeCheckpointReader{cp: cp}, "https://vornik.example", true, zerolog.Nop())
	return n, ch
}

// TestSteeringButtons_DecisionCheckpoint — an AWAITING_INPUT task with an open
// decision checkpoint renders one button per option, encoded by index.
func TestSteeringButtons_DecisionCheckpoint(t *testing.T) {
	meta, _ := json.Marshal(map[string]any{"options": []map[string]string{
		{"id": "retry", "label": "Retry as-is"},
		{"id": "model_fallback", "label": "Retry on fallback model"},
	}})
	cp := &persistence.TaskMessage{Metadata: meta}
	n, ch := notifierWithButtons(t, cp)

	task := &persistence.Task{ID: "task_abc", ProjectID: "p1", ChatTurnID: turnID("chat_1")}
	n.NotifySteeringRequired(context.Background(), task, string(persistence.TaskStatusAwaitingInput))

	if len(ch.sent) != 1 {
		t.Fatalf("want 1 send, got %d", len(ch.sent))
	}
	btns := ch.sent[0].Buttons
	if len(btns) != 2 {
		t.Fatalf("want 2 option button rows, got %d", len(btns))
	}
	if btns[0][0].Label != "Retry as-is" || btns[0][0].CallbackData != "steer:c:task_abc:0" {
		t.Errorf("row 0 wrong: %+v", btns[0][0])
	}
	if btns[1][0].CallbackData != "steer:c:task_abc:1" {
		t.Errorf("row 1 callback wrong: %q", btns[1][0].CallbackData)
	}
}

// TestSteeringButtons_Approval — an AWAITING_APPROVAL task renders
// Approve/Reject buttons (no checkpoint needed).
func TestSteeringButtons_Approval(t *testing.T) {
	n, ch := notifierWithButtons(t, nil)
	task := &persistence.Task{ID: "task_ap", ProjectID: "p1", ChatTurnID: turnID("chat_1")}
	n.NotifySteeringRequired(context.Background(), task, string(persistence.TaskStatusAwaitingApproval))

	if len(ch.sent) != 1 {
		t.Fatalf("want 1 send, got %d", len(ch.sent))
	}
	btns := ch.sent[0].Buttons
	if len(btns) != 1 || len(btns[0]) != 2 {
		t.Fatalf("want one row of two buttons, got %+v", btns)
	}
	if btns[0][0].CallbackData != "steer:approve:task_ap" || btns[0][1].CallbackData != "steer:reject:task_ap" {
		t.Errorf("approve/reject callbacks wrong: %+v", btns[0])
	}
}

// TestSteeringButtons_LongTaskIDNoApprovalButtons — a task id long enough
// that the approve/reject callback would exceed Telegram's 64-byte cap falls
// back to the text prompt (no silently-dropped buttons). Review finding.
func TestSteeringButtons_LongTaskIDNoApprovalButtons(t *testing.T) {
	n, ch := notifierWithButtons(t, nil)
	longID := "task_" + strings.Repeat("z", 60) // "steer:approve:"+65 > 64
	task := &persistence.Task{ID: longID, ProjectID: "p1", ChatTurnID: turnID("chat_1")}
	n.NotifySteeringRequired(context.Background(), task, string(persistence.TaskStatusAwaitingApproval))

	if len(ch.sent) != 1 {
		t.Fatalf("want 1 send, got %d", len(ch.sent))
	}
	if len(ch.sent[0].Buttons) != 0 {
		t.Errorf("over-cap approval callbacks must fall back to no buttons; got %+v", ch.sent[0].Buttons)
	}
}

// TestSteeringButtons_FreeTextNoButtons — an AWAITING_INPUT task with no open
// decision checkpoint (a free-text question) gets no buttons.
func TestSteeringButtons_FreeTextNoButtons(t *testing.T) {
	n, ch := notifierWithButtons(t, nil) // no checkpoint
	task := &persistence.Task{ID: "task_q", ProjectID: "p1", ChatTurnID: turnID("chat_1")}
	n.NotifySteeringRequired(context.Background(), task, string(persistence.TaskStatusAwaitingInput))

	if len(ch.sent) != 1 {
		t.Fatalf("want 1 send, got %d", len(ch.sent))
	}
	if len(ch.sent[0].Buttons) != 0 {
		t.Errorf("free-text prompt must have no buttons; got %+v", ch.sent[0].Buttons)
	}
}
