package sqlite

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TestRepairDanglingCheckpointPointer_DoesNotEraseALivePointer — regression for
// the 2026-09-03 four-week audit's P1: the dangling-pointer repair must clear
// tasks.open_checkpoint_id ONLY when it still holds the value the read that
// triggered the repair actually saw.
//
// GetOpenCheckpoint reads the pointer and repairs it in two separate
// statements. A concurrent Insert opening a NEW checkpoint lands between them,
// and an unguarded `WHERE id = ?` repair erases that live pointer: the task
// then silently stops reading as awaiting a human, which is the very failure
// the repair exists to prevent. Only the postgres backend was unguarded; this
// asserts the sqlite side too, so the parity cannot regress on either.
//
// The interleaving is driven by calling the repair directly with the stale
// pointer the racing read would have been holding — the state a goroutine race
// would have to be lucky to produce, made deterministic.
func TestRepairDanglingCheckpointPointer_DoesNotEraseALivePointer(t *testing.T) {
	ctx := context.Background()
	db, err := Connect(ctx, Config{Path: ":memory:"})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	tasks := NewTaskRepository(db.DB)
	msgs := NewTaskMessageRepository(db.DB)

	task := &persistence.Task{
		ID:        "task-repair-race",
		ProjectID: "proj-repair-race",
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
		ID:          "msg-live-checkpoint",
		TaskID:      task.ID,
		AuthorKind:  "agent",
		MessageKind: persistence.TaskMessageKindCheckpoint,
		Content:     "which branch should I target?",
		CreatedAt:   time.Now().UTC(),
	}
	if err := msgs.Insert(ctx, live); err != nil {
		t.Fatalf("Insert live checkpoint: %v", err)
	}

	msgs.repairDanglingCheckpointPointer(ctx, task.ID, "msg-already-deleted")

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
	ctx := context.Background()
	db, err := Connect(ctx, Config{Path: ":memory:"})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	tasks := NewTaskRepository(db.DB)
	msgs := NewTaskMessageRepository(db.DB)

	task := &persistence.Task{
		ID:        "task-repair-clears",
		ProjectID: "proj-repair-clears",
		Priority:  50,
		Payload:   []byte(`{}`),
		Status:    persistence.TaskStatusQueued,
	}
	if err := tasks.Create(ctx, task); err != nil {
		t.Fatalf("Create task: %v", err)
	}
	msg := &persistence.TaskMessage{
		ID:          "msg-doomed-checkpoint",
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
