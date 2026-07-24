package ui

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/taintlineage"
)

func uiTaintCheckpointMsg(id, hash string) *persistence.TaskMessage {
	meta, _ := json.Marshal(map[string]any{
		"kind": "decision",
		"decision": map[string]any{
			"kind":            "untrusted_review",
			"reason":          "tainted_write",
			"source_set_hash": hash,
		},
		"options": []map[string]any{
			{"id": "allow", "label": "Reviewed — resume & allow"},
			{"id": "cancel", "label": "Block (cancel)"},
		},
	})
	return &persistence.TaskMessage{ID: id, MessageKind: persistence.TaskMessageKindCheckpoint, Metadata: meta}
}

// The UI twin: a non-admin "allow" is refused (auth-on by default → no admin
// session), leaving no latch recorded (§9 no-self-serving-clear). The admin
// happy-path (latch + resume) is covered by the API twin, which shares the
// exact metadata/latch shapes via internal/taintlineage.
func TestUIAnswerCheckpoint_TaintAllow_NonAdminRejected(t *testing.T) {
	open := "cp1"
	taskRepo := &mocks.MockTaskRepository{
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return true, nil
		},
	}
	msgRepo := &uiTcStubMsgRepo{checkpoint: uiTaintCheckpointMsg(open, "hash-ui")}
	srv := NewServer(WithTaskRepository(taskRepo), WithTaskMessageRepository(msgRepo))
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusAwaitingInput, OpenCheckpointID: &open}

	form := url.Values{"checkpoint_id": []string{"cp1"}, "choice": []string{"allow"}}
	req := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Default request context: IsAuthEnabledFromContext returns true (auth on),
	// no admin session → non-admin.

	if got := srv.uiAnswerCheckpoint(context.Background(), task, req); got != "taint-admin-required" {
		t.Fatalf("got %q, want taint-admin-required", got)
	}
	for _, m := range msgRepo.inserts {
		if _, ok := taintlineage.ParseLatchHash(m.Metadata); ok {
			t.Fatalf("no latch may be recorded on a refused non-admin allow")
		}
	}
}

// The UI twin: "cancel" blocks the write (AWAITING_INPUT→CANCELLED).
func TestUIAnswerCheckpoint_TaintCancel_Cancels(t *testing.T) {
	open := "cp1"
	var toStatus persistence.TaskStatus
	taskRepo := &mocks.MockTaskRepository{
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, to persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			toStatus = to
			return true, nil
		},
	}
	msgRepo := &uiTcStubMsgRepo{checkpoint: uiTaintCheckpointMsg(open, "abc")}
	srv := NewServer(WithTaskRepository(taskRepo), WithTaskMessageRepository(msgRepo))
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusAwaitingInput, OpenCheckpointID: &open}

	form := url.Values{"checkpoint_id": []string{"cp1"}, "choice": []string{"cancel"}}
	req := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if got := srv.uiAnswerCheckpoint(context.Background(), task, req); got != "task-cancelled" {
		t.Fatalf("got %q, want task-cancelled", got)
	}
	if toStatus != persistence.TaskStatusCancelled {
		t.Fatalf("cancel must transition to CANCELLED, got %v", toStatus)
	}
}
