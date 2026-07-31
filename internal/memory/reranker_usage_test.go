package memory

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// The reranker was the largest unattributed LLM spender in the daemon:
// it runs on every memory search, so its volume scales with RETRIEVAL
// rather than with tasks. The operator found it on 2026-07-30 from a
// real discrepancy — a consumer instance reported 5,134 calls / $8.79
// over 24h while the gateway counted 6,771 / $12.28 against the same
// key. Calls diverged 1.32x and spend 1.40x; both moving together is
// the signature of a whole CLASS of calls being invisible rather than
// a pricing-table fault. These tests pin the accounting so the class
// cannot go dark again.

func TestLLMReranker_RecordsUsageForBilledCall(t *testing.T) {
	fp := &titlerFakeProvider{replies: []titlerReply{
		{content: `{"scores":{"0":0.2,"1":0.9}}`},
	}}
	rec := &fakeUsageRecorder{}
	rr := &LLMReranker{
		Client:   fp,
		Model:    "rerank-model",
		Logger:   zerolog.Nop(),
		LLMUsage: rec,
		Pricing:  fixedPricing{},
	}

	in := []SearchResult{
		{ChunkID: "a", ProjectID: "proj-1"},
		{ChunkID: "b", ProjectID: "proj-1"},
	}
	out, err := rr.Rerank(context.Background(), "who is on call", in)
	if err != nil {
		t.Fatalf("Rerank returned error: %v", err)
	}
	if out[0].ChunkID != "b" {
		t.Fatalf("expected rerank to reorder b first, got %q", out[0].ChunkID)
	}

	if len(rec.rows) != 1 {
		t.Fatalf("expected exactly 1 usage row for 1 billed call, got %d", len(rec.rows))
	}
	row := rec.rows[0]
	if row.ProjectID != "proj-1" {
		t.Errorf("ProjectID = %q, want proj-1 (attribution comes from the candidates)", row.ProjectID)
	}
	if row.Role != rerankerRole {
		t.Errorf("Role = %q, want %q", row.Role, rerankerRole)
	}
	if row.Source != persistence.TaskLLMUsageSourceMemoryReranker {
		t.Errorf("Source = %q, want %q", row.Source, persistence.TaskLLMUsageSourceMemoryReranker)
	}
	if row.PromptTokens != 120 || row.CompletionTokens != 30 {
		t.Errorf("tokens = (%d,%d), want (120,30)", row.PromptTokens, row.CompletionTokens)
	}
	// fixedPricing: prompt/1000 + 2*completion/1000 = 0.12 + 0.06.
	if row.CostUSD < 0.1799 || row.CostUSD > 0.1801 {
		t.Errorf("CostUSD = %v, want ~0.18", row.CostUSD)
	}
}

// TestLLMReranker_RecordsUsageWhenParseFails is the regression test for
// the blind spot found while investigating the KG extractor on
// 2026-07-31: a call whose response cannot be parsed WAS STILL BILLED.
// The extractor's equivalent path laundered ~83% of its spend into
// "nothing found" because the failure was classified before the cost
// was recorded. Record on billing, not on success — otherwise the
// reranker's degrade-to-RRF path (which is silent by design) becomes a
// second invisible spender.
func TestLLMReranker_RecordsUsageWhenParseFails(t *testing.T) {
	fp := &titlerFakeProvider{replies: []titlerReply{
		{content: `not json at all`},
	}}
	rec := &fakeUsageRecorder{}
	rr := &LLMReranker{
		Client:   fp,
		Logger:   zerolog.Nop(),
		LLMUsage: rec,
		Pricing:  fixedPricing{},
	}

	in := []SearchResult{
		{ChunkID: "a", ProjectID: "proj-2"},
		{ChunkID: "b", ProjectID: "proj-2"},
	}
	out, err := rr.Rerank(context.Background(), "q", in)
	if err != nil {
		t.Fatalf("Rerank must degrade, not error: %v", err)
	}
	if len(out) != 2 || out[0].ChunkID != "a" {
		t.Fatalf("expected degrade to RRF order, got %+v", out)
	}
	if len(rec.rows) != 1 {
		t.Fatalf("an unparseable response was still BILLED — expected 1 usage row, got %d", len(rec.rows))
	}
	if rec.rows[0].ProjectID != "proj-2" {
		t.Errorf("ProjectID = %q, want proj-2", rec.rows[0].ProjectID)
	}
}

// A nil recorder must stay safe — the reranker is wired in deployments
// without a usage repository, and failing to bill is dashboard
// fidelity, not correctness.
func TestLLMReranker_NilRecorderIsSafe(t *testing.T) {
	fp := &titlerFakeProvider{replies: []titlerReply{
		{content: `{"scores":{"0":0.5,"1":0.4}}`},
	}}
	rr := &LLMReranker{Client: fp, Logger: zerolog.Nop()}

	in := []SearchResult{{ChunkID: "a", ProjectID: "p"}, {ChunkID: "b", ProjectID: "p"}}
	if _, err := rr.Rerank(context.Background(), "q", in); err != nil {
		t.Fatalf("nil recorder must not error: %v", err)
	}
}

// No candidates reach the LLM below the 2-result floor, so nothing is
// billed and nothing may be recorded — an empty row would pollute the
// spend dashboard with calls that never happened.
func TestLLMReranker_NoUsageWhenCallSkipped(t *testing.T) {
	fp := &titlerFakeProvider{}
	rec := &fakeUsageRecorder{}
	rr := &LLMReranker{Client: fp, Logger: zerolog.Nop(), LLMUsage: rec}

	in := []SearchResult{{ChunkID: "only", ProjectID: "p"}}
	if _, err := rr.Rerank(context.Background(), "q", in); err != nil {
		t.Fatal(err)
	}
	if len(rec.rows) != 0 {
		t.Fatalf("no LLM call was made; expected 0 usage rows, got %d", len(rec.rows))
	}
}

// The accounting is only real if the container actually wires it, which
// is the step that was missing for the reranker, the memetic architect
// and (until 2026-07-15) the instinct distiller. Pin the constructor
// seam so "the field exists" cannot again be mistaken for "the spend is
// recorded".
func TestNewConfiguredReranker_WiresUsageRecorder(t *testing.T) {
	rec := &fakeUsageRecorder{}
	r := NewConfiguredReranker(
		true, &titlerFakeProvider{}, "m", 20, 8, 600, zerolog.Nop(),
		WithRerankerUsage(rec, fixedPricing{}),
	)
	lr, ok := r.(*LLMReranker)
	if !ok {
		t.Fatalf("expected *LLMReranker, got %T", r)
	}
	if lr.LLMUsage == nil {
		t.Error("LLMUsage not wired — rerank spend would stay invisible")
	}
	if lr.Pricing == nil {
		t.Error("Pricing not wired — rows would land at $0")
	}
}

// A disabled reranker makes no LLM calls, so passing the option must not
// change the Noop decision.
func TestNewConfiguredReranker_UsageOptionDoesNotEnable(t *testing.T) {
	rec := &fakeUsageRecorder{}
	r := NewConfiguredReranker(
		false, &titlerFakeProvider{}, "m", 20, 8, 600, zerolog.Nop(),
		WithRerankerUsage(rec, fixedPricing{}),
	)
	if _, ok := r.(NoopReranker); !ok {
		t.Fatalf("disabled reranker must stay Noop, got %T", r)
	}
}

// Review finding (companion task_20260731103649, finding TWO): memory
// search is project-scoped today, so a candidate set spanning projects
// would be a bug at the search layer — but the reranker cannot see the
// query scope, and would silently attribute ALL the spend to whichever
// project happened to sort first. If cross-project search is ever added
// (an admin "search all projects" view is the obvious way in), that
// becomes effectively random attribution with no paper trail. Warn, and
// still bill: losing the row would be worse than attributing it once.
func TestLLMReranker_WarnsWhenCandidatesSpanProjects(t *testing.T) {
	var logs bytes.Buffer
	fp := &titlerFakeProvider{replies: []titlerReply{
		{content: `{"scores":{"0":0.9,"1":0.1}}`},
	}}
	rec := &fakeUsageRecorder{}
	rr := &LLMReranker{
		Client:   fp,
		Logger:   zerolog.New(&logs),
		LLMUsage: rec,
		Pricing:  fixedPricing{},
	}

	in := []SearchResult{
		{ChunkID: "a", ProjectID: "proj-A"},
		{ChunkID: "b", ProjectID: "proj-B"},
	}
	if _, err := rr.Rerank(context.Background(), "q", in); err != nil {
		t.Fatalf("Rerank must not fail: %v", err)
	}

	if len(rec.rows) != 1 {
		t.Fatalf("spend must still be billed once, got %d rows", len(rec.rows))
	}
	out := logs.String()
	if !strings.Contains(out, "ambiguous") {
		t.Errorf("expected an ambiguous-attribution warning, got: %s", out)
	}
}

// The common case must stay quiet — a warning on every single-project
// search would be noise that trains operators to ignore it.
func TestLLMReranker_NoWarnForSingleProject(t *testing.T) {
	var logs bytes.Buffer
	fp := &titlerFakeProvider{replies: []titlerReply{
		{content: `{"scores":{"0":0.9,"1":0.1}}`},
	}}
	rr := &LLMReranker{
		Client:   fp,
		Logger:   zerolog.New(&logs),
		LLMUsage: &fakeUsageRecorder{},
		Pricing:  fixedPricing{},
	}
	in := []SearchResult{{ChunkID: "a", ProjectID: "p"}, {ChunkID: "b", ProjectID: "p"}}
	if _, err := rr.Rerank(context.Background(), "q", in); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "ambiguous") {
		t.Errorf("single-project search must not warn, got: %s", logs.String())
	}
}
