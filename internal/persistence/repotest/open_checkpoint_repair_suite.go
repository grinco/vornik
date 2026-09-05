package repotest

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunOpenCheckpointRepairSuite pins the observable contract for a DANGLING
// tasks.open_checkpoint_id: GetOpenCheckpoint reports the absence AND the
// pointer does not survive the call.
//
// Design: https://docs.vornik.io §7.3, which
// found this divergence, normalised only the returned error, and left the
// repair asymmetry explicitly "worth revisiting separately". This is that
// revisit.
//
// WHY A POINTER THAT OUTLIVES ITS MESSAGE MATTERS. It is not cosmetic drift.
// `open_checkpoint_id` is what the inbox and the steering path read to decide a
// task is waiting on a human; a pointer at a message nobody can fetch is a task
// that looks blocked forever while no checkpoint exists to answer. Reporting
// the miss without repairing the row leaves the caller correct and the database
// wrong, and the next reader of the row has no way to tell.
//
// THE TWO BACKENDS MAY SATISFY THIS DIFFERENTLY, AND THAT IS THE POINT OF
// ASSERTING THE CONTRACT RATHER THAN THE MECHANISM. Postgres carries
// `tasks_open_checkpoint_id_fkey ... ON DELETE SET NULL`, so a deleted message
// clears the pointer before GetOpenCheckpoint ever runs. Sqlite runs with
// `foreign_keys(OFF)` project-wide (sqlite.go), so nothing clears it for free
// and the repository must do the repair itself. A suite written against either
// mechanism would pass on one backend and prove nothing on the other.
//
// deleteMessage removes a task_messages row OUT OF BAND — which is the only way
// this state arises, and is inherently backend-specific, so each caller supplies
// it.
func RunOpenCheckpointRepairSuite(
	t *testing.T,
	msgs persistence.TaskMessageRepository,
	tasks persistence.TaskRepository,
	deleteMessage func(id string) error,
) {
	t.Helper()
	t.Run("DanglingPointerReportsAbsence", func(t *testing.T) {
		checkpointDanglingReportsAbsence(t, msgs, tasks, deleteMessage)
	})
	t.Run("DanglingPointerDoesNotSurviveTheRead", func(t *testing.T) {
		checkpointDanglingIsRepaired(t, msgs, tasks, deleteMessage)
	})
	t.Run("LivePointerIsNotDisturbed", func(t *testing.T) {
		checkpointLivePointerSurvives(t, msgs, tasks)
	})
}

// seedDanglingCheckpoint creates a task, opens a checkpoint on it, then deletes
// the checkpoint message out of band. Returns the task id.
func seedDanglingCheckpoint(
	t *testing.T,
	msgs persistence.TaskMessageRepository,
	tasks persistence.TaskRepository,
	deleteMessage func(id string) error,
) string {
	t.Helper()
	ctx := context.Background()
	taskID := seedTaskRow(t, ctx, tasks)

	msg := &persistence.TaskMessage{
		ID:          uniqueID("msg"),
		TaskID:      taskID,
		AuthorKind:  "agent",
		MessageKind: persistence.TaskMessageKindCheckpoint,
		Content:     "which branch should I target?",
		CreatedAt:   time.Now().UTC(),
	}
	if err := msgs.Insert(ctx, msg); err != nil {
		t.Fatalf("Insert checkpoint: %v", err)
	}

	// Precondition, asserted rather than assumed: the insert is what sets the
	// pointer, so if it ever stops doing so this suite would be testing the
	// no-checkpoint path while claiming to test the dangling one.
	before, err := tasks.Get(ctx, taskID)
	if err != nil {
		t.Fatalf("Get task after insert: %v", err)
	}
	if before.OpenCheckpointID == nil || *before.OpenCheckpointID != msg.ID {
		t.Fatalf("precondition: open_checkpoint_id = %v, want %q — the insert no longer opens the checkpoint",
			before.OpenCheckpointID, msg.ID)
	}

	if err := deleteMessage(msg.ID); err != nil {
		t.Fatalf("out-of-band delete of the checkpoint message: %v", err)
	}
	return taskID
}

// A pointer at a message that no longer exists is ABSENCE, like every other
// miss — never a scan error leaking out of the repository.
func checkpointDanglingReportsAbsence(
	t *testing.T,
	msgs persistence.TaskMessageRepository,
	tasks persistence.TaskRepository,
	deleteMessage func(id string) error,
) {
	taskID := seedDanglingCheckpoint(t, msgs, tasks, deleteMessage)

	got, err := msgs.GetOpenCheckpoint(context.Background(), taskID)
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("GetOpenCheckpoint on a dangling pointer returned err=%v, want ErrNotFound", err)
	}
	if got != nil {
		t.Fatalf("GetOpenCheckpoint returned %+v alongside the miss, want nil", got)
	}
}

// THE REPAIR. Reporting the miss and leaving the row wrong makes the caller
// correct and the database a liar: the task still reads as holding an open
// checkpoint that cannot be fetched or answered.
func checkpointDanglingIsRepaired(
	t *testing.T,
	msgs persistence.TaskMessageRepository,
	tasks persistence.TaskRepository,
	deleteMessage func(id string) error,
) {
	ctx := context.Background()
	taskID := seedDanglingCheckpoint(t, msgs, tasks, deleteMessage)

	if _, err := msgs.GetOpenCheckpoint(ctx, taskID); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("GetOpenCheckpoint: err=%v, want ErrNotFound", err)
	}

	after, err := tasks.Get(ctx, taskID)
	if err != nil {
		t.Fatalf("Get task after the miss: %v", err)
	}
	if after.OpenCheckpointID != nil {
		t.Fatalf("open_checkpoint_id = %q after reporting the miss, want NULL — "+
			"the pointer names a message that no longer exists, so the task reads as "+
			"awaiting a checkpoint nobody can answer", *after.OpenCheckpointID)
	}
}

// The repair must be surgical. A read that clears a LIVE pointer would resolve
// checkpoints nobody answered — strictly worse than the bug it fixes.
func checkpointLivePointerSurvives(
	t *testing.T,
	msgs persistence.TaskMessageRepository,
	tasks persistence.TaskRepository,
) {
	ctx := context.Background()
	taskID := seedTaskRow(t, ctx, tasks)

	msg := &persistence.TaskMessage{
		ID:          uniqueID("msg"),
		TaskID:      taskID,
		AuthorKind:  "agent",
		MessageKind: persistence.TaskMessageKindCheckpoint,
		Content:     "still waiting",
		CreatedAt:   time.Now().UTC(),
	}
	if err := msgs.Insert(ctx, msg); err != nil {
		t.Fatalf("Insert checkpoint: %v", err)
	}

	got, err := msgs.GetOpenCheckpoint(ctx, taskID)
	if err != nil {
		t.Fatalf("GetOpenCheckpoint on a live checkpoint: %v", err)
	}
	if got == nil || got.ID != msg.ID {
		t.Fatalf("GetOpenCheckpoint returned %+v, want the checkpoint %q", got, msg.ID)
	}

	after, err := tasks.Get(ctx, taskID)
	if err != nil {
		t.Fatalf("Get task: %v", err)
	}
	if after.OpenCheckpointID == nil || *after.OpenCheckpointID != msg.ID {
		t.Fatalf("open_checkpoint_id = %v after a successful read, want %q — "+
			"a live checkpoint was cleared", after.OpenCheckpointID, msg.ID)
	}
}
