package service

// End-to-end regression tests for the /ui/fixit wiring site.
//
// Incident (2026-07-12, post-2026.7.2): the reload banner's "Help me
// fix this" link (/ui/fixit/failed_reload/daemon) rendered "Fix-It
// Doctor is not configured on this deployment" on EVERY deployment —
// task 3.4 shipped the entry-point links and container_http.go wired
// the doctor into the API server (api.WithFixItDoctor), but the UI
// server options ui.WithFixItDoctor + ui.WithFixItSessionReader were
// never called, so ui.Server.fixItDoctor was nil by construction.
// These tests drive the REAL NewContainer boot path (same DB-free
// SQLite + chat recipe as composer_bridge_wiring_test.go) and assert
// the rendered /ui/fixit panel, so a future refactor that drops either
// uiOpts line fails a real test.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
)

// TestNewContainer_WiresFixItDoctorIntoUIServer asserts that a full
// boot with chat configured (ChatClient non-nil) and a SQLite store
// (FixItSessions repo non-nil) serves a CONFIGURED fix-it panel on the
// exact URL the reload banner links to (config_reload.go's FixItHref).
func TestNewContainer_WiresFixItDoctorIntoUIServer(t *testing.T) {
	cfg := newComposerWiringTestConfig(t)

	c, err := NewContainer(cfg, isolatedConfigPath(t))
	if err != nil {
		t.Fatalf("NewContainer: unexpected error: %v", err)
	}
	if c.ChatClient == nil {
		t.Fatal("precondition: chat is configured, ChatClient must be non-nil")
	}
	if c.repos.FixItSessions == nil {
		t.Fatal("precondition: SQLite store must provide a FixItSessions repo")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ui/fixit/failed_reload/daemon", nil)
	c.HTTPServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/fixit/failed_reload/daemon: status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "not configured on this deployment") {
		t.Error("panel rendered the unconfigured notice — ui.WithFixItDoctor is not wired in initHTTPServer")
	}
}

// TestNewContainer_WiresFixItSessionReaderIntoUIServer asserts the
// transcript-resume source is wired: a persisted session's transcript
// must render when the panel is opened with ?session=<id> by the same
// (single-tenant fallback) operator. Without ui.WithFixItSessionReader
// the resume path silently falls back to a fresh page and the marker
// never appears.
func TestNewContainer_WiresFixItSessionReaderIntoUIServer(t *testing.T) {
	cfg := newComposerWiringTestConfig(t)

	c, err := NewContainer(cfg, isolatedConfigPath(t))
	if err != nil {
		t.Fatalf("NewContainer: unexpected error: %v", err)
	}

	const marker = "MARKER-fixit-resume-transcript-9f3a"
	transcript, err := json.Marshal([]map[string]string{
		{"role": "user", "content": marker},
	})
	if err != nil {
		t.Fatalf("marshal transcript: %v", err)
	}
	session := &persistence.FixItSession{
		ID:           persistence.GenerateID("fix"),
		OperatorID:   api.SingleTenantOperatorIDFromConfig(cfg),
		FailureKind:  "failed_reload",
		FailureRefID: "daemon",
		Transcript:   transcript,
	}
	if err := c.repos.FixItSessions.Insert(context.Background(), session); err != nil {
		t.Fatalf("insert fixit session: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ui/fixit/failed_reload/daemon?session="+session.ID, nil)
	c.HTTPServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET with ?session=: status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), marker) {
		t.Error("resumed transcript not rendered — ui.WithFixItSessionReader is not wired in initHTTPServer")
	}
}
