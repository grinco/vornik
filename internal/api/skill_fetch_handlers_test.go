package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// newSkillFetchServer builds a Server with the skill store AND the
// execution→skill association repo on one sqlite backend.
func newSkillFetchServer(t *testing.T) (*Server, persistence.ExecutionInjectedSkillRepository) {
	t.Helper()
	db, err := sqlite.Connect(context.Background(), sqlite.DefaultConfig())
	if err != nil {
		t.Fatalf("sqlite.Connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("sqlite.Migrate: %v", err)
	}
	execSkills := sqlite.NewExecutionInjectedSkillRepository(db.DB)
	return &Server{
		skillStore:    sqlite.NewSkillRepository(db.DB),
		execSkillRepo: execSkills,
	}, execSkills
}

func mkFetchSkill(t *testing.T, s *Server, id, proj, name, maturity string, global bool) {
	t.Helper()
	if err := s.skillStore.Create(context.Background(), &persistence.Skill{
		ID: id, ProjectID: proj, RepoScope: "github.com/x/a", Name: name,
		Description: "when to use " + name, Body: "BODY of " + name, BodySHA256: "h-" + id,
		Maturity: maturity, IsGlobal: global,
	}); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
}

func doFetch(s *Server, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/skills/fetch"+query, nil)
	rec := httptest.NewRecorder()
	s.SkillFetch(rec, req, "p1")
	return rec
}

// TestSkillFetch_ServesBodyAndRecordsUsage pins the progressive-
// disclosure contract (LLD 2026-07-12): the fetch serves the full
// body, stamps `fired`, and records the (execution, skill)
// association — telemetry moved here from injection time so the
// learning loop sees honest use.
func TestSkillFetch_ServesBodyAndRecordsUsage(t *testing.T) {
	s, execSkills := newSkillFetchServer(t)
	mkFetchSkill(t, s, "s1", "p1", "person-dossier", persistence.SkillMaturityTrusted, false)

	rec := doFetch(s, "?name=person-dossier&execution_id=exec-9")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp skillFetchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if resp.Body != "BODY of person-dossier" || resp.Name != "person-dossier" {
		t.Fatalf("unexpected payload: %+v", resp)
	}

	stored, _ := s.skillStore.GetByID(context.Background(), "s1")
	if stored.UsageFired != 1 {
		t.Errorf("fetch must stamp fired=1, got %d", stored.UsageFired)
	}
	ids, _ := execSkills.ListByExecution(context.Background(), "exec-9")
	if len(ids) != 1 || ids[0] != "s1" {
		t.Errorf("fetch must record the execution association, got %v", ids)
	}
}

func TestSkillFetch_ExecutionAssociationRequiresSameProject(t *testing.T) {
	s, execSkills := newSkillFetchServer(t)
	mkFetchSkill(t, s, "s1", "p1", "person-dossier", persistence.SkillMaturityTrusted, false)
	s.executionRepo = &stubExecRepoForFork{exec: &persistence.Execution{ID: "exec-other", ProjectID: "p2"}}

	rec := doFetch(s, "?name=person-dossier&execution_id=exec-other")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	ids, _ := execSkills.ListByExecution(context.Background(), "exec-other")
	if len(ids) != 0 {
		t.Fatalf("cross-project execution must not be associated with skill, got %v", ids)
	}
}

// TestSkillFetch_EligibilityMirrorsIndex: drafts and retired skills are
// never fetchable (they are unreviewed / withdrawn content that must
// not reach the trusted directive channel), cross-project non-global
// skills 404 (existence-leak guard), and globals from another project
// serve fine.
func TestSkillFetch_EligibilityMirrorsIndex(t *testing.T) {
	s, _ := newSkillFetchServer(t)
	mkFetchSkill(t, s, "s-draft", "p1", "draft-skill", persistence.SkillMaturityDraft, false)
	mkFetchSkill(t, s, "s-retired", "p1", "retired-skill", persistence.SkillMaturityRetired, false)
	mkFetchSkill(t, s, "s-other", "p2", "other-project", persistence.SkillMaturityActive, false)
	mkFetchSkill(t, s, "s-global", "p2", "global-skill", persistence.SkillMaturityActive, true)

	for _, name := range []string{"draft-skill", "retired-skill", "other-project", "does-not-exist"} {
		rec := doFetch(s, "?name="+name)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: expected 404, got %d", name, rec.Code)
		}
	}
	if rec := doFetch(s, "?name=global-skill"); rec.Code != http.StatusOK {
		t.Errorf("global skill from another project must fetch, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSkillFetch_Validation(t *testing.T) {
	s, _ := newSkillFetchServer(t)
	if rec := doFetch(s, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("missing name must 400, got %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/skills/fetch?name=x", nil)
	rec := httptest.NewRecorder()
	s.SkillFetch(rec, req, "p1")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST must 405, got %d", rec.Code)
	}
	bare := &Server{}
	if rec := doFetch(bare, "?name=x"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("nil store must 503, got %d", rec.Code)
	}
}
