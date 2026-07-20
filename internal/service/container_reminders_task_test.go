package service

// Regression/unit test for Task 7 (scheduled-task-notifications plan):
// containerTaskCreator adapts *taskcreate.Creator to reminders.TaskCreator.
// This test guards the field mapping (ProjectID/Prompt/CreationSource/
// IdempotencyKey/ExtraContext) — a drift here would silently mis-tag
// scheduled tasks or lose the reminder linkage in ExtraContext.

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/reminders"
	"vornik.io/vornik/internal/taskcreate"
)

var errCreateFailed = errors.New("create failed")

type fakeInnerCreator struct{ got taskcreate.Params }

func (f *fakeInnerCreator) Create(_ context.Context, p taskcreate.Params) (*persistence.Task, error) {
	f.got = p
	return &persistence.Task{ID: "task_new"}, nil
}

func TestContainerTaskCreatorMapsParams(t *testing.T) {
	inner := &fakeInnerCreator{}
	adapter := &containerTaskCreator{creator: inner}
	id, err := adapter.CreateScheduledTask(context.Background(), reminders.ScheduledTaskParams{
		ProjectID: "news", Prompt: "digest", TaskType: "research",
		IdempotencyKey: "rem_1:123", ReminderID: "rem_1",
	})
	if err != nil || id != "task_new" {
		t.Fatalf("id=%s err=%v", id, err)
	}
	if inner.got.ProjectID != "news" || inner.got.Prompt != "digest" ||
		inner.got.CreationSource != persistence.TaskCreationSourceScheduled ||
		inner.got.IdempotencyKey != "rem_1:123" {
		t.Fatalf("mapped params wrong: %+v", inner.got)
	}
	if inner.got.ExtraContext["scheduled_reminder_id"] != "rem_1" {
		t.Fatalf("expected scheduled_reminder_id in ExtraContext, got %+v", inner.got.ExtraContext)
	}
}

func TestContainerTaskCreatorPropagatesError(t *testing.T) {
	adapter := &containerTaskCreator{creator: &failingInnerCreator{}}
	id, err := adapter.CreateScheduledTask(context.Background(), reminders.ScheduledTaskParams{
		ProjectID: "news", Prompt: "digest",
	})
	if err == nil || id != "" {
		t.Fatalf("expected error propagated with empty id, got id=%s err=%v", id, err)
	}
}

type failingInnerCreator struct{}

func (f *failingInnerCreator) Create(_ context.Context, _ taskcreate.Params) (*persistence.Task, error) {
	return nil, errCreateFailed
}
