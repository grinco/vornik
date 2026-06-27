// HTTP-level coverage for the simple read-only handlers in the
// api package. These handlers' control flow is mostly guard
// branches (method-not-allowed, dependency-not-wired, validation)
// and a happy path that defers to an injected interface. The
// tests hit every guard plus the happy path with a stub.

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// TestIsKnownTaskStatus pins every enum the handler accepts plus
// the rejection path. Defends the operator-facing 400 message
// that fires when a typo'd ?status= filter would otherwise hit the
// DB and trip a 500 from the check constraint.
func TestIsKnownTaskStatus(t *testing.T) {
	good := []persistence.TaskStatus{
		persistence.TaskStatusPending,
		persistence.TaskStatusQueued,
		persistence.TaskStatusLeased,
		persistence.TaskStatusRunning,
		persistence.TaskStatusWaitingForChildren,
		persistence.TaskStatusCompleted,
		persistence.TaskStatusFailed,
		persistence.TaskStatusCancelled,
	}
	for _, v := range good {
		if !isKnownTaskStatus(v) {
			t.Errorf("isKnownTaskStatus(%q) = false, want true", v)
		}
	}
	bad := []persistence.TaskStatus{
		persistence.TaskStatus(""),
		persistence.TaskStatus("completed"),
		persistence.TaskStatus("RUNNING "),
		persistence.TaskStatus("unknown"),
	}
	for _, v := range bad {
		if isKnownTaskStatus(v) {
			t.Errorf("isKnownTaskStatus(%q) = true, want false", v)
		}
	}
}

// TestPlayback_FullCorpus hits GET /api/v1/playbook with no class.
// Returns the {entries:[...]} envelope. Coverage for the
// no-class branch + the JSON envelope shape.
func TestPlayback_FullCorpus(t *testing.T) {
	s := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/playbook", nil)
	rr := httptest.NewRecorder()
	s.Playbook(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	if _, ok := got["entries"]; !ok {
		t.Errorf("response missing 'entries' key: %v", got)
	}
}

// TestPlayback_PerClass hits the /api/v1/playbook/{class} branch.
// A known class returns a non-empty body; an unknown class falls
// through to playbook.Lookup which the corpus package itself
// handles (typically returns a sensible fallback).
func TestPlayback_PerClass(t *testing.T) {
	s := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/playbook/RATE_LIMITED", nil)
	rr := httptest.NewRecorder()
	s.Playbook(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if rr.Body.Len() == 0 {
		t.Error("body is empty — handler should return a JSON document")
	}
}

// TestPlayback_RejectsNonGet pins the method gate.
func TestPlayback_RejectsNonGet(t *testing.T) {
	s := NewServer()
	for _, m := range []string{http.MethodPost, http.MethodDelete, http.MethodPut} {
		req := httptest.NewRequest(m, "/api/v1/playbook", nil)
		rr := httptest.NewRecorder()
		s.Playbook(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", m, rr.Code)
		}
	}
}

// TestMemoryStats_Disabled covers the "stats not wired" branch.
// Without a MemoryStatsProvider injected, the handler 503s with
// a typed error envelope.
func TestMemoryStats_Disabled(t *testing.T) {
	s := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/memory/stats", nil)
	rr := httptest.NewRecorder()
	s.MemoryStats(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "MEMORY_DISABLED") {
		t.Errorf("body should mention MEMORY_DISABLED, got %q", rr.Body.String())
	}
}

// TestMemoryStats_RejectsNonGet pins the method gate.
func TestMemoryStats_RejectsNonGet(t *testing.T) {
	s := NewServer()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/memory/stats", nil)
	rr := httptest.NewRecorder()
	s.MemoryStats(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}

// TestMemoryStats_HappyPath covers the wired-provider path. A
// stub MemoryStatsProvider returns one project so the response
// envelope shape (projects + total) is checkable.
func TestMemoryStats_HappyPath(t *testing.T) {
	stub := stubMemoryStats{rows: []MemoryProjectStats{
		{ProjectID: "p1"},
	}}
	s := NewServer(WithMemoryStats(stub))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/memory/stats", nil)
	rr := httptest.NewRecorder()
	s.MemoryStats(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200, body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Projects []MemoryProjectStats `json:"projects"`
		Total    int                  `json:"total"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 || len(resp.Projects) != 1 {
		t.Errorf("got total=%d projects=%d, want 1/1", resp.Total, len(resp.Projects))
	}
}

func TestMemoryStats_ScopedKeyFiltersProjects(t *testing.T) {
	stub := stubMemoryStats{rows: []MemoryProjectStats{
		{ProjectID: "p1"},
		{ProjectID: "p2"},
	}}
	s := NewServer(WithMemoryStats(stub))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/memory/stats", nil)
	ctx := context.WithValue(req.Context(), authEnabledKey, true)
	ctx = context.WithValue(ctx, projectIDKey, []string{"p1"})
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()
	s.MemoryStats(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200, body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Projects []MemoryProjectStats `json:"projects"`
		Total    int                  `json:"total"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 || resp.Projects[0].ProjectID != "p1" {
		t.Fatalf("scoped stats leaked projects: %+v", resp)
	}
}

// TestMemoryBackfillTitles_Disabled and HappyPath cover the
// backfill endpoint's two main branches. The disabled path 503s
// when no MemoryTitleBackfiller is wired; the wired path returns
// the batch result.
func TestMemoryBackfillTitles_Disabled(t *testing.T) {
	s := NewServer()
	req := authDisabledReq(httptest.NewRequest(http.MethodPost, "/api/v1/memory/backfill-titles", nil))
	rr := httptest.NewRecorder()
	s.MemoryBackfillTitles(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

func TestMemoryBackfillTitles_RequiresAdmin(t *testing.T) {
	s := NewServer()
	s.adminConfig.Enabled = true
	s.adminConfig.AllowedKeys = []string{"sk-admin"}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/memory/backfill-titles", nil)
	req = req.WithContext(context.WithValue(req.Context(), apiKeyKey, "sk-project"))
	rr := httptest.NewRecorder()
	s.MemoryBackfillTitles(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

// TestMemoryBackfillTitles_RejectsNonPost — pin the method gate.
func TestMemoryBackfillTitles_RejectsNonPost(t *testing.T) {
	s := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/memory/backfill-titles", nil)
	rr := httptest.NewRecorder()
	s.MemoryBackfillTitles(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}

// TestMemorySearch_RejectsNonGet pins the method gate.
func TestMemorySearch_RejectsNonGet(t *testing.T) {
	s := NewServer()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/x/memory/search", nil)
	rr := httptest.NewRecorder()
	s.MemorySearch(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}

// TestMemorySearch_MissingProject pins the project-id validation.
func TestMemorySearch_MissingProject(t *testing.T) {
	s := NewServer()
	// /api/v1/projects//memory/search → empty projectID
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/", nil)
	rr := httptest.NewRecorder()
	s.MemorySearch(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

// stubMemoryStats satisfies MemoryStatsProvider with a fixed
// canned response.
type stubMemoryStats struct {
	rows []MemoryProjectStats
}

func (s stubMemoryStats) Stats(_ context.Context) ([]MemoryProjectStats, error) {
	return s.rows, nil
}
