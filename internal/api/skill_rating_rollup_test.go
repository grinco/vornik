package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

type stubArmRepo struct {
	rows  []persistence.SkillRatingArms
	since time.Time
	sha   string
}

func (s *stubArmRepo) SkillRatingArms(_ context.Context, _ string, since time.Time) ([]persistence.SkillRatingArms, error) {
	s.since = since
	return s.rows, nil
}

// The body-scoped variant (migration 180). This endpoint serves the
// whole-history view, so it never passes a sha; the stub records what it was
// given so a future caller that starts scoping cannot do so unnoticed.
func (s *stubArmRepo) SkillRatingArmsForBody(_ context.Context, _, sha string, since time.Time) ([]persistence.SkillRatingArms, error) {
	s.since = since
	s.sha = sha
	return s.rows, nil
}

func rollupArms(project, workflow string, tEl, tRat, tUp, bEl, bRat, bUp int) persistence.SkillRatingArms {
	return persistence.SkillRatingArms{
		ProjectID:  project,
		WorkflowID: workflow,
		Treatment:  persistence.RatingArm{EligibleN: tEl, RatedN: tRat, UpN: tUp},
		Baseline:   persistence.RatingArm{EligibleN: bEl, RatedN: bRat, UpN: bUp},
	}
}

func rollupRequest(t *testing.T, path string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	return r.WithContext(context.WithValue(r.Context(), authEnabledKey, false))
}

// Every context carries BOTH arms. The design refuses an aggregate without its
// counterfactual anywhere — API included — because a lift figure alone is the
// vanity metric the whole arc exists to avoid.
func TestSkillRatingRollup_AlwaysRendersTheCounterfactual(t *testing.T) {
	repo := &stubArmRepo{rows: []persistence.SkillRatingArms{
		rollupArms("p1", "digest", 100, 20, 4, 100, 20, 18),
	}}
	srv := NewServer(WithRatingRollupRepository(repo))

	rec := httptest.NewRecorder()
	srv.SkillRatingRollup(rec, rollupRequest(t, "/api/v1/skills/skill-x/rating-rollup"), "skill-x")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out SkillRatingRollupResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Summary != "low_lift" {
		t.Errorf("summary = %q", out.Summary)
	}
	if len(out.Contexts) != 1 {
		t.Fatalf("got %d contexts", len(out.Contexts))
	}
	c := out.Contexts[0]
	if c.Treatment.RatedN == 0 || c.Baseline.RatedN == 0 {
		t.Fatalf("a context rendered without both arms: %+v", c)
	}
	if c.Treatment.Coverage <= 0 || c.Baseline.Coverage <= 0 {
		t.Fatalf("a context rendered without coverage: %+v", c)
	}
}

// The window is an operator knob, and an unparseable one is refused rather than
// silently defaulted — a rollup over a window the caller did not ask for is a
// different measurement wearing the same name.
func TestSkillRatingRollup_RefusesAnUnparseableWindow(t *testing.T) {
	srv := NewServer(WithRatingRollupRepository(&stubArmRepo{}))
	rec := httptest.NewRecorder()
	srv.SkillRatingRollup(rec, rollupRequest(t, "/api/v1/skills/s/rating-rollup?window_hours=soon"), "s")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestSkillRatingRollup_HonoursTheWindow(t *testing.T) {
	repo := &stubArmRepo{}
	srv := NewServer(WithRatingRollupRepository(repo))
	rec := httptest.NewRecorder()
	srv.SkillRatingRollup(rec, rollupRequest(t, "/api/v1/skills/s/rating-rollup?window_hours=48"), "s")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if d := time.Since(repo.since); d < 47*time.Hour || d > 49*time.Hour {
		t.Fatalf("cutoff was %v ago, want ~48h", d)
	}
}

// A skill nobody has injected is not_measurable, and the endpoint says so
// rather than 404ing — "not used in this window" is an answer, not an absence.
func TestSkillRatingRollup_NeverInjectedIsNotMeasurable(t *testing.T) {
	srv := NewServer(WithRatingRollupRepository(&stubArmRepo{}))
	rec := httptest.NewRecorder()
	srv.SkillRatingRollup(rec, rollupRequest(t, "/api/v1/skills/s/rating-rollup"), "s")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	var out SkillRatingRollupResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Summary != "not_measurable" {
		t.Fatalf("summary = %q, want not_measurable", out.Summary)
	}
}

func TestSkillRatingRollup_503WhenUnwired(t *testing.T) {
	srv := NewServer()
	rec := httptest.NewRecorder()
	srv.SkillRatingRollup(rec, rollupRequest(t, "/api/v1/skills/s/rating-rollup"), "s")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

// The prefix handler must still route the pre-existing POST /global, and must
// reach the new GET — a dispatcher that captured the prefix and dropped one of
// them would be a silent regression on a shipped verb.
func TestSkillsRouter_ReachesBothVerbs(t *testing.T) {
	srv := NewServer(WithRatingRollupRepository(&stubArmRepo{}))

	rec := httptest.NewRecorder()
	srv.apiV1SkillsHandler(rec, rollupRequest(t, "/api/v1/skills/s/rating-rollup"))
	if rec.Code != http.StatusOK {
		t.Errorf("rating-rollup via router = %d: %s", rec.Code, rec.Body.String())
	}

	// No skill store wired, so /global answers 503 — which still proves the
	// route reached SkillSetGlobal rather than falling through to a 404.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/skills/s/global", nil)
	req = req.WithContext(context.WithValue(req.Context(), authEnabledKey, false))
	srv.apiV1SkillsHandler(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Error("the router stopped reaching POST /global")
	}
}
