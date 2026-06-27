package graph

import (
	"context"
	"sync"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// fakeUsageRecorder captures TaskLLMUsage rows in memory so
// tests can assert the pipeline's cost-tracking contract:
//
//  1. One row per stage that actually called the LLM.
//  2. Source = TaskLLMUsageSourceKGExtraction.
//  3. project_id / step_id (= chunk_id) populated; task_id nil.
//  4. Role tags identify the stage (kg_extractor / kg_resolver
//     / kg_relationship / kg_validator).
//  5. Token counts and model match the per-stage metrics.
//  6. cost_usd computed via the wired Pricing table.
type fakeUsageRecorder struct {
	mu   sync.Mutex
	rows []persistence.TaskLLMUsage
}

func (f *fakeUsageRecorder) Record(_ context.Context, u *persistence.TaskLLMUsage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, *u)
	return nil
}

// fakePricing returns a fixed per-token rate so tests assert
// the orchestrator's call shape, not the pricing.Table internals.
type fakePricing struct {
	perTok float64
}

func (f *fakePricing) CostUSD(_ string, prompt, completion int) float64 {
	return float64(prompt+completion) * f.perTok
}

func TestPipeline_RecordsLLMUsagePerStage(t *testing.T) {
	chunk := ChunkInput{ID: "chunk-cost", ProjectID: "proj-billing", Content: "Vadim chose PostgreSQL 16."}

	extractReply := `[
	  {"type":"PERSON","name":"Vadim","char_start":0,"char_end":5,"surface":"Vadim"},
	  {"type":"TECHNOLOGY","name":"PostgreSQL 16","char_start":12,"char_end":25,"surface":"PostgreSQL 16"}
	]`
	resolveReply := `[
	  {"candidate_id":"cand-0","decision":"new"},
	  {"candidate_id":"cand-1","decision":"new"}
	]`
	relReply := `[{"from":"kent-new-person-1","to":"kent-new-technology-2","predicate":"DEPENDS_ON","evidence":"Vadim chose PostgreSQL 16"}]`
	valReply := `[{"id":"prop-0","score":0.9,"reason":"explicit"}]`

	p, _, _, _ := newPipeline(t, extractReply, resolveReply, relReply, valReply)
	rec := &fakeUsageRecorder{}
	p.LLMUsage = rec
	p.Pricing = &fakePricing{perTok: 0.0001}

	_, err := p.RunChunk(context.Background(), chunk)
	if err != nil {
		t.Fatalf("RunChunk: %v", err)
	}

	if len(rec.rows) != 4 {
		t.Fatalf("expected 4 usage rows (one per stage), got %d: %+v", len(rec.rows), rec.rows)
	}

	roles := map[string]bool{}
	for _, r := range rec.rows {
		roles[r.Role] = true
		if r.Source != persistence.TaskLLMUsageSourceKGExtraction {
			t.Errorf("row source = %q, want %q", r.Source, persistence.TaskLLMUsageSourceKGExtraction)
		}
		if r.ProjectID != "proj-billing" {
			t.Errorf("row project_id = %q, want proj-billing", r.ProjectID)
		}
		if r.StepID != "chunk-cost" {
			t.Errorf("row step_id = %q, want chunk-cost (the chunk_id)", r.StepID)
		}
		if r.TaskID != nil {
			t.Errorf("row task_id = %v, want nil (KG worker is not task-scoped)", r.TaskID)
		}
		if r.Iterations != 1 {
			t.Errorf("row iterations = %d, want 1", r.Iterations)
		}
	}
	for _, want := range []string{"kg_extractor", "kg_resolver", "kg_relationship", "kg_validator"} {
		if !roles[want] {
			t.Errorf("missing usage row for role %q", want)
		}
	}
}

func TestPipeline_SkipsUsageWhenStageEmittedZeroTokens(t *testing.T) {
	// Pipeline that bails after extraction: chunk has no entities,
	// so resolver/relationship/validator stages never run. We
	// must not record usage rows for stages that produced no
	// tokens — those would clutter the spend dashboard with
	// zero-cost entries.
	chunk := ChunkInput{ID: "chunk-empty", ProjectID: "proj-1", Content: "uneventful chunk"}
	p, _, _, _ := newPipeline(t, `[]`, ``, ``, ``)
	rec := &fakeUsageRecorder{}
	p.LLMUsage = rec
	p.Pricing = &fakePricing{perTok: 0.001}

	_, err := p.RunChunk(context.Background(), chunk)
	if err != nil {
		t.Fatalf("RunChunk: %v", err)
	}
	// Extractor was called and got zero tokens reported by the
	// fake (it stamps Model="fake" but the fake doesn't populate
	// usage). Either zero rows OR rows-with-zero-tokens are
	// expected; the contract is "no zero-token rows".
	for _, r := range rec.rows {
		if r.PromptTokens == 0 && r.CompletionTokens == 0 {
			t.Errorf("unexpected zero-token usage row: %+v", r)
		}
	}
}

func TestPipeline_RecordsUsageWithoutPricing(t *testing.T) {
	// Pricing nil → cost_usd stamped 0 but the row still lands.
	// This keeps the spend dashboard's token-volume view useful
	// even when the operator hasn't priced a model yet.
	chunk := ChunkInput{ID: "chunk-noprice", ProjectID: "proj-1", Content: "Vadim picked Postgres."}
	extractReply := `[{"type":"PERSON","name":"Vadim","char_start":0,"char_end":5,"surface":"Vadim"}]`
	resolveReply := `[{"candidate_id":"cand-0","decision":"new"}]`
	p, _, _, _ := newPipeline(t, extractReply, resolveReply, "[]", "[]")
	rec := &fakeUsageRecorder{}
	p.LLMUsage = rec
	// Pricing intentionally nil.

	_, err := p.RunChunk(context.Background(), chunk)
	if err != nil {
		t.Fatalf("RunChunk: %v", err)
	}
	for _, r := range rec.rows {
		if r.CostUSD != 0 {
			t.Errorf("expected cost_usd = 0 with nil Pricing, got %v in row %+v", r.CostUSD, r)
		}
	}
}

func TestPipeline_NilUsageRecorderIsNoop(t *testing.T) {
	chunk := ChunkInput{ID: "chunk-nilrec", ProjectID: "proj-1", Content: "Vadim picked Postgres."}
	p, _, _, _ := newPipeline(t, "[]", "", "", "")
	// LLMUsage left nil — the orchestrator must tolerate this
	// without panicking. Tests that don't care about cost
	// should be able to skip wiring it.
	_, err := p.RunChunk(context.Background(), chunk)
	if err != nil {
		t.Fatalf("RunChunk: %v", err)
	}
}
