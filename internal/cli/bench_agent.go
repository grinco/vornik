package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	Use:   "rollup <journal>",
	Short: "Print the customer figures: cost, efficiency, accuracy, success rate",
	Args:  cobra.ExactArgs(1),
	RunE:  runBenchAgentRollup,
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
	for _, c := range []*cobra.Command{benchAgentGoldCmd, benchAgentRunCmd} {
		c.Flags().StringVar(&benchAgentTaskSetPath, "tasks", "", "JSON task set to run")
	}

	for _, c := range []*cobra.Command{benchAgentReportCmd, benchAgentRollupCmd, benchAgentCompareCmd} {
		c.Flags().BoolVar(&benchAgentJSON, "json", false, "emit JSON instead of a table")
	}

	benchAgentCmd.AddCommand(benchAgentGoldCmd, benchAgentRunCmd, benchAgentReportCmd,
		benchAgentRollupCmd, benchAgentCompareCmd, benchAgentTaskSetHashCmd)
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
	if benchAgentPreRegPath == "" {
		return fmt.Errorf("--preregistration is required. Choosing what to compare after " +
			"seeing results is the line between a benchmark and a press release, so the " +
			"comparison is committed before the run and its hash is journaled beside every " +
			"figure")
	}
	preReg, err := loadPreRegistration(benchAgentPreRegPath)
	if err != nil {
		return err
	}
	if err := preReg.Validate(); err != nil {
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
		Arm:             agentbench.ArmFields{Name: benchAgentArm, HarnessVersion: agentbench.HarnessVersion},
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

	f, err := os.Create(filepath.Clean(benchAgentJournalPath))
	if err != nil {
		return fmt.Errorf("create journal: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := journal.Write(f); err != nil {
		return err
	}

	if err := journal.CheckReadable(); err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "WARNING: %v\n", err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "wrote %s: %d execution(s)\n",
		benchAgentJournalPath, len(journal.Records))
	return nil
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

func runBenchAgentRollup(cmd *cobra.Command, args []string) error {
	j, err := loadJournal(args[0])
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
	if r.Accuracy.PathCoverageDefined {
		_, _ = fmt.Fprintf(w, "path coverage\t%.3f\n", r.Accuracy.PathCoverage)
	}
	if r.Accuracy.CoreMisses > 0 {
		_, _ = fmt.Fprintf(w, "  core misses\t%d\n", r.Accuracy.CoreMisses)
	}

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
