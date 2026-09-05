package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/agentbench"
	"vornik.io/vornik/internal/membench"
)

// `vornikctl bench memory` — the memory-benchmark harness CLI
// (https://docs.vornik.io §5.10).
//
// Sibling to `vornikctl eval`, which judges TASK outcomes. This one judges
// RETRIEVAL. They share shape and conventions but no code path.

var (
	benchDataset         string
	benchDatasetPath     string
	benchCorpusDir       string
	benchSystem          string
	benchProfile         string
	benchTier2Only       bool
	benchExternalDialect string
	benchAcceptUnverif   bool
	benchExternalDelete  string
	benchMaxItems        int
	benchPerCategory     int
	benchCategory        string
	benchMaxTokens       int
	benchRunDir          string
	benchResume          bool
	benchConfirmWipe     string
	benchDatabase        string
	benchDatasetHash     string
	benchJSON            bool
	benchAnswerModel     string
	benchJudgeModel      string
	benchExternalURL     string
	benchExternalTok     string
	benchExtractionModel string
	benchRecallMethod    string
	benchExternalIngest  string
	benchExternalRecall  string
	benchExternalCfgPath string
	benchDegradedRate    float64
)

var benchCmd = &cobra.Command{
	Use:   "bench",
	Short: "Benchmark subsystems against labelled datasets",
}

var benchMemoryCmd = &cobra.Command{
	Use:   "memory",
	Short: "Score memory retrieval quality on a labelled dataset",
	Long: "Score memory retrieval quality on a labelled dataset.\n\n" +
		"Reports three tiers separately and never blended: judge-free retrieval\n" +
		"metrics (context recall/precision/MRR), judged answer accuracy, and\n" +
		"cost/latency. The judge-free tier is the one cheap enough to gate on.",
}

var benchMemoryRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Run the benchmark and write a manifest, journal and results",
	RunE:  runBenchMemory,
}

var benchMemoryReportCmd = &cobra.Command{
	Use:   "report <run-dir>",
	Short: "Print the scoreboard for a completed run",
	Args:  cobra.ExactArgs(1),
	RunE:  runBenchMemoryReport,
}

// verify-determinism is a DIFFERENT question from `compare`. That one asks
// whether two runs of different configurations may be plotted on one axis; this
// one asks whether the SAME configuration reproduces itself.
var benchMemoryVerifyDeterminismCmd = &cobra.Command{
	Use:   "verify-determinism <run-dir-a> <run-dir-b>",
	Short: "Require two runs of the same fixture to have retrieved byte-identically",
	Long: "Compare the per-question retrieval of two runs and fail on any difference.\n\n" +
		"This is the blocking half of the retrieval CI gate. It needs no committed\n" +
		"baseline, so it can never go stale: there is nothing to re-bless when\n" +
		"retrieval legitimately improves.\n\n" +
		"It compares the CHUNK-LEVEL RANK ORDER, not the metrics. On 2026-08-11 RRF\n" +
		"ties broke arbitrarily and two identical runs ranked differently, while every\n" +
		"metric said they matched — tier-2 collapses chunks to documents and the\n" +
		"document set was unchanged. A metrics-based check is blind to the defect this\n" +
		"exists to catch.\n\n" +
		"Intended use: run the fixture twice with --tier2-only, then verify.",
	Args: cobra.ExactArgs(2),
	RunE: runBenchMemoryVerifyDeterminism,
}

var benchMemoryAggregateCmd = &cobra.Command{
	Use:   "aggregate <run-dir>...",
	Short: "Summarise repeated runs: mean, spread, and the gate tolerance each metric needs",
	Long: "Summarise repeated runs of the same experiment.\n\n" +
		"The variance of a metric — not its value — is what decides whether the\n" +
		"metric can be gated at all. A metric identical across every run can carry\n" +
		"an exact-equality gate; one that moves needs a tolerance, reported here as\n" +
		"3 standard deviations.\n\n" +
		"Refuses to average runs whose comparability keys differ, and refuses any\n" +
		"run already marked untrustworthy.",
	Args: cobra.MinimumNArgs(2),
	RunE: runBenchMemoryAggregate,
}

var benchMemoryCompareCmd = &cobra.Command{
	Use:   "compare <run-dir-a> <run-dir-b>",
	Short: "Compare two runs, refusing when they are not comparable",
	Args:  cobra.ExactArgs(2),
	RunE:  runBenchMemoryCompare,
}

func init() {
	f := benchMemoryRunCmd.Flags()
	f.StringVar(&benchDataset, "dataset", "", "Dataset: longmemeval | locomo | native (required)")
	f.StringVar(&benchDatasetPath, "dataset-path", "", "Path to the dataset file (or gold set, for native)")
	f.StringVar(&benchCorpusDir, "corpus-dir", "", "Directory of documents forming the haystack for the "+
		"native dataset. Required with --dataset native, whose gold set names documents in this "+
		"directory; the longmemeval and locomo datasets carry their own haystacks and ignore it.")
	f.StringVar(&benchSystem, "system", "vornik", "System under test: vornik | external")
	f.StringVar(&benchProfile, "profile", "", "Model profile: local | judged | cloud (required unless every model flag is set)")
	f.IntVar(&benchMaxItems, "max-items", 0, "Cap total items (0 = all)")
	f.IntVar(&benchPerCategory, "max-items-per-category", 0, "Cap items per category (0 = all)")
	f.StringVar(&benchCategory, "category", "", "Only run this category")
	f.IntVar(&benchMaxTokens, "max-tokens", 4096, "Context budget requested from the system")
	f.StringVar(&benchRunDir, "run-dir", "", "Directory for journal/manifest/results (default: bench-runs/<timestamp>)")
	f.BoolVar(&benchResume, "resume", false, "Skip items already judged in the run directory's journal")
	f.BoolVar(&benchTier2Only, "tier2-only", false,
		"Score RETRIEVAL only (context recall/precision/MRR): no answer generation, no judge, "+
			"no model credentials. Accuracy is reported as undefined, not zero. This is the mode "+
			"a CI gate uses — the RRF retrieval path is deterministic where judged accuracy has "+
			"sd ~4.5% at n=30 and would fire on noise. It also stops REQUESTING the reranked "+
			"path, and REFUSES the run if recall reports a rerank happened anyway or cannot say "+
			"which path it took: an LLM reranker is billed per call and reorders between "+
			"identical runs, so it must be off on the deployment under test "+
			"(memory.reranker.enabled: false) — this flag alone cannot disable it")
	f.StringVar(&benchExternalDelete, "external-bank-delete-path", "",
		"Route template that DELETES one bank, e.g. '/v1/default/banks/{bank}'. Without it "+
			"teardown is a no-op and each run re-ingests into the previous run's bank: the "+
			"corpus accumulates, precision falls run over run, and repeated runs are not "+
			"comparable. Hindsight measured 0.806 then 0.639 precision on identical items "+
			"for exactly this reason")
	f.BoolVar(&benchAcceptUnverif, "accept-unverified-path", false,
		"Permit a --tier2-only run whose retrieval path cannot be shown deterministic: an "+
			"external service that does not report a retrieval method, or a competitor that "+
			"reranks internally. REQUIRED for --system external. It does not silence the "+
			"check — the run is stamped retrieval_path_unverified in its comparability key, "+
			"so it can never compare clean against a gate baseline that proved determinism")
	f.StringVar(&benchExternalDialect, "external-dialect", "",
		"Request-BODY shape for --system external: empty = the conventional guess, "+
			"'hindsight' = the shape verified against hindsight 0.9.0 (batched `items[]` "+
			"ingest keyed `timestamp`, idempotent PUT bank create). Paths stay separately "+
			"configurable; only bodies and the create method differ")
	f.StringVar(&benchConfirmWipe, "i-know-this-wipes", "", "Name the database this run will bulk-write (required)")
	f.StringVar(&benchDatabase, "database", "", "Target database name (required)")
	f.StringVar(&benchDatasetHash, "dataset-sha256", "", "Expected dataset digest; verified before the run")
	f.StringVar(&benchAnswerModel, "answer-model", "", "Model that answers from retrieved context")
	f.StringVar(&benchJudgeModel, "judge-model", "", "Model that grades answers")
	f.StringVar(&benchExtractionModel, "our-extraction-model", "",
		"The daemon's OWN ingest/retrieval model (titler, classifier, reranker) for the "+
			"comparability key. Unset marks the key partial — the companion surface cannot "+
			"report it, and guessing would misrepresent the run.")
	f.StringVar(&benchRecallMethod, "recall-method", "",
		"The retrieval path actually EXERCISED, for the comparability key — e.g. "+
			"context-assembly+rerank | context-assembly | interactive. Verify it against the "+
			"usage ledger, not the config: a reranker can be enabled and correctly wired and "+
			"still never fire, in which case the flag you requested proves nothing. Unset "+
			"records the method as unknown rather than assuming one.")
	f.StringVar(&benchExternalURL, "external-url", "", "Base URL of the external system (--system external)")
	f.StringVar(&benchExternalTok, "external-token", "", "Bearer token for the external system")
	f.StringVar(&benchExternalIngest, "external-ingest-path", "", "Override the external ingest route (default: a conventional shape)")
	f.StringVar(&benchExternalRecall, "external-recall-path", "", "Override the external recall route")
	f.StringVar(&benchExternalCfgPath, "external-config-path", "", "Route reporting the external system's effective config; unset marks the comparability key PARTIAL")
	f.Float64Var(&benchDegradedRate, "max-degraded-rate", 0,
		fmt.Sprintf("Tighten the untrustworthy threshold below the %.0f%% ceiling (0 = ceiling)",
			membench.MaxDegradedThreshold*100))
	f.BoolVar(&benchJSON, "json", false, "Print results as JSON")

	benchMemoryAggregateCmd.Flags().BoolVar(&benchJSON, "json", false, "Print the aggregation as JSON")
	benchMemoryCmd.AddCommand(benchMemoryVerifyDeterminismCmd)
	benchMemoryCmd.AddCommand(benchMemoryRunCmd, benchMemoryReportCmd,
		benchMemoryAggregateCmd, benchMemoryCompareCmd)
	benchCmd.AddCommand(benchMemoryCmd)
	rootCmd.AddCommand(benchCmd)
}

// harnessVersion identifies the harness in the comparability key. Bumped
// deliberately when metric definitions or selection logic change, because either
// makes older runs incomparable.
//
// v2 (2026-08-11): ContextPrecision moved from chunk-level to document-level
// after the first live baseline showed the chunk-level form was structurally
// pinned at gold_docs/budget. Any v1 run's precision figure is not comparable to
// a v2 one, and `bench memory compare` will now correctly refuse to diff them.
//
// v3 (2026-08-11): recall_params gained the retrieval method. Every v1/v2 run was
// taken against a build where context-assembly recall silently dropped the rerank
// request, so the reranker never fired — yet nothing in the key said so, and a
// reranked run would have compared clean against them. Bumping the version is what
// makes that incomparability explicit instead of leaving it to a field a v2 run
// never carried.
// v4 (2026-08-11): the key gained corpus_sha256 and observed_embedder. Every v3
// run predates both, so nothing in a stored v3 key can distinguish which corpus it
// read or which embedding model produced its vectors — the same reason v3 itself was
// not merely a new field. The bump is what makes that incomparability explicit.
//
// v5 (2026-09-03): the key gained corpus_regime and daemon_revision. A v4 key
// could not tell a warm corpus from a cold one — bench-runs/reproduce-1..3
// (warm) and head-20260827-tr6-r1..3 (cold) both keyed e865104e9959, and
// aggregate would have merged them — nor could it tell two releases apart, so
// runs of different builds with the same config compared clean. Existing
// published rows keep their v4 keys as history; they are not recomputed, because
// a key is a record of what a run could say about itself at the time.
const harnessVersion = "5"

func runBenchMemory(cmd *cobra.Command, _ []string) error {
	// The destructive-run guard comes FIRST, before any dataset is read or any
	// model is contacted. A run that is going to be refused should be refused
	// before it costs anything.
	if err := membench.CheckDestructiveTarget(benchDatabase, benchConfirmWipe); err != nil {
		return err
	}

	// "No default" has to mean refusal, or it silently becomes a default nobody
	// chose (design §5.6).
	// --tier2-only needs NO model selection, which is the entire point: a gate that
	// required a judge could not run on a fork PR, and one that billed per PR gets
	// switched off. Requiring a profile here anyway would have made the flag
	// useless for the job it exists to do.
	if !benchTier2Only && benchProfile == "" && (benchAnswerModel == "" || benchJudgeModel == "") {
		return fmt.Errorf("model selection required: pass --profile (local|judged|cloud), " +
			"set both --answer-model and --judge-model explicitly, or pass --tier2-only to " +
			"score retrieval without any model")
	}
	if err := applyProfile(); err != nil {
		return err
	}

	ds, path, err := resolveDataset()
	if err != nil {
		return err
	}

	// Re-verify the dataset even when it was fetched earlier: a file can be
	// replaced between fetch and use, and the manifest records the hash of what
	// was actually read.
	actualHash, err := membench.VerifyFile(path, benchDatasetHash)
	if err != nil {
		return err
	}

	sys, err := resolveSystem()
	if err != nil {
		return err
	}

	// The string guard above proves the operator typed a name twice and that it is
	// not denylisted. It cannot prove the run will write THAT database — and on
	// 2026-08-12 it did not: a run naming a freshly-created throwaway wrote twelve
	// fixture documents into the production corpus, leaving the named database with
	// zero tables. This asks the system where its writes actually land, and fails
	// closed when it cannot say.
	if err := membench.VerifyWriteTarget(cmd.Context(), sys, benchDatabase); err != nil {
		return err
	}

	// The clear the flag has always promised. ONLY here — after both guards
	// have proved the target is neither production nor a database the operator
	// did not name. Before them this would be the most dangerous call in the
	// package; after them it is the one --i-know-this-wipes authorised.
	//
	// Without it the store accumulates and the run dedups itself: admitted
	// deposits fell 426 -> 426 -> 209 over three runs of the same 120 items
	// until two items lost their whole haystack, and accuracy moved
	// 0.692 -> 0.750 on a manual wipe. See design §5.8.
	regime, err := clearBenchStore(cmd.Context(), sys, "membench/%")
	if err != nil {
		return err
	}

	runDir := benchRunDir
	if runDir == "" {
		runDir = filepath.Join("bench-runs", time.Now().UTC().Format("20060102-150405"))
	}
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return fmt.Errorf("create run dir: %w", err)
	}

	// In tier-2-only mode NO LLM client is constructed at all. Building one and
	// then not calling it would still require an endpoint and credentials, which
	// is precisely what makes the gate un-runnable on a fork PR — the flag has to
	// remove the dependency, not merely the traffic.
	var generator *membench.AnswerGenerator
	var judge *membench.Judge
	if !benchTier2Only {
		llm, err := newBenchLLM()
		if err != nil {
			return err
		}
		// The judge may run on a different model from the answerer — that split IS
		// the "judged" profile, and folding them into one client would make it
		// unexpressible.
		judgeLLM := llm.withModel(benchJudgeModel)
		generator = membench.NewAnswerGenerator(llm)
		judge = membench.NewJudge(judgeLLM)
	}

	runner := &membench.Runner{
		System:               sys,
		Dataset:              ds,
		Generator:            generator,
		Judge:                judge,
		RunDir:               runDir,
		MaxTokens:            benchMaxTokens,
		Limits:               membench.Limits{MaxItems: benchMaxItems, MaxItemsPerCategory: benchPerCategory, Category: benchCategory},
		Resume:               benchResume,
		Tier2Only:            benchTier2Only,
		AcceptUnverifiedPath: benchAcceptUnverif,
		DegradedThreshold:    benchDegradedRate,
		HarnessVersion:       harnessVersion,
		AnswerModel:          benchAnswerModel,
		JudgeModel:           benchJudgeModel,
		DatasetPath:          path,
		DatasetSHA256:        actualHash,
		SingleSystem:         benchSystem != "external",
		RecallMethod:         benchRecallMethod,
		DeclaredEmbedder:     benchExtractionModel,
		CorpusRegime:         regime,
		DaemonRevision:       benchDaemonRevision(cmd.Context(), sys),
		HarnessRevision:      agentbench.HarnessBuild(),
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "running %s on %s (run dir %s)\n", sys.Name(), ds.Name(), runDir)

	res, err := runner.Run(context.Background(), path)
	if err != nil {
		return err
	}
	return printResult(cmd, res, runDir)
}

// applyProfile fills unset model flags from the named preset. An explicit flag
// always wins, so a profile is a starting point rather than an override.
func applyProfile() error {
	type preset struct{ answer, judge string }
	// Model names are HOST-SPECIFIC: these are the locally-served models on the
	// deployment this was built for. A profile naming an absent model fails at the
	// first completion with a model-not-found, so treat these as a convenience for
	// the common case and pass --answer-model / --judge-model explicitly anywhere
	// else. `ollama list` shows what is actually available.
	presets := map[string]preset{
		// Local: everything on a small open-weight model. Near-zero marginal cost,
		// so the full dataset is affordable; judge noise is the trade.
		"local": {answer: "qwen3.5:9b", judge: "qwen3.5:9b"},
		// Judged: small model answering, a larger local model grading. Judge calls
		// are one per question, so the extra cost is bounded while grading noise
		// drops — the split that makes this profile worth having.
		"judged": {answer: "qwen3.5:9b", judge: "gemma4:26b"},
		// Cloud: a strong hosted model throughout. Best absolute scores, real money,
		// and needs VORNIK_BENCH_LLM_URL pointed at a cloud endpoint.
		"cloud": {answer: "claude-opus-5", judge: "claude-opus-5"},
	}
	if benchProfile == "" {
		return nil
	}
	p, ok := presets[benchProfile]
	if !ok {
		names := make([]string, 0, len(presets))
		for k := range presets {
			names = append(names, k)
		}
		sort.Strings(names)
		return fmt.Errorf("unknown profile %q; known profiles: %s",
			benchProfile, strings.Join(names, ", "))
	}
	if benchAnswerModel == "" {
		benchAnswerModel = p.answer
	}
	if benchJudgeModel == "" {
		benchJudgeModel = p.judge
	}
	return nil
}

func resolveDataset() (membench.Dataset, string, error) {
	switch benchDataset {
	case "longmemeval":
		return membench.LongMemEval{}, defaultPath("longmemeval.json"), nil
	case "locomo":
		return membench.LoCoMo{}, defaultPath("locomo10.json"), nil
	case "native":
		// The gold set names documents by id, and those ids only resolve against a
		// corpus directory. Refusing beats an empty haystack, which would score
		// every question as a retrieval failure and read as a product defect.
		if strings.TrimSpace(benchCorpusDir) == "" {
			return nil, "", fmt.Errorf("--dataset native needs --corpus-dir: the gold set " +
				"names documents in a directory you supply, and an empty haystack would " +
				"score every question as a retrieval failure")
		}
		return membench.Native{CorpusDir: benchCorpusDir}, defaultPath("native-goldset.json"), nil
	case "":
		return nil, "", fmt.Errorf("--dataset is required (longmemeval | locomo | native)")
	default:
		return nil, "", fmt.Errorf("unknown dataset %q", benchDataset)
	}
}

func defaultPath(name string) string {
	if benchDatasetPath != "" {
		return benchDatasetPath
	}
	return filepath.Join("bench", name)
}

func resolveSystem() (membench.MemorySystem, error) {
	switch benchSystem {
	case "vornik", "":
		url, token := daemonURLAndToken()
		if url == "" || token == "" {
			return nil, fmt.Errorf("the vornik adapter needs VORNIK_URL and " +
				"VORNIK_COMPANION_TOKEN (a companion key with memory_read + memory_write)")
		}
		// ExtractionModel is what the DAEMON uses on the ingest/retrieval side —
		// its titler, classifier, graph extractor and reranker. It is NOT the
		// answer model.
		//
		// Defaulting it from --answer-model (as this did) made the manifest claim
		// `our_extraction_model = glm-5.2:cloud` when the daemon was actually
		// running Bedrock models for that work. A comparability key that
		// misreports the configuration is worse than one that admits ignorance:
		// two runs with genuinely different extraction models would have matched.
		//
		// The companion surface does not expose the daemon's model wiring, so an
		// unset value stays EMPTY and marks the key partial — accurate, because we
		// have not verified it.
		return membench.NewVornikSystem(membench.VornikConfig{
			BaseURL:         url,
			Token:           token,
			ExtractionModel: benchExtractionModel,
			// Tier-2-only stops REQUESTING the reranked path. It cannot switch off
			// a reranker the daemon applies anyway, which is why the runner also
			// verifies the observed method per recall and refuses on a rerank.
			NoRerank: membench.GateSuppressesRerank(benchTier2Only, benchAcceptUnverif),
		}), nil
	case "external":
		if benchExternalURL == "" {
			return nil, fmt.Errorf("--system external needs --external-url")
		}
		// Paths are configurable because the comparison service's exact routes
		// cannot be verified until a live run; the defaults are a conventional
		// shape, not a confirmed contract.
		return membench.NewExternalSystem(membench.ExternalConfig{
			BaseURL:         benchExternalURL,
			Token:           benchExternalTok,
			IngestPath:      benchExternalIngest,
			RecallPath:      benchExternalRecall,
			ConfigPath:      benchExternalCfgPath,
			Dialect:         benchExternalDialect,
			BankDeletePath:  benchExternalDelete,
			ExtractionModel: benchAnswerModel,
		}), nil
	default:
		return nil, fmt.Errorf("unknown system %q (vornik | external)", benchSystem)
	}
}

func daemonURLAndToken() (url, token string) {
	return os.Getenv("VORNIK_URL"), os.Getenv("VORNIK_COMPANION_TOKEN")
}

func printResult(cmd *cobra.Command, res membench.Result, runDir string) error {
	out := cmd.OutOrStdout()
	if benchJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}

	// Trust first. A degraded run's numbers must not be read before the warning
	// that they should not be quoted.
	if !res.Trust.Trustworthy {
		_, _ = fmt.Fprintf(out, "\n!! UNTRUSTWORTHY RUN — do not quote these numbers\n   %s\n\n", res.Trust.Reason)
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "CATEGORY\tCORRECT\tINCORRECT\tINVALID\tERROR\tACCURACY\tRECALL\tPRECISION\tMRR\tSCORED")
	cats := make([]string, 0, len(res.Counts))
	for c := range res.Counts {
		cats = append(cats, c)
	}
	sort.Strings(cats)
	for _, cat := range cats {
		c := res.Counts[cat]
		m := res.Metrics[cat]
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%s\t%.3f\t%.3f\t%.3f\t%d\n",
			cat, c.Correct, c.Incorrect, c.Invalid, c.Error,
			formatAccuracy(c), m.ContextRecall, m.ContextPrecision, m.MRR, m.Scored)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	// Readiness before the comparability note: it changes what the numbers ARE,
	// which the reader needs before anything about whether they are comparable.
	if res.EmbeddingReadiness != nil && *res.EmbeddingReadiness < 0.95 {
		_, _ = fmt.Fprintf(out, "\nNOTE: only %.1f%% of stored content was embedded when this run "+
			"scored, so these retrieval numbers are keyword-dominant rather than "+
			"semantic. Wait for the embedding queue to drain and re-run for a "+
			"semantic baseline.\n", *res.EmbeddingReadiness*100)
	}
	if res.Fields.Partial() {
		_, _ = fmt.Fprintf(out, "\nNOTE: comparability key is PARTIAL — the external system's "+
			"configuration could not be read, so this run cannot be proven comparable "+
			"to another.\n")
	}
	_, _ = fmt.Fprintf(out, "\nartifacts: %s\n", runDir)
	return nil
}

// formatAccuracy renders an undefined accuracy as "n/a" rather than 0.000, which
// would be indistinguishable from getting everything wrong.
func formatAccuracy(c membench.OutcomeCounts) string {
	if c.Judged() == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.3f", c.Accuracy())
}

func runBenchMemoryReport(cmd *cobra.Command, args []string) error {
	res, err := loadResult(args[0])
	if err != nil {
		return err
	}
	return printResult(cmd, res, args[0])
}

func runBenchMemoryVerifyDeterminism(cmd *cobra.Command, args []string) error {
	a, err := loadResult(args[0])
	if err != nil {
		return err
	}
	b, err := loadResult(args[1])
	if err != nil {
		return err
	}
	if err := membench.CompareRetrieval(a.Retrievals, b.Retrievals); err != nil {
		return fmt.Errorf("retrieval is NOT deterministic: %w", err)
	}
	cmd.Printf("retrieval is deterministic: %d questions retrieved identically across both runs\n",
		len(a.Retrievals))
	return nil
}

func runBenchMemoryCompare(cmd *cobra.Command, args []string) error {
	a, err := loadResult(args[0])
	if err != nil {
		return err
	}
	b, err := loadResult(args[1])
	if err != nil {
		return err
	}
	// Refuses and names the differing fields. This is the mechanism that stops
	// two incomparable runs being plotted on one axis (design §5.6).
	if err := membench.CheckComparable(a.Fields, b.Fields); err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "CATEGORY\tA RECALL\tB RECALL\tA PRECISION\tB PRECISION\tA MRR\tB MRR")
	cats := map[string]bool{}
	for c := range a.Metrics {
		cats[c] = true
	}
	for c := range b.Metrics {
		cats[c] = true
	}
	names := make([]string, 0, len(cats))
	for c := range cats {
		names = append(names, c)
	}
	sort.Strings(names)
	for _, cat := range names {
		am, bm := a.Metrics[cat], b.Metrics[cat]
		_, _ = fmt.Fprintf(tw, "%s\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\n",
			cat, am.ContextRecall, bm.ContextRecall,
			am.ContextPrecision, bm.ContextPrecision, am.MRR, bm.MRR)
	}
	return tw.Flush()
}

// runBenchMemoryAggregate summarises several runs of one experiment.
func runBenchMemoryAggregate(cmd *cobra.Command, args []string) error {
	runs := make([]membench.Result, 0, len(args))
	for _, dir := range args {
		res, err := loadResult(dir)
		if err != nil {
			return err
		}
		runs = append(runs, res)
	}
	agg, err := membench.Aggregate(runs)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if benchJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(agg)
	}

	_, _ = fmt.Fprintf(out, "\n%d runs, comparability key %s\n", agg.Runs, agg.Fields.Key()[:12])
	if agg.Fields.Partial() {
		_, _ = fmt.Fprintf(out, "key is PARTIAL — some configuration was unverifiable\n")
	}
	_, _ = fmt.Fprintln(out)

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "METRIC\tMEAN\tSD\tMIN\tMAX\tSPREAD\tGATE")
	rows := []struct {
		name string
		s    membench.Stat
	}{
		{"accuracy", agg.Accuracy},
		{"context recall", agg.ContextRecall},
		{"context precision", agg.ContextPrecision},
		{"mrr", agg.MRR},
	}
	for _, r := range rows {
		gate := fmt.Sprintf("±%.4f", r.s.GateTolerance)
		if r.s.Deterministic {
			gate = "exact (deterministic)"
		}
		_, _ = fmt.Fprintf(w, "%s\t%.4f\t%.4f\t%.4f\t%.4f\t%.4f\t%s\n",
			r.name, r.s.Mean, r.s.SD, r.s.Min, r.s.Max, r.s.Spread, gate)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	// Stated rather than left implicit: a threshold narrower than the noise fails
	// on noise alone, and a gate that does that gets switched off and stays off.
	_, _ = fmt.Fprintf(out, "\nGATE is the narrowest defensible CI threshold (%.0f sd). A metric marked\n"+
		"deterministic across these runs can be gated on exact equality instead.\n",
		membench.GateSigma)
	return nil
}

func loadResult(runDir string) (membench.Result, error) {
	var res membench.Result
	raw, err := os.ReadFile(filepath.Join(runDir, "results.json"))
	if err != nil {
		return res, fmt.Errorf("read results from %s: %w", runDir, err)
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return res, fmt.Errorf("parse results in %s: %w", runDir, err)
	}
	return res, nil
}

// benchDaemonRevision asks the system under test which build it is, or returns
// "" when it cannot say.
//
// Optional-interface rather than a field on MemorySystem: an external competitor
// has no vornik revision to report, and widening the interface would oblige
// every adapter to answer a question only one of them can. Empty marks the
// comparability key PARTIAL, which is the honest reading — a run that cannot
// name its build must not compare clean against one that can.
func benchDaemonRevision(ctx context.Context, sys membench.MemorySystem) string {
	rep, ok := sys.(interface{ DaemonRevision(context.Context) string })
	if !ok {
		return ""
	}
	return rep.DaemonRevision(ctx)
}

// clearBenchStore resets the benchmark project's retrievable memory before a run.
//
// The project is read from the daemon (companion whoami) rather than taken from
// a flag: the harness already refuses to trust a typed database name, and a
// typed PROJECT name would be the same hole one field over — a clear pointed at
// the wrong project is worse than no clear.
//
// The DSN is VORNIK_BENCH_DSN, the same variable `bench agent` already requires,
// and it is read HERE rather than at startup so it is only needed by a run that
// has already passed both guards. Its database must match the verified target;
// a DSN pointing elsewhere is the 2026-08-12 incident with an extra step.
func clearBenchStore(ctx context.Context, sys membench.MemorySystem, scopePrefix string) (membench.CorpusRegime, error) {
	reporter, ok := sys.(interface {
		WriteTargetProject(context.Context) (string, error)
	})
	if !ok {
		return membench.CorpusRegimeUnknown, fmt.Errorf("refusing to run: the %q system cannot report its project, so the "+
			"per-run clear cannot know which store to reset. An uncleared store dedups the "+
			"run against itself and the score drifts silently", sys.Name())
	}
	project, err := reporter.WriteTargetProject(ctx)
	if err != nil {
		return membench.CorpusRegimeUnknown, fmt.Errorf("refusing to run: could not establish which project to clear (%w)", err)
	}
	if strings.TrimSpace(project) == "" {
		return membench.CorpusRegimeUnknown, fmt.Errorf("refusing to run: the daemon reported an EMPTY project, which names " +
			"no store to clear. Treating empty as 'nothing to do' is how the absent clear " +
			"stayed invisible")
	}

	dsn := strings.TrimSpace(os.Getenv("VORNIK_BENCH_DSN"))
	if dsn == "" {
		return membench.CorpusRegimeUnknown, fmt.Errorf("refusing to run: VORNIK_BENCH_DSN is required so the run can clear " +
			"the benchmark store before it starts. Without the clear the store accumulates " +
			"across runs and the run dedups against itself (design §5.8)")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return membench.CorpusRegimeUnknown, fmt.Errorf("clear: open %s: %w", benchDatabase, err)
	}
	defer func() { _ = db.Close() }()

	res, err := membench.ClearBenchmarkStore(ctx, db, project, scopePrefix)
	if err != nil {
		return membench.CorpusRegimeUnknown, err
	}
	fmt.Printf("cleared benchmark store for project %q: %d chunks, %d entities, %d edges, %d mentions\n",
		project, res.Chunks, res.Entities, res.Edges, res.Mentions)
	// COLD is an OBSERVATION, and this is where it is made: the clear ran to
	// completion against the verified project, so the corpus this run scores is
	// exactly what this run ingests. Returning it — rather than having the caller
	// assume cold because it called a function named clear — is what keeps
	// ComparabilityFields.CorpusRegime observed rather than declared.
	return membench.CorpusRegimeCold, nil
}
