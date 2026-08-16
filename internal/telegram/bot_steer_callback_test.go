package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

type fakeChildCanceller struct{ calls []string }

func (f *fakeChildCanceller) CancelChildren(_ context.Context, id string) {
	f.calls = append(f.calls, id)
}

// steerBot builds a bot whose Telegram HTTP calls hit an ok-returning fake
// server, with the given task + task-message repos wired.
func steerBot(t *testing.T, taskRepo persistence.TaskRepository, msgRepo persistence.TaskMessageRepository) *Bot {
	t.Helper()
	b := newBotWithCustomServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	})
	b.taskRepo = taskRepo
	b.taskMessageRepo = msgRepo
	return b
}

func optionsMeta(t *testing.T) []byte {
	t.Helper()
	m, _ := json.Marshal(map[string]any{"options": []map[string]string{
		{"id": "retry", "label": "Retry as-is"},
		{"id": "model_fallback", "label": "Retry on fallback model"},
	}})
	return m
}

// TestSteerCallback_Choice records the tapped option as a checkpoint answer
// (metadata.choice = the option id) and resumes AWAITING_INPUT → QUEUED.
func TestSteerCallback_Choice(t *testing.T) {
	cpID := "cp1"
	var toStatus persistence.TaskStatus
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "p", Status: persistence.TaskStatusAwaitingInput, OpenCheckpointID: &cpID}, nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, to persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			toStatus = to
			return true, nil
		},
	}
	msgRepo := &stubTaskMessageRepo{openByTask: map[string]*persistence.TaskMessage{
		"task_x": {ID: cpID, Metadata: optionsMeta(t)},
	}}
	b := steerBot(t, taskRepo, msgRepo)

	if err := b.handleSteerCallback(context.Background(), 555, 555, "cb", 10, "c", "task_x:1"); err != nil {
		t.Fatalf("handleSteerCallback: %v", err)
	}
	if len(msgRepo.inserts) != 1 {
		t.Fatalf("want 1 recorded answer, got %d", len(msgRepo.inserts))
	}
	ins := msgRepo.inserts[0]
	if ins.MessageKind != persistence.TaskMessageKindAnswer {
		t.Errorf("kind = %s, want answer", ins.MessageKind)
	}
	if !containsSub(string(ins.Metadata), `"choice":"model_fallback"`) {
		t.Errorf("metadata missing chosen option id; got %s", ins.Metadata)
	}
	if toStatus != persistence.TaskStatusQueued {
		t.Errorf("transition target = %s, want QUEUED", toStatus)
	}
}

// TestSteerCallback_Approve resumes an AWAITING_APPROVAL task.
func TestSteerCallback_Approve(t *testing.T) {
	var from []persistence.TaskStatus
	var to persistence.TaskStatus
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "p", Status: persistence.TaskStatusAwaitingApproval}, nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, f []persistence.TaskStatus, t2 persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			from, to = f, t2
			return true, nil
		},
	}
	b := steerBot(t, taskRepo, &stubTaskMessageRepo{})
	if err := b.handleSteerCallback(context.Background(), 555, 555, "cb", 10, "approve", "task_ap"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if len(from) != 1 || from[0] != persistence.TaskStatusAwaitingApproval || to != persistence.TaskStatusQueued {
		t.Errorf("approve transition = %v→%s, want [AWAITING_APPROVAL]→QUEUED", from, to)
	}
}

// TestSteerCallback_Reject cancels the task AND cascades to its children.
func TestSteerCallback_Reject(t *testing.T) {
	var to persistence.TaskStatus
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "p", Status: persistence.TaskStatusAwaitingApproval}, nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, t2 persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			to = t2
			return true, nil
		},
	}
	cc := &fakeChildCanceller{}
	b := steerBot(t, taskRepo, &stubTaskMessageRepo{})
	b.childCanceller = cc
	if err := b.handleSteerCallback(context.Background(), 555, 555, "cb", 10, "reject", "task_rej"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if to != persistence.TaskStatusCancelled {
		t.Errorf("reject transition target = %s, want CANCELLED", to)
	}
	if len(cc.calls) != 1 || cc.calls[0] != "task_rej" {
		t.Errorf("reject must cascade CancelChildren(task_rej); got %v", cc.calls)
	}
}

// TestSteerCallback_AuthDenied — a user not cleared for the task's project
// can't act; no repo mutation happens.
func TestSteerCallback_AuthDenied(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "proj-b", Status: persistence.TaskStatusAwaitingApproval}, nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			t.Fatal("transition must not run for an out-of-scope task")
			return false, nil
		},
	}
	b := steerBot(t, taskRepo, &stubTaskMessageRepo{})
	// Scope user 42 to proj-a only.
	b.config.AllowedUsers = map[int64]UserAccess{42: {Allowed: true, Projects: []string{"proj-a"}}}

	if err := b.handleSteerCallback(context.Background(), 42, 42, "cb", 10, "approve", "task_b"); err != nil {
		t.Fatalf("handleSteerCallback: %v", err)
	}
}

func containsSub(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && indexOfSub(s, sub) >= 0
}
func indexOfSub(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
