package executor

import (
	"context"
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
