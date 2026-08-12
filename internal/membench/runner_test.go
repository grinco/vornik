package membench

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The runner (design §5.5, §5.8, §5.9). This is where the invariants the rest of
// the package only describes actually get enforced: leakage detection, the
// outcome taxonomy, resume, and terminal-vs-transient error handling.

// fakeSystem is a scriptable MemorySystem.
type fakeSystem struct {
	name string

	// stored records what was ingested per scope, so a leakage test can plant a
	// needle in one scope and query another.
	stored map[string][]Item

	// recallFn overrides retrieval. Default returns everything in the scope,
	// which is a perfect memory system — useful as the baseline against which a
	// degraded one is compared.
	recallFn func(scope string, q Query) (Recalled, error)

	ingestErr  error
	prepareErr error
	cfg        string

	// leaky makes Recall ignore the scope, simulating a broken isolation filter.
	leaky bool

	// failRealScopes makes Ingest/Recall fail ONLY for benchmark item scopes,
	// leaving the leakage probes healthy.
	//
	// The distinction is the point: a probe failure means isolation could not be
	// VERIFIED, which must abort the run; a per-item failure is a fault on one
	// item, which the outcome taxonomy records as OutcomeError. A fake that
	// failed both could not tell the two behaviours apart.
	failRealScopes error
}

// isProbe reports whether a scope belongs to the leakage assertion rather than a
// benchmark item.
func isProbe(scope string) bool { return strings.Contains(scope, "leak-probe") }

func newFakeSystem(name string) *fakeSystem {
	return &fakeSystem{name: name, stored: map[string][]Item{}}
}

func (f *fakeSystem) Name() string { return f.name }

func (f *fakeSystem) Prepare(_ context.Context, _ string) error { return f.prepareErr }

func (f *fakeSystem) Teardown(_ context.Context, _ string) error { return nil }

func (f *fakeSystem) Config(_ context.Context) (string, error) { return f.cfg, nil }

func (f *fakeSystem) Ingest(_ context.Context, scope string, items []Item) (IngestStats, error) {
	if f.ingestErr != nil {
		return IngestStats{}, f.ingestErr
	}
	if f.failRealScopes != nil && !isProbe(scope) {
		return IngestStats{}, f.failRealScopes
	}
	f.stored[scope] = append(f.stored[scope], items...)
	var bytes int
	for _, it := range items {
		bytes += len(it.Content)
	}
	return IngestStats{Deposits: len(items), Bytes: bytes, ChunksStored: -1}, nil
}

func (f *fakeSystem) Recall(_ context.Context, scope string, q Query) (Recalled, error) {
	if f.recallFn != nil && !isProbe(scope) {
		return f.recallFn(scope, q)
	}
	if f.failRealScopes != nil && !isProbe(scope) {
		return Recalled{}, f.failRealScopes
	}
	// The fake fuses nothing and reranks nothing, so it reports the unreranked
	// path truthfully. Reporting NOTHING would be a different claim — "cannot
	// say" — which a tier-2-only run refuses by design.
	out := Recalled{RetrievalMethod: "context-assembly"}
	pools := [][]Item{f.stored[scope]}
	if f.leaky {
		pools = pools[:0]
		for _, v := range f.stored {
			pools = append(pools, v)
		}
	}
	for _, pool := range pools {
		for _, it := range pool {
			out.Hits = append(out.Hits, Hit{SourceID: it.DocumentID, Text: it.Content, Score: 1})
		}
	}
	return out, nil
}

// oneItemDataset is a minimal in-memory Dataset.
type oneItemDataset struct {
	name  string
	items []BenchItem
}

func (d oneItemDataset) Name() string { return d.name }

func (d oneItemDataset) Load(_ string, lim Limits) ([]BenchItem, error) {
	return applyLimits(d.items, lim), nil
}

func simpleItems() []BenchItem {
	return []BenchItem{
		{
			ID:       "q1",
			Category: "multi-session",
			Haystack: []Item{
				{DocumentID: "q1_s1", Content: "Alice works at Google."},
				{DocumentID: "q1_s2", Content: "The weather is fine."},
			},
			QAs: []QA{{
				Question:        "Where does Alice work?",
				GoldAnswer:      "Google",
				GoldDocumentIDs: []string{"q1_s1"},
			}},
		},
		{
			ID:       "q2",
			Category: "multi-session",
			Haystack: []Item{
				{DocumentID: "q2_s1", Content: "Bob moved to Berlin."},
			},
			QAs: []QA{{
				Question:        "Where did Bob move?",
				GoldAnswer:      "Berlin",
				GoldDocumentIDs: []string{"q2_s1"},
			}},
		},
	}
}

func newTestRunner(t *testing.T, sys MemorySystem, llmReplies []string) (*Runner, string) {
	t.Helper()
	dir := t.TempDir()
	llm := &stubLLM{replies: llmReplies}
	r := &Runner{
		System:    sys,
		Dataset:   oneItemDataset{name: "test", items: simpleItems()},
		Generator: NewAnswerGenerator(llm),
		Judge:     NewJudge(llm),
		RunDir:    dir,
		MaxTokens: 4096,
	}
	return r, dir
}

// TestRunner_ScoresAndJournalsEveryItem — the happy path end to end.
func TestRunner_ScoresAndJournalsEveryItem(t *testing.T) {
	sys := newFakeSystem("fake")
	// Two items, each needing an answer then a verdict.
	r, dir := newTestRunner(t, sys, []string{
		"Alice works at Google.", `{"correct":true}`,
		"Bob moved to Berlin.", `{"correct":true}`,
	})

	res, err := r.Run(context.Background(), "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	c := res.Counts["multi-session"]
	if c.Correct != 2 {
		t.Errorf("counts = %+v, want 2 correct", c)
	}
	if !res.Trust.Trustworthy {
		t.Errorf("clean run stamped untrustworthy: %s", res.Trust.Reason)
	}

	// The journal must exist and mark both items complete, or --resume is a lie.
	replay, err := LoadJournal(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		t.Fatalf("LoadJournal: %v", err)
	}
	for _, id := range []string{"q1", "q2"} {
		if !replay.Completed(id) {
			t.Errorf("item %s not journalled as complete", id)
		}
	}
}

// TestRunner_ComputesTier2Metrics — the judge-free numbers the CI gate uses. The
// fake system returns everything in scope, so recall is perfect and precision is
// the fraction of the haystack that is gold.
func TestRunner_ComputesTier2Metrics(t *testing.T) {
	sys := newFakeSystem("fake")
	r, _ := newTestRunner(t, sys, []string{
		"a", `{"correct":true}`, "b", `{"correct":true}`,
	})

	res, err := r.Run(context.Background(), "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	m := res.Metrics["multi-session"]
	if m.ContextRecall < 0.99 {
		t.Errorf("context recall = %v, want ~1.0 (the fake returns the whole scope)",
			m.ContextRecall)
	}
	// q1 has 1 gold of 2 documents (0.5), q2 has 1 of 1 (1.0) → mean 0.75.
	if m.ContextPrecision < 0.7 || m.ContextPrecision > 0.8 {
		t.Errorf("context precision = %v, want ~0.75", m.ContextPrecision)
	}
	if m.MRR <= 0 {
		t.Errorf("MRR = %v, want > 0", m.MRR)
	}
}

// TestRunner_LeakageAssertionAbortsRun is the §5.5 guard. A system whose scope
// filter leaks must abort the run, not silently score cross-contaminated recall —
// which would produce a plausible number over a corrupted experiment.
func TestRunner_LeakageAssertionAbortsRun(t *testing.T) {
	sys := newFakeSystem("leaky")
	sys.leaky = true
	r, _ := newTestRunner(t, sys, []string{"a", `{"correct":true}`, "b", `{"correct":true}`})

	_, err := r.Run(context.Background(), "")
	if err == nil {
		t.Fatal("a leaking system completed a run; cross-contaminated recall would " +
			"have been scored as a result")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "leak") {
		t.Errorf("error %q does not identify the failure as scope leakage", err)
	}
}

// TestRunner_LeakageAssertionPassesOnIsolatedSystem — the guard must not
// false-positive, or it would block every legitimate run.
func TestRunner_LeakageAssertionPassesOnIsolatedSystem(t *testing.T) {
	sys := newFakeSystem("clean")
	r, _ := newTestRunner(t, sys, []string{"a", `{"correct":true}`, "b", `{"correct":true}`})

	if _, err := r.Run(context.Background(), ""); err != nil {
		t.Errorf("a properly isolated system was rejected: %v", err)
	}
}

// TestRunner_RecallErrorIsOutcomeError — an adapter fault must be recorded as
// error, never as a wrong answer. Blaming retrieval for an HTTP failure is the
// core lie the taxonomy exists to prevent.
func TestRunner_RecallErrorIsOutcomeError(t *testing.T) {
	sys := newFakeSystem("fake")
	sys.failRealScopes = errors.New("connection reset")
	r, _ := newTestRunner(t, sys, nil)

	res, err := r.Run(context.Background(), "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	c := res.Counts["multi-session"]
	if c.Error != 2 {
		t.Errorf("counts = %+v, want 2 errors", c)
	}
	if c.Incorrect != 0 {
		t.Error("a recall failure was scored as a wrong answer")
	}
}

// TestRunner_QuotaExhaustedAbortsWithoutRetry — terminal, per §5.3. Continuing
// would score later items zero for a billing reason and produce a number that
// reads as a retrieval result.
func TestRunner_QuotaExhaustedAbortsWithoutRetry(t *testing.T) {
	sys := newFakeSystem("fake")
	calls := 0
	sys.recallFn = func(string, Query) (Recalled, error) {
		calls++
		return Recalled{}, errors.Join(errors.New("http 429"), ErrQuotaExhausted)
	}
	_ = calls
	r, _ := newTestRunner(t, sys, nil)

	_, err := r.Run(context.Background(), "")
	if err == nil {
		t.Fatal("quota exhaustion did not abort the run")
	}
	if !errors.Is(err, ErrQuotaExhausted) {
		t.Errorf("error %v does not match ErrQuotaExhausted", err)
	}
	if calls != 1 {
		t.Errorf("made %d recall calls after quota exhaustion, want exactly 1 "+
			"(no retry, no continuation)", calls)
	}
}

// TestRunner_ResumeSkipsCompletedItems — a resumed run must not re-run finished
// work, and must still report over the whole population.
func TestRunner_ResumeSkipsCompletedItems(t *testing.T) {
	sys := newFakeSystem("fake")
	r, dir := newTestRunner(t, sys, []string{
		"a", `{"correct":true}`, "b", `{"correct":true}`,
	})
	if _, err := r.Run(context.Background(), ""); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// Second runner over the same directory with resume on. Its LLM has NO
	// replies: if it tries to judge anything the stub errors, which is exactly
	// how we detect work being redone.
	llm := &stubLLM{}
	r2 := &Runner{
		System:    sys,
		Dataset:   oneItemDataset{name: "test", items: simpleItems()},
		Generator: NewAnswerGenerator(llm),
		Judge:     NewJudge(llm),
		RunDir:    dir,
		MaxTokens: 4096,
		Resume:    true,
	}
	res, err := r2.Run(context.Background(), "")
	if err != nil {
		t.Fatalf("resumed run: %v", err)
	}
	if len(llm.calls) != 0 {
		t.Errorf("resumed run made %d LLM calls; completed items must be skipped",
			len(llm.calls))
	}
	// And the recovered verdicts must still be reported.
	if got := res.Counts["multi-session"].Correct; got != 2 {
		t.Errorf("resumed run reports %d correct, want 2 recovered from the journal", got)
	}
}

// TestRunner_IngestErrorIsOutcomeError — a failed ingest means the item was never
// really tested; it must not be scored as a retrieval miss.
func TestRunner_IngestErrorIsOutcomeError(t *testing.T) {
	sys := newFakeSystem("fake")
	sys.failRealScopes = errors.New("deposit refused")
	r, _ := newTestRunner(t, sys, nil)

	res, err := r.Run(context.Background(), "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Counts["multi-session"].Error != 2 {
		t.Errorf("counts = %+v, want 2 errors", res.Counts["multi-session"])
	}
}

// TestRunner_UntrustworthyWhenDegraded — the §5.9 stamp must actually fire from
// the runner, not just exist as a function.
func TestRunner_UntrustworthyWhenDegraded(t *testing.T) {
	sys := newFakeSystem("fake")
	sys.failRealScopes = errors.New("boom")
	r, _ := newTestRunner(t, sys, nil)

	res, err := r.Run(context.Background(), "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Trust.Trustworthy {
		t.Error("an all-errors run was stamped trustworthy")
	}
}

// TestRunner_WritesManifest — the manifest carries the comparability key and the
// trust verdict. Without it on disk a result cannot be checked against another
// run later.
func TestRunner_WritesManifest(t *testing.T) {
	sys := newFakeSystem("fake")
	sys.cfg = "test-model"
	r, dir := newTestRunner(t, sys, []string{"a", `{"correct":true}`, "b", `{"correct":true}`})

	if _, err := r.Run(context.Background(), ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	data, err := readFileString(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("manifest not written: %v", err)
	}
	for _, want := range []string{"comparability_key", "trust", "dataset_name"} {
		if !strings.Contains(data, want) {
			t.Errorf("manifest omits %q", want)
		}
	}
}

// TestRunner_MissingDependenciesRefused — a runner with no system or no dataset
// must fail at the start rather than producing an empty, plausible-looking result.
func TestRunner_MissingDependenciesRefused(t *testing.T) {
	cases := map[string]*Runner{
		"no system":  {Dataset: oneItemDataset{}, RunDir: t.TempDir()},
		"no dataset": {System: newFakeSystem("f"), RunDir: t.TempDir()},
		"no run dir": {System: newFakeSystem("f"), Dataset: oneItemDataset{}},
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := r.Run(context.Background(), ""); err == nil {
				t.Error("Run succeeded with a missing dependency; an empty result " +
					"would look like a dataset with no items")
			}
		})
	}
}

// TestRunner_ProbeFailureAbortsRatherThanScoring — a system that cannot serve the
// leakage probe has not proven its isolation, and a run whose isolation is
// unverified must not produce numbers. This is deliberately DIFFERENT from a
// per-item fault, which the taxonomy records as OutcomeError: here we do not know
// whether scores would even be meaningful.
func TestRunner_ProbeFailureAbortsRatherThanScoring(t *testing.T) {
	sys := newFakeSystem("fake")
	sys.ingestErr = errors.New("store unreachable")
	r, _ := newTestRunner(t, sys, nil)

	if _, err := r.Run(context.Background(), ""); err == nil {
		t.Error("a run whose leakage probe could not complete produced a result; " +
			"isolation was never verified so no number from it is meaningful")
	}
}

// TestRunner_SilentSystemFailsProbe — an adapter that returns nothing at all
// would pass a naive isolation check trivially (no hits anywhere means no leak)
// while scoring zero on everything. The probe must catch that, or a broken
// adapter is indistinguishable from a perfectly isolated one.
func TestRunner_SilentSystemFailsProbe(t *testing.T) {
	sys := newFakeSystem("silent")
	sys.recallFn = func(string, Query) (Recalled, error) { return Recalled{}, nil }
	// Override for probes too, which is what makes it "silent" everywhere.
	silent := &silentSystem{fakeSystem: sys}
	r, _ := newTestRunner(t, silent, nil)

	_, err := r.Run(context.Background(), "")
	if err == nil {
		t.Fatal("a system that retrieves nothing passed the isolation check")
	}
	if !strings.Contains(err.Error(), "own") {
		t.Errorf("error %q does not explain that the canary was unfindable in its "+
			"own scope", err)
	}
}

// silentSystem returns no hits for every scope, probes included.
type silentSystem struct{ *fakeSystem }

func (s *silentSystem) Recall(context.Context, string, Query) (Recalled, error) {
	return Recalled{}, nil
}

// sharedHaystackDataset declares that all its items share one haystack.
type sharedHaystackDataset struct{ oneItemDataset }

func (sharedHaystackDataset) SharedHaystack() bool { return true }

// TestRunner_SharedHaystackIngestedOnce — for the native dataset every question
// is asked against the SAME corpus. Ingesting it per item would re-upload the
// whole corpus once per question: with 30 questions over 277 documents that is
// 8,310 deposits instead of 277, which makes the gate we most want to run often
// impractically slow and expensive.
//
// Found by review of the gold set against the runner, not by the design.
func TestRunner_SharedHaystackIngestedOnce(t *testing.T) {
	sys := newFakeSystem("fake")
	ingests := map[string]int{}
	base := sys.Ingest
	wrapped := &countingSystem{fakeSystem: sys, counts: ingests, inner: base}

	r := &Runner{
		System:    wrapped,
		Dataset:   sharedHaystackDataset{oneItemDataset{name: "native", items: simpleItems()}},
		Generator: NewAnswerGenerator(&stubLLM{replies: []string{"a", `{"correct":true}`, "b", `{"correct":true}`}}),
		Judge:     NewJudge(&stubLLM{replies: []string{`{"correct":true}`, `{"correct":true}`}}),
		RunDir:    t.TempDir(),
		MaxTokens: 4096,
	}
	// One LLM for both roles keeps the reply script simple; the judge stub above
	// is separate so answer/verdict alternation cannot desynchronise.
	llm := &stubLLM{replies: []string{
		"a", `{"correct":true}`,
		"b", `{"correct":true}`,
	}}
	r.Generator = NewAnswerGenerator(llm)
	r.Judge = NewJudge(llm)

	if _, err := r.Run(context.Background(), ""); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Two items share one haystack, so exactly ONE non-probe ingest call.
	realIngests := 0
	for scope, n := range ingests {
		if !isProbe(scope) {
			realIngests += n
		}
	}
	if realIngests != 1 {
		t.Errorf("shared-haystack dataset ingested %d times, want exactly 1 "+
			"(scopes: %v)", realIngests, ingests)
	}
}

// TestRunner_PerItemHaystackStillIngestedPerItem — the public datasets have a
// genuine per-item haystack and must keep per-item isolation. The shared-haystack
// optimisation must not leak into them, or one item's distractors would become
// another's.
func TestRunner_PerItemHaystackStillIngestedPerItem(t *testing.T) {
	sys := newFakeSystem("fake")
	ingests := map[string]int{}
	wrapped := &countingSystem{fakeSystem: sys, counts: ingests, inner: sys.Ingest}

	llm := &stubLLM{replies: []string{"a", `{"correct":true}`, "b", `{"correct":true}`}}
	r := &Runner{
		System:    wrapped,
		Dataset:   oneItemDataset{name: "longmemeval", items: simpleItems()},
		Generator: NewAnswerGenerator(llm),
		Judge:     NewJudge(llm),
		RunDir:    t.TempDir(),
		MaxTokens: 4096,
	}
	if _, err := r.Run(context.Background(), ""); err != nil {
		t.Fatalf("Run: %v", err)
	}

	realIngests := 0
	for scope, n := range ingests {
		if !isProbe(scope) {
			realIngests += n
		}
	}
	if realIngests != 2 {
		t.Errorf("per-item dataset ingested %d times, want 2 (one per item); "+
			"scopes: %v", realIngests, ingests)
	}
}

// countingSystem records ingest calls per scope.
type countingSystem struct {
	*fakeSystem
	counts map[string]int
	inner  func(context.Context, string, []Item) (IngestStats, error)
}

func (c *countingSystem) Ingest(ctx context.Context, scope string, items []Item) (IngestStats, error) {
	c.counts[scope]++
	return c.inner(ctx, scope, items)
}

// notingSystem reports an adaptation it had to make.
type notingSystem struct {
	*fakeSystem
	notes []string
}

func (n *notingSystem) Notes() []string { return n.notes }

// TestRunner_ManifestRecordsMethodNotes — an adapter that had to adapt the
// harness's request (a token budget converted to a result count, say) reports it,
// and the manifest must WRITE it. A conversion that happens but is never written
// down is indistinguishable from one that did not happen, which is exactly the
// asymmetry §5.6 requires be reported rather than hidden.
//
// The external adapter records such notes; before this wiring nothing consumed
// them, so they were collected and discarded.
func TestRunner_ManifestRecordsMethodNotes(t *testing.T) {
	sys := &notingSystem{
		fakeSystem: newFakeSystem("noting"),
		notes:      []string{"token budget 4096 converted to top_k=8"},
	}
	llm := &stubLLM{replies: []string{"a", `{"correct":true}`, "b", `{"correct":true}`}}
	dir := t.TempDir()
	r := &Runner{
		System:    sys,
		Dataset:   oneItemDataset{name: "test", items: simpleItems()},
		Generator: NewAnswerGenerator(llm),
		Judge:     NewJudge(llm),
		RunDir:    dir,
		MaxTokens: 4096,
	}
	if _, err := r.Run(context.Background(), ""); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got, err := readFileString(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if !strings.Contains(got, "top_k=8") {
		t.Error("the adapter's method note was not written to the manifest; a " +
			"recorded-but-unwritten conversion is indistinguishable from one that " +
			"never happened")
	}
}

// TestRunner_ManifestOmitsNotesForPlainAdapter — an adapter that honoured the
// request as given must not have an empty notes field implying it adapted
// something.
func TestRunner_ManifestOmitsNotesForPlainAdapter(t *testing.T) {
	llm := &stubLLM{replies: []string{"a", `{"correct":true}`, "b", `{"correct":true}`}}
	dir := t.TempDir()
	r := &Runner{
		System:    newFakeSystem("plain"),
		Dataset:   oneItemDataset{name: "test", items: simpleItems()},
		Generator: NewAnswerGenerator(llm),
		Judge:     NewJudge(llm),
		RunDir:    dir,
		MaxTokens: 4096,
	}
	if _, err := r.Run(context.Background(), ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := readFileString(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "method_notes") {
		t.Error("a plain adapter produced a method_notes field")
	}
}

// TestRunner_ConfigErrorIsNotFatal — the external adapter returns ("", err) when
// its optional config endpoint is unreachable. That must mark the comparability
// key PARTIAL and continue, not abort: a run should not be lost because an
// optional metadata route was down.
func TestRunner_ConfigErrorIsNotFatal(t *testing.T) {
	sys := &configFailingSystem{fakeSystem: newFakeSystem("cfgfail")}
	llm := &stubLLM{replies: []string{"a", `{"correct":true}`, "b", `{"correct":true}`}}
	r := &Runner{
		System:       sys,
		Dataset:      oneItemDataset{name: "test", items: simpleItems()},
		Generator:    NewAnswerGenerator(llm),
		Judge:        NewJudge(llm),
		RunDir:       t.TempDir(),
		MaxTokens:    4096,
		SingleSystem: false,
	}
	res, err := r.Run(context.Background(), "")
	if err != nil {
		t.Fatalf("an unreadable optional config aborted the run: %v", err)
	}
	if !res.Fields.Partial() {
		t.Error("an unverifiable config did not mark the comparability key partial; " +
			"'could not verify' would silently pass as 'unchanged'")
	}
}

type configFailingSystem struct{ *fakeSystem }

func (c *configFailingSystem) Config(context.Context) (string, error) {
	return "", errors.New("config endpoint unreachable")
}

// readySystem reports embedding readiness.
type readySystem struct {
	*fakeSystem
	fraction float64
	err      error
}

func (r *readySystem) EmbeddingReadiness(context.Context) (float64, error) {
	return r.fraction, r.err
}

// TestRunner_RecordsEmbeddingReadiness — found by the first live smoke run: the
// harness ingests a corpus and immediately recalls it, but embedding is ASYNC.
// On a cold corpus only 126 of 3,187 chunks were embedded when scoring began, so
// the tier-2 numbers measured keyword-only retrieval — with nothing in the output
// saying so. A reader would have taken them for semantic-retrieval scores.
//
// Readiness must therefore be recorded, and a low value must be visible.
//
// SUPERSEDED IN PART on 2026-08-12: recording a low value and scoring anyway was
// not enough. It let a head-to-head report vornik losing recall 0.917 to 1.000 when
// the identical items over the same, settled corpus scored 1.000 — the run was
// racing its own embed queue. The runner now WAITS for readiness and refuses when
// it never settles (see settle_test.go).
//
// This test keeps the reporting half of the contract, with settling explicitly
// disabled: an operator who chooses to score a cold corpus must still SEE how cold
// it was.
func TestRunner_RecordsEmbeddingReadiness(t *testing.T) {
	sys := &readySystem{fakeSystem: newFakeSystem("ready"), fraction: 0.04}
	llm := &stubLLM{replies: []string{"a", `{"correct":true}`, "b", `{"correct":true}`}}
	dir := t.TempDir()
	r := &Runner{
		System:    sys,
		Dataset:   oneItemDataset{name: "test", items: simpleItems()},
		Generator: NewAnswerGenerator(llm),
		Judge:     NewJudge(llm),
		RunDir:    dir,
		MaxTokens: 4096,
	}
	r.SettleDisabled() // scoring a cold corpus is now an explicit choice
	res, err := r.Run(context.Background(), "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.EmbeddingReadiness == nil {
		t.Fatal("readiness not recorded; a keyword-only run would be indistinguishable " +
			"from a semantic one")
	}
	if *res.EmbeddingReadiness != 0.04 {
		t.Errorf("readiness = %v, want 0.04", *res.EmbeddingReadiness)
	}
	got, err := readFileString(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "embedding_readiness") {
		t.Error("readiness not written to the manifest")
	}
}

// TestRunner_LowReadinessDoesNotFailTheRun — keyword-only retrieval is a
// LEGITIMATE configuration (a deployment with no embedder runs that way), so low
// readiness must be reported, not treated as a broken run. Conflating "measured
// something different" with "measured nothing" would block valid baselines.
func TestRunner_LowReadinessDoesNotFailTheRun(t *testing.T) {
	sys := &readySystem{fakeSystem: newFakeSystem("cold"), fraction: 0.0}
	llm := &stubLLM{replies: []string{"a", `{"correct":true}`, "b", `{"correct":true}`}}
	r := &Runner{
		System: sys, Dataset: oneItemDataset{name: "t", items: simpleItems()},
		Generator: NewAnswerGenerator(llm), Judge: NewJudge(llm),
		RunDir: t.TempDir(), MaxTokens: 4096,
	}
	res, err := r.Run(context.Background(), "")
	if err != nil {
		t.Fatalf("zero readiness aborted the run: %v", err)
	}
	if !res.Trust.Trustworthy {
		t.Error("zero readiness stamped the run untrustworthy; keyword-only is a " +
			"valid configuration, just a different measurement")
	}
}

// TestRunner_ReadinessErrorIsNotFatal — an adapter that cannot report readiness
// (no stats endpoint) must not lose the run.
func TestRunner_ReadinessErrorIsNotFatal(t *testing.T) {
	sys := &readySystem{fakeSystem: newFakeSystem("noreport"), err: errors.New("no stats endpoint")}
	llm := &stubLLM{replies: []string{"a", `{"correct":true}`, "b", `{"correct":true}`}}
	r := &Runner{
		System: sys, Dataset: oneItemDataset{name: "t", items: simpleItems()},
		Generator: NewAnswerGenerator(llm), Judge: NewJudge(llm),
		RunDir: t.TempDir(), MaxTokens: 4096,
	}
	res, err := r.Run(context.Background(), "")
	if err != nil {
		t.Fatalf("an unreportable readiness aborted the run: %v", err)
	}
	if res.EmbeddingReadiness != nil {
		t.Error("readiness reported despite the adapter failing to determine it; " +
			"unknown must stay nil rather than becoming a number")
	}
}

// TestRunner_NoReadinessCapabilityIsNil — an adapter without the capability leaves
// it unknown rather than implying 100%.
func TestRunner_NoReadinessCapabilityIsNil(t *testing.T) {
	llm := &stubLLM{replies: []string{"a", `{"correct":true}`, "b", `{"correct":true}`}}
	r := &Runner{
		System: newFakeSystem("plain"), Dataset: oneItemDataset{name: "t", items: simpleItems()},
		Generator: NewAnswerGenerator(llm), Judge: NewJudge(llm),
		RunDir: t.TempDir(), MaxTokens: 4096,
	}
	res, err := r.Run(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if res.EmbeddingReadiness != nil {
		t.Errorf("readiness = %v for an adapter with no capability, want nil",
			*res.EmbeddingReadiness)
	}
}

// TestRunner_RecordsPerQueryRetrieval — without this, two configurations that
// score identically are indistinguishable from two that retrieved identically.
//
// The Cohere/Titan A/B produced byte-identical tier-2 metrics across all five
// categories, which is not credible for two different embedding models. The
// leading hypothesis is that the metrics collapse chunks to DOCUMENTS, so two
// embedders that retrieve the same document set in different chunk order score
// the same. That hypothesis is untestable from the artifacts as they stand,
// because nothing records WHAT was retrieved — only how it scored.
func TestRunner_RecordsPerQueryRetrieval(t *testing.T) {
	sys := newFakeSystem("fake")
	llm := &stubLLM{replies: []string{"a", `{"correct":true}`, "b", `{"correct":true}`}}
	dir := t.TempDir()
	r := &Runner{
		System:    sys,
		Dataset:   oneItemDataset{name: "test", items: simpleItems()},
		Generator: NewAnswerGenerator(llm),
		Judge:     NewJudge(llm),
		RunDir:    dir,
		MaxTokens: 4096,
	}
	res, err := r.Run(context.Background(), "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(res.Retrievals) != 2 {
		t.Fatalf("recorded %d retrievals, want one per question", len(res.Retrievals))
	}
	byItem := map[string]RetrievalDetail{}
	for _, d := range res.Retrievals {
		byItem[d.ItemID] = d
	}
	q1, ok := byItem["q1"]
	if !ok {
		t.Fatal("q1 has no retrieval record")
	}
	if q1.Question == "" {
		t.Error("question text not recorded; a reader cannot tell which query this was")
	}
	if len(q1.GoldDocuments) != 1 || q1.GoldDocuments[0] != "q1_s1" {
		t.Errorf("gold documents = %v, want [q1_s1]", q1.GoldDocuments)
	}
	// The fake returns the whole scope, so both documents come back — and the
	// ORDER must survive, because that is what distinguishes two rankings over
	// the same document set.
	if len(q1.RetrievedChunks) != 2 {
		t.Errorf("retrieved chunks = %v, want both scope documents", q1.RetrievedChunks)
	}
	if len(q1.RetrievedDocuments) == 0 {
		t.Error("distinct retrieved documents not recorded")
	}
}

// TestRunner_RetrievalPreservesChunkOrderAndDuplicates — the whole point of the
// instrumentation. RetrievedDocuments collapses to a distinct set (what the
// metrics see), while RetrievedChunks keeps rank order INCLUDING repeats from one
// document (what the metrics are blind to). Recording only the collapsed form
// would leave the hypothesis untestable.
func TestRunner_RetrievalPreservesChunkOrderAndDuplicates(t *testing.T) {
	sys := newFakeSystem("dupes")
	sys.recallFn = func(string, Query) (Recalled, error) {
		return Recalled{Hits: []Hit{
			{SourceID: "docA", Score: 0.9},
			{SourceID: "docA", Score: 0.8},
			{SourceID: "docB", Score: 0.7},
			{SourceID: "docA", Score: 0.6},
		}}, nil
	}
	llm := &stubLLM{replies: []string{"a", `{"correct":true}`, "b", `{"correct":true}`}}
	r := &Runner{
		System: sys, Dataset: oneItemDataset{name: "t", items: simpleItems()},
		Generator: NewAnswerGenerator(llm), Judge: NewJudge(llm),
		RunDir: t.TempDir(), MaxTokens: 4096,
	}
	res, err := r.Run(context.Background(), "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	d := res.Retrievals[0]

	wantChunks := []string{"docA", "docA", "docB", "docA"}
	if len(d.RetrievedChunks) != len(wantChunks) {
		t.Fatalf("chunks = %v, want %v", d.RetrievedChunks, wantChunks)
	}
	for i := range wantChunks {
		if d.RetrievedChunks[i] != wantChunks[i] {
			t.Fatalf("chunk order lost: %v, want %v", d.RetrievedChunks, wantChunks)
		}
	}
	// Distinct documents in FIRST-APPEARANCE order, which is the document-level
	// ranking the metrics actually score.
	wantDocs := []string{"docA", "docB"}
	if len(d.RetrievedDocuments) != 2 ||
		d.RetrievedDocuments[0] != wantDocs[0] || d.RetrievedDocuments[1] != wantDocs[1] {
		t.Errorf("documents = %v, want %v in first-appearance order", d.RetrievedDocuments, wantDocs)
	}
}

// TestRunner_RetrievalRecordedForErrorsToo — an item whose recall failed must still
// appear, with an empty retrieval, or a reader comparing two runs cannot tell "no
// documents found" from "this item never ran".
func TestRunner_RetrievalRecordedForErrorsToo(t *testing.T) {
	sys := newFakeSystem("failing")
	sys.failRealScopes = errors.New("recall exploded")
	llm := &stubLLM{}
	r := &Runner{
		System: sys, Dataset: oneItemDataset{name: "t", items: simpleItems()},
		Generator: NewAnswerGenerator(llm), Judge: NewJudge(llm),
		RunDir: t.TempDir(), MaxTokens: 4096,
	}
	res, err := r.Run(context.Background(), "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Retrievals) == 0 {
		t.Fatal("a failed item recorded no retrieval at all")
	}
	for _, d := range res.Retrievals {
		if d.Error == "" {
			t.Errorf("item %s failed but its retrieval record carries no error", d.ItemID)
		}
	}
}

// TestRunner_RetrievalWrittenToResultsFile — it has to reach disk to be useful for
// a cross-run diff.
func TestRunner_RetrievalWrittenToResultsFile(t *testing.T) {
	llm := &stubLLM{replies: []string{"a", `{"correct":true}`, "b", `{"correct":true}`}}
	dir := t.TempDir()
	r := &Runner{
		System: newFakeSystem("f"), Dataset: oneItemDataset{name: "t", items: simpleItems()},
		Generator: NewAnswerGenerator(llm), Judge: NewJudge(llm),
		RunDir: dir, MaxTokens: 4096,
	}
	if _, err := r.Run(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	raw, err := readFileString(filepath.Join(dir, "results.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"retrievals", "retrieved_chunks", "retrieved_documents", "gold_documents"} {
		if !strings.Contains(raw, want) {
			t.Errorf("results.json omits %q", want)
		}
	}
}

// TestComparabilityFields_RecordsRecallMethod — a run whose retrieval path
// reranks is NOT the same experiment as one that does not, so the comparability
// key must separate them.
//
// Regression test for the 2026-08-11 finding: recall_params carried only
// max_tokens, so a run against the pre-fix build (where context-assembly recall
// silently dropped the rerank request and the reranker never fired) produced a
// key BYTE-IDENTICAL to a post-fix run that did rerank. Two different systems
// compared clean, which is the exact failure the key exists to prevent.
func TestComparabilityFields_RecordsRecallMethod(t *testing.T) {
	sys := newFakeSystem("fake")

	reranked, _ := newTestRunner(t, sys, nil)
	reranked.RecallMethod = "context-assembly+rerank"
	plain, _ := newTestRunner(t, sys, nil)
	plain.RecallMethod = "interactive"

	rf := reranked.comparabilityFields(context.Background())
	pf := plain.comparabilityFields(context.Background())

	if !strings.Contains(rf.RecallParams, "context-assembly+rerank") {
		t.Errorf("recall_params must name the retrieval method, got %q", rf.RecallParams)
	}
	if rf.RecallParams == pf.RecallParams {
		t.Fatalf("reranked and plain runs share recall_params %q", rf.RecallParams)
	}
	if rf.Key() == pf.Key() {
		t.Fatal("reranked and plain runs produced the same comparability key")
	}
}

// TestComparabilityFields_UnsetRecallMethodIsNotSilent — an unset method must be
// visibly unknown, not quietly comparable to a known one. Otherwise a caller
// that forgets to set it inherits whatever key the last one had.
func TestComparabilityFields_UnsetRecallMethodIsNotSilent(t *testing.T) {
	sys := newFakeSystem("fake")
	r, _ := newTestRunner(t, sys, nil)
	f := r.comparabilityFields(context.Background())
	if !strings.Contains(f.RecallParams, "unknown") {
		t.Errorf("unset RecallMethod must read as unknown, got %q", f.RecallParams)
	}
}

// TestRunner_ErrorOutcomeNamesTheCause — an errored item must journal WHY.
//
// Regression test for the 2026-08-11 debugging cost: the answer and judge
// transport-failure paths discarded their error and journalled the bare string
// "qa failed". A misconfigured LLM endpoint (a 404 from a base URL missing its
// /v1 suffix) therefore produced five errored items whose recorded detail said
// nothing at all, and the cause had to be found by probing the endpoint by hand.
// The run is already correctly marked untrustworthy; the gap was diagnostic.
func TestRunner_ErrorOutcomeNamesTheCause(t *testing.T) {
	sys := newFakeSystem("fake")
	// No replies: the stub fails the ANSWER call with "stub exhausted".
	r, dir := newTestRunner(t, sys, nil)

	if _, err := r.Run(context.Background(), ""); err != nil {
		t.Fatalf("run: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	journal := string(body)
	if !strings.Contains(journal, "stub exhausted") {
		t.Errorf("journal must name the underlying cause, got:\n%s", journal)
	}
	if strings.Contains(journal, `"detail":"qa failed"`) {
		t.Error(`journal still carries the bare "qa failed" with no cause`)
	}
}

// TestRunner_RecallErrorNamesTheCauseInTheJournal — same diagnostic rule as the
// answer/judge paths: a recall failure records its cause in results.json's
// retrieval detail, and the journal must not be the one place that says only
// "qa failed".
func TestRunner_RecallErrorNamesTheCauseInTheJournal(t *testing.T) {
	sys := newFakeSystem("fake")
	sys.recallFn = func(string, Query) (Recalled, error) {
		return Recalled{}, errors.New("connection reset by peer")
	}
	r, dir := newTestRunner(t, sys, nil)

	if _, err := r.Run(context.Background(), ""); err != nil {
		t.Fatalf("run: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	if !strings.Contains(string(body), "connection reset by peer") {
		t.Errorf("journal must name the recall cause, got:\n%s", body)
	}
}

// TestComparabilityFields_RecordsTheCorpus — two runs over DIFFERENT corpora must
// not share a comparability key. Regression for the 2026-08-11 hole: the key
// covered the gold set's sha256 but never the external haystack the native dataset
// reads from disk, so editing the corpus left runs comparing clean.
func TestComparabilityFields_RecordsTheCorpus(t *testing.T) {
	sys := newFakeSystem("fake")
	r, _ := newTestRunner(t, sys, nil)
	r.RecallMethod = "interactive"

	before := r.comparabilityFields(context.Background())
	r.CorpusItems = []Item{{DocumentID: "d1", Content: "original"}}
	original := r.comparabilityFields(context.Background())
	r.CorpusItems = []Item{{DocumentID: "d1", Content: "edited"}}
	edited := r.comparabilityFields(context.Background())

	if original.CorpusSHA256 == "" {
		t.Fatal("corpus digest not recorded")
	}
	if original.CorpusSHA256 == edited.CorpusSHA256 {
		t.Error("an edited corpus produced the same digest")
	}
	if original.Key() == edited.Key() {
		t.Error("runs over different corpora share a comparability key")
	}
	// A dataset carrying its own haystack has no external corpus, and that must
	// stay distinguishable from one that does.
	if before.CorpusSHA256 != "" {
		t.Errorf("no corpus should mean an empty digest, got %q", before.CorpusSHA256)
	}
}

// embedderReportingSystem is a MemorySystem that reports its OBSERVED embedder.
type embedderReportingSystem struct {
	*fakeSystem
	observed string
}

func (e embedderReportingSystem) ObservedEmbedder(context.Context) (string, error) {
	return e.observed, nil
}

// TestComparabilityFields_RecordsObservedEmbedder — the provider label must come
// from the system, not from what the operator typed.
//
// Regression for the 2026-08-11 finding: titan and cohere runs produced
// byte-identical tier-2 metrics, and because the embedder was only ever an
// operator-supplied string, nothing could establish which vectors were queried.
func TestComparabilityFields_RecordsObservedEmbedder(t *testing.T) {
	base := newFakeSystem("fake")
	r, _ := newTestRunner(t, embedderReportingSystem{fakeSystem: base, observed: "bedrock/cohere.embed-v4:0"}, nil)
	r.RecallMethod = "interactive"
	a := r.comparabilityFields(context.Background())

	r2, _ := newTestRunner(t, embedderReportingSystem{fakeSystem: newFakeSystem("fake"), observed: "bedrock/titan-embed-text-v2:0"}, nil)
	r2.RecallMethod = "interactive"
	b := r2.comparabilityFields(context.Background())

	if a.ObservedEmbedder == "" {
		t.Fatal("observed embedder not recorded")
	}
	if a.ObservedEmbedder == b.ObservedEmbedder {
		t.Fatal("two providers reported the same observed embedder")
	}
	if a.Key() == b.Key() {
		t.Error("runs on different embedders share a comparability key")
	}
}

// TestComparabilityFields_UnreportedEmbedderMarksKeyPartial — a system that cannot
// say which embedder it uses leaves the key unverified, and Partial() is how that
// is admitted rather than assumed unchanged.
func TestComparabilityFields_UnreportedEmbedderMarksKeyPartial(t *testing.T) {
	r, _ := newTestRunner(t, newFakeSystem("fake"), nil)
	r.RecallMethod = "interactive"
	f := r.comparabilityFields(context.Background())
	if f.ObservedEmbedder != "" {
		t.Errorf("ObservedEmbedder = %q, want empty when unreportable", f.ObservedEmbedder)
	}
	if !f.Partial() {
		t.Error("an unverified embedder must mark the key partial")
	}
}

// TestRunner_RefusesWhenDeclaredEmbedderContradictsObserved — the exact mislabel
// that motivated this: an operator declares cohere while the daemon runs titan.
// Recording both would leave the contradiction for a reader to notice; refusing
// stops a mislabelled number from existing.
func TestRunner_RefusesWhenDeclaredEmbedderContradictsObserved(t *testing.T) {
	sys := embedderReportingSystem{fakeSystem: newFakeSystem("fake"), observed: "bedrock/titan-embed-text-v2:0"}
	r, _ := newTestRunner(t, sys, nil)
	r.DeclaredEmbedder = "bedrock:cohere.embed-v4(embed)+gpt-oss-120b(titler)"

	_, err := r.Run(context.Background(), "")
	if err == nil {
		t.Fatal("run proceeded with a declared embedder the system contradicts")
	}
	for _, want := range []string{"titan", "cohere"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name both embedders, missing %q: %v", want, err)
		}
	}
}

// TestRunner_AcceptsWhenDeclaredEmbedderMatchesObserved — the check must not fire
// on a declared string that legitimately embeds the observed model id inside a
// longer composite.
func TestRunner_AcceptsWhenDeclaredEmbedderMatchesObserved(t *testing.T) {
	sys := embedderReportingSystem{fakeSystem: newFakeSystem("fake"), observed: "bedrock/cohere.embed-v4:0"}
	r, _ := newTestRunner(t, sys, []string{"a", `{"correct":true}`, "b", `{"correct":true}`})
	r.DeclaredEmbedder = "bedrock:cohere.embed-v4(embed)+gpt-oss-120b(titler)"

	if _, err := r.Run(context.Background(), ""); err != nil {
		t.Fatalf("matching embedder was rejected: %v", err)
	}
}
