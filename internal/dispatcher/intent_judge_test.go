package dispatcher

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/intentjudge"
	"vornik.io/vornik/internal/persistence"
)

// memIntentVerdictRepo records inserts + refinements in memory so
// the test can assert on what landed without sqlmock noise.
type memIntentVerdictRepo struct {
	mu         sync.Mutex
	inserted   []*persistence.IntentVerdict
	refined    []*persistence.IntentVerdict
	failInsert bool
	failRefine bool
}

func (m *memIntentVerdictRepo) Insert(_ context.Context, v *persistence.IntentVerdict) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failInsert {
		return errors.New("simulated insert failure")
	}
	cp := *v
	m.inserted = append(m.inserted, &cp)
	return nil
}
func (m *memIntentVerdictRepo) UpdateLLMRefinement(_ context.Context, v *persistence.IntentVerdict) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failRefine {
		return errors.New("simulated refine failure")
	}
	cp := *v
	m.refined = append(m.refined, &cp)
	return nil
}

// TestEvaluate_HeuristicOnly_PersistsRow — refiner is nil; only
// the heuristic verdict lands in the repo. Pin the canonical
// shape so a future schema-shift surfaces here.
func TestEvaluate_HeuristicOnly_PersistsRow(t *testing.T) {
	repo := &memIntentVerdictRepo{}
	cfg := &intentJudgeConfig{Repo: repo, RefineMinRisk: intentjudge.RiskMedium}

	v := cfg.evaluate(context.Background(), "assistant", nil, nil, nil,
		"run_shell", `{"cmd":"echo hi"}`,
		runSync, zerolog.Nop())

	if v.Tier != intentjudge.TierHeuristic {
		t.Errorf("Tier = %q, want heuristic", v.Tier)
	}
	if len(repo.inserted) != 1 {
		t.Fatalf("inserted rows = %d, want 1", len(repo.inserted))
	}
	row := repo.inserted[0]
	if row.ProjectID != "assistant" || row.ToolName != "run_shell" {
		t.Errorf("row fields wrong: %+v", row)
	}
	if row.HeuristicRisk != string(v.Risk) {
		t.Errorf("heuristic_risk = %q, want %q", row.HeuristicRisk, v.Risk)
	}
	if row.FinalRisk != string(v.Risk) {
		t.Errorf("final_risk should mirror heuristic for the no-refiner path: %q vs %q",
			row.FinalRisk, v.Risk)
	}
}

// TestEvaluate_InsertFailureReturnsHeuristic — DB failure on
// insert MUST NOT prevent the dispatcher from acting on the
// heuristic verdict. We return the heuristic regardless; the
// tool call still fires.
func TestEvaluate_InsertFailureReturnsHeuristic(t *testing.T) {
	repo := &memIntentVerdictRepo{failInsert: true}
	cfg := &intentJudgeConfig{Repo: repo, RefineMinRisk: intentjudge.RiskMedium}

	v := cfg.evaluate(context.Background(), "p", nil, nil, nil,
		"run_shell", `{}`, runSync, zerolog.Nop())
	if v.Risk == "" {
		t.Errorf("heuristic verdict missing despite insert failure: %+v", v)
	}
	if len(repo.inserted) != 0 {
		t.Errorf("inserted rows = %d, want 0 (failure path)", len(repo.inserted))
	}
}

// TestEvaluate_RefinerRunsAsync — when a refiner is wired and
// risk meets the floor, the LLM call fires (via the synchronous
// spawn injector) and an UpdateLLMRefinement lands on the repo.
func TestEvaluate_RefinerRunsAsync(t *testing.T) {
	repo := &memIntentVerdictRepo{}
	refiner := &intentjudge.LLMRefiner{Provider: &stubProvider{resp: respWith(
		`{"risk":"high","confidence":0.9,"recommendation":"review","reasoning":"writes to /etc"}`,
	)}}
	cfg := &intentJudgeConfig{Repo: repo, Refiner: refiner, RefineMinRisk: intentjudge.RiskLow}

	cfg.evaluate(context.Background(), "p", nil, nil, nil,
		"run_shell", `{"cmd":"rm -rf /etc"}`,
		runSync, zerolog.Nop())

	if len(repo.inserted) != 1 {
		t.Fatalf("inserted rows = %d, want 1", len(repo.inserted))
	}
	if len(repo.refined) != 1 {
		t.Fatalf("refined rows = %d, want 1 (refiner should have fired)", len(repo.refined))
	}
	r := repo.refined[0]
	if r.LLMRisk == nil || *r.LLMRisk != "high" {
		t.Errorf("llm_risk = %v, want high", r.LLMRisk)
	}
	if r.LLMRecommendation == nil || *r.LLMRecommendation != "review" {
		t.Errorf("llm_rec = %v, want review", r.LLMRecommendation)
	}
}

// TestEvaluate_RefinerSkippedBelowFloor — RefineMinRisk=high
// means a "low" heuristic verdict must NOT trigger an LLM call.
// Saves spend on the benign hot path.
func TestEvaluate_RefinerSkippedBelowFloor(t *testing.T) {
	repo := &memIntentVerdictRepo{}
	calls := 0
	refiner := &intentjudge.LLMRefiner{Provider: &stubProvider{
		// If the refiner is invoked, the test will know via this
		// counter — stubProvider's Complete bumps it via the resp
		// field, but the counter lives in this closure.
		resp: respWith(`{"risk":"low","confidence":0.1,"recommendation":"approve","reasoning":"x"}`),
	}}
	// We want to PROVE the refiner wasn't called. The cleanest
	// way is to look at what evaluate did to the repo: no refined
	// row at all means the refiner branch was skipped.
	cfg := &intentJudgeConfig{Repo: repo, Refiner: refiner, RefineMinRisk: intentjudge.RiskHigh}

	cfg.evaluate(context.Background(), "p", nil, nil, nil,
		"current_time", `{}`, // current_time is a known-benign tool
		runSync, zerolog.Nop())

	if len(repo.refined) != 0 {
		t.Errorf("refined = %d, want 0 (below RefineMinRisk floor)", len(repo.refined))
	}
	// Sanity: heuristic row WAS inserted.
	if len(repo.inserted) != 1 {
		t.Errorf("inserted = %d, want 1", len(repo.inserted))
	}
	_ = calls
}

// TestEvaluate_NilConfigSafe — receiver methods on a nil pointer
// must not panic. The dispatcher hot path calls evaluate without
// checking a.intentJudge first.
func TestEvaluate_NilConfigSafe(t *testing.T) {
	var cfg *intentJudgeConfig
	v := cfg.evaluate(context.Background(), "p", nil, nil, nil, "t", "{}", runSync, zerolog.Nop())
	if v.Risk != "" {
		t.Errorf("nil config returned non-empty verdict: %+v", v)
	}
}

// TestShouldRefine — the gate math.
func TestShouldRefine(t *testing.T) {
	cases := []struct {
		r, min intentjudge.Risk
		want   bool
	}{
		{intentjudge.RiskCritical, intentjudge.RiskMedium, true},
		{intentjudge.RiskHigh, intentjudge.RiskMedium, true},
		{intentjudge.RiskMedium, intentjudge.RiskMedium, true},
		{intentjudge.RiskLow, intentjudge.RiskMedium, false},
		{intentjudge.RiskLow, intentjudge.RiskLow, true},
		{intentjudge.RiskMedium, intentjudge.RiskHigh, false},
		// Unknown values rank as 0; default behaviour:
		// shouldRefine reports true (0 >= 0).
		{intentjudge.Risk("garbage"), intentjudge.Risk("also-garbage"), true},
	}
	for _, c := range cases {
		if got := shouldRefine(c.r, c.min); got != c.want {
			t.Errorf("shouldRefine(%q,%q) = %v, want %v", c.r, c.min, got, c.want)
		}
	}
}

// runSync is the spawnAsync injector for tests — runs the
// passed-in function in the test goroutine so assertions land
// deterministically without a sleep.
func runSync(fn func()) { fn() }

// stubProvider is the minimal chat.Provider for refiner tests.
// Mirrors the one in internal/intentjudge/llm_test.go; kept
// inline here because Go test packages don't share helpers
// across import paths.
type stubProvider struct {
	resp *chat.ChatResponse
	err  error
}

func (s *stubProvider) Complete(_ context.Context, _ []chat.Message) (*chat.ChatResponse, error) {
	return s.resp, s.err
}
func (s *stubProvider) CompleteWithTools(_ context.Context, _ []chat.Message, _ []chat.Tool) (*chat.ChatResponse, error) {
	return s.resp, s.err
}
func (s *stubProvider) CompleteWithToolsStream(_ context.Context, _ []chat.Message, _ []chat.Tool, _ chat.StreamCallback) (*chat.ChatResponse, error) {
	return s.resp, s.err
}
func (s *stubProvider) Model() string              { return "stub" }
func (s *stubProvider) SetMetrics(_ *chat.Metrics) {}

func respWith(content string) *chat.ChatResponse {
	return &chat.ChatResponse{
		Choices: []struct {
			Index        int          `json:"index"`
			Message      chat.Message `json:"message"`
			FinishReason string       `json:"finish_reason"`
		}{
			{Index: 0, Message: chat.Message{Role: "assistant", Content: content}, FinishReason: "stop"},
		},
	}
}

// Compile-time guard: chat.Provider surface still satisfied by
// the stub, in case the interface evolves.
var _ chat.Provider = (*stubProvider)(nil)

func TestWithIntentJudge_AssignsConfigAndDefaultsRisk(t *testing.T) {
	repo := &memIntentVerdictRepo{}
	a := NewAgent(nil, nil, nil, nil, nil, WithIntentJudge(repo, nil, ""))
	if a.intentJudge == nil {
		t.Fatal("intentJudge not assigned")
	}
	if a.intentJudge.Repo != repo {
		t.Fatal("intentJudge.Repo not assigned")
	}
	// Empty risk floor → "medium" (the documented default).
	if a.intentJudge.RefineMinRisk != intentjudge.RiskMedium {
		t.Fatalf("RefineMinRisk default = %q, want %q", a.intentJudge.RefineMinRisk, intentjudge.RiskMedium)
	}
}

func TestWithIntentJudge_PreservesExplicitRisk(t *testing.T) {
	repo := &memIntentVerdictRepo{}
	a := NewAgent(nil, nil, nil, nil, nil, WithIntentJudge(repo, nil, intentjudge.RiskHigh))
	if a.intentJudge == nil || a.intentJudge.RefineMinRisk != intentjudge.RiskHigh {
		t.Fatalf("RefineMinRisk = %q, want %q", a.intentJudge.RefineMinRisk, intentjudge.RiskHigh)
	}
}

// TestEvaluate_NoProjectSkipsPersistence — global/cross-project tools
// (e.g. list_projects) carry no project context. The project-scoped
// intent_verdict repo rejects an empty project_id, so evaluate must
// skip the insert entirely (no failed round-trip, no warn log) and
// still return the heuristic verdict so the tool call proceeds.
func TestEvaluate_NoProjectSkipsPersistence(t *testing.T) {
	repo := &memIntentVerdictRepo{}
	// A refiner that WOULD persist a refinement if ever reached — so a
	// non-empty repo.refined would prove the early return failed to skip it.
	refiner := &intentjudge.LLMRefiner{Provider: &stubProvider{resp: respWith(
		`{"risk":"low","confidence":0.9,"recommendation":"allow","reasoning":"read-only"}`,
	)}}
	cfg := &intentJudgeConfig{Repo: repo, Refiner: refiner, RefineMinRisk: intentjudge.RiskLow}

	// projectID is whitespace-only (global tool, e.g. list_projects).
	v := cfg.evaluate(context.Background(), "  ", nil, nil, nil,
		"list_projects", `{}`,
		runSync, zerolog.Nop())

	if v.Tier != intentjudge.TierHeuristic {
		t.Errorf("Tier = %q, want heuristic verdict still returned", v.Tier)
	}
	if len(repo.inserted) != 0 {
		t.Errorf("no-project call must NOT persist a verdict; inserted = %d", len(repo.inserted))
	}
	if len(repo.refined) != 0 {
		t.Errorf("no-project call must NOT run the async refiner; refined = %d", len(repo.refined))
	}
}
