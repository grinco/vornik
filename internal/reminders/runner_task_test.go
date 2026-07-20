package reminders

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
)

// fakeCreator is the TaskCreator test double — captures the params
// the runner hands it at spawn time.
type fakeCreator struct {
	gotProject, gotPrompt, gotType, gotIdem string
	taskID                                  string
	err                                     error
}

func (f *fakeCreator) CreateScheduledTask(_ context.Context, p ScheduledTaskParams) (string, error) {
	f.gotProject, f.gotPrompt, f.gotType, f.gotIdem = p.ProjectID, p.Prompt, p.TaskType, p.IdempotencyKey
	return f.taskID, f.err
}

// TestTaskKindFireCreatesTaskAndReArms covers design §4.1's spawn
// path: a task-kind reminder due at tick time must call the
// Creator with the right params, then re-arm atomically via
// MarkTaskSpawned with a non-nil next fire time (it's recurring).
func TestTaskKindFireCreatesTaskAndReArms(t *testing.T) {
	repo := newStubRepo()
	creator := &fakeCreator{taskID: "task_9"}
	fixed := time.Date(2026, 7, 20, 7, 0, 0, 0, time.UTC)
	r := New(Config{
		Repo: repo, Creator: creator, DefaultTaskType: "research",
		Logger: zerolog.Nop(), Clock: func() time.Time { return fixed },
	})
	rem := &persistence.Reminder{
		ID: "rem_1", Kind: persistence.ReminderKindTask, ProjectID: "news",
		Content: "Daily digest", CronExpr: "0 7 * * *", Status: persistence.ReminderStatusFiring,
	}
	repo.queue = []*persistence.Reminder{rem}

	r.tickOnce(context.Background())

	if creator.gotProject != "news" || creator.gotPrompt != "Daily digest" || creator.gotType != "research" {
		t.Fatalf("creator params wrong: %+v", creator)
	}
	if repo.spawnedTaskID != "task_9" || repo.spawnedID != "rem_1" {
		t.Fatalf("MarkTaskSpawned not called correctly: id=%s task=%s", repo.spawnedID, repo.spawnedTaskID)
	}
	if repo.spawnedNext == nil {
		t.Fatal("recurring reminder must re-arm with a non-nil nextFireAt")
	}
	// Idempotency key format: rem.ID + ":" + FireAt unix seconds. Review
	// finding 3 — assert the exact format the design's idempotency
	// contract relies on (a re-leased slot must derive the same key).
	wantIdem := rem.ID + ":" + strconv.FormatInt(rem.FireAt.Unix(), 10)
	if creator.gotIdem != wantIdem {
		t.Fatalf("gotIdem = %q, want %q", creator.gotIdem, wantIdem)
	}
}

// TestTaskKindOneShotPassesNilNext: a task-kind reminder with no
// CronExpr is one-shot — MarkTaskSpawned must be called with a nil
// nextFireAt so the row terminalizes rather than re-arming.
func TestTaskKindOneShotPassesNilNext(t *testing.T) {
	repo := newStubRepo()
	creator := &fakeCreator{taskID: "task_9"}
	r := New(Config{Repo: repo, Creator: creator, DefaultTaskType: "research", Logger: zerolog.Nop()})
	repo.queue = []*persistence.Reminder{{
		ID: "rem_2", Kind: persistence.ReminderKindTask, ProjectID: "news",
		Content: "one shot", Status: persistence.ReminderStatusFiring, // no CronExpr => one-shot
	}}
	r.tickOnce(context.Background())
	if repo.spawnedNext != nil {
		t.Fatal("one-shot must pass nil nextFireAt")
	}
}

// TestTaskKindCreatorNilMarksErrored: task-kind reminder with no
// Creator wired must not panic — row goes errored so an operator
// notices rather than the heartbeat looping forever.
func TestTaskKindCreatorNilMarksErrored(t *testing.T) {
	repo := newStubRepo()
	r := New(Config{Repo: repo, Logger: zerolog.Nop()})
	repo.queue = []*persistence.Reminder{{
		ID: "rem_3", Kind: persistence.ReminderKindTask, ProjectID: "news",
		Content: "no creator", Status: persistence.ReminderStatusFiring,
	}}
	r.tickOnce(context.Background())
	if _, ok := repo.errored["rem_3"]; !ok {
		t.Fatal("expected rem_3 to be marked errored when no Creator is wired")
	}
}

// TestTaskKindCreatorErrorMarksErrored is the crash-safety guarantee this
// whole task exists for (design §4.1 / finding 1 of the Task 5 review): if
// the task creator fails, the row must be marked errored and MUST NOT be
// re-armed — MarkTaskSpawned is never reached. A crash or a hard failure
// here must leave the row recoverable ('firing'-equivalent), never
// silently re-armed with no task on record. spawnedCalls (a call counter,
// not just the zero-value proxy of spawnedID=="") is the direct
// assertion that MarkTaskSpawned was never invoked.
func TestTaskKindCreatorErrorMarksErrored(t *testing.T) {
	repo := newStubRepo()
	creator := &fakeCreator{err: errors.New("task create failed")}
	r := New(Config{Repo: repo, Creator: creator, Logger: zerolog.Nop()})
	repo.queue = []*persistence.Reminder{{
		ID: "rem_4", Kind: persistence.ReminderKindTask, ProjectID: "news",
		Content: "boom", Status: persistence.ReminderStatusFiring,
	}}
	r.tickOnce(context.Background())
	msg, ok := repo.errored["rem_4"]
	if !ok {
		t.Fatal("expected rem_4 to be marked errored when creator returns an error")
	}
	if msg == "" {
		t.Fatal("error message should not be empty")
	}
	if repo.spawnedCalls != 0 {
		t.Fatalf("MarkTaskSpawned must not be called when task creation failed; spawnedCalls = %d", repo.spawnedCalls)
	}
	if repo.spawnedID != "" {
		t.Fatal("MarkTaskSpawned must not be called when task creation failed")
	}
}

// TestTaskKindReArmCronInvalidMarksErrored mirrors the text-kind
// TestRunner_RecurringInvalidCronAtDeliveryGoesErrored for the task-kind
// path (review finding 2): the task creator succeeds (the task DID run —
// that outcome is not lost), but the recurring row's CronExpr is
// unparseable at re-arm time. The row must go errored with the
// "re-arm cron invalid: ..." message and MarkTaskSpawned must never be
// called — the design accepts that the task ran while the reminder row
// itself surfaces as errored for an operator to fix the cron expression.
func TestTaskKindReArmCronInvalidMarksErrored(t *testing.T) {
	repo := newStubRepo()
	creator := &fakeCreator{taskID: "task_bad_cron"}
	r := New(Config{Repo: repo, Creator: creator, DefaultTaskType: "research", Logger: zerolog.Nop()})
	repo.queue = []*persistence.Reminder{{
		ID: "rem_5", Kind: persistence.ReminderKindTask, ProjectID: "news",
		Content: "weekly digest", CronExpr: "not a cron", Status: persistence.ReminderStatusFiring,
	}}
	r.tickOnce(context.Background())

	msg, ok := repo.errored["rem_5"]
	if !ok {
		t.Fatal("expected rem_5 to be marked errored when re-arm cron is invalid")
	}
	if !strings.Contains(msg, "re-arm cron invalid") {
		t.Fatalf("error message = %q, want it to contain %q", msg, "re-arm cron invalid")
	}
	if repo.spawnedCalls != 0 {
		t.Fatalf("MarkTaskSpawned must not be called when re-arm cron parse fails; spawnedCalls = %d", repo.spawnedCalls)
	}
}
