package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// Regression 2026-06-06 (post auth-flip): global (project-less)
// reminders are operator-owned (telegram:…); the API visibility
// filter had an admin bypass for admin KEYS only, so a browser
// session-admin was scoped down to "rows whose operator id equals my
// principal" — i.e. none — and the UI dashboard sharing the same
// semantics rendered "No reminders". A session whose role resolved
// to admin is admin-class and must see every row.
func TestReminderVisibleToRequest_SessionAdminSeesAll(t *testing.T) {
	s := &Server{adminConfig: config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}}
	rem := &persistence.Reminder{ID: "g", OperatorID: "telegram:42"} // global row
	req := httptest.NewRequest(http.MethodGet, "/api/v1/reminders", nil).
		WithContext(stampSessionIdentity("admin"))
	if !s.reminderVisibleToRequest(req, rem) {
		t.Fatal("session-admin must see operator-owned global reminders")
	}
}

// Guard: a plain session user does NOT inherit the admin bypass.
func TestReminderVisibleToRequest_SessionUserStaysScoped(t *testing.T) {
	s := &Server{adminConfig: config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}}
	rem := &persistence.Reminder{ID: "g", OperatorID: "telegram:42"}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/reminders", nil).
		WithContext(stampSessionIdentity("user"))
	if s.reminderVisibleToRequest(req, rem) {
		t.Fatal("foreign operator-owned reminder leaked to a non-admin session")
	}
}

// Task 12 — PauseReminder on a pending row flips it to paused and
// forwards to repo.Pause exactly once.
func TestPauseReminder_PendingRowSucceeds(t *testing.T) {
	repo := &reminderRepoSpy{rows: map[string]*persistence.Reminder{
		"r-1": testReminder("r-1", "", "telegram:42"),
	}}
	s := NewServer(WithLogger(zerolog.Nop()), WithReminderRepository(repo))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/reminders/r-1/pause", nil).
		WithContext(context.WithValue(context.Background(), authEnabledKey, false))
	rec := httptest.NewRecorder()
	s.PauseReminder(rec, req, "r-1")

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(repo.paused) != 1 || repo.paused[0] != "r-1" {
		t.Fatalf("expected repo.Pause called with r-1; got %v", repo.paused)
	}
	var got ReminderEntryJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != string(persistence.ReminderStatusPaused) {
		t.Errorf("status=%q, want paused", got.Status)
	}
}

// PauseReminder surfaces the repo's ErrNotFound (row isn't pending —
// mid-run or already terminal) as 409 Conflict, not a bare 404/500,
// so the CLI can print a friendly "not pausable" message.
func TestPauseReminder_NotPendingReturns409(t *testing.T) {
	repo := &reminderRepoSpy{
		rows:     map[string]*persistence.Reminder{"r-1": testReminder("r-1", "", "telegram:42")},
		pauseErr: persistence.ErrNotFound,
	}
	s := NewServer(WithLogger(zerolog.Nop()), WithReminderRepository(repo))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/reminders/r-1/pause", nil).
		WithContext(context.WithValue(context.Background(), authEnabledKey, false))
	rec := httptest.NewRecorder()
	s.PauseReminder(rec, req, "r-1")

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

// ResumeReminder on a recurring (cron_expr set) row re-arms it and
// calls repo.Resume with a fire time strictly in the future.
func TestResumeReminder_RecurringRowSucceeds(t *testing.T) {
	row := testReminder("r-1", "", "telegram:42")
	row.Status = persistence.ReminderStatusPaused
	row.CronExpr = "0 9 * * 1"
	repo := &reminderRepoSpy{rows: map[string]*persistence.Reminder{"r-1": row}}
	s := NewServer(WithLogger(zerolog.Nop()), WithReminderRepository(repo))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/reminders/r-1/resume", nil).
		WithContext(context.WithValue(context.Background(), authEnabledKey, false))
	rec := httptest.NewRecorder()
	s.ResumeReminder(rec, req, "r-1")

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(repo.resumed) != 1 || repo.resumed[0] != "r-1" {
		t.Fatalf("expected repo.Resume called with r-1; got %v", repo.resumed)
	}
	if len(repo.resumeArgs) != 1 || !repo.resumeArgs[0].After(time.Now()) {
		t.Fatalf("expected Resume's nextFireAt to be in the future; got %v", repo.resumeArgs)
	}
	var got ReminderEntryJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != string(persistence.ReminderStatusPending) {
		t.Errorf("status=%q, want pending", got.Status)
	}
}

// ResumeReminder refuses one-shot reminders (no cron_expr) — there's
// no recurrence rule to re-derive fire_at from.
func TestResumeReminder_OneShotRefused(t *testing.T) {
	row := testReminder("r-1", "", "telegram:42")
	row.Status = persistence.ReminderStatusPaused
	repo := &reminderRepoSpy{rows: map[string]*persistence.Reminder{"r-1": row}}
	s := NewServer(WithLogger(zerolog.Nop()), WithReminderRepository(repo))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/reminders/r-1/resume", nil).
		WithContext(context.WithValue(context.Background(), authEnabledKey, false))
	rec := httptest.NewRecorder()
	s.ResumeReminder(rec, req, "r-1")

	if rec.Code < 400 || rec.Code >= 500 {
		t.Fatalf("status=%d, want 4xx refusal; body=%s", rec.Code, rec.Body.String())
	}
	if len(repo.resumed) != 0 {
		t.Fatalf("one-shot resume should not reach repo.Resume; got %v", repo.resumed)
	}
}

// ResumeReminder surfaces the repo's ErrNotFound (row isn't paused)
// as 409 Conflict.
func TestResumeReminder_NotPausedReturns409(t *testing.T) {
	row := testReminder("r-1", "", "telegram:42")
	row.CronExpr = "0 9 * * 1"
	repo := &reminderRepoSpy{
		rows:      map[string]*persistence.Reminder{"r-1": row},
		resumeErr: persistence.ErrNotFound,
	}
	s := NewServer(WithLogger(zerolog.Nop()), WithReminderRepository(repo))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/reminders/r-1/resume", nil).
		WithContext(context.WithValue(context.Background(), authEnabledKey, false))
	rec := httptest.NewRecorder()
	s.ResumeReminder(rec, req, "r-1")

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

// The reminders router dispatches {id}/pause and {id}/resume to the
// new handlers, not falling through to ShowReminder treating
// "pause"/"resume" as part of the id.
func TestRemindersRouter_PauseAndResumeDispatch(t *testing.T) {
	row := testReminder("r-1", "", "telegram:42")
	row.CronExpr = "0 9 * * 1"
	repo := &reminderRepoSpy{rows: map[string]*persistence.Reminder{"r-1": row}}
	s := NewServer(WithLogger(zerolog.Nop()), WithReminderRepository(repo))

	authOff := context.WithValue(context.Background(), authEnabledKey, false)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/reminders/r-1/pause", nil).WithContext(authOff)
	rec := httptest.NewRecorder()
	s.remindersRouter(rec, req)
	if rec.Code != http.StatusOK || len(repo.paused) != 1 {
		t.Fatalf("router didn't dispatch pause; status=%d paused=%v body=%s", rec.Code, repo.paused, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/reminders/r-1/resume", nil).WithContext(authOff)
	rec = httptest.NewRecorder()
	s.remindersRouter(rec, req)
	if rec.Code != http.StatusOK || len(repo.resumed) != 1 {
		t.Fatalf("router didn't dispatch resume; status=%d resumed=%v body=%s", rec.Code, repo.resumed, rec.Body.String())
	}
}
