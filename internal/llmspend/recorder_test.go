package llmspend

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

type fakeRepo struct {
	rows []*persistence.TaskLLMUsage
	err  error
}

func (f *fakeRepo) Record(_ context.Context, u *persistence.TaskLLMUsage) error {
	if f.err != nil {
		return f.err
	}
	f.rows = append(f.rows, u)
	return nil
}

// fixedPricing bills $1 per 1k prompt tokens and $2 per 1k completion tokens.
type fixedPricing struct{}

func (fixedPricing) CostUSD(_ string, prompt, completion int) float64 {
	return float64(prompt)/1000 + float64(completion)*2/1000
}

type countingSink struct{ counts map[string]int }

func (c *countingSink) Inc(source string) {
	if c.counts == nil {
		c.counts = map[string]int{}
	}
	c.counts[source]++
}

func TestRecord_WritesTheRowShapeCallersUsedToHandRoll(t *testing.T) {
	repo := &fakeRepo{}
	r := New(repo, fixedPricing{}, "memory_titler", "memory_titler")

	task := "task_1"
	r.Record(context.Background(), Input{
		ProjectID:    "janka",
		Model:        "m",
		PromptTokens: 1000, CompletionTokens: 500,
		TaskID: &task, StepID: "chunk_7",
	})

	if len(repo.rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(repo.rows))
	}
	row := repo.rows[0]
	// The fields each of the 19 hand-rolled literals decided independently.
	if row.Source != "memory_titler" || row.Role != "memory_titler" {
		t.Errorf("source/role = %q/%q, want memory_titler for both", row.Source, row.Role)
	}
	if row.ProjectID != "janka" || row.StepID != "chunk_7" {
		t.Errorf("attribution not carried: project=%q step=%q", row.ProjectID, row.StepID)
	}
	if row.TaskID == nil || *row.TaskID != "task_1" {
		t.Errorf("TaskID not carried: %v", row.TaskID)
	}
	if row.ID == "" {
		t.Error("ID not generated — every caller used to do this by hand")
	}
	if row.Iterations != 1 {
		t.Errorf("Iterations = %d, want 1", row.Iterations)
	}
	// 1000 prompt + 500 completion under fixedPricing = 1.0 + 1.0.
	if row.CostUSD != 2.0 {
		t.Errorf("CostUSD = %v, want 2.0", row.CostUSD)
	}
}

// TestRecord_ZeroValueRecorderIsLoud is the core anti-regression property. A
// zero-value Recorder is what an unwired component would hold, and the whole
// defect class is that such a thing does nothing QUIETLY.
func TestRecord_ZeroValueRecorderIsLoud(t *testing.T) {
	sink := &countingSink{}
	var zero Recorder // never constructed — exactly what "unwired" looks like
	zero.sink = sink

	zero.Record(context.Background(), Input{ProjectID: "janka", Model: "m", PromptTokens: 10})

	if sink.counts["__unset__"] != 1 {
		t.Errorf("a zero-value Recorder recorded nothing and reported nothing (%v) — "+
			"silent absence is the defect this package removes", sink.counts)
	}
	if zero.Enabled() {
		t.Error("zero value must not report itself Enabled")
	}
}

// TestDisabled_WritesNothingAndSaysSo: an explicit no-billing choice is silent by
// design, unlike the zero value. The difference matters — Disabled() is a decision
// a reviewer can see and slice D's law can find.
func TestDisabled_WritesNothingAndSaysSo(t *testing.T) {
	sink := &countingSink{}
	r := Disabled()
	r.sink = sink
	r.Record(context.Background(), Input{ProjectID: "janka", Model: "m", PromptTokens: 10})

	if len(sink.counts) != 0 {
		t.Errorf("Disabled() must not count a failure: %v", sink.counts)
	}
	if r.Enabled() {
		t.Error("Disabled() must report Enabled() == false")
	}
}

// TestNew_NilRepoIsDisabledNotHalfBuilt: a deployment without a ledger repo must
// get a deliberately disabled recorder, never an enabled one holding nil — that
// would be the nil dereference this package exists to prevent.
func TestNew_NilRepoIsDisabledNotHalfBuilt(t *testing.T) {
	r := New(nil, fixedPricing{}, "s", "role")
	if r.Enabled() {
		t.Error("New(nil repo) must yield a disabled recorder")
	}
	// Must not panic.
	r.Record(context.Background(), Input{ProjectID: "p", Model: "m", PromptTokens: 5})
}

// TestRecord_LedgerFailureIsCountedNotSwallowed is the answer to the review's
// sharpest point: the old behaviour was nineteen `_ = repo.Record(...)` statements
// with nowhere to hang a metric, so a systematic write failure was invisible.
func TestRecord_LedgerFailureIsCountedNotSwallowed(t *testing.T) {
	sink := &countingSink{}
	repo := &fakeRepo{err: errors.New("db down")}
	r := New(repo, fixedPricing{}, "memory_embedder", "memory_embedder", WithFailureSink(sink))

	r.Record(context.Background(), Input{ProjectID: "janka", Model: "m", PromptTokens: 42})

	if sink.counts["memory_embedder"] != 1 {
		t.Errorf("a failed ledger write was not counted: %v — an unalarmed silence is how "+
			"three regressions escaped", sink.counts)
	}
}

// TestRecord_NoTokensWritesNothing: a call that billed nothing should not pollute
// the ledger with an empty row. Distinct from a failure — nothing went wrong.
func TestRecord_NoTokensWritesNothing(t *testing.T) {
	repo := &fakeRepo{}
	r := New(repo, fixedPricing{}, "s", "role")
	r.Record(context.Background(), Input{ProjectID: "janka", Model: "m"})
	if len(repo.rows) != 0 {
		t.Errorf("got %d rows for a zero-token call, want 0", len(repo.rows))
	}
}

// TestRecord_TokensEstimatedSurvives: migration 159's flag must reach the row, or
// a derived count silently reads as a provider measurement.
func TestRecord_TokensEstimatedSurvives(t *testing.T) {
	repo := &fakeRepo{}
	r := New(repo, fixedPricing{}, "memory_embedder", "memory_embedder")
	r.Record(context.Background(), Input{
		ProjectID: "janka", Model: "cohere.embed-v4",
		PromptTokens: 4096, TokensEstimated: true,
	})
	if len(repo.rows) != 1 || !repo.rows[0].TokensEstimated {
		t.Error("TokensEstimated did not survive into the row")
	}
}

// TestRecord_NilPricingStillRecordsTokens: a local endpoint charges nothing and a
// hosted model may be missing from pricing.yaml. Either way the TOKENS are real
// and must be recorded; only the dollar figure is unknown.
func TestRecord_NilPricingStillRecordsTokens(t *testing.T) {
	repo := &fakeRepo{}
	r := New(repo, nil, "memory_embedder", "memory_embedder")
	r.Record(context.Background(), Input{ProjectID: "janka", Model: "m", PromptTokens: 100})

	if len(repo.rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(repo.rows))
	}
	if repo.rows[0].PromptTokens != 100 {
		t.Errorf("tokens lost when pricing was nil: %d", repo.rows[0].PromptTokens)
	}
	if repo.rows[0].CostUSD != 0 {
		t.Errorf("CostUSD = %v with no pricing table, want 0", repo.rows[0].CostUSD)
	}
}

func (f *fakeRepo) Upsert(_ context.Context, u *persistence.TaskLLMUsage) error {
	if f.err != nil {
		return f.err
	}
	// Replace-by-id, like the real repo's ON CONFLICT DO UPDATE.
	for i, existing := range f.rows {
		if existing.ID == u.ID {
			f.rows[i] = u
			return nil
		}
	}
	f.rows = append(f.rows, u)
	return nil
}

// TestUpsert_ReplacesByStableID: the streaming path reports cumulative usage per
// iteration, so the second report must REPLACE the first. Appending instead would
// multiply a step's cost by its iteration count.
func TestUpsert_ReplacesByStableID(t *testing.T) {
	repo := &fakeRepo{}
	r := New(repo, fixedPricing{}, "workflow_step", "worker")

	for _, tokens := range []int{100, 250} {
		if err := r.Upsert(context.Background(), "tu_task_step_worker", Input{
			ProjectID: "janka", Model: "m", PromptTokens: tokens,
		}); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}
	if len(repo.rows) != 1 {
		t.Fatalf("got %d rows, want 1 — cumulative reports must overwrite, not accumulate", len(repo.rows))
	}
	if repo.rows[0].PromptTokens != 250 {
		t.Errorf("PromptTokens = %d, want the latest report (250)", repo.rows[0].PromptTokens)
	}
}

// TestUpsert_ReturnsErrorUnlikeRecord pins the deliberate asymmetry. Record is
// called after the provider already charged, with nobody to tell; Upsert is called
// by an HTTP handler whose client (the agent) can retry, and whose contract already
// answers 500.
func TestUpsert_ReturnsErrorUnlikeRecord(t *testing.T) {
	repo := &fakeRepo{err: errors.New("db down")}
	r := New(repo, fixedPricing{}, "workflow_step", "worker")

	if err := r.Upsert(context.Background(), "tu_1", Input{ProjectID: "p", Model: "m", PromptTokens: 5}); err == nil {
		t.Error("Upsert must surface a failed write — the agent can retry, so swallowing it loses reported usage")
	}
	// Record, on the same failing repo, must NOT panic or block.
	r.Record(context.Background(), Input{ProjectID: "p", Model: "m", PromptTokens: 5})
}

func TestUpsert_RequiresAStableID(t *testing.T) {
	r := New(&fakeRepo{}, fixedPricing{}, "workflow_step", "worker")
	if err := r.Upsert(context.Background(), "", Input{ProjectID: "p", Model: "m", PromptTokens: 5}); err == nil {
		t.Error("an empty id would generate a fresh row per report, defeating the point of Upsert")
	}
}

// TestRow_OverridesAreCarried: the agent path reports its own role and cost, which
// a Recorder with a fixed role and a pricing table cannot express.
func TestRow_OverridesAreCarried(t *testing.T) {
	repo := &fakeRepo{}
	r := New(repo, fixedPricing{}, "workflow_step", "component-default-role")
	cost := 4.25
	key := "akey_1"

	if err := r.Upsert(context.Background(), "tu_2", Input{
		ProjectID: "janka", Model: "m", PromptTokens: 10,
		RoleOverride: "researcher", CostUSD: &cost, APIKeyID: &key, Iterations: 7,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	row := repo.rows[0]
	if row.Role != "researcher" {
		t.Errorf("Role = %q, want the per-call override", row.Role)
	}
	if row.CostUSD != 4.25 {
		t.Errorf("CostUSD = %v, want the caller's figure (not the pricing table's)", row.CostUSD)
	}
	if row.APIKeyID == nil || *row.APIKeyID != "akey_1" {
		t.Errorf("APIKeyID not carried: %v", row.APIKeyID)
	}
	if row.Iterations != 7 {
		t.Errorf("Iterations = %d, want 7", row.Iterations)
	}
	// Source stays the Recorder's — it is a property of the component, not the call.
	if row.Source != "workflow_step" {
		t.Errorf("Source = %q, want the Recorder's", row.Source)
	}
}

// TestRecord_CacheHitWritesAZeroTokenRow is the exception to "no tokens, no row",
// and migrating the KG pipeline is what surfaced it: that component deliberately
// writes zero-token rows for response-cache hits so a cached stage stays visible.
// A naive migration would have silently deleted that visibility.
func TestRecord_CacheHitWritesAZeroTokenRow(t *testing.T) {
	repo := &fakeRepo{}
	r := New(repo, fixedPricing{}, "kg_extraction", "kg_extractor")

	r.Record(context.Background(), Input{ProjectID: "janka", Model: "m", CacheHit: true})

	if len(repo.rows) != 1 {
		t.Fatalf("got %d rows, want 1 — a cached stage must stay visible, not vanish", len(repo.rows))
	}
	row := repo.rows[0]
	if !row.CacheHit {
		t.Error("CacheHit flag did not reach the row")
	}
	if row.PromptTokens != 0 || row.CostUSD != 0 {
		t.Errorf("a hit reached no provider, so it must cost nothing: tokens=%d cost=%v",
			row.PromptTokens, row.CostUSD)
	}
}
