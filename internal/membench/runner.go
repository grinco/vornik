package membench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The runner (design §5.5, §5.8, §5.9).
//
// This is where the invariants the rest of the package describes are actually
// enforced. Three of them are load-bearing:
//
//   - The leakage assertion runs BEFORE any item is scored, so a broken isolation
//     filter aborts rather than producing a plausible number over a corrupted
//     experiment.
//   - Every failure is classified into the taxonomy rather than collapsed into
//     "incorrect". An HTTP fault is not a wrong answer.
//   - ErrQuotaExhausted is terminal. Continuing past it scores later items zero
//     for a billing reason, which reads as a retrieval result.

// leakageNeedle is planted in a probe scope and must never surface elsewhere. The
// string is deliberately unlike anything in a real corpus so a hit on it cannot
// be a coincidence.
const leakageNeedle = "MEMBENCH-LEAKAGE-CANARY-8f3a1c7e do not retrieve me from another scope"

// SharedHaystackDataset is an optional Dataset capability: it declares that every
// item shares ONE haystack, so the corpus is ingested once for the whole run
// rather than once per item.
//
// The native dataset is the case that needs it. Its questions are all asked
// against the same document corpus, so per-item ingest would re-upload the whole
// corpus once per question — 30 questions over 277 documents is 8,310 deposits
// instead of 277. That would make the gate we most want to run per-change the most
// expensive one to run, which defeats the point.
//
// An optional interface rather than a Dataset method so the two public loaders,
// whose haystacks genuinely ARE per-item, need no change and cannot accidentally
// opt in.
type SharedHaystackDataset interface {
	SharedHaystack() bool
}

// sharedHaystack reports whether this run's dataset shares one haystack.
func (r *Runner) sharedHaystack() bool {
	ds, ok := r.Dataset.(SharedHaystackDataset)
	return ok && ds.SharedHaystack()
}

// leakageProbeScopes is how many isolated scopes the multi-scope sweep uses.
// Round-2 review noted that a single planted needle catches only a persistent
// logic error; a sweep across several scopes plus a query from an empty one
// exercises isolation more broadly without a quadratic test matrix.
const leakageProbeScopes = 4

// Metrics is the tier-2 summary for one category.
type Metrics struct {
	ContextRecall    float64 `json:"context_recall"`
	ContextPrecision float64 `json:"context_precision"`
	MRR              float64 `json:"mrr"`
	// Scored is how many questions contributed a defined value. Reported because
	// a mean over three items and a mean over three hundred are not the same
	// claim, and NaN-skipping makes the denominator invisible otherwise.
	Scored int `json:"scored"`
}

// RetrievalDetail records WHAT one question retrieved, not just how it scored.
//
// Added because the Cohere/Titan A/B produced byte-identical tier-2 metrics across
// every category — not credible for two different embedding models — and the
// artifacts made the discrepancy impossible to diagnose. The metrics collapse
// chunks to documents, so two embedders retrieving the same document SET in
// different chunk ORDER score identically; without the raw retrieval there is no
// way to confirm or kill that explanation.
//
// Both forms are kept deliberately: RetrievedDocuments is what the metrics see,
// RetrievedChunks is what they are blind to. Recording only the first would leave
// the same blind spot in the diagnostics as in the metrics.
type RetrievalDetail struct {
	ItemID   string `json:"item_id"`
	Category string `json:"category"`
	Question string `json:"question"`
	// GoldDocuments is the label this retrieval was scored against.
	GoldDocuments []string `json:"gold_documents"`
	// RetrievedChunks is every hit in RANK ORDER, including repeats from the same
	// document — the chunk-level ranking.
	RetrievedChunks []string `json:"retrieved_chunks"`
	// RetrievedDocuments is the distinct documents in first-appearance order — the
	// document-level ranking the metrics actually score.
	RetrievedDocuments []string `json:"retrieved_documents"`
	// Error is non-empty when the item faulted, so "retrieved nothing" stays
	// distinguishable from "never ran".
	Error string `json:"error,omitempty"`
}

// Result is one run's outcome.
type Result struct {
	Counts  map[string]OutcomeCounts `json:"counts"`
	Metrics map[string]Metrics       `json:"metrics"`
	Trust   Trust                    `json:"trust"`
	Fields  ComparabilityFields      `json:"comparability_fields"`
	// EmbeddingReadiness is the fraction of stored content that was semantically
	// searchable when the run scored. nil = the system could not report it.
	// A low value means the tier-2 numbers describe keyword-dominant retrieval.
	EmbeddingReadiness *float64 `json:"embedding_readiness,omitempty"`
	// Retrievals is the per-question retrieval record, so two runs can be diffed
	// on WHAT they retrieved rather than only on how it scored.
	Retrievals []RetrievalDetail `json:"retrievals,omitempty"`
}

// Runner executes a benchmark run.
type Runner struct {
	System    MemorySystem
	Dataset   Dataset
	Generator *AnswerGenerator
	Judge     *Judge

	// RunDir holds journal.jsonl, manifest.json and results.json.
	RunDir string

	// MaxTokens is the context budget requested from the system. The SAME value
	// goes to both systems in a comparison, or the numbers are not comparable.
	MaxTokens int

	Limits Limits
	Resume bool

	// DegradedThreshold may TIGHTEN the trust bar but never loosen it past
	// MaxDegradedThreshold (§5.9).
	DegradedThreshold float64

	// Models are recorded in the comparability key. The runner cannot discover
	// them, so the caller passes what it configured.
	HarnessVersion string
	AnswerModel    string
	JudgeModel     string
	DatasetPath    string
	DatasetSHA256  string
	SingleSystem   bool

	// RecallMethod names the retrieval path being measured, and it belongs in
	// the comparability key rather than in a note.
	//
	// Retrieval that reranks is not the same experiment as retrieval that only
	// fuses ranks — it spends an extra model call to reorder the result, which is
	// the whole point of running it. Yet the key originally carried only
	// max_tokens, so the 2026-08-11 pre-fix runs (context-assembly recall
	// silently dropped the rerank request, and the reranker never fired across
	// the entire corpus) shared a byte-identical key with post-fix reranked runs.
	// Two different systems compared clean.
	//
	// The runner cannot discover this for the same reason it cannot discover the
	// models: only the caller knows what it asked the adapter for. An empty value
	// is recorded as unknown, never silently folded in with a known method.
	RecallMethod string

	// CorpusItems is the haystack a shared-corpus run ingested, recorded in the
	// comparability key. Set by Run from the loaded dataset; nil when the dataset
	// carries its own haystacks, which dataset_sha256 already covers.
	CorpusItems []Item

	// DeclaredEmbedder is what the OPERATOR says the system embeds with. It is
	// checked against the observed value and never substituted for it: a run whose
	// declaration contradicts the system is refused, because the alternative is a
	// number labelled with a model that did not produce it.
	DeclaredEmbedder string
}

// EmbedderReporter is an optional MemorySystem capability: the embedder actually in
// force, as the system reports it.
//
// The motivating failure is worth stating. A titan arm and a cohere arm of an
// embedder comparison produced byte-identical tier-2 metrics — impossible for two
// different embedding models — and because the embedder was only ever a string an
// operator typed at a command line, nothing could establish which vectors either arm
// had queried. An adapter that can ask its system settles it.
type EmbedderReporter interface {
	ObservedEmbedder(ctx context.Context) (string, error)
}

// observedEmbedder asks the system what it embeds with, empty when it cannot say.
func (r *Runner) observedEmbedder(ctx context.Context) string {
	rep, ok := r.System.(EmbedderReporter)
	if !ok {
		return ""
	}
	got, err := rep.ObservedEmbedder(ctx)
	if err != nil {
		// Unreadable is the same as unreported for comparability: unverified, and
		// marked partial rather than assumed.
		return ""
	}
	return strings.TrimSpace(got)
}

// assertEmbedderAsDeclared refuses a run whose declared embedder the system
// contradicts.
//
// Matching is containment of the observed MODEL id in the declared string, because
// the declared value is a free-form composite naming several models ("...(embed)+
// ...(titler)") while the observed value is one concrete id. Containment is the
// strongest check that does not require parsing a format nobody specified.
//
// Silent when either side is unknown: a missing declaration is not a contradiction,
// and an unreportable system is already handled by marking the key partial.
func (r *Runner) assertEmbedderAsDeclared(declared, observed string) error {
	if strings.TrimSpace(declared) == "" || observed == "" {
		return nil
	}
	model := observed
	if i := strings.LastIndex(model, "/"); i >= 0 {
		model = model[i+1:]
	}
	if i := strings.Index(model, ":"); i >= 0 {
		model = model[:i]
	}
	if model == "" || strings.Contains(strings.ToLower(declared), strings.ToLower(model)) {
		return nil
	}
	return fmt.Errorf("refusing to run: --our-extraction-model declares %q but the "+
		"system reports it is embedding with %q. A number labelled with a model that "+
		"did not produce it is worse than no number; re-embed the corpus or correct "+
		"the declaration", declared, observed)
}

// preflight validates configuration and loads the dataset, before anything is
// ingested, scored or billed.
//
// Split out of Run so both read as phases rather than one function whose first
// third is guards. Every check here shares a property: failing it later would mean
// discarding the whole run, and NOT checking it means publishing a number that
// misdescribes the experiment.
func (r *Runner) preflight(ctx context.Context, datasetPath string) ([]BenchItem, error) {
	if r.System == nil {
		return nil, errors.New("membench: no memory system configured")
	}
	if r.Dataset == nil {
		return nil, errors.New("membench: no dataset configured")
	}
	if r.RunDir == "" {
		return nil, errors.New("membench: no run directory configured")
	}
	if datasetPath == "" {
		datasetPath = r.DatasetPath
	}

	items, err := r.Dataset.Load(datasetPath, r.Limits)
	if err != nil {
		return nil, fmt.Errorf("load dataset: %w", err)
	}

	// A declared embedder the system contradicts is refused here rather than
	// recorded and left for a reader to notice.
	if err := r.assertEmbedderAsDeclared(r.DeclaredEmbedder, r.observedEmbedder(ctx)); err != nil {
		return nil, err
	}
	return items, nil
}

// Run executes the benchmark and writes the journal, manifest and results.
//
// datasetPath is the file to load; an empty string uses r.DatasetPath, which lets
// an in-memory Dataset ignore it entirely.
func (r *Runner) Run(ctx context.Context, datasetPath string) (Result, error) {
	items, err := r.preflight(ctx, datasetPath)
	if err != nil {
		return Result{}, err
	}

	// Isolation is checked BEFORE scoring anything. A leak discovered afterwards
	// would mean discarding the whole run, and — worse — a leak never checked for
	// means publishing a number from a corrupted experiment.
	if err := r.assertNoLeakage(ctx); err != nil {
		return Result{}, err
	}

	replay, err := LoadJournal(filepath.Join(r.RunDir, "journal.jsonl"))
	if err != nil {
		return Result{}, err
	}
	journal, err := OpenJournal(filepath.Join(r.RunDir, "journal.jsonl"))
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = journal.Close() }()

	// Seed from the journal so a resumed run reports over the whole population
	// rather than only the items it re-ran.
	counts := map[string]OutcomeCounts{}
	if r.Resume {
		for cat, c := range replay.CountsByCategory() {
			counts[cat] = c
		}
	}
	recalls := map[string][]float64{}
	precisions := map[string][]float64{}
	mrrs := map[string][]float64{}
	var retrievals []RetrievalDetail
	worstLoss := 0.0

	// A shared-haystack dataset is ingested once for the whole run. Every item
	// then recalls against that one scope.
	// Record the haystack BEFORE ingest, so the comparability key describes the
	// corpus this run read even if ingest then fails partway.
	if r.sharedHaystack() && len(items) > 0 {
		r.CorpusItems = items[0].Haystack
	}
	sharedScope, sharedLoss, err := r.ingestSharedHaystack(ctx, items)
	if err != nil {
		return Result{}, err
	}
	if sharedLoss > worstLoss {
		worstLoss = sharedLoss
	}

	for _, item := range items {
		if r.Resume && replay.Completed(item.ID) {
			continue
		}
		// Any error reaching here is already terminal: runItem folds recoverable
		// faults into the item's outcome and only propagates what must stop the run.
		outcome, stats, err := r.runItem(ctx, item, sharedScope, journal, recalls, precisions, mrrs, &retrievals)
		if err != nil {
			return Result{}, err
		}
		if loss := stats.HaystackLoss(); loss > worstLoss {
			worstLoss = loss
		}
		c := counts[item.Category]
		c.Add(outcome)
		counts[item.Category] = c
	}

	res := r.assemble(ctx, counts, recalls, precisions, mrrs, worstLoss)
	res.EmbeddingReadiness = r.readiness(ctx)
	res.Retrievals = retrievals
	if err := r.writeArtifacts(res); err != nil {
		return res, err
	}
	return res, nil
}

// assemble turns the accumulated per-category samples into the run's Result.
//
// Split from Run so that function reads as the phase sequence it is — load,
// verify isolation, ingest, score — without the summarisation arithmetic
// interleaved.
func (r *Runner) assemble(
	ctx context.Context,
	counts map[string]OutcomeCounts,
	recalls, precisions, mrrs map[string][]float64,
	worstLoss float64,
) Result {
	res := Result{
		Counts:  counts,
		Metrics: map[string]Metrics{},
		Fields:  r.comparabilityFields(ctx),
	}
	for cat := range counts {
		res.Metrics[cat] = summarise(recalls[cat], precisions[cat], mrrs[cat])
	}
	res.Trust = AssessTrust(totalCounts(counts), worstLoss, r.DegradedThreshold)
	return res
}

// ingestSharedHaystack loads a shared corpus once and returns the scope it went
// into, or an empty scope when the dataset's haystacks are per-item.
//
// Split out of Run because it is a distinct phase with its own failure mode: a
// shared-haystack ingest failure is fatal to the whole run (every item depends on
// that one corpus), unlike a per-item ingest failure which is one item's
// OutcomeError.
func (r *Runner) ingestSharedHaystack(ctx context.Context, items []BenchItem) (scope string, loss float64, err error) {
	if !r.sharedHaystack() || len(items) == 0 {
		return "", 0, nil
	}
	scope = "membench/" + r.Dataset.Name() + "/shared"
	if err := r.System.Prepare(ctx, scope); err != nil {
		return "", 0, fmt.Errorf("prepare shared haystack: %w", err)
	}
	stats, err := r.System.Ingest(ctx, scope, items[0].Haystack)
	if err != nil {
		return "", 0, fmt.Errorf("ingest shared haystack: %w", err)
	}
	return scope, stats.HaystackLoss(), nil
}

// scoreQA recalls, answers and judges one question, recording the retrieval detail
// and the tier-2 samples.
//
// Split from runItem so that function reads as its two phases — ingest, then score
// — rather than interleaving them with the per-question error handling. Returns
// OutcomeError for a recoverable fault; a returned error is terminal.
func (r *Runner) scoreQA(
	ctx context.Context,
	item BenchItem,
	qa QA,
	scope string,
	journal *Journal,
	recalls, precisions, mrrs map[string][]float64,
	retrievals *[]RetrievalDetail,
) (Outcome, error) {
	recalled, err := r.System.Recall(ctx, scope, Query{Text: qa.Question, MaxTokens: r.MaxTokens})
	if err != nil {
		if errors.Is(err, ErrQuotaExhausted) {
			return "", err
		}
		*retrievals = append(*retrievals, RetrievalDetail{
			ItemID: item.ID, Category: item.Category, Question: qa.Question,
			GoldDocuments: qa.GoldDocumentIDs, Error: "recall: " + err.Error(),
		})
		return OutcomeError, fmt.Errorf("recall: %w", err)
	}
	_ = journal.Record(JournalEntry{ItemID: item.ID, Phase: PhaseRecalled, Category: item.Category})

	// Tier-2 metrics come from the retrieved DOCUMENT ids against the gold ids —
	// no LLM involved, which is what makes this tier affordable to gate on.
	ids := recalled.SourceIDs()
	*retrievals = append(*retrievals, RetrievalDetail{
		ItemID:             item.ID,
		Category:           item.Category,
		Question:           qa.Question,
		GoldDocuments:      qa.GoldDocumentIDs,
		RetrievedChunks:    ids,
		RetrievedDocuments: distinctInOrder(ids),
	})
	recalls[item.Category] = append(recalls[item.Category], ContextRecall(ids, qa.GoldDocumentIDs))
	precisions[item.Category] = append(precisions[item.Category], ContextPrecision(ids, qa.GoldDocumentIDs))
	mrrs[item.Category] = append(mrrs[item.Category], MRR(ids, qa.GoldDocumentIDs))

	answer, err := r.Generator.Answer(ctx, qa.Question, recalled.Hits)
	if err != nil {
		return OutcomeError, fmt.Errorf("answer: %w", err)
	}
	_ = journal.Record(JournalEntry{ItemID: item.ID, Phase: PhaseAnswered, Category: item.Category})

	gold := qa.GoldAnswer
	if qa.Rubric != "" {
		gold = qa.Rubric
	}
	outcome, err := r.Judge.Judge(ctx, JudgeRequest{
		Category: item.Category, Question: qa.Question,
		GoldAnswer: gold, Answer: answer,
	})
	if err != nil {
		// A judge transport failure is an error outcome, not a verdict — but the
		// reason still has to survive, or the journal records five identical
		// errored items and no way to tell a bad endpoint from a bad model.
		return OutcomeError, fmt.Errorf("judge: %w", err)
	}
	return outcome, nil
}

// runItem ingests one item's haystack, recalls, answers and judges it.
func (r *Runner) runItem(
	ctx context.Context,
	item BenchItem,
	sharedScope string,
	journal *Journal,
	recalls, precisions, mrrs map[string][]float64,
	retrievals *[]RetrievalDetail,
) (Outcome, IngestStats, error) {
	// A shared haystack was already ingested for the whole run; this item only
	// recalls against it.
	scope := sharedScope
	var stats IngestStats
	if scope == "" {
		scope = r.scopeFor(item.ID)

		if err := r.System.Prepare(ctx, scope); err != nil {
			if errors.Is(err, ErrQuotaExhausted) {
				return "", IngestStats{}, err
			}
			return r.recordError(journal, item, "prepare: "+err.Error())
		}
		var err error
		stats, err = r.System.Ingest(ctx, scope, item.Haystack)
		if err != nil {
			if errors.Is(err, ErrQuotaExhausted) {
				return "", stats, err
			}
			for _, qa := range item.QAs {
				*retrievals = append(*retrievals, RetrievalDetail{
					ItemID: item.ID, Category: item.Category, Question: qa.Question,
					GoldDocuments: qa.GoldDocumentIDs, Error: "ingest: " + err.Error(),
				})
			}
			return r.recordErrorWithStats(journal, item, "ingest: "+err.Error(), stats)
		}
	}
	_ = journal.Record(JournalEntry{ItemID: item.ID, Phase: PhaseIngested, Category: item.Category})

	// One QA per item in every dataset we load today; the loop keeps the shape
	// honest for a dataset that carries several.
	var last Outcome
	for _, qa := range item.QAs {
		outcome, err := r.scoreQA(ctx, item, qa, scope, journal, recalls, precisions, mrrs, retrievals)
		// A quota exhaustion is terminal for the whole run; anything else that
		// produced an error OUTCOME is this item's failure, and its cause is the
		// detail worth journalling.
		if err != nil && errors.Is(err, ErrQuotaExhausted) {
			return "", stats, err
		}
		if outcome == OutcomeError {
			detail := "qa failed"
			if err != nil {
				detail = err.Error()
			}
			o, st, rerr := r.recordErrorWithStats(journal, item, detail, stats)
			return o, st, rerr
		}
		if err != nil {
			return "", stats, err
		}
		last = outcome
	}

	_ = journal.Record(JournalEntry{
		ItemID: item.ID, Phase: PhaseJudged, Category: item.Category, Outcome: last,
	})
	return last, stats, nil
}

func (r *Runner) recordError(j *Journal, item BenchItem, detail string) (Outcome, IngestStats, error) {
	return r.recordErrorWithStats(j, item, detail, IngestStats{})
}

// recordErrorWithStats journals an error outcome and returns it. The item is
// marked JUDGED so a resumed run does not retry a deterministic failure forever,
// while --only-failed can still find it by its outcome.
func (r *Runner) recordErrorWithStats(
	j *Journal, item BenchItem, detail string, stats IngestStats,
) (Outcome, IngestStats, error) {
	_ = j.Record(JournalEntry{
		ItemID:   item.ID,
		Phase:    PhaseJudged,
		Category: item.Category,
		Outcome:  OutcomeError,
		Detail:   detail,
	})
	return OutcomeError, stats, nil
}

// scopeFor derives an item's isolation scope. Prefixed with the dataset name so
// two datasets in one database cannot collide.
func (r *Runner) scopeFor(itemID string) string {
	return "membench/" + r.Dataset.Name() + "/" + itemID
}

// assertNoLeakage is the §5.5 guard, in three parts.
//
// Part 1 plants a needle in one scope and queries a sibling. Part 2 sweeps
// several scopes and queries an empty one, which catches a partial leak a single
// probe would miss. Part 3 confirms the needle IS retrievable in its own scope —
// without that, a system that returns nothing at all would pass the isolation
// check trivially, and a broken adapter would look like a perfectly isolated one.
func (r *Runner) assertNoLeakage(ctx context.Context) error {
	probes := make([]string, 0, leakageProbeScopes)
	for i := 0; i < leakageProbeScopes; i++ {
		probes = append(probes, fmt.Sprintf("membench/leak-probe/%d", i))
	}

	for i, scope := range probes {
		if err := r.System.Prepare(ctx, scope); err != nil {
			return fmt.Errorf("leakage probe: prepare %s: %w", scope, err)
		}
		item := Item{
			DocumentID: fmt.Sprintf("leak-probe-%d", i),
			Content:    fmt.Sprintf("%s (probe %d)", leakageNeedle, i),
		}
		if _, err := r.System.Ingest(ctx, scope, []Item{item}); err != nil {
			return fmt.Errorf("leakage probe: ingest into %s: %w", scope, err)
		}
	}

	// Part 3 first: prove the needle is findable where it belongs, so a
	// return-nothing adapter cannot pass by silence.
	own, err := r.System.Recall(ctx, probes[0], Query{Text: leakageNeedle, MaxTokens: r.MaxTokens})
	if err != nil {
		return fmt.Errorf("leakage probe: recall in own scope: %w", err)
	}
	if len(own.Hits) == 0 {
		return fmt.Errorf("leakage probe: the canary was not retrievable in its own " +
			"scope — the adapter appears to retrieve nothing, which would make the " +
			"isolation check pass trivially and every score zero")
	}

	// Parts 1 and 2: no probe's needle may surface in another probe's scope, nor
	// in a scope that was never written to.
	empty := "membench/leak-probe/empty"
	for _, scope := range append(probes[1:], empty) {
		got, err := r.System.Recall(ctx, scope, Query{Text: leakageNeedle, MaxTokens: r.MaxTokens})
		if err != nil {
			return fmt.Errorf("leakage probe: recall in %s: %w", scope, err)
		}
		for _, h := range got.Hits {
			if strings.Contains(h.Text, leakageNeedle) && h.SourceID != scopeProbeID(scope) {
				return fmt.Errorf("SCOPE LEAK detected: the canary planted in another "+
					"scope surfaced in %s as %q. Aborting rather than scoring "+
					"cross-contaminated recall — every number from this run would be "+
					"meaningless", scope, h.SourceID)
			}
		}
	}
	return nil
}

// scopeProbeID is the document a given probe scope legitimately owns, so a hit on
// its OWN needle is not mistaken for a leak.
func scopeProbeID(scope string) string {
	i := strings.LastIndex(scope, "/")
	if i < 0 {
		return ""
	}
	return "leak-probe-" + scope[i+1:]
}

func (r *Runner) comparabilityFields(ctx context.Context) ComparabilityFields {
	cfg, err := r.System.Config(ctx)
	if err != nil {
		// An unreadable config is the same as an absent one for comparability
		// purposes: unverified, and marked partial rather than assumed unchanged.
		cfg = ""
	}
	f := ComparabilityFields{
		HarnessVersion:     r.HarnessVersion,
		DatasetName:        r.Dataset.Name(),
		DatasetSHA256:      r.DatasetSHA256,
		ItemSelection:      describeLimits(r.Limits),
		OurExtractionModel: cfg,
		AnswerModel:        r.AnswerModel,
		JudgeModel:         r.JudgeModel,
		RecallParams:       fmt.Sprintf("max_tokens=%d;method=%s", r.MaxTokens, recallMethodOrUnknown(r.RecallMethod)),
		CorpusSHA256:       CorpusDigest(r.CorpusItems),
		ObservedEmbedder:   r.observedEmbedder(ctx),
		AnswerPromptSHA256: AnswerPromptSHA256(),
		JudgePromptSHA256:  JudgePromptSHA256(),
		SingleSystem:       r.SingleSystem,
	}
	if r.SingleSystem {
		return f
	}
	f.TheirExtractionModel = cfg
	f.ExternalConfigSHA256 = cfg
	return f
}

// recallMethodOrUnknown keeps an unset method visibly unknown in the key.
//
// Substituting a default would be worse than useless: a caller that forgot to
// declare its retrieval path would silently inherit the key of one that did, and
// the resulting comparison would read as clean.
func recallMethodOrUnknown(method string) string {
	if m := strings.TrimSpace(method); m != "" {
		return m
	}
	return "unknown"
}

func describeLimits(l Limits) string {
	return fmt.Sprintf("category=%s;max_items=%d;max_per_category=%d",
		l.Category, l.MaxItems, l.MaxItemsPerCategory)
}

func summarise(recalls, precisions, mrrs []float64) Metrics {
	rMean, n := MeanIgnoringNaN(recalls)
	pMean, _ := MeanIgnoringNaN(precisions)
	mMean, _ := MeanIgnoringNaN(mrrs)
	return Metrics{
		ContextRecall:    zeroIfNaN(rMean),
		ContextPrecision: zeroIfNaN(pMean),
		MRR:              zeroIfNaN(mMean),
		Scored:           n,
	}
}

// zeroIfNaN flattens an undefined mean for JSON, which cannot represent NaN.
// Scored is what tells a reader the difference between "no data" and "zero", so
// the information is not lost — it moves.
func zeroIfNaN(f float64) float64 {
	if math.IsNaN(f) {
		return 0
	}
	return f
}

func totalCounts(byCat map[string]OutcomeCounts) OutcomeCounts {
	var total OutcomeCounts
	for _, c := range byCat {
		total.Correct += c.Correct
		total.Incorrect += c.Incorrect
		total.Invalid += c.Invalid
		total.Error += c.Error
	}
	return total
}

// MethodNoter is an optional MemorySystem capability: an adapter that had to
// adapt the harness's request to its service's shape reports what it changed.
//
// A conversion that happens but is never written down is indistinguishable from
// one that did not happen. The top-k conversion is the live case: an adapter over
// a service that accepts only a result count cannot honour a token budget
// exactly, and §5.6 requires that asymmetry be reported rather than hidden.
type MethodNoter interface {
	Notes() []string
}

// EmbeddingReadinessReporter is an optional MemorySystem capability: the fraction
// of the system's stored content that is currently semantically searchable.
//
// Added because the first live run exposed a hole nothing else could see.
// Embedding is ASYNC, and the harness ingests a corpus then immediately recalls
// it — on a cold corpus only 126 of 3,187 chunks were embedded when scoring
// began, so the tier-2 numbers measured KEYWORD-ONLY retrieval while looking
// exactly like semantic-retrieval scores. Nothing in the output distinguished the
// two, which is the kind of silent mismeasurement this whole harness exists to
// prevent.
//
// Reported, never fatal: keyword-only is a legitimate configuration (a deployment
// with no embedder runs that way permanently), so a low value changes what the
// numbers MEAN rather than making them invalid.
type EmbeddingReadinessReporter interface {
	EmbeddingReadiness(ctx context.Context) (float64, error)
}

// readiness returns the system's embedding readiness, or nil when it cannot be
// determined. Nil rather than a number, because an unknown fraction must not read
// as a measured one.
func (r *Runner) readiness(ctx context.Context) *float64 {
	rep, ok := r.System.(EmbeddingReadinessReporter)
	if !ok {
		return nil
	}
	f, err := rep.EmbeddingReadiness(ctx)
	if err != nil {
		return nil
	}
	return &f
}

// manifest is the run's provenance record.
type manifest struct {
	System            string              `json:"system"`
	DatasetName       string              `json:"dataset_name"`
	DatasetSHA256     string              `json:"dataset_sha256,omitempty"`
	ComparabilityKey  string              `json:"comparability_key"`
	ComparabilityPart bool                `json:"comparability_partial"`
	Fields            ComparabilityFields `json:"comparability_fields"`
	Trust             Trust               `json:"trust"`
	// MethodNotes records adaptations the adapter had to make — a token budget
	// converted to a result count, for instance. Empty when the adapter honoured
	// the request as given.
	MethodNotes []string `json:"method_notes,omitempty"`
	// EmbeddingReadiness — see Result.EmbeddingReadiness. On the manifest because
	// it is provenance: it says what the numbers are a measurement OF.
	EmbeddingReadiness *float64 `json:"embedding_readiness,omitempty"`
	FinishedAt         string   `json:"finished_at"`
}

func (r *Runner) writeArtifacts(res Result) error {
	m := manifest{
		System:            r.System.Name(),
		DatasetName:       r.Dataset.Name(),
		DatasetSHA256:     r.DatasetSHA256,
		ComparabilityKey:  res.Fields.Key(),
		ComparabilityPart: res.Fields.Partial(),
		Fields:            res.Fields,
		Trust:             res.Trust,
		FinishedAt:        time.Now().UTC().Format(time.RFC3339),
	}
	if noter, ok := r.System.(MethodNoter); ok {
		m.MethodNotes = noter.Notes()
	}
	m.EmbeddingReadiness = res.EmbeddingReadiness
	if err := writeJSON(filepath.Join(r.RunDir, "manifest.json"), m); err != nil {
		return err
	}
	return writeJSON(filepath.Join(r.RunDir, "results.json"), res)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// readFileString is used by tests to inspect written artifacts.
func readFileString(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}

// distinctInOrder collapses a chunk-level ranking to its distinct documents in
// FIRST-APPEARANCE order — the document-level ranking the tier-2 metrics score.
//
// First-appearance rather than sorted: the order is the ranking, and sorting would
// discard exactly the information this instrumentation exists to preserve.
func distinctInOrder(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
