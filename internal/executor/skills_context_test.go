package executor

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

func TestRenderSkillsBlock(t *testing.T) {
	if got := renderSkillsBlock(nil); got != "" {
		t.Fatalf("empty skills should render empty, got %q", got)
	}
	got := renderSkillsBlock([]SkillBlock{
		{Name: "trace-hang", Description: "when a model call hangs", Body: "1. probe the model\n2. swap it"},
	})
	for _, want := range []string{"## LEARNED SKILLS", "trace-hang", "when a model call hangs", "probe the model"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered block missing %q:\n%s", want, got)
		}
	}
}

func TestComposeSystemPromptWithSkills(t *testing.T) {
	if got := composeSystemPromptWithSkills("ROLE", nil); got != "ROLE" {
		t.Fatalf("no skills must leave prompt unchanged, got %q", got)
	}
	got := composeSystemPromptWithSkills("ROLE", []SkillBlock{{Name: "s", Body: "do it"}})
	if !strings.HasPrefix(got, "ROLE") || !strings.Contains(got, "LEARNED SKILLS") {
		t.Fatalf("skills must append after the role prompt, got %q", got)
	}
}

func newSkillRepoForExec(t *testing.T) persistence.SkillRepository {
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

func TestResolveSkills_OnlyApprovedMatchingRole(t *testing.T) {
	repo := newSkillRepoForExec(t)
	ctx := context.Background()
	mk := func(id, name, maturity string, roles []string) {
		if err := repo.Create(ctx, &persistence.Skill{
			ID: id, ProjectID: "p1", RepoScope: "github.com/x/a", Name: name,
			Description: "d", Body: "b", BodySHA256: "h", Maturity: maturity, Roles: roles,
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	mk("s-draft", "draft-skill", persistence.SkillMaturityDraft, []string{"researcher"})
	mk("s-active", "active-skill", persistence.SkillMaturityActive, []string{"researcher"})
	mk("s-any", "anyrole-skill", persistence.SkillMaturityActive, nil)
	mk("s-writer", "writer-skill", persistence.SkillMaturityActive, []string{"writer"})

	e := &Executor{skillRepo: repo}
	got := e.resolveSkills(ctx, "p1", "researcher", "exec-t")

	names := map[string]bool{}
	for _, s := range got {
		names[s.Name] = true
	}
	if names["draft-skill"] {
		t.Error("draft skill must not be injected")
	}
	if names["writer-skill"] {
		t.Error("writer-only skill must not inject for researcher")
	}
	if !names["active-skill"] || !names["anyrole-skill"] {
		t.Errorf("expected active researcher + any-role skills, got %v", names)
	}
}

func TestResolveSkills_NilStore(t *testing.T) {
	e := &Executor{}
	if got := e.resolveSkills(context.Background(), "p1", "researcher", "exec-x"); got != nil {
		t.Fatalf("nil store must yield nil, got %v", got)
	}
}

// fakeExecRepo implements just enough of ExecutionRepository for the
// worked-credit test: List returns a fixed execution set.
type fakeExecRepo struct {
	ExecutionRepository
	execs []*persistence.Execution
}

func (f fakeExecRepo) List(_ context.Context, _ persistence.ExecutionFilter) ([]*persistence.Execution, error) {
	return f.execs, nil
}

// newExecLearningDB spins up one in-memory SQLite DB so the skill store
// and the execution→skill association share a backend.
func newExecLearningDB(t *testing.T) *sqlite.DB {
	t.Helper()
	db, err := sqlite.Connect(context.Background(), sqlite.DefaultConfig())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestResolveSkills_StampsFiredAndRecordsAssociation(t *testing.T) {
	db := newExecLearningDB(t)
	skillRepo := sqlite.NewSkillRepository(db.DB)
	execSkillRepo := sqlite.NewExecutionInjectedSkillRepository(db.DB)
	ctx := context.Background()
	if err := skillRepo.Create(ctx, &persistence.Skill{
		ID: "s1", ProjectID: "p1", RepoScope: "github.com/x/a", Name: "n",
		Description: "d", Body: "b", BodySHA256: "h",
		Maturity: persistence.SkillMaturityActive, Roles: []string{"researcher"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	e := &Executor{skillRepo: skillRepo, execSkillRepo: execSkillRepo}
	e.resolveSkills(ctx, "p1", "researcher", "exec-1")

	got, _ := skillRepo.GetByID(ctx, "s1")
	if got.UsageFired != 1 {
		t.Errorf("expected fired=1, got %d", got.UsageFired)
	}
	ids, _ := execSkillRepo.ListByExecution(ctx, "exec-1")
	if len(ids) != 1 || ids[0] != "s1" {
		t.Errorf("association not recorded: %v", ids)
	}
}

func TestCreditSkillsWorked(t *testing.T) {
	db := newExecLearningDB(t)
	skillRepo := sqlite.NewSkillRepository(db.DB)
	execSkillRepo := sqlite.NewExecutionInjectedSkillRepository(db.DB)
	ctx := context.Background()
	if err := skillRepo.Create(ctx, &persistence.Skill{
		ID: "s1", ProjectID: "p1", Name: "n", Description: "d", Body: "b", BodySHA256: "h",
		Maturity: persistence.SkillMaturityActive,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := execSkillRepo.Record(ctx, "exec-1", "s1"); err != nil {
		t.Fatalf("record: %v", err)
	}
	e := &Executor{
		skillRepo:     skillRepo,
		execSkillRepo: execSkillRepo,
		execRepo:      fakeExecRepo{execs: []*persistence.Execution{{ID: "exec-1"}}},
	}
	e.creditSkillsWorked(ctx, "task-1")
	got, _ := skillRepo.GetByID(ctx, "s1")
	if got.UsageWorked != 1 {
		t.Errorf("expected worked=1, got %d", got.UsageWorked)
	}
}
