package ned

import (
	"context"
	"errors"
	"testing"
	"vornik.io/vornik/internal/llmspend"

	"vornik.io/vornik/internal/memory/graph"
	"vornik.io/vornik/internal/persistence"
)

// fakeExtractor returns scripted candidates + metrics and records that it was
// called (so a test can assert personal scope never invokes NED — asserted in
// the dispatcher package — and that shared scope always does).
type fakeExtractor struct {
	cands   []graph.Candidate
	metrics *graph.ExtractMetrics
	err     error
	calls   int
}

func (f *fakeExtractor) Extract(_ context.Context, _ string) ([]graph.Candidate, *graph.ExtractMetrics, error) {
	f.calls++
	return f.cands, f.metrics, f.err
}

// fakeResolver returns scripted resolutions in input order.
type fakeResolver struct {
	resns   []graph.Resolution
	metrics *graph.ResolveMetrics
	err     error
	calls   int
}

func (f *fakeResolver) Resolve(_ context.Context, _ string, _ []graph.Candidate) ([]graph.Resolution, *graph.ResolveMetrics, error) {
	f.calls++
	return f.resns, f.metrics, f.err
}

// recordingUsage captures the task_llm_usage rows the gate records.
type recordingUsage struct {
	rows []*persistence.TaskLLMUsage
}

func (r *recordingUsage) Record(_ context.Context, u *persistence.TaskLLMUsage) error {
	r.rows = append(r.rows, u)
	return nil
}

// Upsert satisfies llmspend.UsageRepo; the NED gate only ever calls Record, so
// this just delegates.
func (r *recordingUsage) Upsert(ctx context.Context, u *persistence.TaskLLMUsage) error {
	return r.Record(ctx, u)
}

// fixedPricing returns a constant cost so the billing test can assert a
// non-zero cost_usd was computed.
type fixedPricing struct{ usd float64 }

func (p fixedPricing) CostUSD(_ string, _, _ int) float64 { return p.usd }

func person(name string) graph.Candidate {
	return graph.Candidate{Type: persistence.EntityTypePerson, Name: name}
}

// no PERSON candidate → proceed, and the resolver is never consulted.
func TestScreen_NoPersonProceeds(t *testing.T) {
	res := &fakeResolver{}
	g := &Gate{
		Extractor: &fakeExtractor{cands: []graph.Candidate{{Type: persistence.EntityTypeProduct, Name: "Widget"}}},
		Resolver:  res,
	}
	d := g.Screen(context.Background(), "proj", "we shipped Widget")
	if !d.Proceeds() || !d.Authorization().Granted() {
		t.Fatalf("no-person deposit must proceed with a granted token; got verdict=%d granted=%v", d.Verdict, d.Authorization().Granted())
	}
	if res.calls != 0 {
		t.Errorf("resolver must not be consulted when no PERSON was extracted; calls=%d", res.calls)
	}
	if len(d.MatchedEntityIDs) != 0 {
		t.Errorf("no matched ids expected; got %v", d.MatchedEntityIDs)
	}
}

// all persons resolve to `match` → proceed, and the matched entity ids are
// returned so the dispatcher can record a data-subject link per subject.
func TestScreen_AllMatchProceedsWithIDs(t *testing.T) {
	g := &Gate{
		Extractor: &fakeExtractor{cands: []graph.Candidate{person("Alice"), person("Bob")}},
		Resolver: &fakeResolver{resns: []graph.Resolution{
			{Decision: "match", MatchID: "ent-alice"},
			{Decision: "match", MatchID: "ent-bob"},
		}},
	}
	d := g.Screen(context.Background(), "proj", "Alice and Bob agreed")
	if !d.Proceeds() || !d.Authorization().Granted() {
		t.Fatalf("all-match must proceed with a granted token; verdict=%d", d.Verdict)
	}
	if len(d.MatchedEntityIDs) != 2 || d.MatchedEntityIDs[0] != "ent-alice" || d.MatchedEntityIDs[1] != "ent-bob" {
		t.Errorf("matched ids wrong: %v", d.MatchedEntityIDs)
	}
}

// any `new` → block, naming the unresolved person, with no token.
func TestScreen_NewBlocksAndNamesPerson(t *testing.T) {
	g := &Gate{
		Extractor: &fakeExtractor{cands: []graph.Candidate{person("Alice"), person("Carol")}},
		Resolver: &fakeResolver{resns: []graph.Resolution{
			{Decision: "match", MatchID: "ent-alice"},
			{Decision: "new"},
		}},
	}
	d := g.Screen(context.Background(), "proj", "Alice knows Carol")
	if d.Proceeds() || d.Authorization().Granted() {
		t.Fatalf("a `new` person must block with no token; verdict=%d granted=%v", d.Verdict, d.Authorization().Granted())
	}
	if d.Verdict != VerdictBlock {
		t.Fatalf("verdict = %d, want VerdictBlock", d.Verdict)
	}
	if len(d.BlockedPersons) != 1 || d.BlockedPersons[0] != "Carol" {
		t.Errorf("blocked persons must name Carol only; got %v", d.BlockedPersons)
	}
}

// any `ambiguous` → block.
func TestScreen_AmbiguousBlocks(t *testing.T) {
	g := &Gate{
		Extractor: &fakeExtractor{cands: []graph.Candidate{person("Alice")}},
		Resolver:  &fakeResolver{resns: []graph.Resolution{{Decision: "ambiguous"}}},
	}
	d := g.Screen(context.Background(), "proj", "Alice is around")
	if d.Verdict != VerdictBlock || d.Authorization().Granted() {
		t.Fatalf("ambiguous must block with no token; verdict=%d", d.Verdict)
	}
}

// a `match` with an EMPTY id cannot be linked, so it blocks (safe direction).
func TestScreen_MatchWithoutIDBlocks(t *testing.T) {
	g := &Gate{
		Extractor: &fakeExtractor{cands: []graph.Candidate{person("Alice")}},
		Resolver:  &fakeResolver{resns: []graph.Resolution{{Decision: "match", MatchID: ""}}},
	}
	if d := g.Screen(context.Background(), "proj", "Alice"); d.Verdict != VerdictBlock {
		t.Fatalf("a match with no id must block; verdict=%d", d.Verdict)
	}
}

// D6.3: an extract error fails CLOSED — VerdictError, distinct from block, no
// token, and the resolver is never reached.
func TestScreen_ExtractErrorFailsClosed(t *testing.T) {
	res := &fakeResolver{}
	g := &Gate{
		Extractor: &fakeExtractor{err: errors.New("model timeout")},
		Resolver:  res,
	}
	d := g.Screen(context.Background(), "proj", "anything")
	if d.Verdict != VerdictError || d.Authorization().Granted() {
		t.Fatalf("an extract error must fail closed with no token; verdict=%d granted=%v", d.Verdict, d.Authorization().Granted())
	}
	if d.Err == nil {
		t.Error("VerdictError must carry the underlying error")
	}
	if res.calls != 0 {
		t.Errorf("resolver must not run after an extract error; calls=%d", res.calls)
	}
}

// D6.3: a resolve error also fails CLOSED.
func TestScreen_ResolveErrorFailsClosed(t *testing.T) {
	g := &Gate{
		Extractor: &fakeExtractor{cands: []graph.Candidate{person("Alice")}},
		Resolver:  &fakeResolver{err: errors.New("gateway 503")},
	}
	d := g.Screen(context.Background(), "proj", "Alice")
	if d.Verdict != VerdictError || d.Authorization().Granted() {
		t.Fatalf("a resolve error must fail closed; verdict=%d", d.Verdict)
	}
}

// an unconfigured gate fails CLOSED rather than proceeding.
func TestScreen_UnconfiguredFailsClosed(t *testing.T) {
	if d := (&Gate{}).Screen(context.Background(), "proj", "x"); d.Verdict != VerdictError {
		t.Fatalf("an unconfigured gate must fail closed; verdict=%d", d.Verdict)
	}
	var nilGate *Gate
	if d := nilGate.Screen(context.Background(), "proj", "x"); d.Verdict != VerdictError {
		t.Fatalf("a nil gate must fail closed; verdict=%d", d.Verdict)
	}
}

// D6.4 (the reranker/distiller regression class): the gate records a
// task_llm_usage row for its extract + resolve spend, under the distinct
// chat_remember_ned source, with cost computed from pricing.
func TestScreen_RecordsBillingRows(t *testing.T) {
	usage := &recordingUsage{}
	g := &Gate{
		Extractor: &fakeExtractor{
			cands:   []graph.Candidate{person("Alice")},
			metrics: &graph.ExtractMetrics{Model: "gpt-oss-120b", PromptTokens: 300, CompletionTokens: 40},
		},
		Resolver: &fakeResolver{
			resns:   []graph.Resolution{{Decision: "match", MatchID: "ent-alice"}},
			metrics: &graph.ResolveMetrics{Model: "gpt-oss-120b", PromptTokens: 500, CompletionTokens: 20},
		},
		Spend: llmspend.New(usage, fixedPricing{usd: 0.0021},
			persistence.TaskLLMUsageSourceChatRememberNED, RoleExtractor),
	}
	if d := g.Screen(context.Background(), "proj-b", "Alice again"); !d.Proceeds() {
		t.Fatalf("expected proceed; verdict=%d", d.Verdict)
	}
	if len(usage.rows) != 2 {
		t.Fatalf("expected 2 billing rows (extract + resolve); got %d", len(usage.rows))
	}
	for _, r := range usage.rows {
		if r.Source != persistence.TaskLLMUsageSourceChatRememberNED {
			t.Errorf("row source = %q, want chat_remember_ned", r.Source)
		}
		if r.ProjectID != "proj-b" {
			t.Errorf("row project = %q, want proj-b", r.ProjectID)
		}
		if r.TaskID != nil {
			t.Errorf("a chat deposit is not task-scoped; task_id must be nil, got %v", r.TaskID)
		}
		if r.CostUSD != 0.0021 {
			t.Errorf("cost_usd = %v, want 0.0021 (from pricing)", r.CostUSD)
		}
	}
	if usage.rows[0].Role != RoleExtractor || usage.rows[1].Role != RoleResolver {
		t.Errorf("roles = %q,%q; want %q,%q", usage.rows[0].Role, usage.rows[1].Role, RoleExtractor, RoleResolver)
	}
}

// A zero-token stage (the resolver short-circuit) records nothing rather than
// an empty-row, matching the reranker/KG-worker convention.
func TestScreen_ZeroTokenStageNotBilled(t *testing.T) {
	usage := &recordingUsage{}
	g := &Gate{
		Extractor: &fakeExtractor{
			cands:   []graph.Candidate{person("Alice")},
			metrics: &graph.ExtractMetrics{Model: "m", PromptTokens: 10, CompletionTokens: 5},
		},
		Resolver: &fakeResolver{
			resns:   []graph.Resolution{{Decision: "match", MatchID: "ent-alice"}},
			metrics: &graph.ResolveMetrics{Model: "m"}, // short-circuit: 0 tokens
		},
		Spend: llmspend.New(usage, fixedPricing{usd: 0.0021},
			persistence.TaskLLMUsageSourceChatRememberNED, RoleExtractor),
	}
	g.Screen(context.Background(), "proj", "Alice")
	if len(usage.rows) != 1 {
		t.Fatalf("only the token-bearing extract stage should bill; got %d rows", len(usage.rows))
	}
}

// --- Classify (the §7 calibration axis) -------------------------------------

// no PERSON candidate → OutcomeNone, and the resolver is never consulted.
func TestClassify_NoPersonIsNone(t *testing.T) {
	res := &fakeResolver{}
	g := &Gate{
		Extractor: &fakeExtractor{cands: []graph.Candidate{{Type: persistence.EntityTypeProduct, Name: "Widget"}}},
		Resolver:  res,
	}
	got, err := g.Classify(context.Background(), "proj", "we shipped Widget")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != OutcomeNone {
		t.Errorf("outcome = %s, want none", got)
	}
	if res.calls != 0 {
		t.Errorf("resolver must not run when no PERSON was extracted; calls=%d", res.calls)
	}
}

// every person matches → OutcomeMatch.
func TestClassify_AllMatchIsMatch(t *testing.T) {
	g := &Gate{
		Extractor: &fakeExtractor{cands: []graph.Candidate{person("Alice"), person("Bob")}},
		Resolver: &fakeResolver{resns: []graph.Resolution{
			{Decision: "match", MatchID: "ent-alice"},
			{Decision: "match", MatchID: "ent-bob"},
		}},
	}
	if got, _ := g.Classify(context.Background(), "proj", "Alice and Bob"); got != OutcomeMatch {
		t.Errorf("outcome = %s, want match", got)
	}
}

// `new` dominates `ambiguous` in the precedence (it is the ship-blocking signal).
func TestClassify_NewDominatesAmbiguous(t *testing.T) {
	g := &Gate{
		Extractor: &fakeExtractor{cands: []graph.Candidate{person("Alice"), person("Carol"), person("Dan")}},
		Resolver: &fakeResolver{resns: []graph.Resolution{
			{Decision: "match", MatchID: "ent-alice"},
			{Decision: "ambiguous"},
			{Decision: "new"},
		}},
	}
	if got, _ := g.Classify(context.Background(), "proj", "Alice Carol Dan"); got != OutcomeNew {
		t.Errorf("outcome = %s, want new (new must dominate ambiguous)", got)
	}
}

// an ambiguous person with no `new` → OutcomeAmbiguous.
func TestClassify_AmbiguousIsAmbiguous(t *testing.T) {
	g := &Gate{
		Extractor: &fakeExtractor{cands: []graph.Candidate{person("Alice"), person("Eve")}},
		Resolver: &fakeResolver{resns: []graph.Resolution{
			{Decision: "match", MatchID: "ent-alice"},
			{Decision: "ambiguous"},
		}},
	}
	if got, _ := g.Classify(context.Background(), "proj", "Alice Eve"); got != OutcomeAmbiguous {
		t.Errorf("outcome = %s, want ambiguous", got)
	}
}

// a `match` with an empty id counts as unresolved → ambiguous (same safe
// direction Screen takes when it blocks such a case).
func TestClassify_MatchWithoutIDIsAmbiguous(t *testing.T) {
	g := &Gate{
		Extractor: &fakeExtractor{cands: []graph.Candidate{person("Alice")}},
		Resolver:  &fakeResolver{resns: []graph.Resolution{{Decision: "match", MatchID: ""}}},
	}
	if got, _ := g.Classify(context.Background(), "proj", "Alice"); got != OutcomeAmbiguous {
		t.Errorf("outcome = %s, want ambiguous", got)
	}
}

// an extract error surfaces to the caller (the harness tallies it separately),
// and the resolver never runs.
func TestClassify_ExtractErrorReturned(t *testing.T) {
	res := &fakeResolver{}
	g := &Gate{Extractor: &fakeExtractor{err: errors.New("model timeout")}, Resolver: res}
	if _, err := g.Classify(context.Background(), "proj", "anything"); err == nil {
		t.Fatal("expected an extract error to surface")
	}
	if res.calls != 0 {
		t.Errorf("resolver must not run after an extract error; calls=%d", res.calls)
	}
}

// a resolve error surfaces to the caller.
func TestClassify_ResolveErrorReturned(t *testing.T) {
	g := &Gate{
		Extractor: &fakeExtractor{cands: []graph.Candidate{person("Alice")}},
		Resolver:  &fakeResolver{err: errors.New("gateway 503")},
	}
	if _, err := g.Classify(context.Background(), "proj", "Alice"); err == nil {
		t.Fatal("expected a resolve error to surface")
	}
}

// an unconfigured gate errors rather than reporting a bogus outcome.
func TestClassify_UnconfiguredErrors(t *testing.T) {
	if _, err := (&Gate{}).Classify(context.Background(), "proj", "x"); err == nil {
		t.Fatal("an unconfigured gate must error")
	}
}

// Classify reuses the SAME billed path as Screen: extract + resolve spend lands
// under chat_remember_ned so the measurement's own cost is recorded (D6.4).
func TestClassify_RecordsBillingRows(t *testing.T) {
	usage := &recordingUsage{}
	g := &Gate{
		Extractor: &fakeExtractor{
			cands:   []graph.Candidate{person("Alice")},
			metrics: &graph.ExtractMetrics{Model: "gpt-oss-120b", PromptTokens: 300, CompletionTokens: 40},
		},
		Resolver: &fakeResolver{
			resns:   []graph.Resolution{{Decision: "new"}},
			metrics: &graph.ResolveMetrics{Model: "gpt-oss-20b", PromptTokens: 500, CompletionTokens: 20},
		},
		Spend: llmspend.New(usage, fixedPricing{usd: 0.0021},
			persistence.TaskLLMUsageSourceChatRememberNED, RoleExtractor),
	}
	if got, _ := g.Classify(context.Background(), "proj-c", "Alice"); got != OutcomeNew {
		t.Fatalf("outcome = %s, want new", got)
	}
	if len(usage.rows) != 2 {
		t.Fatalf("expected 2 billing rows (extract + resolve); got %d", len(usage.rows))
	}
	for _, r := range usage.rows {
		if r.Source != persistence.TaskLLMUsageSourceChatRememberNED {
			t.Errorf("row source = %q, want chat_remember_ned", r.Source)
		}
	}
}

// The type-level guardrail (review I3): the zero-value token is UNUSABLE, and
// the only source of a granted token is Gate.Screen returning proceed. No
// exported constructor and an unexported field mean no package outside `ned`
// can forge one — this test documents that property from a same-package
// vantage where construction WOULD be possible if a field were exported.
func TestSharedWriteAuthorization_ZeroValueUnusable(t *testing.T) {
	var zero SharedWriteAuthorization
	if zero.Granted() {
		t.Fatal("the zero-value token must not be granted — a forgotten/forged token must fail closed")
	}
	// Even a struct literal from OUTSIDE this package cannot set `granted`
	// (it is unexported); within the package the only minting site is
	// granted(), exercised through Screen. Confirm Screen is a granting path.
	g := &Gate{Extractor: &fakeExtractor{}, Resolver: &fakeResolver{}}
	if !g.Screen(context.Background(), "p", "no names here").Authorization().Granted() {
		t.Fatal("a proceed verdict must mint a granted token")
	}
}
