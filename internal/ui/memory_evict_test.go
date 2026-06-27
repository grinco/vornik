// Package ui: tests for the hard-eviction surface — form parser,
// action handler, and the audit-log render. The handler delegates
// to a wired MemoryEvictor; stubs in this file satisfy the
// interface without dragging the memory package into the test set.
package ui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// TestParseEvictChunkIDs pins the CSV/newline parser the eviction
// form leans on. Operators paste from clipboards / search results /
// chat transcripts; the parser must absorb mixed delimiters and
// whitespace without silently corrupting an ID.
func TestParseEvictChunkIDs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", []string{}},
		{"single", "chunk_1", []string{"chunk_1"}},
		{"comma-sep", "a,b,c", []string{"a", "b", "c"}},
		{"newline-sep", "a\nb\nc", []string{"a", "b", "c"}},
		{"mixed", "a, b\nc", []string{"a", "b", "c"}},
		{"trailing comma", "a,b,", []string{"a", "b"}},
		{"empty fragment", "a,,b", []string{"a", "b"}},
		{"only whitespace", " , \n , ", []string{}},
		{"whitespace around", " a , b ", []string{"a", "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseEvictChunkIDs(tc.in)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseEvictChunkIDs(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// stubMemoryEvictor satisfies ui.MemoryEvictor for handler tests.
// Captures the last HardEvict call so the test can pin the
// project-scope filter + chunk-ID list + reason + evictedBy.
type stubMemoryEvictor struct {
	lastProject string
	lastIDs     []string
	lastReason  string
	lastBy      string
	deleted     int
	hardErr     error
	auditRows   []MemoryEvictionAuditRow
}

func (s *stubMemoryEvictor) HardEvict(_ context.Context, projectID string, chunkIDs []string, reason, evictedBy string) (int, error) {
	s.lastProject = projectID
	s.lastIDs = chunkIDs
	s.lastReason = reason
	s.lastBy = evictedBy
	if s.hardErr != nil {
		return 0, s.hardErr
	}
	return s.deleted, nil
}

func (s *stubMemoryEvictor) ListEvictionAudits(_ context.Context, _ string, _ int) ([]MemoryEvictionAuditRow, error) {
	return s.auditRows, nil
}

// TestMemoryEvictAction_HappyPath — confirm + chunks + reason all
// thread through to HardEvict, then the page redirects back to
// /ui/memory/<project>. This is the core operator flow.
func TestMemoryEvictAction_HappyPath(t *testing.T) {
	ev := &stubMemoryEvictor{deleted: 2}
	srv := NewServer(WithMemoryEvictor(ev))

	form := url.Values{}
	form.Set("chunks", "chunk_1, chunk_2")
	form.Set("reason", "GDPR DSAR 2026-05-20")
	form.Set("confirm", "yes")
	req := httptest.NewRequest(http.MethodPost, "/ui/memory/janka/evict", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.MemoryEvictAction(rec, req, "janka")

	if rec.Code != http.StatusSeeOther {
		t.Errorf("status: got %d, want 303 (SeeOther); body=%q", rec.Code, rec.Body.String())
	}
	if ev.lastProject != "janka" {
		t.Errorf("project filter mismatch: got %q", ev.lastProject)
	}
	if !reflect.DeepEqual(ev.lastIDs, []string{"chunk_1", "chunk_2"}) {
		t.Errorf("chunk IDs: got %v", ev.lastIDs)
	}
	if ev.lastReason != "GDPR DSAR 2026-05-20" {
		t.Errorf("reason: got %q", ev.lastReason)
	}
	if ev.lastBy == "" {
		t.Error("evictedBy must not be empty (audit row would lose attribution)")
	}
}

// TestMemoryEvictAction_NoConfirm — without confirm=yes the handler
// must refuse. The form's checkbox is the safety gate (mirroring
// the vornikctl CLI --confirm flag); dropping it silently would let
// a misclick destroy data.
func TestMemoryEvictAction_NoConfirm(t *testing.T) {
	ev := &stubMemoryEvictor{}
	srv := NewServer(WithMemoryEvictor(ev))

	form := url.Values{}
	form.Set("chunks", "chunk_1")
	// confirm intentionally omitted
	req := httptest.NewRequest(http.MethodPost, "/ui/memory/janka/evict", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.MemoryEvictAction(rec, req, "janka")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing confirm: got %d, want 400", rec.Code)
	}
	if ev.lastIDs != nil {
		t.Errorf("evictor must NOT have been called without confirm; saw %v", ev.lastIDs)
	}
}

// TestMemoryEvictAction_EmptyChunks — an operator who pastes only
// whitespace must get a clean error, not a silent no-op (which
// would mask a clipboard bug).
func TestMemoryEvictAction_EmptyChunks(t *testing.T) {
	ev := &stubMemoryEvictor{}
	srv := NewServer(WithMemoryEvictor(ev))

	form := url.Values{}
	form.Set("chunks", "   ,   ,   ")
	form.Set("confirm", "yes")
	req := httptest.NewRequest(http.MethodPost, "/ui/memory/janka/evict", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.MemoryEvictAction(rec, req, "janka")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty chunks: got %d, want 400", rec.Code)
	}
}

// TestMemoryEvictAction_NoEvictorWired — without WithMemoryEvictor
// the handler must 503 rather than panic. The route is registered
// regardless of wiring; the safety check belongs in the handler.
func TestMemoryEvictAction_NoEvictorWired(t *testing.T) {
	srv := NewServer() // no WithMemoryEvictor

	form := url.Values{}
	form.Set("chunks", "chunk_1")
	form.Set("confirm", "yes")
	req := httptest.NewRequest(http.MethodPost, "/ui/memory/janka/evict", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.MemoryEvictAction(rec, req, "janka")

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no evictor wired: got %d, want 503", rec.Code)
	}
}

// TestMemoryEvictAction_BackendError — when the evictor returns an
// error the handler must surface a 500 with the error in the body.
// Operators rely on the error text to retry or escalate.
func TestMemoryEvictAction_BackendError(t *testing.T) {
	ev := &stubMemoryEvictor{hardErr: errors.New("simulated DB outage")}
	srv := NewServer(WithMemoryEvictor(ev))

	form := url.Values{}
	form.Set("chunks", "chunk_1")
	form.Set("confirm", "yes")
	req := httptest.NewRequest(http.MethodPost, "/ui/memory/janka/evict", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.MemoryEvictAction(rec, req, "janka")

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("backend error: got %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "simulated DB outage") {
		t.Errorf("backend error message lost; got %q", rec.Body.String())
	}
}

// TestMemoryEvictAction_MethodNotAllowed — GET (and friends) on the
// evict endpoint must 405. Defensive: a GET that triggered eviction
// would let a CSRF prefetch attack delete chunks via just a link.
func TestMemoryEvictAction_MethodNotAllowed(t *testing.T) {
	ev := &stubMemoryEvictor{}
	srv := NewServer(WithMemoryEvictor(ev))

	req := httptest.NewRequest(http.MethodGet, "/ui/memory/janka/evict", nil)
	rec := httptest.NewRecorder()
	srv.MemoryEvictAction(rec, req, "janka")

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET evict: got %d, want 405", rec.Code)
	}
}
