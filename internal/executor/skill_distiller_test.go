package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// fakeDistillLLM satisfies chat.Provider by embedding it (nil) and
// overriding only Complete, which is all the distiller calls.
type fakeDistillLLM struct {
	chat.Provider
	content string
}

func (f fakeDistillLLM) Complete(_ context.Context, _ []chat.Message) (*chat.ChatResponse, error) {
	resp := &chat.ChatResponse{}
	resp.Choices = append(resp.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{Message: chat.Message{Content: f.content}})
	return resp, nil
}

func TestParseDistillCandidate(t *testing.T) {
	if c, ok := parseDistillCandidate(`{"skip":true}`); !ok || !c.Skip {
		t.Fatalf("skip verdict: ok=%v skip=%v", ok, c.Skip)
	}
	fenced := "```json\n{\"skip\":false,\"name\":\"x\",\"description\":\"d\",\"body\":\"b\"}\n```"
	c, ok := parseDistillCandidate(fenced)
	if !ok || c.Skip || c.Name != "x" {
		t.Fatalf("fenced parse failed: ok=%v %+v", ok, c)
	}
	if _, ok := parseDistillCandidate("not json at all"); ok {
		t.Fatalf("garbage must not parse")
	}
}

func TestSkillDistillLimiter(t *testing.T) {
	l := &skillDistillLimiter{}
	now := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)
	for i := 0; i < skillDistillMaxPerWindow; i++ {
		if !l.allow("p1", now) {
			t.Fatalf("call %d within window should be allowed", i)
		}
	}
	if l.allow("p1", now) {
		t.Fatal("over-cap call in the same window must be denied")
	}
	if !l.allow("p1", now.Add(skillDistillWindow+time.Minute)) {
		t.Fatal("after the window expires it should allow again")
	}
	if !l.allow("p2", now) {
		t.Fatal("a different project must have its own budget")
	}
}

func newDistillSkillRepo(t *testing.T) persistence.SkillRepository {
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

func TestMaybeDistillSkill_ProposesDraft(t *testing.T) {
	repo := newDistillSkillRepo(t)
	e := &Executor{
		skillRepo:    repo,
		distillerLLM: fakeDistillLLM{content: `{"skip":false,"name":"trace-hang","description":"when a model hangs","body":"# do it\nprobe","domain":"software","roles":["strategist"]}`},
		logger:       zerolog.Nop(),
	}
	task := &persistence.Task{ID: "task-1", ProjectID: "p1", Payload: []byte(`{"prompt":"debug the hang"}`)}
	e.maybeDistillSkill(context.Background(), task, "result text")

	drafts, _ := repo.ListDrafts(context.Background(), 0)
	if len(drafts) != 1 || drafts[0].Name != "trace-hang" {
		t.Fatalf("expected one draft 'trace-hang', got %+v", drafts)
	}
	if drafts[0].OriginClient != "vornik-distiller" || drafts[0].Maturity != persistence.SkillMaturityDraft {
		t.Fatalf("wrong origin/maturity: %+v", drafts[0])
	}
}

func TestMaybeDistillSkill_SkipAndDedup(t *testing.T) {
	repo := newDistillSkillRepo(t)
	ctx := context.Background()
	// Model says skip → no draft.
	e := &Executor{skillRepo: repo, distillerLLM: fakeDistillLLM{content: `{"skip":true}`}, logger: zerolog.Nop()}
	e.maybeDistillSkill(ctx, &persistence.Task{ID: "t", ProjectID: "p1", Payload: []byte(`{"prompt":"x"}`)}, "r")
	if d, _ := repo.ListDrafts(ctx, 0); len(d) != 0 {
		t.Fatalf("skip must propose nothing, got %+v", d)
	}
	// Existing same-named skill → dedup, no second draft.
	if err := repo.Create(ctx, &persistence.Skill{
		ID: "pre", ProjectID: "p1", Name: "dup-skill", Description: "d", Body: "b", BodySHA256: "h",
		Maturity: persistence.SkillMaturityActive,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	e2 := &Executor{skillRepo: repo, distillerLLM: fakeDistillLLM{content: `{"skip":false,"name":"dup-skill","description":"d","body":"b2"}`}, logger: zerolog.Nop()}
	e2.maybeDistillSkill(ctx, &persistence.Task{ID: "t2", ProjectID: "p1", Payload: []byte(`{"prompt":"y"}`)}, "r")
	if d, _ := repo.ListDrafts(ctx, 0); len(d) != 0 {
		t.Fatalf("dedup must skip the same-named skill, got %+v", d)
	}
}

// The near-duplicate gate (2026-09-16). The semantic preflight has existed
// since §12.2 and was wired into exactly ONE entrypoint — the companion
// `skill_propose` MCP tool. This path, which produces the overwhelming majority
// of the catalogue, deduped on case-insensitive EXACT NAME and its comment
// claimed that "avoids near-duplicate drafts".
//
// It could not. Measured on the reference deployment: 105 of 128 skills came
// from here, 69 of them never fired once, and none carried a distinctness
// justification. A singular/plural typo was enough —
// `prague-restaurant-feed-refresh` (23 firings) acquired a permanent dead twin
// `prague-restaurants-feed-refresh` (0). The damage is not only clutter: usage
// splits across the twins, so the maturity worker under-rates both halves of
// what is really one skill.
//
// This is the same shape as the config assistant's mayAutoApply: a gate built,
// reviewed, and attached to one door out of several.

type fakeDupeChecker struct {
	matches []SkillDupeMatch
	err     error
	calls   int
	gotName string
}

func (f *fakeDupeChecker) NearDuplicateSkills(_ context.Context, c *persistence.Skill) ([]SkillDupeMatch, error) {
	f.calls++
	f.gotName = c.Name
	return f.matches, f.err
}

func TestMaybeDistillSkill_SkipsANearDuplicate(t *testing.T) {
	repo := newDistillSkillRepo(t)
	chk := &fakeDupeChecker{matches: []SkillDupeMatch{
		{Name: "prague-restaurant-feed-refresh", Score: 0.91, Reason: "similar_embedding"},
	}}
	e := &Executor{
		skillRepo:    repo,
		dupeChecker:  chk,
		distillerLLM: fakeDistillLLM{content: `{"skip":false,"name":"prague-restaurants-feed-refresh","description":"d","body":"b"}`},
		logger:       zerolog.Nop(),
	}
	e.maybeDistillSkill(context.Background(), &persistence.Task{
		ID: "t", ProjectID: "p1", Payload: []byte(`{"prompt":"x"}`)}, "r")

	if d, _ := repo.ListDrafts(context.Background(), 0); len(d) != 0 {
		t.Fatalf("a near-duplicate must not be written; got %+v", d)
	}
	if chk.calls != 1 {
		t.Errorf("checker called %d times, want 1", chk.calls)
	}
	if chk.gotName != "prague-restaurants-feed-refresh" {
		t.Errorf("checker saw %q, want the candidate's name", chk.gotName)
	}
}

func TestMaybeDistillSkill_WritesWhenNothingIsSimilar(t *testing.T) {
	repo := newDistillSkillRepo(t)
	chk := &fakeDupeChecker{} // no matches
	e := &Executor{
		skillRepo:    repo,
		dupeChecker:  chk,
		distillerLLM: fakeDistillLLM{content: `{"skip":false,"name":"genuinely-new","description":"d","body":"b"}`},
		logger:       zerolog.Nop(),
	}
	e.maybeDistillSkill(context.Background(), &persistence.Task{
		ID: "t", ProjectID: "p1", Payload: []byte(`{"prompt":"x"}`)}, "r")

	d, _ := repo.ListDrafts(context.Background(), 0)
	if len(d) != 1 || d[0].Name != "genuinely-new" {
		t.Fatalf("a distinct skill must still be proposed; got %+v", d)
	}
}

// An embedder outage must not stop the distiller proposing. §12.2 already makes
// that choice on the MCP path ("an embedder outage must never block
// authoring"); the automatic path inherits it rather than inventing a stricter
// rule nobody asked for.
func TestMaybeDistillSkill_CheckerFailureDoesNotBlock(t *testing.T) {
	repo := newDistillSkillRepo(t)
	chk := &fakeDupeChecker{err: errors.New("embedder down")}
	e := &Executor{
		skillRepo:    repo,
		dupeChecker:  chk,
		distillerLLM: fakeDistillLLM{content: `{"skip":false,"name":"still-proposed","description":"d","body":"b"}`},
		logger:       zerolog.Nop(),
	}
	e.maybeDistillSkill(context.Background(), &persistence.Task{
		ID: "t", ProjectID: "p1", Payload: []byte(`{"prompt":"x"}`)}, "r")

	if d, _ := repo.ListDrafts(context.Background(), 0); len(d) != 1 {
		t.Fatalf("a checker error must degrade to the old behaviour, not silence the distiller; got %+v", d)
	}
}

// Unwired checker = the pre-2026-09-16 behaviour, exactly. A deployment that
// has not wired the gate must keep working, and the exact-name dedup still runs.
func TestMaybeDistillSkill_NilCheckerKeepsExactNameDedup(t *testing.T) {
	repo := newDistillSkillRepo(t)
	e := &Executor{
		skillRepo:    repo,
		distillerLLM: fakeDistillLLM{content: `{"skip":false,"name":"no-checker","description":"d","body":"b"}`},
		logger:       zerolog.Nop(),
	}
	e.maybeDistillSkill(context.Background(), &persistence.Task{
		ID: "t", ProjectID: "p1", Payload: []byte(`{"prompt":"x"}`)}, "r")
	if d, _ := repo.ListDrafts(context.Background(), 0); len(d) != 1 {
		t.Fatalf("nil checker must not disable proposing; got %+v", d)
	}
}
