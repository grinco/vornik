package skills

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

func newSkillRepo(t *testing.T) persistence.SkillRepository {
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

func seed(t *testing.T, repo persistence.SkillRepository, id, maturity string) {
	t.Helper()
	if err := repo.Create(context.Background(), &persistence.Skill{
		ID: id, ProjectID: "p1", RepoScope: "github.com/x/a", Name: id,
		Description: "d", Body: "b", BodySHA256: "h", Maturity: maturity,
	}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func TestApplyDecision_ApproveDraft(t *testing.T) {
	repo := newSkillRepo(t)
	ctx := context.Background()
	seed(t, repo, "d1", persistence.SkillMaturityDraft)
	got, err := ApplyDecision(ctx, repo, "d1", Approve)
	if err != nil {
		t.Fatalf("ApplyDecision: %v", err)
	}
	if got != persistence.SkillMaturityActive {
		t.Fatalf("expected active, got %s", got)
	}
}

func TestApplyDecision_ApproveIdempotent(t *testing.T) {
	repo := newSkillRepo(t)
	ctx := context.Background()
	seed(t, repo, "a1", persistence.SkillMaturityActive)
	got, err := ApplyDecision(ctx, repo, "a1", Approve)
	if err != nil || got != persistence.SkillMaturityActive {
		t.Fatalf("idempotent approve: got %s err %v", got, err)
	}
}

func TestApplyDecision_RejectActiveCreditsCorrected(t *testing.T) {
	repo := newSkillRepo(t)
	ctx := context.Background()
	seed(t, repo, "a2", persistence.SkillMaturityActive)
	got, err := ApplyDecision(ctx, repo, "a2", Reject)
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if got != persistence.SkillMaturityRetired {
		t.Fatalf("expected retired, got %s", got)
	}
	s, _ := repo.GetByID(ctx, "a2")
	if s.UsageCorrected != 1 {
		t.Fatalf("rejecting active must credit corrected, got %d", s.UsageCorrected)
	}
}

func TestApplyDecision_RejectDraftNoCorrected(t *testing.T) {
	repo := newSkillRepo(t)
	ctx := context.Background()
	seed(t, repo, "d2", persistence.SkillMaturityDraft)
	if _, err := ApplyDecision(ctx, repo, "d2", Reject); err != nil {
		t.Fatalf("reject: %v", err)
	}
	s, _ := repo.GetByID(ctx, "d2")
	if s.UsageCorrected != 0 {
		t.Fatalf("rejecting draft must not credit corrected, got %d", s.UsageCorrected)
	}
}
