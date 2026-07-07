package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

func newSkillRepoUI(t *testing.T) persistence.SkillRepository {
	t.Helper()
	db, err := sqlite.Connect(context.Background(), sqlite.DefaultConfig())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return sqlite.NewSkillRepository(db.DB)
}

func TestAdminSkills_ListsDrafts(t *testing.T) {
	repo := newSkillRepoUI(t)
	if err := repo.Create(context.Background(), &persistence.Skill{
		ID: "s1", ProjectID: "p1", Name: "trace-hang", Description: "when a model hangs",
		Body: "# steps", BodySHA256: "h", Maturity: persistence.SkillMaturityDraft,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := NewServer(WithSkillRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/admin/skills", nil)
	rec := httptest.NewRecorder()
	s.AdminSkills(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "trace-hang") {
		t.Fatalf("draft skill not rendered in inbox")
	}
}

func TestAdminSkills_ApprovePost(t *testing.T) {
	repo := newSkillRepoUI(t)
	if err := repo.Create(context.Background(), &persistence.Skill{
		ID: "s2", ProjectID: "p1", Name: "gate", Description: "d",
		Body: "b", BodySHA256: "h", Maturity: persistence.SkillMaturityDraft,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := NewServer(WithSkillRepository(repo))
	form := url.Values{"id": {"s2"}, "action": {"approve"}}
	req := httptest.NewRequest(http.MethodPost, "/admin/skills", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.AdminSkills(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	got, _ := repo.GetByID(context.Background(), "s2")
	if got.Maturity != persistence.SkillMaturityActive {
		t.Fatalf("approve did not activate: %s", got.Maturity)
	}
}

func TestAdminSkills_RepoUnwired(t *testing.T) {
	s := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/admin/skills", nil)
	rec := httptest.NewRecorder()
	s.AdminSkills(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unwired should still render (200), got %d", rec.Code)
	}
}
