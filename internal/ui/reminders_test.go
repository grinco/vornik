package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// TestUIReminders_RendersKindColumnAndPauseButton pins Task 13: the
// reminders dashboard must show a KIND column and, for a pending
// task-kind row, a Pause button (and a link to the spawned task).
// Fails pre-change (no Kind/CanPause fields, no template column).
func TestUIReminders_RendersKindColumnAndPauseButton(t *testing.T) {
	rem := uiReminder("a", "project-a")
	rem.Kind = persistence.ReminderKindTask
	rem.LastTaskID = "task-123"
	repo := &uiReminderRepo{rows: map[string]*persistence.Reminder{"a": rem}}
	srv := NewServer(WithReminderRepository(repo))
	req := scopedUIRequest(http.MethodGet, "/reminders", []string{"project-a"})
	rec := httptest.NewRecorder()
	srv.Reminders(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Kind") {
		t.Fatalf("KIND column header missing:\n%s", body)
	}
	if !strings.Contains(body, ">task<") {
		t.Fatalf("task-kind badge missing:\n%s", body)
	}
	if !strings.Contains(body, "/ui/reminders/a/pause") {
		t.Fatalf("pause form action missing for pending row:\n%s", body)
	}
	if !strings.Contains(body, "/ui/tasks/task-123") {
		t.Fatalf("task-detail link missing for task-kind row with last_task_id:\n%s", body)
	}
}

// TestUIReminders_TextRowHasNoPauseWhenPausedAlready asserts a paused
// row renders Resume instead of Pause, and shows the default "text"
// kind label when Kind is unset (pre-migration rows).
func TestUIReminders_PausedRowRendersResumeNotPause(t *testing.T) {
	rem := uiReminder("p", "project-a")
	rem.Status = persistence.ReminderStatusPaused
	rem.CronExpr = "0 9 * * *"
	repo := &uiReminderRepo{rows: map[string]*persistence.Reminder{"p": rem}}
	srv := NewServer(WithReminderRepository(repo))
	req := scopedUIRequest(http.MethodGet, "/reminders", []string{"project-a"})
	rec := httptest.NewRecorder()
	srv.Reminders(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, ">text<") {
		t.Fatalf("empty Kind must render as default 'text':\n%s", body)
	}
	if !strings.Contains(body, "/ui/reminders/p/resume") {
		t.Fatalf("resume form action missing for paused row:\n%s", body)
	}
	if strings.Contains(body, "/ui/reminders/p/pause") {
		t.Fatalf("pause form must not render for an already-paused row:\n%s", body)
	}
}

// TestUIReminderPause_CallsRepoPause pins the happy path: a POST to
// /ui/reminders/{id}/pause on a pending, visible reminder calls
// repo.Pause and redirects.
func TestUIReminderPause_CallsRepoPause(t *testing.T) {
	repo := &uiReminderRepo{rows: map[string]*persistence.Reminder{
		"a": uiReminder("a", "project-a"),
	}}
	srv := NewServer(WithReminderRepository(repo))
	req := scopedUIRequest(http.MethodPost, "/reminders/a/pause", []string{"project-a"})
	rec := httptest.NewRecorder()
	srv.ReminderPause(rec, req, "a")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if len(repo.paused) != 1 || repo.paused[0] != "a" {
		t.Fatalf("expected repo.Pause called for 'a', got %v", repo.paused)
	}
	if repo.rows["a"].Status != persistence.ReminderStatusPaused {
		t.Fatalf("row status = %s, want paused", repo.rows["a"].Status)
	}
}

// TestUIReminderPause_RejectsForeignReminder mirrors the cancel/delete
// ownership tests: a project-A caller must not be able to pause a
// project-B reminder, even though the row exists.
func TestUIReminderPause_RejectsForeignReminder(t *testing.T) {
	repo := &uiReminderRepo{rows: map[string]*persistence.Reminder{
		"b": uiReminder("b", "project-b"),
	}}
	srv := NewServer(WithReminderRepository(repo))
	req := scopedUIRequest(http.MethodPost, "/reminders/b/pause", []string{"project-a"})
	rec := httptest.NewRecorder()
	srv.ReminderPause(rec, req, "b")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(repo.paused) != 0 {
		t.Fatalf("foreign reminder was paused: %v", repo.paused)
	}
}

// TestUIReminderPause_NotPendingIsFriendlyConflict pins the
// ErrNotFound->409 friendly-error mapping for a row that exists but
// isn't in 'pending' state (e.g. mid-run / firing).
func TestUIReminderPause_NotPendingIsFriendlyConflict(t *testing.T) {
	rem := uiReminder("f", "project-a")
	rem.Status = persistence.ReminderStatusFiring
	repo := &uiReminderRepo{rows: map[string]*persistence.Reminder{"f": rem}}
	srv := NewServer(WithReminderRepository(repo))
	req := scopedUIRequest(http.MethodPost, "/reminders/f/pause", []string{"project-a"})
	rec := httptest.NewRecorder()
	srv.ReminderPause(rec, req, "f")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "pause") {
		t.Fatalf("expected a friendly pause-failure message, got: %s", rec.Body.String())
	}
}

// TestUIReminderResume_CallsRepoResume pins the happy path: a paused
// recurring reminder resumes via repo.Resume with a recomputed
// fire_at, and redirects.
func TestUIReminderResume_CallsRepoResume(t *testing.T) {
	rem := uiReminder("p", "project-a")
	rem.Status = persistence.ReminderStatusPaused
	rem.CronExpr = "0 9 * * *"
	repo := &uiReminderRepo{rows: map[string]*persistence.Reminder{"p": rem}}
	srv := NewServer(WithReminderRepository(repo))
	req := scopedUIRequest(http.MethodPost, "/reminders/p/resume", []string{"project-a"})
	rec := httptest.NewRecorder()
	srv.ReminderResume(rec, req, "p")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if len(repo.resumed) != 1 || repo.resumed[0] != "p" {
		t.Fatalf("expected repo.Resume called for 'p', got %v", repo.resumed)
	}
	if repo.rows["p"].Status != persistence.ReminderStatusPending {
		t.Fatalf("row status = %s, want pending", repo.rows["p"].Status)
	}
}

// TestUIReminderResume_OneShotRejected pins the one-shot guard: a
// reminder with no cron_expr can't be resumed (there's no recurrence
// rule to re-derive fire_at from), and repo.Resume must never be
// called in that case.
func TestUIReminderResume_OneShotRejected(t *testing.T) {
	rem := uiReminder("o", "project-a")
	rem.Status = persistence.ReminderStatusPaused
	rem.CronExpr = ""
	repo := &uiReminderRepo{rows: map[string]*persistence.Reminder{"o": rem}}
	srv := NewServer(WithReminderRepository(repo))
	req := scopedUIRequest(http.MethodPost, "/reminders/o/resume", []string{"project-a"})
	rec := httptest.NewRecorder()
	srv.ReminderResume(rec, req, "o")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if len(repo.resumed) != 0 {
		t.Fatalf("one-shot reminder must not call repo.Resume: %v", repo.resumed)
	}
}

// TestUIReminderResume_RejectsForeignReminder mirrors the pause/cancel
// ownership tests for the resume surface.
func TestUIReminderResume_RejectsForeignReminder(t *testing.T) {
	rem := uiReminder("b", "project-b")
	rem.Status = persistence.ReminderStatusPaused
	rem.CronExpr = "0 9 * * *"
	repo := &uiReminderRepo{rows: map[string]*persistence.Reminder{"b": rem}}
	srv := NewServer(WithReminderRepository(repo))
	req := scopedUIRequest(http.MethodPost, "/reminders/b/resume", []string{"project-a"})
	rec := httptest.NewRecorder()
	srv.ReminderResume(rec, req, "b")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(repo.resumed) != 0 {
		t.Fatalf("foreign reminder was resumed: %v", repo.resumed)
	}
}
