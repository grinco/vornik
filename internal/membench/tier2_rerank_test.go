package membench

import (
	"context"
	"strings"
	"testing"
)

// `--tier2-only` must control and then VERIFY the retrieval path it measures.
//
// The mode's help text promises "no answer generation, no judge, no model
// credentials"; §12's rationale for building it added "and therefore no reranker,
// and nothing billable", which is the entire affordability argument for a
// per-change CI gate. On 2026-08-12 three tier-2-only runs of a ten-question
// fixture billed 30 cloud reranker calls (`openai.gpt-oss-20b-1:0`, ten per run,
// proven from the deployment's usage ledger) and produced three different chunk
// rankings over a byte-identical corpus. verify-determinism refused every pairing.
//
// The mode suppressed answering and judging — it genuinely does — and had no
// effect whatsoever on the retrieval path, the one thing it measures. Whether the
// reranker fired was `memory.reranker.enabled` on the deployment, which the
// harness neither set nor read.
//
// Two properties are needed, and one without the other is worthless:
//
//   - CONTROL: tier-2-only stops requesting the reranked path.
//   - VERIFICATION: it refuses a run whose recall says a rerank happened anyway.
//
// Control alone is hope — it assumes the request was honoured. Verification alone
// leaves the gate unrunnable on any deployment with the reranker on. And the
// verification must fail CLOSED, because a deployment that cannot report its
// method is not a deployment that reported "no rerank".

// methodSystem is a MemorySystem that reports a retrieval method per recall,
// switchable mid-run so the mixture case can be tested.
type methodSystem struct {
	*fakeSystem
	method  string
	nth     int
	afterN  int
	andThen string
}

func (m *methodSystem) Recall(ctx context.Context, scope string, q Query) (Recalled, error) {
	out, err := m.fakeSystem.Recall(ctx, scope, q)
	if err != nil {
		return out, err
	}
	m.nth++
	out.RetrievalMethod = m.method
	if m.andThen != "" && m.nth > m.afterN {
		out.RetrievalMethod = m.andThen
	}
	return out, nil
}

func tier2Runner(t *testing.T, sys MemorySystem) *Runner {
	t.Helper()
	return &Runner{
		System:    sys,
		Dataset:   oneItemDataset{name: "test", items: simpleItems()},
		RunDir:    t.TempDir(),
		MaxTokens: 4096,
		Tier2Only: true,
	}
}

func TestTier2Only_RunsWhenNoRerankHappened(t *testing.T) {
	sys := &methodSystem{fakeSystem: newFakeSystem("fake"), method: "context-assembly"}
	res, err := tier2Runner(t, sys).Run(context.Background(), "")
	if err != nil {
		t.Fatalf("an unreranked tier-2-only run must be allowed: %v", err)
	}
	// And the observed method is what the run records, not what an operator typed.
	if got := res.Fields.ObservedRecallMethod; got != "context-assembly" {
		t.Errorf("ObservedRecallMethod = %q, want \"context-assembly\"", got)
	}
}

// TestTier2Only_RefusesWhenARerankFired is the 2026-08-12 regression.
func TestTier2Only_RefusesWhenARerankFired(t *testing.T) {
	sys := &methodSystem{fakeSystem: newFakeSystem("fake"), method: "context-assembly+rerank"}

	_, err := tier2Runner(t, sys).Run(context.Background(), "")
	if err == nil {
		t.Fatal("a tier-2-only run whose recall reported a rerank was allowed to proceed — " +
			"this is the run that billed 30 cloud reranker calls in a mode documented as " +
			"needing none, and whose rankings then differed on every repeat")
	}
	// The message has to name the lever, or an operator cannot act on it.
	if !strings.Contains(err.Error(), "reranker") {
		t.Errorf("error %q should name the reranker", err)
	}
	if !strings.Contains(err.Error(), "memory.reranker.enabled") {
		t.Errorf("error %q should name the config key that turns it off — the flag cannot "+
			"fix this itself, so the message is the only place the operator learns where to "+
			"look", err)
	}
}

// TestTier2Only_RefusesAMixedRun: the reranked path is not all-or-nothing. The
// 2026-08-14 baseline reranked 355 of 400 queries and lost 45 to the 8s deadline,
// so a run can be part reranked and part RRF. Checking only the first recall
// would pass that run and gate on an ordering half of it never used.
func TestTier2Only_RefusesAMixedRun(t *testing.T) {
	sys := &methodSystem{
		fakeSystem: newFakeSystem("fake"),
		method:     "context-assembly",
		afterN:     1,
		andThen:    "context-assembly+rerank",
	}

	if _, err := tier2Runner(t, sys).Run(context.Background(), ""); err == nil {
		t.Fatal("a run that started unreranked and then reranked was accepted; a first-recall " +
			"check is not enough because the reranker's deadline makes mixtures the norm")
	}
}

// TestTier2Only_FailsClosedWhenTheMethodIsUnknown is the central decision, and the
// same law as the write-target guard: "could not verify" must refuse.
//
// An empty method means the daemon is too old to report, or something upstream
// dropped the field. Treating that as "no rerank" would give the gate a check that
// passes exactly when it learned nothing — which is what `--i-know-this-wipes`
// did on 2026-08-12, right before writing twelve documents into production.
func TestTier2Only_FailsClosedWhenTheMethodIsUnknown(t *testing.T) {
	sys := &methodSystem{fakeSystem: newFakeSystem("fake"), method: ""}

	_, err := tier2Runner(t, sys).Run(context.Background(), "")
	if err == nil {
		t.Fatal("a tier-2-only run whose system never reported its retrieval method was " +
			"allowed; an unverifiable precondition must refuse, or the mode promises " +
			"determinism it never checked")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "did not report") &&
		!strings.Contains(strings.ToLower(err.Error()), "cannot report") {
		t.Errorf("error %q should say the system did not report its retrieval method, so an "+
			"operator can tell a stale daemon from a reranked one", err)
	}
}

// TestJudgedRun_AllowsARerankedPath: the refusal belongs to tier-2-only, not to
// the harness. The official baseline is deliberately measured on the reranked
// path (operator decision 2026-08-11: the gate protects what AGENTS retrieve),
// and a judged run at n=10 absorbs the variance the gate cannot.
func TestJudgedRun_AllowsARerankedPath(t *testing.T) {
	sys := &methodSystem{fakeSystem: newFakeSystem("fake"), method: "context-assembly+rerank"}
	r := tier2Runner(t, sys)
	r.Tier2Only = false
	llm := &stubLLM{replies: []string{"a", `{"correct":true}`, "b", `{"correct":true}`}}
	r.Generator = NewAnswerGenerator(llm)
	r.Judge = NewJudge(llm)

	if _, err := r.Run(context.Background(), ""); err != nil {
		t.Errorf("a judged run on the reranked path must be allowed — that is the official "+
			"baseline's path: %v", err)
	}
}

// TestTier2Only_ObservedMethodOverridesTheDeclaredOne closes the second-order
// hole. Those three 2026-08-12 runs were stamped `--recall-method
// context-assembly` while the reranker was firing, and nothing objected: the flag
// was a free-text label an operator typed. Its own help text says to verify it
// against the usage ledger — advice that cannot be enforced by more advice.
func TestTier2Only_ObservedMethodOverridesTheDeclaredOne(t *testing.T) {
	sys := &methodSystem{fakeSystem: newFakeSystem("fake"), method: "context-assembly"}
	r := tier2Runner(t, sys)
	r.RecallMethod = "context-assembly+rerank" // an operator's wrong label

	res, err := r.Run(context.Background(), "")
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if got := res.Fields.ObservedRecallMethod; got != "context-assembly" {
		t.Errorf("ObservedRecallMethod = %q, want \"context-assembly\"", got)
	}
	// And the key's own recall_params must carry the observed value, not the label:
	// a declared value the system contradicts must never reach the comparability
	// key, or two different experiments can share one.
	if strings.Contains(res.Fields.RecallParams, "+rerank") {
		t.Errorf("recall_params %q carries the operator's contradicted label; the "+
			"comparability key would then describe a path this run did not take",
			res.Fields.RecallParams)
	}
}

// The gate property and the tier-2 property are not the same property, and
// conflating them made the external arm unmeasurable.
//
// `--tier2-only` carries two things: "score retrieval, skip answering and judging"
// (which is about affordability and judge independence) and "prove the path was
// deterministic" (which is what a CI gate needs). A head-to-head against a
// shipping third-party product needs the first and cannot have the second — a
// competitor's internal reranker IS the product, and no external service reports
// our `retrieval_method`. Refusing those runs means the benchmark cannot measure
// the thing the comparison exists to measure.
//
// The fix is NOT to skip the check for systems that cannot answer: that is the
// fail-open hole this whole arc keeps closing. It is to make the gap explicit,
// opt-in, and CARRIED IN THE COMPARABILITY KEY, so an unverified run can never be
// silently compared against a verified one.

func TestTier2Only_AcceptUnverifiedPathAllowsAnUnreportingSystem(t *testing.T) {
	sys := &methodSystem{fakeSystem: newFakeSystem("external"), method: ""}
	r := tier2Runner(t, sys)
	r.AcceptUnverifiedPath = true

	res, err := r.Run(context.Background(), "")
	if err != nil {
		t.Fatalf("an explicitly-accepted unverified path was still refused: %v", err)
	}
	if !res.Fields.RetrievalPathUnverified {
		t.Error("the run does not record that its retrieval path was unverified; the " +
			"number would then look exactly like one from a verified run")
	}
}

// TestTier2Only_AcceptUnverifiedPathAllowsAReportedRerank: measuring a competitor
// that reranks internally is legitimate. Marking it is mandatory.
func TestTier2Only_AcceptUnverifiedPathAllowsAReportedRerank(t *testing.T) {
	sys := &methodSystem{fakeSystem: newFakeSystem("external"), method: "context-assembly+rerank"}
	r := tier2Runner(t, sys)
	r.AcceptUnverifiedPath = true

	res, err := r.Run(context.Background(), "")
	if err != nil {
		t.Fatalf("accepted-unverified run refused: %v", err)
	}
	if !res.Fields.RetrievalPathUnverified {
		t.Error("a reranked tier-2 run was not marked unverified")
	}
}

// TestUnverifiedPath_ChangesTheComparabilityKey is the load-bearing assertion. If
// the key were unchanged, a gate baseline taken on a verified deterministic path
// would compare clean against a run that never established one — which is the
// 2026-08-11 failure (two different experiments, one key) with new clothes.
func TestUnverifiedPath_ChangesTheComparabilityKey(t *testing.T) {
	verified := ComparabilityFields{
		HarnessVersion: "v1", DatasetSHA256: "abc", AnswerModel: "m", JudgeModel: "j",
		Tier2Only: true,
	}
	unverified := verified
	unverified.RetrievalPathUnverified = true

	if verified.Key() == unverified.Key() {
		t.Error("a run that PROVED its path deterministic and one that merely assumed " +
			"so share a comparability key")
	}
}

// TestTier2Only_StillRefusesByDefault: the escape hatch must be opt-in, or the
// gate silently loses the property it was built for.
func TestTier2Only_StillRefusesByDefault(t *testing.T) {
	sys := &methodSystem{fakeSystem: newFakeSystem("external"), method: ""}
	if _, err := tier2Runner(t, sys).Run(context.Background(), ""); err == nil {
		t.Fatal("an unreporting system was accepted WITHOUT the explicit flag")
	}
}

// TestAcceptUnverifiedPath_MeansMeasureAsShipped couples the two levers, because
// the alternative is an unfair comparison.
//
// --tier2-only suppresses OUR reranker to obtain determinism. Hindsight reranks
// internally with a local cross-encoder and offers no way to turn it off — that IS
// the shipping product. Measuring an unreranked vornik against a reranked
// competitor would hand the competitor an advantage created entirely by our own
// gate plumbing, and the resulting number would be worse than no number.
//
// So --accept-unverified-path, which already says "I am not requiring a proven
// deterministic path", also means "measure the system as it ships": the reranker
// suppression lifts. Gate mode is tier2-only WITHOUT the acceptance flag.
func TestAcceptUnverifiedPath_MeansMeasureAsShipped(t *testing.T) {
	cases := []struct {
		tier2, accept, wantNoRerank bool
		why                         string
	}{
		{tier2: true, accept: false, wantNoRerank: true,
			why: "gate mode: determinism required, so our reranker must be out of the path"},
		{tier2: true, accept: true, wantNoRerank: false,
			why: "comparison mode: measure as shipped, or the competitor's internal " +
				"reranker faces our unreranked path"},
		{tier2: false, accept: false, wantNoRerank: false,
			why: "a judged baseline is deliberately measured on the reranked path"},
	}
	for _, c := range cases {
		if got := GateSuppressesRerank(c.tier2, c.accept); got != c.wantNoRerank {
			t.Errorf("GateSuppressesRerank(tier2=%v, accept=%v) = %v, want %v — %s",
				c.tier2, c.accept, got, c.wantNoRerank, c.why)
		}
	}
}

// TestObservedRecallMethod_MixtureIsUnambiguous: the method names contain "+", so
// joining a mixture with "+" produced "context-assembly+context-assembly+rerank" —
// read once in a real run artifact and mistaken for a third method.
func TestObservedRecallMethod_MixtureIsUnambiguous(t *testing.T) {
	got := observedRecallMethod(map[string]struct{}{
		"context-assembly":        {},
		"context-assembly+rerank": {},
	})
	if got != "context-assembly|context-assembly+rerank" {
		t.Errorf("mixture rendered as %q; a reader must be able to see it is TWO methods", got)
	}
	// And a single method stays bare, so the common case is unchanged.
	if only := observedRecallMethod(map[string]struct{}{"context-assembly": {}}); only != "context-assembly" {
		t.Errorf("single method rendered as %q", only)
	}
}
