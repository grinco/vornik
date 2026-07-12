package executor

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

func TestRenderSkillIndexBlock(t *testing.T) {
	if got := renderSkillIndexBlock(nil); got != "" {
		t.Fatalf("empty skills should render empty, got %q", got)
	}
	got := renderSkillIndexBlock([]SkillIndexEntry{
		{Name: "trace-hang", Description: "when a model call hangs"},
	})
	for _, want := range []string{"## LEARNED SKILLS", "trace-hang", "when a model call hangs", "skill_fetch"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered index missing %q:\n%s", want, got)
		}
	}
}

// TestRenderSkillIndexBlock_NoBodies pins the progressive-disclosure
// contract (LLD 2026-07-12): the index must NOT inline skill bodies —
// v1 injected up to 5 full bodies (~14KB observed) into every step
// regardless of task relevance.
func TestRenderSkillIndexBlock_NoBodies(t *testing.T) {
	entries := []SkillIndexEntry{{Name: "s1", Description: "short"}}
	got := renderSkillIndexBlock(entries)
	if len(got) > 600 {
		t.Fatalf("index block for one skill should be compact, got %d bytes", len(got))
	}
	// Long descriptions are truncated so a rambling description can't
	// turn the index back into a body dump.
	long := strings.Repeat("x", 1000)
	got = renderSkillIndexBlock([]SkillIndexEntry{{Name: "s2", Description: long}})
	if strings.Contains(got, long) {
		t.Fatal("index must truncate long descriptions")
	}
}

func TestComposeSystemPromptWithSkillIndex(t *testing.T) {
	if got := composeSystemPromptWithSkillIndex("ROLE", nil); got != "ROLE" {
		t.Fatalf("no skills must leave prompt unchanged, got %q", got)
	}
	got := composeSystemPromptWithSkillIndex("ROLE", []SkillIndexEntry{{Name: "s", Description: "d"}})
	if !strings.HasPrefix(got, "ROLE") || !strings.Contains(got, "LEARNED SKILLS") {
		t.Fatalf("index must append after the role prompt, got %q", got)
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

func TestResolveSkillIndex_OnlyApprovedMatchingRole(t *testing.T) {
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
	got := e.resolveSkillIndex(ctx, "p1", "researcher")

	names := map[string]bool{}
	for _, s := range got {
		names[s.Name] = true
	}
	if names["draft-skill"] {
		t.Error("draft skill must not be indexed")
	}
	if names["writer-skill"] {
		t.Error("writer-only skill must not index for researcher")
	}
	if !names["active-skill"] || !names["anyrole-skill"] {
		t.Errorf("expected active researcher + any-role skills, got %v", names)
	}
}

func TestResolveSkillIndex_IncludesGlobalFromOtherProject(t *testing.T) {
	repo := newSkillRepoForExec(t)
	ctx := context.Background()
	mk := func(id, proj, name string, global bool) {
		if err := repo.Create(ctx, &persistence.Skill{
			ID: id, ProjectID: proj, RepoScope: "github.com/x/a", Name: name,
			Description: "d", Body: "b", BodySHA256: "h-" + id,
			Maturity: persistence.SkillMaturityActive, Roles: []string{"researcher"},
			IsGlobal: global,
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	// Global skill authored under project A; a non-global one under A.
	mk("g-a", "projA", "global-from-a", true)
	mk("l-a", "projA", "local-to-a", false)
	// Project B's own skill.
	mk("l-b", "projB", "local-to-b", false)

	e := &Executor{skillRepo: repo}
	got := e.resolveSkillIndex(ctx, "projB", "researcher")

	names := map[string]bool{}
	for _, s := range got {
		names[s.Name] = true
	}
	if !names["local-to-b"] {
		t.Error("project B must see its own skill")
	}
	if !names["global-from-a"] {
		t.Error("project B must index project A's GLOBAL skill")
	}
	if names["local-to-a"] {
		t.Error("isolation leak: project B indexed project A's non-global skill")
	}

	// A global skill whose home IS the current project indexes exactly
	// once (dedup guard).
	gotA := e.resolveSkillIndex(ctx, "projA", "researcher")
	count := 0
	for _, s := range gotA {
		if s.Name == "global-from-a" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("home-project global must appear exactly once, got %d", count)
	}
}

// TestResolveSkillIndex_DoesNotStampFired pins the v2 telemetry
// contract: `fired` is recorded at FETCH time by the API handler, not
// at injection — v1's fired-on-inject made usage_fired meaningless
// (188 "fires" for skills the model may never have read) and fed the
// maturity promote/decay worker noise.
func TestResolveSkillIndex_DoesNotStampFired(t *testing.T) {
	repo := newSkillRepoForExec(t)
	ctx := context.Background()
	if err := repo.Create(ctx, &persistence.Skill{
		ID: "s1", ProjectID: "p1", RepoScope: "github.com/x/a", Name: "n",
		Description: "d", Body: "b", BodySHA256: "h",
		Maturity: persistence.SkillMaturityActive, Roles: []string{"researcher"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	e := &Executor{skillRepo: repo}
	if got := e.resolveSkillIndex(ctx, "p1", "researcher"); len(got) != 1 {
		t.Fatalf("expected 1 index entry, got %d", len(got))
	}
	stored, _ := repo.GetByID(ctx, "s1")
	if stored.UsageFired != 0 {
		t.Errorf("indexing must not stamp fired; got usage_fired=%d", stored.UsageFired)
	}
}

func TestResolveSkillIndex_NilStore(t *testing.T) {
	e := &Executor{}
	if got := e.resolveSkillIndex(context.Background(), "p1", "researcher"); got != nil {
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
