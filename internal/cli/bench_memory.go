package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

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
const harnessVersion = "4"

func runBenchMemory(cmd *cobra.Command, _ []string) error {
	// The destructive-run guard comes FIRST, before any dataset is read or any
	// model is contacted. A run that is going to be refused should be refused
	// before it costs anything.
	if err := membench.CheckDestructiveTarget(benchDatabase, benchConfirmWipe); err != nil {
		return err
	}

	// "No default" has to mean refusal, or it silently becomes a default nobody
	// chose (design §5.6).
	if benchProfile == "" && (benchAnswerModel == "" || benchJudgeModel == "") {
		return fmt.Errorf("model selection required: pass --profile (local|judged|cloud) " +
			"or set both --answer-model and --judge-model explicitly")
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

	runDir := benchRunDir
	if runDir == "" {
		runDir = filepath.Join("bench-runs", time.Now().UTC().Format("20060102-150405"))
	}
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return fmt.Errorf("create run dir: %w", err)
	}

	llm, err := newBenchLLM()
	if err != nil {
		return err
	}
	// The judge may run on a different model from the answerer — that split IS
	// the "judged" profile, and folding them into one client would make it
	// unexpressible.
	judgeLLM := llm.withModel(benchJudgeModel)

	runner := &membench.Runner{
		System:            sys,
		Dataset:           ds,
		Generator:         membench.NewAnswerGenerator(llm),
		Judge:             membench.NewJudge(judgeLLM),
		RunDir:            runDir,
		MaxTokens:         benchMaxTokens,
		Limits:            membench.Limits{MaxItems: benchMaxItems, MaxItemsPerCategory: benchPerCategory, Category: benchCategory},
		Resume:            benchResume,
		DegradedThreshold: benchDegradedRate,
		HarnessVersion:    harnessVersion,
		AnswerModel:       benchAnswerModel,
		JudgeModel:        benchJudgeModel,
		DatasetPath:       path,
		DatasetSHA256:     actualHash,
		SingleSystem:      benchSystem != "external",
		RecallMethod:      benchRecallMethod,
		DeclaredEmbedder:  benchExtractionModel,
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
