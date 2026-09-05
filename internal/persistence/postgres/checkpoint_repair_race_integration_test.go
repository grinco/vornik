//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TestRepairDanglingCheckpointPointer_DoesNotEraseALivePointer — regression for
// the 2026-09-03 four-week audit's P1. This is the backend that HAD the bug:
// the repair ran `UPDATE tasks SET open_checkpoint_id = NULL WHERE id = $1`
// with no predicate on the pointer value the triggering read had actually seen,
// while the sqlite twin (written later, to "match postgres") carried the guard
// and named this exact race in its comment.
//
// The read and the repair are two statements. A concurrent Insert opening a NEW
// checkpoint between them gets its LIVE pointer erased by the unguarded repair,
// and the task then silently stops reading as awaiting a human — the failure
// the repair exists to prevent. MarkCheckpointResolved in the same file already
// guarded the analogous race with `AND open_checkpoint_id = $2`.
//
// The interleaving is driven by calling the repair directly with the stale
// pointer the racing read would have been holding, which makes deterministic a
// state a goroutine race would have to be lucky to hit.
func TestRepairDanglingCheckpointPointer_DoesNotEraseALivePointer(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()

	tasks := NewTaskRepository(db.DB)
	msgs := NewTaskMessageRepository(db.DB)

	task := &persistence.Task{
		ID:        uniqueRaceID("task"),
		ProjectID: uniqueRaceID("proj"),
		Priority:  50,
		Payload:   []byte(`{}`),
		Status:    persistence.TaskStatusQueued,
	}
	if err := tasks.Create(ctx, task); err != nil {
		t.Fatalf("Create task: %v", err)
	}

	// The checkpoint the concurrent Insert opened while the racing read was
	// still holding the stale pointer.
	live := &persistence.TaskMessage{
		ID:          uniqueRaceID("msg"),
		TaskID:      task.ID,
		AuthorKind:  "agent",
		MessageKind: persistence.TaskMessageKindCheckpoint,
		Content:     "which branch should I target?",
		CreatedAt:   time.Now().UTC(),
	}
	if err := msgs.Insert(ctx, live); err != nil {
		t.Fatalf("Insert live checkpoint: %v", err)
	}

	msgs.repairDanglingCheckpointPointer(ctx, task.ID, uniqueRaceID("already-deleted"))

	after, err := tasks.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("Get task after the repair: %v", err)
	}
	if after.OpenCheckpointID == nil || *after.OpenCheckpointID != live.ID {
		t.Fatalf("open_checkpoint_id = %v after repairing a DIFFERENT pointer, want %q — "+
			"the repair erased a live checkpoint, so the task stops reading as awaiting a human",
			after.OpenCheckpointID, live.ID)
	}
}

// TestRepairDanglingCheckpointPointer_ClearsTheStalePointer — the guard must
// not cost the repair its job: when the pointer is still the one that was read,
// it is cleared.
func TestRepairDanglingCheckpointPointer_ClearsTheStalePointer(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()

	tasks := NewTaskRepository(db.DB)
	msgs := NewTaskMessageRepository(db.DB)

	task := &persistence.Task{
		ID:        uniqueRaceID("task"),
		ProjectID: uniqueRaceID("proj"),
		Priority:  50,
		Payload:   []byte(`{}`),
		Status:    persistence.TaskStatusQueued,
	}
	if err := tasks.Create(ctx, task); err != nil {
		t.Fatalf("Create task: %v", err)
	}
	msg := &persistence.TaskMessage{
		ID:          uniqueRaceID("msg"),
		TaskID:      task.ID,
		AuthorKind:  "agent",
		MessageKind: persistence.TaskMessageKindCheckpoint,
		Content:     "still waiting",
		CreatedAt:   time.Now().UTC(),
	}
	if err := msgs.Insert(ctx, msg); err != nil {
		t.Fatalf("Insert checkpoint: %v", err)
	}

	msgs.repairDanglingCheckpointPointer(ctx, task.ID, msg.ID)

	after, err := tasks.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("Get task after the repair: %v", err)
	}
	if after.OpenCheckpointID != nil {
		t.Fatalf("open_checkpoint_id = %q, want NULL — the guarded repair no longer repairs",
			*after.OpenCheckpointID)
	}
}

// uniqueRaceID keeps these two cases independent of the shared suites' fixture
// ids, so neither needs resetSuiteTables and a rerun cannot collide with itself.
func uniqueRaceID(prefix string) string {
	return prefix + "-ckpt-race-" + time.Now().UTC().Format("20060102150405.000000000")
}
