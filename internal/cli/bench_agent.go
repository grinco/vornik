package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/agentbench"
	"vornik.io/vornik/internal/membench"
)

// `vornikctl bench agent` — the agent-quality harness CLI
// (https://docs.vornik.io §6.2).
//
// Sibling to `bench memory`, which scores RETRIEVAL. This one scores the
// decisions our control logic makes: what the lead granted, whether roles
// followed their output schemas, and whether agents called tools correctly.
//
// L1 (prompt composition) has no command here on purpose: it is a pure function
// of config and binary, needs no daemon and no database, and runs as `go test`
// so it can gate every commit.

var (
	benchAgentProject      string
	benchAgentBenchProject string
	benchAgentSwarm        string
	benchAgentDatabase     string
	benchAgentConfirmWipe  string
	benchAgentRuns         int
	benchAgentTaskSetHash  string
	benchAgentGoldPath     string
	benchAgentPreRegPath   string
	benchAgentTaskSetPath  string
	benchAgentJournalPath  string
	benchAgentRunID        string
	benchAgentArm          string
	benchAgentRepeats      int
	benchAgentContextPol   string
	benchAgentDaemonBinary string
	benchAgentDaemonConfig string
	benchAgentJSON         bool
)

var benchAgentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Score agent decision quality: tool grants, schema following, tool use",
	Long: "Score the decisions Vornik's control logic makes.\n\n" +
		"Three probes, reported as vectors and never blended into one score:\n" +
		"  tool-grant       did the lead grant what the task demonstrably needed\n" +
		"  schema-following did each role produce output matching its schema\n" +
		"  tool-use         did the agent call real tools with valid arguments\n\n" +
		"schema-following and tool-use need no gold set — their ground truth is\n" +
		"configuration, not a recording — so they can gate before any gold pass.",
}

var benchAgentGoldCmd = &cobra.Command{
	Use:   "gold",
	Short: "Record ground truth from an unrestricted-ceiling arm",
	Long: "Record ground truth by running the task set with no grant ceiling and\n" +
		"keeping the tools each PASSING run actually invoked.\n\n" +
		"Refuses when the task set is unchanged: regenerating against the same set\n" +
		"replaces the ground truth the gate measures against, which makes prior\n" +
		"numbers incomparable and future ones unfalsifiable. To rebuild anyway,\n" +
		"delete the pinned manifest — that is a reviewable diff.",
	RunE: runBenchAgentGold,
}

var benchAgentRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Run one arm and journal its probe verdicts",
	RunE:  runBenchAgentRun,
}

var benchAgentReportCmd = &cobra.Command{
	Use:   "report <journal>",
	Short: "Print a run's scoreboard",
	Args:  cobra.ExactArgs(1),
	RunE:  runBenchAgentReport,
}

var benchAgentRollupCmd = &cobra.Command{
	Use:   "rollup <journal>...",
	Short: "Print the customer figures: cost, efficiency, accuracy, success rate",
	Long: "Print the customer figures for one run, or for several batches of one run.\n\n" +
		"Several journals are merged before rolling up, and merging REFUSES arms\n" +
		"that disagree — a batched pass is one experiment or it is not a pass.",
	Args: cobra.MinimumNArgs(1),
	RunE: runBenchAgentRollup,
}

var benchAgentRescoreCmd = &cobra.Command{
	Use:   "rescore <journal>",
	Short: "Re-score a completed journal under the current contract, without re-running",
	Long: "Re-score a completed journal under the CURRENT scoring contract.\n\n" +
		"Bumping HarnessVersion correctly makes every earlier figure incomparable,\n" +
		"and until this existed the only way back was to re-run the pass — hours of\n" +
		"wall clock and, on a prepaid allowance, days of waiting for a quota reset.\n" +
		"The evidence a probe reads is already in the ledger, so this re-scores the\n" +
		"evidence instead of re-buying it. No agent runs.\n\n" +
		"Cost, tokens, success and every arm axis describing the RUN are preserved:\n" +
		"re-scoring changes what a number MEANS, not what happened.",
	Args: cobra.ExactArgs(1),
	RunE: runBenchAgentRescore,
}

var benchAgentGoldMergeCmd = &cobra.Command{
	Use:   "gold-merge <batch.json>...",
	Short: "Combine per-batch gold manifests into one pinned set",
	Long: "Combine per-batch gold manifests into one.\n\n" +
		"A full gold pass is hours long; losing it to a dropped session means\n" +
		"re-spending all of it. Running in batches caps that loss to one batch —\n" +
		"this is what makes the partials usable afterwards.\n\n" +
		"Paths accumulate across batches. An exclusion survives only if NO batch\n" +
		"recorded a path, because a task that passed once was measurable.",
	Args: cobra.MinimumNArgs(2),
	RunE: runBenchAgentGoldMerge,
}

var benchAgentTaskSetHashCmd = &cobra.Command{
	Use:   "taskset-hash <tasks.json>",
	Short: "Print a task set's digest, which the gold fence compares against",
	Long: "Print a task set's digest.\n\n" +
		"This is what --task-set-hash takes and what the regeneration fence compares\n" +
		"against, so it must be computed the same way the harness computes it —\n" +
		"order-independent and length-prefixed, not a sha256 of the file.",
	Args: cobra.ExactArgs(1),
	RunE: runBenchAgentTaskSetHash,
}

var benchAgentCompareCmd = &cobra.Command{
	Use:   "compare <journal-a> <journal-b>",
	Short: "Diff two runs, refusing incomparable arms",
	Args:  cobra.ExactArgs(2),
	RunE:  runBenchAgentCompare,
}

func init() {
	for _, c := range []*cobra.Command{benchAgentGoldCmd, benchAgentRunCmd} {
		c.Flags().StringVar(&benchAgentProject, "project", "", "project to run in")
		c.Flags().StringVar(&benchAgentBenchProject, "benchmark-project", "",
			"the only project this deployment permits benchmarking in")
		c.Flags().StringVar(&benchAgentSwarm, "swarm", "", "swarm whose roles execute the tasks")
		c.Flags().StringVar(&benchAgentDatabase, "database", "", "target database")
		c.Flags().StringVar(&benchAgentConfirmWipe, "i-know-this-wipes", "",
			"must equal --database; this run bulk-writes and clears it")
	}
	benchAgentGoldCmd.Flags().IntVar(&benchAgentRuns, "runs", 3,
		"unrestricted-ceiling runs per task; a task no run passes is excluded")
	benchAgentGoldCmd.Flags().StringVar(&benchAgentTaskSetHash, "task-set-hash", "",
		"digest of the task set being recorded; the regeneration fence compares against it")
	benchAgentGoldCmd.Flags().StringVar(&benchAgentGoldPath, "gold", "gold.json",
		"pinned gold manifest to write, and to fence against if it exists")

	benchAgentRunCmd.Flags().StringVar(&benchAgentGoldPath, "gold", "",
		"pinned gold manifest; omit to run only the probes that need none")
	benchAgentRunCmd.Flags().StringVar(&benchAgentPreRegPath, "preregistration", "",
		"REQUIRED: committed manifest stating the arms, metric, intended delta and computed n")
	benchAgentRunCmd.Flags().StringVar(&benchAgentJournalPath, "journal", "journal.json",
		"where to write the run journal")
	benchAgentRunCmd.Flags().StringVar(&benchAgentRunID, "run-id", "", "identifier for this run")
	benchAgentRunCmd.Flags().StringVar(&benchAgentArm, "arm", "", "name of the arm being run")
	benchAgentRunCmd.Flags().IntVar(&benchAgentRepeats, "repeats", 1,
		"runs per task; repeats shrink a task's contribution to sigma_d but add no pairs")
	benchAgentRunCmd.Flags().StringVar(&benchAgentContextPol, "context-policy", "",
		"REQUIRED: names the policy under test (suppression set, advert gating, ceiling). "+
			"It is the independent variable, so a run that does not name it cannot be compared")
	benchAgentRunCmd.Flags().StringVar(&benchAgentDaemonBinary, "daemon-binary", "",
		"path to the daemon binary under test; hashed into the arm key so a release change "+
			"refuses comparison instead of silently producing one")
	benchAgentRunCmd.Flags().StringVar(&benchAgentDaemonConfig, "daemon-config", "",
		"path to the config the daemon reads; hashed into the arm key")
	for _, c := range []*cobra.Command{benchAgentGoldCmd, benchAgentRunCmd} {
		c.Flags().StringVar(&benchAgentTaskSetPath, "tasks", "", "JSON task set to run")
	}

	for _, c := range []*cobra.Command{benchAgentReportCmd, benchAgentRollupCmd, benchAgentCompareCmd} {
		c.Flags().BoolVar(&benchAgentJSON, "json", false, "emit JSON instead of a table")
	}

	benchAgentCmd.AddCommand(benchAgentGoldCmd, benchAgentRunCmd, benchAgentReportCmd,
		benchAgentRollupCmd, benchAgentCompareCmd, benchAgentTaskSetHashCmd,
		benchAgentGoldMergeCmd, benchAgentRescoreCmd)
	benchAgentRescoreCmd.Flags().StringVar(&benchAgentJournalPath, "out", "",
		"where to write the re-scored journal (required)")
	benchAgentRescoreCmd.Flags().StringVar(&benchAgentGoldPath, "gold", "",
		"gold manifest the grant probe scores against")
	benchAgentRescoreCmd.Flags().StringVar(&benchAgentDatabase, "database", "",
		"the benchmark database to READ traces from")
	benchAgentGoldMergeCmd.Flags().StringVar(&benchAgentGoldPath, "out", "gold.json",
		"where to write the merged manifest")
	benchCmd.AddCommand(benchAgentCmd)
}

// checkAgentRunScope is the FIRST statement in every path that touches the
// daemon. Order is the point: an operator who is shown an error and fixes it
// must not be walked toward the wipe by a sequence of lesser complaints.
func checkAgentRunScope() error {
	return membench.CheckRunScope(agentRunScope())
}

func runBenchAgentGold(cmd *cobra.Command, _ []string) error {
	if err := checkAgentRunScope(); err != nil {
		return err
	}
	if benchAgentTaskSetHash == "" {
		return fmt.Errorf("--task-set-hash is required: without it the regeneration fence " +
			"has nothing to compare against, so gold could be silently rebuilt against a " +
			"different task set")
	}

	pinned, err := loadGoldIfPresent(benchAgentGoldPath)
	if err != nil {
		return err
	}
	if err := agentbench.CheckRegeneration(pinned, benchAgentTaskSetHash); err != nil {
		return err
	}

	tasks, err := loadTaskSet(benchAgentTaskSetPath)
	if err != nil {
		return err
	}
	runner, store, closeDB, err := buildRunnerParts()
	if err != nil {
		return err
	}
	defer closeDB()
	if err := verifyDaemonTarget(cmd.Context(), runner); err != nil {
		return err
	}

	// The unrestricted-ceiling arm: no gold, and the grant probe deliberately
	// absent — this pass RECORDS what an unconstrained agent needed, so scoring
	// it against a ground truth that does not exist yet would be circular.
	r := &agentbench.Runner{Tasks: runner, Traces: store}
	var observed []agentbench.UnrestrictedRun
	for _, spec := range agentbench.SortTasks(tasks) {
		for i := 0; i < benchAgentRuns; i++ {
			outcome, err := runner.Run(cmd.Context(), spec)
			if err != nil {
				return fmt.Errorf("unrestricted run of %q: %w", spec.ID, err)
			}
			invoked, err := collectInvoked(cmd.Context(), store, outcome.TaskID, outcome.Executions)
			if err != nil {
				return err
			}
			observed = append(observed, agentbench.UnrestrictedRun{
				TaskID: spec.ID, Passed: outcome.Succeeded, Invoked: invoked,
				ErrorText: outcome.ErrorText,
			})
		}
	}
	_ = r

	manifest, err := agentbench.BuildGold(benchAgentTaskSetHash, observed, benchAgentRuns)
	if err != nil {
		return err
	}
	blob, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal gold manifest: %w", err)
	}
	if err := os.WriteFile(benchAgentGoldPath, append(blob, '\n'), 0o600); err != nil {
		return fmt.Errorf("write gold manifest: %w", err)
	}

	excluded := 0
	for _, e := range manifest.Entries {
		if e.Excluded {
			excluded++
		}
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"wrote %s: %d task(s), %d excluded.\nOPERATOR REVIEW REQUIRED before this gates "+
			"anything: gold defines what \"correct\" means, so it is not self-certifiable "+
			"by the harness that produced it.\n",
		benchAgentGoldPath, len(manifest.Entries), excluded)
	return nil
}

// collectInvoked gathers the tools an unrestricted execution actually used.
//
// Resolves executions from the LEDGER when the submitter cannot name them, which
// is always: the companion status payload carries no execution ids. The first
// version passed the submitter's empty list straight through, so every gold entry
// recorded zero tools and every task was excluded as "invoked no tools" — a gold
// set that looked deliberate and was empty. Same defect as the runner's, fixed
// there first and missed here.
func collectInvoked(ctx context.Context, store agentbench.TraceStore, taskID string, executions []string) ([]string, error) {
	if len(executions) == 0 {
		found, err := store.Executions(ctx, taskID)
		if err != nil {
			return nil, fmt.Errorf("list executions for %q: %w", taskID, err)
		}
		executions = found
	}
	if len(executions) == 0 {
		return nil, fmt.Errorf("task %q produced no executions, so it has no ground truth "+
			"to record", taskID)
	}
	var invoked []string
	for _, execID := range executions {
		_, traces, err := store.Assemble(ctx, taskID, execID)
		if err != nil {
			return nil, fmt.Errorf("assemble %s: %w", execID, err)
		}
		for _, tr := range traces {
			invoked = append(invoked, tr.Invoked...)
		}
	}
	return invoked, nil
}

func runBenchAgentRun(cmd *cobra.Command, _ []string) error {
	if err := checkAgentRunScope(); err != nil {
		return err
	}
	preReg, err := requirePreRegistration()
	if err != nil {
		return err
	}
	if benchAgentGoldPath != "" {
		if _, err := loadGoldIfPresent(benchAgentGoldPath); err != nil {
			return err
		}
	}

	gold, err := loadGoldIfPresent(benchAgentGoldPath)
	if err != nil {
		return err
	}
	tasks, err := loadTaskSet(benchAgentTaskSetPath)
	if err != nil {
		return err
	}
	daemon, store, closeDB, err := buildRunnerParts()
	if err != nil {
		return err
	}
	defer closeDB()
	if err := verifyDaemonTarget(cmd.Context(), daemon); err != nil {
		return err
	}

	power, err := agentbench.CheckPower(preReg.SigmaD, preReg.SigmaN, preReg.TargetDelta, len(tasks))
	if err != nil {
		return err
	}

	arm, err := buildArm(tasks, gold)
	if err != nil {
		return err
	}

	r := &agentbench.Runner{
		Tasks:  daemon,
		Traces: store,
		// The gold-free probes always run. The grant probe is added only when a
		// gold set exists, so a run without one still measures schema following
		// and tool use rather than producing nothing.
		Probes: probeSet(gold != nil),
	}
	journal, err := r.Run(cmd.Context(), agentbench.RunConfig{
		RunID:           benchAgentRunID,
		Arm:             arm,
		Scope:           agentRunScope(),
		PreRegistration: preReg,
		Power:           power,
		Tasks:           agentbench.SortTasks(tasks),
		Gold:            gold,
		Repeats:         benchAgentRepeats,
	})
	if err != nil {
		return err
	}

	stampObservedModels(cmd.Context(), store, &journal)

	f, err := os.Create(filepath.Clean(benchAgentJournalPath))
	if err != nil {
		return fmt.Errorf("create journal: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := journal.Write(f); err != nil {
		return err
	}

	reportRunWritten(cmd, journal)
	return nil
}

// reportRunWritten prints the warning BEFORE the confirmation, so a degraded
// run's journal path is never read without the reason it is degraded.
func reportRunWritten(cmd *cobra.Command, journal agentbench.Journal) {
	if err := journal.CheckReadable(); err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "WARNING: %v\n", err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "wrote %s: %d execution(s)\n",
		benchAgentJournalPath, len(journal.Records))
}

// requirePreRegistration loads and validates the committed pre-registration.
//
// Required, not encouraged: choosing what to compare after seeing results is the
// line between a benchmark and a press release, so the comparison is committed
// before the run and its hash is journaled beside every figure.
func requirePreRegistration() (agentbench.PreRegistration, error) {
	if benchAgentPreRegPath == "" {
		return agentbench.PreRegistration{}, fmt.Errorf("--preregistration is required. " +
			"Choosing what to compare after seeing results is the line between a benchmark " +
			"and a press release, so the comparison is committed before the run and its hash " +
			"is journaled beside every figure")
	}
	preReg, err := loadPreRegistration(benchAgentPreRegPath)
	if err != nil {
		return agentbench.PreRegistration{}, err
	}
	if err := preReg.Validate(); err != nil {
		return agentbench.PreRegistration{}, err
	}
	return preReg, nil
}

// stampObservedModels fills the arm's model map from the LEDGER after the run.
//
// Observed, not declared: a router fallback can serve a different model on a
// different provider without anything declaring the arm changed. This deployment
// did exactly that — glm-5.2 (Ollama) silently became zai.glm-5 (Bedrock) for
// 473k tokens — so two runs would key identically having measured different
// systems.
//
// Best-effort: a store that cannot answer leaves the models empty, which keeps
// the key PARTIAL rather than asserting a model set nobody verified.
func stampObservedModels(ctx context.Context, store *agentbench.SQLTraceStore, journal *agentbench.Journal) {
	var execIDs []string
	for _, rec := range journal.Records {
		if rec.ExecutionID != "" {
			execIDs = append(execIDs, rec.ExecutionID)
		}
	}
	observed, err := store.ObservedModels(ctx, execIDs)
	if err != nil || len(observed) == 0 {
		return
	}
	journal.Manifest.Arm.Models = observed
	journal.Manifest.ArmKey = journal.Manifest.Arm.Key()
	journal.Manifest.ArmPartial = journal.Manifest.Arm.Partial()
}

// buildArm assembles the comparability key from everything knowable BEFORE the
// run. Models are filled in afterwards from the ledger (see runBenchAgentRun).
//
// WHY THIS MATTERS FOR RELEASES. The key is what makes two runs comparable or
// refuses them. Until 2026-08-14 the CLI populated only the arm's NAME and the
// harness version, so every run reported PARTIAL and the key refused nothing —
// the mechanism was fully tested and fed nothing. Release-over-release
// comparison needs the binary, the config, the task set, the gold set, the probe
// set and the policy, or "apples to apples" is an assertion rather than a check.
func buildArm(tasks []agentbench.TaskSpec, gold *agentbench.GoldManifest) (agentbench.ArmFields, error) {
	if strings.TrimSpace(benchAgentContextPol) == "" {
		return agentbench.ArmFields{}, fmt.Errorf("--context-policy is required: it names the " +
			"independent variable, and a run that does not say what policy it tested cannot be " +
			"compared with one that does")
	}

	ids := make([]string, 0, len(tasks))
	bodies := make(map[string]string, len(tasks))
	for _, t := range tasks {
		ids = append(ids, t.ID)
		bodies[t.ID] = t.Workflow + "\x00" + t.Prompt
	}

	arm := agentbench.ArmFields{
		HarnessVersion: agentbench.HarnessVersion,
		Name:           benchAgentArm,
		ContextPolicy:  benchAgentContextPol,
		TaskSetSHA256:  agentbench.TaskSetDigest(ids, bodies),
		Probes:         probeNames(gold != nil),
	}
	if gold != nil {
		h, err := gold.SHA256()
		if err != nil {
			return agentbench.ArmFields{}, err
		}
		arm.GoldSHA256 = h
	}
	if benchAgentDaemonBinary != "" {
		h, err := sha256File(benchAgentDaemonBinary)
		if err != nil {
			return agentbench.ArmFields{}, fmt.Errorf("hash daemon binary: %w", err)
		}
		arm.BinarySHA256 = h
	}
	if benchAgentDaemonConfig != "" {
		h, err := sha256File(benchAgentDaemonConfig)
		if err != nil {
			return agentbench.ArmFields{}, fmt.Errorf("hash daemon config: %w", err)
		}
		arm.ConfigSHA256 = h
	}
	return arm, nil
}

// probeNames lists the probes a run will use, sorted by the arm key itself.
func probeNames(haveGold bool) []string {
	names := make([]string, 0, 3)
	for _, p := range probeSet(haveGold) {
		names = append(names, p.Name())
	}
	return names
}

// probeSet returns the probes for a run. The two whose ground truth is
// configuration always run; the grant probe needs a recording.
func probeSet(haveGold bool) []agentbench.Probe {
	probes := []agentbench.Probe{agentbench.SchemaProbe{}, agentbench.ToolUseProbe{}}
	if haveGold {
		probes = append(probes, agentbench.GrantProbe{})
	}
	return probes
}

// verifyDaemonTarget refuses unless the DAEMON confirms it writes the database
// the operator named. Fails closed: a daemon that cannot answer is refused, not
// waved through, because "unverified" is not "safe" and treating it as safe is
// what let a benchmark write production on 2026-08-12.
func verifyDaemonTarget(ctx context.Context, daemon *agentbench.DaemonTaskRunner) error {
	return membench.VerifyWriteTargetOf(ctx, "vornik daemon", daemon, benchAgentDatabase)
}

// buildRunnerParts constructs the daemon submitter and the ledger store.
func buildRunnerParts() (*agentbench.DaemonTaskRunner, *agentbench.SQLTraceStore, func(), error) {
	url, token := daemonURLAndToken()
	if url == "" || token == "" {
		return nil, nil, nil, fmt.Errorf("VORNIK_URL and VORNIK_COMPANION_TOKEN must be set")
	}
	daemon := agentbench.NewDaemonTaskRunner(agentbench.DaemonConfig{
		BaseURL: url, Token: token, Project: benchAgentProject,
	})

	db, err := openBenchDB()
	if err != nil {
		return nil, nil, nil, err
	}
	return daemon, &agentbench.SQLTraceStore{DB: db, Dialect: agentbench.Postgres},
		func() { _ = db.Close() }, nil
}

// openBenchDB opens the ledger the run will READ. The guard has already
// confirmed this is the benchmark database, and the store's interface is
// read-only, so a harness bug cannot mutate the deployment it measures.
func openBenchDB() (*sql.DB, error) {
	dsn := os.Getenv("VORNIK_BENCH_DSN")
	if dsn == "" {
		return nil, fmt.Errorf("VORNIK_BENCH_DSN must name the database the daemon writes " +
			"— the same one --database and --i-know-this-wipes authorised")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open benchmark database: %w", err)
	}
	return db, nil
}

// loadTaskSet reads the benchmark task set.
func loadTaskSet(path string) ([]agentbench.TaskSpec, error) {
	if path == "" {
		return nil, fmt.Errorf("--tasks is required: the task set is what the run measures")
	}
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("read task set: %w", err)
	}
	var tasks []agentbench.TaskSpec
	if err := json.Unmarshal(data, &tasks); err != nil {
		return nil, fmt.Errorf("parse task set: %w", err)
	}
	if len(tasks) == 0 {
		return nil, fmt.Errorf("task set %s is empty", path)
	}
	return tasks, nil
}

func agentRunScope() membench.RunScope {
	return membench.RunScope{
		Database:         benchAgentDatabase,
		Confirmation:     benchAgentConfirmWipe,
		ProjectID:        benchAgentProject,
		BenchmarkProject: benchAgentBenchProject,
		SwarmID:          benchAgentSwarm,
	}
}

func runBenchAgentReport(cmd *cobra.Command, args []string) error {
	j, err := loadJournal(args[0])
	if err != nil {
		return err
	}

	// The warning prints BEFORE the table, always: a degraded run's figures must
	// not be readable without it.
	if err := j.CheckReadable(); err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "WARNING: %v\n\n", err)
	}
	if benchAgentJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(j.Manifest)
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "run\t%s\n", j.Manifest.RunID)
	_, _ = fmt.Fprintf(w, "arm\t%s\n", j.Manifest.Arm.Name)
	_, _ = fmt.Fprintf(w, "arm key\t%s\n", shortKey(j.Manifest.ArmKey))
	if j.Manifest.ArmPartial {
		_, _ = fmt.Fprintf(w, "\tPARTIAL — comparability unverified\n")
	}
	_, _ = fmt.Fprintf(w, "pre-registration\t%s\n", shortKey(j.Manifest.PreRegistrationHash))
	_, _ = fmt.Fprintf(w, "executions\t%d\n", len(j.Records))
	p := j.Manifest.Power
	if p.SigmaD > 0 {
		_, _ = fmt.Fprintf(w, "sigma_d\t%.4f (n=%d)\n", p.SigmaD, p.SigmaN)
		_, _ = fmt.Fprintf(w, "resolvable delta\t%.4f at %d pairs\n", p.ResolvableDelta, p.AvailablePairs)
	}
	return w.Flush()
}

// loadMergedJournal reads one or more journal batches as a single experiment.
//
// Merging enforces that the batches ARE one experiment — it refuses arms that
// disagree. Rolling each up separately and eyeballing the numbers would not.
func loadMergedJournal(cmd *cobra.Command, paths []string) (agentbench.Journal, error) {
	journals := make([]agentbench.Journal, 0, len(paths))
	for _, path := range paths {
		loaded, err := loadJournal(path)
		if err != nil {
			return agentbench.Journal{}, err
		}
		journals = append(journals, loaded)
	}
	merged, err := agentbench.MergeJournals(journals...)
	if err != nil {
		return agentbench.Journal{}, err
	}
	if len(journals) > 1 {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "merged %d journal(s), %d execution record(s)\n\n",
			len(journals), len(merged.Records))
	}
	return merged, nil
}

func runBenchAgentRollup(cmd *cobra.Command, args []string) error {
	j, err := loadMergedJournal(cmd, args)
	if err != nil {
		return err
	}
	if err := j.CheckReadable(); err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "WARNING: %v\n\n", err)
	}

	r := j.Rollup()
	if benchAgentJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(r)
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "arm\t%s\n", r.Arm)
	_, _ = fmt.Fprintf(w, "attempted\t%d\n", r.Attempted)

	// Cost: all three figures together. $/attempt and the success rate are what
	// make $/success readable, and $/success alone invites the "cost of our
	// successes" misreading it is designed to avoid.
	_, _ = fmt.Fprintf(w, "\nCOST\t\n")
	_, _ = fmt.Fprintf(w, "total\t$%.4f\n", r.TotalCostUSD)
	_, _ = fmt.Fprintf(w, "per attempt\t$%.4f\n", r.CostPerAttemptUSD)
	if r.CostPerSuccessDefined {
		_, _ = fmt.Fprintf(w, "per SUCCESS\t$%.4f (failed-run spend included)\n", r.CostPerSuccessUSD)
	} else {
		_, _ = fmt.Fprintf(w, "per SUCCESS\tundefined (no successes)\n")
	}

	_, _ = fmt.Fprintf(w, "\nSUCCESS\t\n")
	if rate, ok := r.Failures.TaskSuccessRate(); ok {
		_, _ = fmt.Fprintf(w, "task success\t%.1f%%\n", rate*100)
	} else {
		_, _ = fmt.Fprintf(w, "task success\tundefined\n")
	}
	for _, class := range []agentbench.FailureClass{
		agentbench.FailureTask, agentbench.FailureContextOverflow,
		agentbench.FailureInfra, agentbench.FailureHarness,
	} {
		if n := r.Failures.ByClass[class]; n > 0 {
			_, _ = fmt.Fprintf(w, "  %s\t%d\n", class, n)
		}
	}

	_, _ = fmt.Fprintf(w, "\nEFFICIENCY\t\n")
	_, _ = fmt.Fprintf(w, "tokens/task\t%.0f\n", r.Efficiency.TokensPerTask)
	_, _ = fmt.Fprintf(w, "tool calls/task\t%.1f\n", r.Efficiency.ToolCallsPerTask)
	if r.Efficiency.GrantPrecisionDefined {
		_, _ = fmt.Fprintf(w, "grant precision\t%.3f\n", r.Efficiency.GrantPrecision)
	}
	_, _ = fmt.Fprintf(w, "escalations\t%d\n", r.Efficiency.Escalations)
	_, _ = fmt.Fprintf(w, "schema retries\t%d\n", r.Efficiency.SchemaRetries)

	_, _ = fmt.Fprintf(w, "\nACCURACY\t\n")
	if r.Accuracy.SchemaConformanceDefined {
		_, _ = fmt.Fprintf(w, "schema conformance\t%.3f\n", r.Accuracy.SchemaConformance)
	}
	if r.Accuracy.ToolCallValidityDefined {
		_, _ = fmt.Fprintf(w, "tool call validity\t%.3f\n", r.Accuracy.ToolCallValidity)
	}
	if r.Accuracy.UnknownToolCalls > 0 {
		_, _ = fmt.Fprintf(w, "  invented tool names\t%d\n", r.Accuracy.UnknownToolCalls)
	}
	if r.Accuracy.ArgumentErrors > 0 {
		_, _ = fmt.Fprintf(w, "  bad arguments\t%d\n", r.Accuracy.ArgumentErrors)
	}
	printGrantAccuracy(w, r)

	// Printed apart from EFFICIENCY, and labelled, because it improves when the
	// lead asks for less: rolled into an efficiency headline it would reward the
	// wrong behaviour.
	if r.RequestPrecisionDefined {
		_, _ = fmt.Fprintf(w, "\nDIAGNOSTIC (not an optimisation target)\t\n")
		_, _ = fmt.Fprintf(w, "request precision\t%.3f — read against escalations (%d)\n",
			r.RequestPrecision, r.Efficiency.Escalations)
	}
	return w.Flush()
}

// runBenchAgentGoldMerge combines per-batch manifests.
func runBenchAgentGoldMerge(cmd *cobra.Command, args []string) error {
	manifests := make([]agentbench.GoldManifest, 0, len(args))
	for _, path := range args {
		m, err := loadGoldIfPresent(path)
		if err != nil {
			return err
		}
		if m == nil {
			return fmt.Errorf("batch manifest not found: %s", path)
		}
		manifests = append(manifests, *m)
	}

	merged, err := agentbench.MergeGold(manifests...)
	if err != nil {
		return err
	}
	blob, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal merged manifest: %w", err)
	}
	if err := os.WriteFile(benchAgentGoldPath, append(blob, '\n'), 0o600); err != nil {
		return fmt.Errorf("write merged manifest: %w", err)
	}

	excluded := 0
	for _, e := range merged.Entries {
		if e.Excluded {
			excluded++
		}
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"merged %d batch(es) -> %s: %d task(s), %d excluded, %d run(s) total\n",
		len(manifests), benchAgentGoldPath, len(merged.Entries), excluded, merged.Runs)
	return nil
}

// runBenchAgentTaskSetHash prints the digest for a task set.
//
// A separate command rather than a flag because the hash is needed BEFORE the
// run that uses it, and because computing it by hand — sha256 of the file — would
// produce a value the fence rejects: the digest is order-independent and
// length-prefixed, so a reordered file must hash the same and a rename must not
// be able to compensate for an edit.
func runBenchAgentTaskSetHash(cmd *cobra.Command, args []string) error {
	tasks, err := loadTaskSet(args[0])
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(tasks))
	bodies := make(map[string]string, len(tasks))
	for _, t := range tasks {
		if _, dup := bodies[t.ID]; dup {
			return fmt.Errorf("task id %q appears twice: the digest would silently "+
				"cover only one of them", t.ID)
		}
		ids = append(ids, t.ID)
		// The prompt AND the workflow are the task: the same prompt on a
		// different workflow is a different experiment.
		bodies[t.ID] = t.Workflow + "\x00" + t.Prompt
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s  %d task(s)\n",
		agentbench.TaskSetDigest(ids, bodies), len(tasks))
	return nil
}

func runBenchAgentCompare(cmd *cobra.Command, args []string) error {
	a, err := loadJournal(args[0])
	if err != nil {
		return err
	}
	b, err := loadJournal(args[1])
	if err != nil {
		return err
	}

	ra, rb := a.Rollup(), b.Rollup()
	observed := absDelta(ra.Accuracy.PathCoverage, rb.Accuracy.PathCoverage)
	verdict, err := agentbench.CompareJournals(a, b, observed)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s vs %s\npath coverage %s\n",
		ra.Arm, rb.Arm, verdict)
	return nil
}

func absDelta(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}

func shortKey(k string) string {
	if len(k) > 12 {
		return k[:12]
	}
	if k == "" {
		return "(none)"
	}
	return k
}

func loadJournal(path string) (agentbench.Journal, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return agentbench.Journal{}, fmt.Errorf("open journal: %w", err)
	}
	defer func() { _ = f.Close() }()
	return agentbench.ReadJournal(f)
}

// loadGoldIfPresent returns nil when nothing is pinned yet, which is what makes
// the first build permitted and every later one fenced.
func loadGoldIfPresent(path string) (*agentbench.GoldManifest, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(filepath.Clean(path))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read gold manifest: %w", err)
	}
	var m agentbench.GoldManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse gold manifest: %w", err)
	}
	return &m, nil
}

func loadPreRegistration(path string) (agentbench.PreRegistration, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return agentbench.PreRegistration{}, fmt.Errorf("read pre-registration: %w", err)
	}
	var p agentbench.PreRegistration
	if err := json.Unmarshal(data, &p); err != nil {
		return agentbench.PreRegistration{}, fmt.Errorf("parse pre-registration: %w", err)
	}
	return p, nil
}

func runBenchAgentRescore(cmd *cobra.Command, args []string) error {
	if benchAgentJournalPath == "" {
		return fmt.Errorf("--out is required: re-scoring writes a NEW journal rather than " +
			"overwriting the original, so the figure a decision was made on stays readable")
	}
	journal, err := loadJournal(args[0])
	if err != nil {
		return err
	}
	gold, err := loadGoldIfPresent(benchAgentGoldPath)
	if err != nil {
		return err
	}

	db, err := openBenchDB()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	store := &agentbench.SQLTraceStore{DB: db, Dialect: agentbench.Postgres}

	rescored, err := agentbench.Rescore(cmd.Context(), journal, store, probeSet(gold != nil), gold)
	if err != nil {
		return err
	}

	blob, err := json.MarshalIndent(rescored, "", "  ")
	if err != nil {
		return fmt.Errorf("encode re-scored journal: %w", err)
	}
	if err := os.WriteFile(benchAgentJournalPath, append(blob, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", benchAgentJournalPath, err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"re-scored %d execution(s) from harness %s to %s -> %s\n",
		len(rescored.Records), journal.Manifest.Arm.HarnessVersion,
		agentbench.HarnessVersion, benchAgentJournalPath)
	return nil
}

// printGrantAccuracy writes the tool-grant half of the ACCURACY block.
//
// Extracted so the shell-coverage qualifier stays adjacent to the core-miss
// figure it qualifies: those two lines must be read together, and separating
// them in the code is the first step toward separating them on the page.
func printGrantAccuracy(w *tabwriter.Writer, r agentbench.Rollup) {
	if r.Accuracy.PathCoverageDefined {
		_, _ = fmt.Fprintf(w, "path coverage\t%.3f\n", r.Accuracy.PathCoverage)
	}
	if r.Accuracy.CoreMisses > 0 {
		_, _ = fmt.Fprintf(w, "  core misses\t%d\n", r.Accuracy.CoreMisses)
	}
	// Printed whenever ANY core requirement was met by a shell, including when
	// core misses are zero — that is exactly the case it exists to qualify. A
	// clean core-miss sheet earned through blanket shell grants is not a tight
	// policy, and the reader is pointed at the metric that says so.
	if r.Accuracy.CoreShellCovered > 0 {
		_, _ = fmt.Fprintf(w, "  core covered VIA SHELL\t%d of %d substitution(s) — read against grant precision (%.3f)\n",
			r.Accuracy.CoreShellCovered, r.Accuracy.CoreSubstituted, r.Efficiency.GrantPrecision)
	} else if r.Accuracy.CoreSubstituted > 0 {
		_, _ = fmt.Fprintf(w, "  core covered by an equivalent\t%d\n", r.Accuracy.CoreSubstituted)
	}
}
