package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/admin"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
)

type uiReminderRepo struct {
	rows      map[string]*persistence.Reminder
	cancelled []string
	deleted   []string
}

func (r *uiReminderRepo) Insert(context.Context, *persistence.Reminder) error { return nil }
func (r *uiReminderRepo) Get(_ context.Context, id string) (*persistence.Reminder, error) {
	rem, ok := r.rows[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	cp := *rem
	return &cp, nil
}
func (r *uiReminderRepo) List(context.Context, persistence.ReminderListFilter) ([]*persistence.Reminder, error) {
	out := make([]*persistence.Reminder, 0, len(r.rows))
	for _, rem := range r.rows {
		cp := *rem
		out = append(out, &cp)
	}
	return out, nil
}
func (r *uiReminderRepo) LeaseDue(context.Context, time.Time, int) ([]*persistence.Reminder, error) {
	return nil, nil
}
func (r *uiReminderRepo) MarkFired(context.Context, string) error           { return nil }
func (r *uiReminderRepo) MarkErrored(context.Context, string, string) error { return nil }
func (r *uiReminderRepo) Reschedule(context.Context, string, time.Time) error {
	return nil
}
func (r *uiReminderRepo) MarkExpired(context.Context, string) error { return nil }
func (r *uiReminderRepo) Cancel(_ context.Context, id string) error {
	r.cancelled = append(r.cancelled, id)
	return nil
}
func (r *uiReminderRepo) Delete(_ context.Context, id string) error {
	if _, ok := r.rows[id]; !ok {
		return persistence.ErrNotFound
	}
	r.deleted = append(r.deleted, id)
	delete(r.rows, id)
	return nil
}
func (r *uiReminderRepo) CountPendingByOperator(context.Context, string) (int, error) {
	return 0, nil
}
func (r *uiReminderRepo) UpdateFields(context.Context, string, time.Time, string) error {
	return nil
}

func TestUIReminders_FiltersByScopedAPIKey(t *testing.T) {
	repo := &uiReminderRepo{rows: map[string]*persistence.Reminder{
		"a": uiReminder("a", "project-a"),
		"b": uiReminder("b", "project-b"),
	}}
	srv := NewServer(WithReminderRepository(repo))
	req := scopedUIRequest(http.MethodGet, "/reminders", []string{"project-a"})
	rec := httptest.NewRecorder()
	srv.Reminders(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "reminder-a") {
		t.Fatalf("scoped reminder missing: %s", body)
	}
	if strings.Contains(body, "reminder-b") {
		t.Fatalf("foreign reminder leaked: %s", body)
	}
}

func TestUIReminderCancel_RejectsForeignReminder(t *testing.T) {
	repo := &uiReminderRepo{rows: map[string]*persistence.Reminder{
		"b": uiReminder("b", "project-b"),
	}}
	srv := NewServer(WithReminderRepository(repo))
	req := scopedUIRequest(http.MethodPost, "/reminders/b/cancel", []string{"project-a"})
	rec := httptest.NewRecorder()
	srv.ReminderCancel(rec, req, "b")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", rec.Code)
	}
	if len(repo.cancelled) != 0 {
		t.Fatalf("foreign reminder was cancelled: %v", repo.cancelled)
	}
}

// TestUIReminderDelete_PhysicallyRemovesRow pins the B-12 UI
// surface: POST /ui/reminders/{id}/delete physically removes
// the row (distinct from cancel which only flips status).
func TestUIReminderDelete_PhysicallyRemovesRow(t *testing.T) {
	repo := &uiReminderRepo{rows: map[string]*persistence.Reminder{
		"a": uiReminder("a", "project-a"),
	}}
	srv := NewServer(WithReminderRepository(repo))
	req := scopedUIRequest(http.MethodPost, "/reminders/a/delete", []string{"project-a"})
	rec := httptest.NewRecorder()
	srv.ReminderDelete(rec, req, "a")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, want 303 redirect; body=%s", rec.Code, rec.Body.String())
	}
	if len(repo.deleted) != 1 || repo.deleted[0] != "a" {
		t.Fatalf("expected 'a' in deleted list, got %v", repo.deleted)
	}
	if _, exists := repo.rows["a"]; exists {
		t.Fatal("row 'a' must be gone from the underlying store after delete")
	}
}

// TestUIReminderRedirect_BackToReferer pins the fix for the bug where
// deleting/cancelling a reminder from /ui/reminders bounced the operator to
// /ui/projects. Browsers send Referer as an ABSOLUTE URL, so the old
// strings.HasPrefix(ref, "/ui/") guard never matched and always fell back.
func TestUIReminderRedirect_BackToReferer(t *testing.T) {
	const referer = "https://vornik.example:8080/ui/reminders?status=fired"
	const wantDest = "/ui/reminders?status=fired"

	t.Run("delete returns to the reminders page", func(t *testing.T) {
		repo := &uiReminderRepo{rows: map[string]*persistence.Reminder{"a": uiReminder("a", "project-a")}}
		srv := NewServer(WithReminderRepository(repo))
		req := scopedUIRequest(http.MethodPost, "/reminders/a/delete", []string{"project-a"})
		req.Header.Set("Referer", referer)
		rec := httptest.NewRecorder()
		srv.ReminderDelete(rec, req, "a")
		if got := rec.Header().Get("Location"); got != wantDest {
			t.Fatalf("Location=%q, want %q (back to the originating reminders page, not /ui/projects)", got, wantDest)
		}
	})

	t.Run("cancel returns to the reminders page", func(t *testing.T) {
		repo := &uiReminderRepo{rows: map[string]*persistence.Reminder{"a": uiReminder("a", "project-a")}}
		srv := NewServer(WithReminderRepository(repo))
		req := scopedUIRequest(http.MethodPost, "/reminders/a/cancel", []string{"project-a"})
		req.Header.Set("Referer", referer)
		rec := httptest.NewRecorder()
		srv.ReminderCancel(rec, req, "a")
		if got := rec.Header().Get("Location"); got != wantDest {
			t.Fatalf("Location=%q, want %q", got, wantDest)
		}
	})

	t.Run("empty Referer falls back to /ui/projects", func(t *testing.T) {
		repo := &uiReminderRepo{rows: map[string]*persistence.Reminder{"a": uiReminder("a", "project-a")}}
		srv := NewServer(WithReminderRepository(repo))
		req := scopedUIRequest(http.MethodPost, "/reminders/a/delete", []string{"project-a"})
		rec := httptest.NewRecorder()
		srv.ReminderDelete(rec, req, "a")
		if got := rec.Header().Get("Location"); got != "/ui/projects" {
			t.Fatalf("Location=%q, want /ui/projects fallback when no Referer", got)
		}
	})

	t.Run("off-site Referer is reduced to its local path (no open redirect)", func(t *testing.T) {
		repo := &uiReminderRepo{rows: map[string]*persistence.Reminder{"a": uiReminder("a", "project-a")}}
		srv := NewServer(WithReminderRepository(repo))
		req := scopedUIRequest(http.MethodPost, "/reminders/a/delete", []string{"project-a"})
		req.Header.Set("Referer", "https://evil.example/phish")
		rec := httptest.NewRecorder()
		srv.ReminderDelete(rec, req, "a")
		// Non-/ui/ path → fallback; never the attacker's host.
		got := rec.Header().Get("Location")
		if got != "/ui/projects" {
			t.Fatalf("Location=%q, want /ui/projects (off-site / non-ui Referer must not be honoured)", got)
		}
	})
}

// TestUIReminderDelete_RejectsForeignReminder mirrors the cancel
// scope test — a project-A operator cannot delete a project-B
// reminder, even via the UI POST surface.
func TestUIReminderDelete_RejectsForeignReminder(t *testing.T) {
	repo := &uiReminderRepo{rows: map[string]*persistence.Reminder{
		"b": uiReminder("b", "project-b"),
	}}
	srv := NewServer(WithReminderRepository(repo))
	req := scopedUIRequest(http.MethodPost, "/reminders/b/delete", []string{"project-a"})
	rec := httptest.NewRecorder()
	srv.ReminderDelete(rec, req, "b")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", rec.Code)
	}
	if len(repo.deleted) != 0 {
		t.Fatalf("foreign reminder was deleted: %v", repo.deleted)
	}
	if _, exists := repo.rows["b"]; !exists {
		t.Fatal("row 'b' must remain in the store after rejected delete")
	}
}

// TestUIReminderDelete_DeleteAfterCancelWipesTerminalRow proves
// the operator workflow that motivated B-12: cancel a stale
// pending reminder, then physically remove it via the new
// Delete button. The combined flow leaves zero rows.
func TestUIReminderDelete_DeleteAfterCancelWipesTerminalRow(t *testing.T) {
	rem := uiReminder("a", "project-a")
	rem.Status = persistence.ReminderStatusCancelled // pre-cancelled
	repo := &uiReminderRepo{rows: map[string]*persistence.Reminder{
		"a": rem,
	}}
	srv := NewServer(WithReminderRepository(repo))
	req := scopedUIRequest(http.MethodPost, "/reminders/a/delete", []string{"project-a"})
	rec := httptest.NewRecorder()
	srv.ReminderDelete(rec, req, "a")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if len(repo.rows) != 0 {
		t.Fatalf("store must be empty after delete of terminal row; got %d row(s)", len(repo.rows))
	}
}

// Regression: auth-disabled deployments (single-tenant local
// installs) used to show an empty reminders page because the
// scope filter dropped every row when there was no API-key
// principal. Mirrors the API-side fix.
func TestUIReminders_AuthDisabledShowsAllRows(t *testing.T) {
	repo := &uiReminderRepo{rows: map[string]*persistence.Reminder{
		"a": uiReminder("a", "project-a"),
		"b": uiReminder("b", "project-b"),
		"g": uiReminder("g", ""),
	}}
	srv := NewServer(WithReminderRepository(repo))
	req := authOffUIRequest(http.MethodGet, "/reminders")
	rec := httptest.NewRecorder()
	srv.Reminders(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, id := range []string{"reminder-a", "reminder-b", "reminder-g"} {
		if !strings.Contains(body, id) {
			t.Fatalf("auth-off page missing %s; body=%s", id, body)
		}
	}
}

// Regression 2026-06-06 (post auth-flip): the reminders dashboard
// rendered "No reminders" for an admin browsing with auth ENABLED.
// Global (project-less) reminders are operator-owned (telegram:…);
// a session-admin's principal never equals that operator id, and the
// UI filter had no admin bypass at all (the API filter had one for
// admin KEYS only). An admin-class request — admin.Middleware stamps
// IsAdmin on every UI page for admin keys AND session-admins — must
// see every row.
func TestUIReminders_AdminSeesAllRows(t *testing.T) {
	repo := &uiReminderRepo{rows: map[string]*persistence.Reminder{
		"a": uiReminder("a", "project-a"),
		"g": uiReminder("g", ""), // global, operator-owned — the live break
	}}
	srv := NewServer(WithReminderRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/reminders", nil)
	ctx := api.ContextWithScopeForTesting(req.Context()) // auth ON, no project scope
	ctx = admin.ContextWithAdmin(ctx, "session:admin")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.Reminders(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, id := range []string{"reminder-a", "reminder-g"} {
		if !strings.Contains(body, id) {
			t.Fatalf("admin page missing %s", id)
		}
	}
}

// Guard: a non-admin authenticated caller still must NOT see foreign
// operator-owned global rows — the admin bypass must not widen
// regular-operator visibility.
func TestUIReminders_NonAdminStillScoped(t *testing.T) {
	repo := &uiReminderRepo{rows: map[string]*persistence.Reminder{
		"g": uiReminder("g", ""), // owned by telegram:42
	}}
	srv := NewServer(WithReminderRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/reminders", nil)
	req = req.WithContext(api.ContextWithScopeForTesting(req.Context()))
	rec := httptest.NewRecorder()
	srv.Reminders(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "reminder-g") {
		t.Fatal("foreign operator-owned reminder leaked to a non-admin caller")
	}
}

// filteringReminderRepo honours ProjectID / OperatorID / PageSize like a
// real DB query (the rows-map uiReminderRepo deliberately ignores the
// filter). Used to prove the under-display fix: a global page filled by
// other projects must not hide a scoped caller's own rows.
type filteringReminderRepo struct {
	uiReminderRepo
	all []*persistence.Reminder
}

func (r *filteringReminderRepo) List(_ context.Context, f persistence.ReminderListFilter) ([]*persistence.Reminder, error) {
	var out []*persistence.Reminder
	for _, rem := range r.all {
		if f.ProjectID != "" && rem.ProjectID != f.ProjectID {
			continue
		}
		if f.OperatorID != "" && rem.OperatorID != f.OperatorID {
			continue
		}
		cp := *rem
		out = append(out, &cp)
	}
	if f.PageSize > 0 && len(out) > f.PageSize {
		out = out[:f.PageSize] // simulate ORDER BY … LIMIT N
	}
	return out, nil
}

// TestUIReminders_ScopedUserSeesOwnRowsPastGlobalCap pins the
// cross-project visibility scope audit follow-up: a scoped session with
// no explicit ?project must query its
// own project(s) directly, not the global latest-N slice that other
// projects' reminders dominate.
func TestUIReminders_ScopedUserSeesOwnRowsPastGlobalCap(t *testing.T) {
	var all []*persistence.Reminder
	// A full page (200) of project-b reminders ordered ahead of the
	// caller's single project-a row.
	for i := 0; i < 200; i++ {
		all = append(all, uiReminder("b"+string(rune('A'+i%26))+string(rune('0'+i%10)), "project-b"))
	}
	all = append(all, uiReminder("mine", "project-a"))
	repo := &filteringReminderRepo{all: all}
	srv := NewServer(WithReminderRepository(repo))

	req := scopedUIRequest(http.MethodGet, "/reminders", []string{"project-a"})
	rec := httptest.NewRecorder()
	srv.Reminders(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "reminder-mine") {
		t.Fatalf("scoped caller's own reminder must be visible past the global page cap:\n%s", body)
	}
	if strings.Contains(body, "project-b") {
		t.Fatal("foreign-project reminders leaked into a scoped reminders page")
	}
}

// authOffUIRequest builds a request that passes through the
// auth middleware with auth disabled, so the auth-enabled flag
// is stamped on the context exactly like a real auth-off
// deployment.
func authOffUIRequest(method, target string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	var captured *http.Request
	api.AuthMiddleware(api.AuthConfig{Enabled: false})(
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			captured = r
		}),
	).ServeHTTP(httptest.NewRecorder(), req)
	if captured == nil {
		return req
	}
	return captured
}

func uiReminder(id, projectID string) *persistence.Reminder {
	return &persistence.Reminder{
		ID:         id,
		OperatorID: "telegram:42",
		Channel:    "telegram",
		ChannelRef: "42",
		ProjectID:  projectID,
		FireAt:     time.Now().Add(time.Hour),
		Content:    "reminder-" + id,
		Status:     persistence.ReminderStatusPending,
		CreatedAt:  time.Now(),
		CreatedVia: "test",
	}
}
