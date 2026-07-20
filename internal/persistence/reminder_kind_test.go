package persistence

import "testing"

func TestReminderKind(t *testing.T) {
	text := &Reminder{Kind: ReminderKindText}
	if text.IsTaskKind() {
		t.Fatal("text reminder must not be task-kind")
	}
	task := &Reminder{Kind: ReminderKindTask}
	if !task.IsTaskKind() {
		t.Fatal("task reminder must be task-kind")
	}
	var nilRem *Reminder
	if nilRem.IsTaskKind() {
		t.Fatal("nil reminder must not panic and must be false")
	}
}

func TestNewRemindersStatusesNonTerminal(t *testing.T) {
	for _, s := range []ReminderStatus{ReminderStatusAwaitingTask, ReminderStatusPaused} {
		if s.IsTerminal() {
			t.Fatalf("status %q must be non-terminal", s)
		}
	}
}
