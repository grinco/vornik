package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

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
