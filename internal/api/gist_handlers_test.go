package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/memory"
)

// stubGistReader is the minimal GistReader for handler tests.
type stubGistReader struct {
	gist *memory.PersistedGist
	err  error
}

func (s *stubGistReader) GetGist(_ context.Context, _ string) (*memory.PersistedGist, error) {
	return s.gist, s.err
}

func newGistServer(r GistReader) *Server {
	return &Server{logger: zerolog.Nop(), gistReader: r}
}

// TestGetProjectGist_Happy — populated row renders ranked terms +
// generated_at + chunks_scanned. Pin the wire shape so the UI /
// CLI consumers don't break on a silent rename.
func TestGetProjectGist_Happy(t *testing.T) {
	when := time.Date(2026, 5, 16, 14, 0, 0, 0, time.UTC)
	r := &stubGistReader{gist: &memory.PersistedGist{
		ProjectID: "assistant",
		Terms: []memory.TermFrequency{
			{Term: "ibkr", Count: 42},
			{Term: "trading", Count: 17},
		},
		ChunksScanned: 128,
		GeneratedAt:   when,
		DurationMs:    37,
	}}
	s := newGistServer(r)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/assistant/gist", nil)
	rec := httptest.NewRecorder()
	s.GetProjectGist(rec, req, "assistant")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got gistResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ProjectID != "assistant" || got.ChunksScanned != 128 || got.DurationMs != 37 {
		t.Errorf("meta wrong: %+v", got)
	}
	if len(got.Terms) != 2 || got.Terms[0].Term != "ibkr" || got.Terms[1].Count != 17 {
		t.Errorf("terms wrong: %+v", got.Terms)
	}
	if !got.GeneratedAt.Equal(when) {
		t.Errorf("generated_at = %v, want %v", got.GeneratedAt, when)
	}
}

// TestGetProjectGist_NotFoundMaps404 — the loop hasn't run yet
// for this project; the API returns 404 GIST_NOT_FOUND so the UI
// can render an empty-state instead of an error toast.
func TestGetProjectGist_NotFoundMaps404(t *testing.T) {
	s := newGistServer(&stubGistReader{err: memory.ErrGistNotFound})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/assistant/gist", nil)
	rec := httptest.NewRecorder()
	s.GetProjectGist(rec, req, "assistant")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "GIST_NOT_FOUND") {
		t.Errorf("body missing error code: %s", rec.Body.String())
	}
}

// TestGetProjectGist_DBErrorMaps500 — a generic repo error must
// surface as 500 DB_ERROR. Distinct from the NOT_FOUND case so
// operators reading logs can tell "no data yet" from "DB blew up".
func TestGetProjectGist_DBErrorMaps500(t *testing.T) {
	s := newGistServer(&stubGistReader{err: errors.New("connection refused")})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/assistant/gist", nil)
	rec := httptest.NewRecorder()
	s.GetProjectGist(rec, req, "assistant")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// TestGetProjectGist_NotConfiguredReturns503 — when the gist
// reader isn't wired (memory subsystem off) we return 503 so the
// caller distinguishes "feature off" from "feature on but
// broken". Operators can grep logs for GIST_NOT_CONFIGURED to
// find deployments that should opt in.
func TestGetProjectGist_NotConfiguredReturns503(t *testing.T) {
	s := newGistServer(nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/assistant/gist", nil)
	rec := httptest.NewRecorder()
	s.GetProjectGist(rec, req, "assistant")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

// TestGetProjectGist_RejectsEmptyProject — defensive: an empty
// project ID would let an attacker probe whether the consolidate
// loop ran for the global bucket. 400 instead.
func TestGetProjectGist_RejectsEmptyProject(t *testing.T) {
	s := newGistServer(&stubGistReader{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects//gist", nil)
	rec := httptest.NewRecorder()
	s.GetProjectGist(rec, req, "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}
