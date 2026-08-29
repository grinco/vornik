package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/agentbench"
	"vornik.io/vornik/internal/membench"
	"vornik.io/vornik/internal/quality"
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
	benchAgentProject            string
	benchAgentBenchProject       string
	benchAgentSwarm              string
	benchAgentDatabase           string
	benchAgentConfirmWipe        string
	benchAgentRuns               int
	benchAgentTaskSetHash        string
	benchAgentGoldPath           string
	benchAgentPreRegPath         string
	benchAgentTaskSetPath        string
	benchAgentTaskSetFull        string
	benchAgentJournalPath        string
	benchAgentRunID              string
	benchAgentArm                string
	benchAgentRepeats            int
	benchAgentContextPol         string
	benchAgentDaemonBinary       string
	benchAgentDaemonConfig       string
	benchAgentJSON               bool
	benchAgentCalibrationPath    string
	benchAgentNoiseFloorPath     string
	benchAgentGatePolicyPath     string
	benchAgentCalibrationOutPath string
	benchAgentNoiseFloorOutPath  string
	benchAgentReleaseArms        []string
	benchAgentReleaseRationale   string
	benchAgentReleasePreRegOut   string

	// benchAgentAllowUnreproducible accepts a harness whose build cannot be
	// traced to a commit. Named rather than inferred: see
	// checkScoringProvenance.
	benchAgentAllowUnreproducible bool
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

var benchAgentCalibrateCmd = &cobra.Command{
	Use:   "calibrate <journal>",
	Short: "Build a task calibration artifact from a repeated journal",
	Args:  cobra.ExactArgs(1),
	RunE:  runBenchAgentCalibrate,
}

var benchAgentNoiseFloorCmd = &cobra.Command{
	Use:   "noise-floor <same-config-journal-a> <same-config-journal-b>",
	Short: "Measure paired release-gate noise from two same-config arms",
	Args:  cobra.ExactArgs(2),
	RunE:  runBenchAgentNoiseFloor,
}

var benchAgentGateCmd = &cobra.Command{
	Use:   "gate <baseline-journal> <candidate-journal>",
	Short: "Evaluate the pre-registered agent benchmark release gate",
	Args:  cobra.ExactArgs(2),
	RunE:  runBenchAgentGate,
}

var benchAgentReleasePreRegCmd = &cobra.Command{
	Use:   "release-preregistration",
	Short: "Derive the shared release pre-registration from reviewed gate artifacts",
	RunE:  runBenchAgentReleasePreRegistration,
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
	benchAgentRunCmd.Flags().StringVar(&benchAgentTaskSetFull, "task-set-full", "",
		"the WHOLE task set this run is one batch of. The arm's task-derived axes "+
			"describe this file rather than --tasks, so batches of one arm merge instead "+
			"of being refused as disagreeing arms. Takes a FILE, never a digest: a hash "+
			"typed by hand can be wrong in a way nothing detects, a file cannot")
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
	benchAgentRunCmd.Flags().BoolVar(&benchAgentAllowUnreproducible, "i-know-this-is-unreproducible", false,
		"score with a dirty or unstamped harness. The resulting figures cannot be "+
			"regenerated from any commit and must not be published")
	benchAgentRunCmd.Flags().StringVar(&benchAgentCalibrationPath, "calibration", "",
		"release calibration artifact pinned by the pre-registration")
	benchAgentRunCmd.Flags().StringVar(&benchAgentNoiseFloorPath, "noise-floor", "",
		"release noise-floor artifact pinned by the pre-registration")
	benchAgentRunCmd.Flags().StringVar(&benchAgentGatePolicyPath, "gate-policy", "",
		"release gate policy pinned by the pre-registration")
	for _, c := range []*cobra.Command{benchAgentGoldCmd, benchAgentRunCmd} {
		c.Flags().StringVar(&benchAgentTaskSetPath, "tasks", "", "JSON task set to run")
	}

	for _, c := range []*cobra.Command{benchAgentReportCmd, benchAgentRollupCmd, benchAgentCompareCmd} {
		c.Flags().BoolVar(&benchAgentJSON, "json", false, "emit JSON instead of a table")
	}

	benchAgentCmd.AddCommand(benchAgentGoldCmd, benchAgentRunCmd, benchAgentReportCmd,
		benchAgentRollupCmd, benchAgentCompareCmd, benchAgentTaskSetHashCmd,
		benchAgentGoldMergeCmd, benchAgentRescoreCmd, benchAgentCalibrateCmd,
		benchAgentNoiseFloorCmd, benchAgentGateCmd, benchAgentReleasePreRegCmd)
	benchAgentCalibrateCmd.Flags().StringVar(&benchAgentCalibrationOutPath, "out", "calibration.json",
		"where to write the immutable calibration artifact")
	benchAgentNoiseFloorCmd.Flags().StringVar(&benchAgentNoiseFloorOutPath, "out", "noise-floor.json",
		"where to write the immutable noise-floor artifact")
	for _, c := range []*cobra.Command{benchAgentGateCmd} {
		c.Flags().StringVar(&benchAgentCalibrationPath, "calibration", "", "calibration artifact (required)")
		c.Flags().StringVar(&benchAgentNoiseFloorPath, "noise-floor", "", "noise-floor artifact (required)")
		c.Flags().StringVar(&benchAgentGatePolicyPath, "policy", "", "release gate policy (required)")
		c.Flags().BoolVar(&benchAgentJSON, "json", false, "emit the complete decision as JSON")
	}
	benchAgentReleasePreRegCmd.Flags().StringVar(&benchAgentCalibrationPath, "calibration", "", "calibration artifact (required)")
	benchAgentReleasePreRegCmd.Flags().StringVar(&benchAgentNoiseFloorPath, "noise-floor", "", "noise-floor artifact (required)")
	benchAgentReleasePreRegCmd.Flags().StringVar(&benchAgentGatePolicyPath, "policy", "", "release gate policy (required)")
	benchAgentReleasePreRegCmd.Flags().StringSliceVar(&benchAgentReleaseArms, "arms", nil, "baseline and candidate arm names (required)")
	benchAgentReleasePreRegCmd.Flags().StringVar(&benchAgentReleaseRationale, "rationale", "", "why this release comparison is being run (required)")
	benchAgentReleasePreRegCmd.Flags().StringVar(&benchAgentReleasePreRegOut, "out", "release-preregistration.json", "where to write the shared pre-registration")
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
	if err := clearAgentBenchStore(cmd.Context()); err != nil {
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
	// When this run is one batch of a larger set, the arm's axes describe the
	// WHOLE set so the batches merge. See buildArmOver.
	var axisTasks []agentbench.TaskSpec
	if p := strings.TrimSpace(benchAgentTaskSetFull); p != "" {
		axisTasks, err = loadTaskSet(p)
		if err != nil {
			return fmt.Errorf("--task-set-full: %w", err)
		}
	}
	arm, err := buildArmOver(tasks, axisTasks, gold)
	if err != nil {
		return err
	}
	if err := validateReleaseRunInputs(preReg, arm, tasks); err != nil {
		return err
	}
	// Checked after the cheap declarative guards and before anything is built or
	// spent. Ordering is deliberate: a run with no pre-registration has a more
	// fundamental problem than an unreproducible scorer, and an operator sent
	// round a loop of refusals fixes the last one they were shown.
	if err := checkScoringProvenance(cmd); err != nil {
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
	if err := clearAgentBenchStore(cmd.Context()); err != nil {
		return err
	}
	if err := verifyModelWindow(cmd.Context(), cmd); err != nil {
		return err
	}

	availablePairs := len(tasks)
	if preReg.ReleaseGateEnabled() {
		availablePairs = 0
		for _, task := range tasks {
			if task.Tier == agentbench.TaskTierGate {
				availablePairs++
			}
		}
	}
	power, err := agentbench.CheckPower(preReg.SigmaD, preReg.SigmaN, preReg.TargetDelta, availablePairs)
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
		DaemonBuild:     agentbench.BinaryBuild(benchAgentDaemonBinary),
	})
	if err != nil {
		return err
	}

	stampObservedModels(cmd.Context(), store, &journal)
	stampObservedAgentImages(cmd.Context(), store, &journal)

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

// stampObservedAgentImages fills the arm from immutable runtime IDs persisted
// by the executor. A role using more than one ID makes the run untrustworthy:
// splitting after results would cherry-pick sub-arms, while selecting one would
// hide an agent-loop change.
func stampObservedAgentImages(ctx context.Context, store *agentbench.SQLTraceStore, journal *agentbench.Journal) {
	var execIDs []string
	for _, rec := range journal.Records {
		if rec.ExecutionID != "" {
			execIDs = append(execIDs, rec.ExecutionID)
		}
	}
	observed, err := store.ObservedAgentImages(ctx, execIDs)
	applyObservedAgentImages(journal, observed, err)
}

func applyObservedAgentImages(journal *agentbench.Journal, observed map[string]string, observationErr error) {
	if observationErr != nil || len(observed) == 0 {
		journal.Manifest.Untrustworthy = true
		if observationErr != nil {
			journal.Manifest.UntrustworthyReason = "AGENT_IMAGE_PROVENANCE_MISSING: " + observationErr.Error()
		} else {
			journal.Manifest.UntrustworthyReason = "AGENT_IMAGE_PROVENANCE_MISSING: no immutable runtime image IDs were journaled"
		}
		return
	}
	journal.Manifest.Arm.AgentImages = observed
	for role, ids := range observed {
		if strings.Contains(ids, "+sha256:") {
			journal.Manifest.Untrustworthy = true
			journal.Manifest.UntrustworthyReason = fmt.Sprintf(
				"AGENT_IMAGE_DRIFT: role %q used multiple immutable image IDs in one run (%s)", role, ids)
			break
		}
	}
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
// buildArmOver describes an arm that RAN `tasks` but whose task-derived axes
// describe `axisTasks` — the whole set — when one is given.
//
// This is what lets a long arm run in task batches and still merge. `bench
// agent rollup` refuses journals whose arms disagree, which is correct: the
// batches are one experiment or they are not a pass. Without this, every batch
// would describe only its own slice and the merge would be refused, which is
// why a 7-hour arm could not be resumed at all (design §12.13).
//
// ALL THREE task-derived axes are overridden together, not just the task-set
// digest. Scoring policy and tier policy are computed from the same slice, so
// overriding one and not the others would produce an arm that agrees on what it
// ran and disagrees on how it was scored — the subtler half of the same bug.
//
// A nil axisTasks means "describe what you ran", which is every existing
// caller and is unchanged.
func buildArmOver(tasks, axisTasks []agentbench.TaskSpec, gold *agentbench.GoldManifest) (agentbench.ArmFields, error) {
	if strings.TrimSpace(benchAgentContextPol) == "" {
		return agentbench.ArmFields{}, fmt.Errorf("--context-policy is required: it names the " +
			"independent variable, and a run that does not say what policy it tested cannot be " +
			"compared with one that does")
	}
	if err := agentbench.ValidateTaskTiers(tasks); err != nil {
		return agentbench.ArmFields{}, err
	}

	// The axes describe the whole set when batching, the run itself otherwise.
	axis := axisTasks
	if axis == nil {
		axis = tasks
	}

	ids := make([]string, 0, len(axis))
	bodies := make(map[string]string, len(axis))
	for _, t := range axis {
		ids = append(ids, t.ID)
		bodies[t.ID] = t.Workflow + "\x00" + t.Prompt
	}

	arm := agentbench.ArmFields{
		HarnessVersion:      agentbench.HarnessVersion,
		Name:                benchAgentArm,
		ContextPolicy:       benchAgentContextPol,
		TaskSetSHA256:       agentbench.TaskSetDigest(ids, bodies),
		ScoringPolicySHA256: agentbench.ScoringPolicyDigest(axis),
		TierPolicySHA256:    agentbench.TierPolicyDigest(axis),
		Probes:              probeNames(gold != nil),
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

// clearAgentBenchStore resets the benchmark project's memory before a pass.
//
// WHY. memory_search is alwaysGranted, so every graded agent can query project
// memory — correctly, since production agents can too. But this project's store
// is never reset, so it accumulates the completion write-ups of previous graded
// attempts at the SAME tasks. Measured on the 2026-08-21 gold pass: 865 chunks
// accumulated, 118 retrievals during the pass and 118 of them returning hits,
// and for dp-14-path-sandbox 24 of 70 retrieved chunks mentioned that task's
// own prior solution. Gold then records retrieval, not capability.
//
// Not new: retrievals-with-hits were 190/190 on 2026-08-14, the date of the
// published provisional arm.
//
// PROJECT-WIDE, not scope-prefixed: the contamination is this project's own
// memory whatever scope it was written under, so a repo_scope predicate would
// miss rows. Wholesale is right for a benchmark project and WRONG for
// production — erasing one subject must not drop a project's graph, which is
// tracked separately as a P0.
//
// Only after verifyDaemonTarget: that is what proves the daemon writes the
// database --i-know-this-wipes authorised. Design §5.2a.
func clearAgentBenchStore(ctx context.Context) error {
	project := strings.TrimSpace(benchAgentProject)
	if project == "" {
		return fmt.Errorf("refusing to run: no benchmark project named, so the pre-run clear " +
			"cannot know which store to reset")
	}
	db, err := openBenchDB()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	res, err := membench.ClearBenchmarkStore(ctx, db, project, "")
	if err != nil {
		return err
	}
	fmt.Printf("cleared benchmark store for project %q: %d chunks, %d entities, %d edges, %d mentions\n",
		project, res.Chunks, res.Entities, res.Edges, res.Mentions)
	return nil
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
	// Printed beside the key because it is the axis the key deliberately does
	// NOT cover: which binary computed these numbers. A stale one journals
	// zeros for metrics it is too old to read while every keyed axis matches.
	if b := j.Manifest.HarnessBuild; b != "" {
		if agentbench.HarnessBuildTrustworthy(b) {
			_, _ = fmt.Fprintf(w, "scored by\t%s\n", b)
		} else {
			_, _ = fmt.Fprintf(w, "scored by\t%s — NOT reproducible from any commit\n", b)
		}
	}
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
	printSchemaAccuracy(w, r)
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
	// Refuse incomparable provenance before inspecting the metric. This keeps
	// the arm-key safety boundary first even for legacy journals whose
	// pre-registration did not yet name a supported metric.
	if _, err := agentbench.CompareJournals(a, b, 0); err != nil {
		return err
	}

	metricA := strings.TrimSpace(a.Manifest.PreRegistration.Metric)
	metricB := strings.TrimSpace(b.Manifest.PreRegistration.Metric)
	if metricA != metricB {
		return fmt.Errorf("the two runs pre-registered different metrics (%q vs %q)", metricA, metricB)
	}
	ra, rb := a.Rollup(), b.Rollup()
	if metricA == agentbench.PinnedCaseValidationMetric {
		comparison, err := agentbench.CompareTaskScores(a.TaskScores, b.TaskScores,
			quality.ScoreKindPinnedCaseValidation)
		if err != nil {
			return err
		}
		verdict, err := agentbench.CompareJournals(a, b, comparison.MeanDelta)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s vs %s\n%s %s\n"+
			"signed mean delta = %.4f; magnitude = %.4f; sigma_d = %.4f; pairs = %d\n",
			ra.Arm, rb.Arm, metricA, verdict, comparison.MeanDelta, comparison.Magnitude,
			comparison.SigmaD, comparison.PairCount)
		return nil
	}

	observed, label, err := rollupMetricDelta(metricA, ra, rb)
	if err != nil {
		return err
	}
	verdict, err := agentbench.CompareJournals(a, b, observed)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s vs %s\n%s %s\n", ra.Arm, rb.Arm, label, verdict)
	return nil
}

func runBenchAgentCalibrate(cmd *cobra.Command, args []string) error {
	journal, err := loadJournal(args[0])
	if err != nil {
		return err
	}
	journalHash, err := sha256File(args[0])
	if err != nil {
		return fmt.Errorf("hash calibration journal: %w", err)
	}
	artifact, err := agentbench.BuildCalibration(journal, journalHash)
	if err != nil {
		return err
	}
	if err := writeAgentBenchArtifact(benchAgentCalibrationOutPath, artifact); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\nsha256 %s\n", benchAgentCalibrationOutPath, artifact.SHA256())
	return nil
}

func runBenchAgentNoiseFloor(cmd *cobra.Command, args []string) error {
	a, err := loadJournal(args[0])
	if err != nil {
		return err
	}
	b, err := loadJournal(args[1])
	if err != nil {
		return err
	}
	shaA, err := sha256File(args[0])
	if err != nil {
		return fmt.Errorf("hash first noise-floor journal: %w", err)
	}
	shaB, err := sha256File(args[1])
	if err != nil {
		return fmt.Errorf("hash second noise-floor journal: %w", err)
	}
	artifact, err := agentbench.BuildNoiseFloor(a, b, shaA, shaB)
	if err != nil {
		return err
	}
	if err := writeAgentBenchArtifact(benchAgentNoiseFloorOutPath, artifact); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\nsha256 %s\n", benchAgentNoiseFloorOutPath, artifact.SHA256())
	return nil
}

func runBenchAgentGate(cmd *cobra.Command, args []string) error {
	if benchAgentCalibrationPath == "" || benchAgentNoiseFloorPath == "" || benchAgentGatePolicyPath == "" {
		return fmt.Errorf("--calibration, --noise-floor, and --policy are required")
	}
	a, err := loadJournal(args[0])
	if err != nil {
		return err
	}
	b, err := loadJournal(args[1])
	if err != nil {
		return err
	}
	calibration, noise, policy, err := loadReleaseArtifacts()
	if err != nil {
		return err
	}
	decision := agentbench.EvaluateReleaseGate(a, b, calibration, noise, policy)
	if benchAgentJSON {
		blob, marshalErr := json.MarshalIndent(decision, "", "  ")
		if marshalErr != nil {
			return marshalErr
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s\n", blob)
	} else {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\nscore delta %.4f (floor %.4f, %d paired gate tasks)\n",
			decision.Status, decision.Reason, decision.GateScore.MeanDelta,
			decision.GateScore.ResolvableFloor, decision.GateScore.PairCount)
	}
	if decision.Status != agentbench.GateStatusPass {
		return fmt.Errorf("release gate %s: %s", decision.Status, decision.Reason)
	}
	return nil
}

func runBenchAgentReleasePreRegistration(cmd *cobra.Command, _ []string) error {
	if benchAgentCalibrationPath == "" || benchAgentNoiseFloorPath == "" || benchAgentGatePolicyPath == "" {
		return fmt.Errorf("--calibration, --noise-floor, and --policy are required")
	}
	calibration, noise, policy, err := loadReleaseArtifacts()
	if err != nil {
		return err
	}
	pre, err := agentbench.BuildReleasePreRegistration(benchAgentReleaseArms,
		benchAgentReleaseRationale, calibration, noise, policy)
	if err != nil {
		return err
	}
	if err := writeAgentBenchArtifact(benchAgentReleasePreRegOut, pre); err != nil {
		return err
	}
	hash, err := pre.Hash()
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\nsha256 %s\nrequired paired gate tasks %d\n",
		benchAgentReleasePreRegOut, hash, pre.ComputedPairs)
	return nil
}

func validateReleaseRunInputs(pre agentbench.PreRegistration, arm agentbench.ArmFields, tasks []agentbench.TaskSpec) error {
	pathsPresent := benchAgentCalibrationPath != "" || benchAgentNoiseFloorPath != "" || benchAgentGatePolicyPath != ""
	if !pre.ReleaseGateEnabled() {
		if pathsPresent {
			return fmt.Errorf("release artifacts were supplied but the pre-registration does not pin them")
		}
		return nil
	}
	if benchAgentCalibrationPath == "" || benchAgentNoiseFloorPath == "" || benchAgentGatePolicyPath == "" {
		return fmt.Errorf("release pre-registration requires --calibration, --noise-floor, and --gate-policy")
	}
	calibration, noise, policy, err := loadReleaseArtifacts()
	if err != nil {
		return err
	}
	tiers := make(map[string]agentbench.TaskTier, len(tasks))
	for _, task := range tasks {
		tiers[task.ID] = task.Tier
	}
	if err := agentbench.ValidateReleaseRunPlan(pre, arm, tiers, benchAgentRepeats, calibration, noise, policy); err != nil {
		return fmt.Errorf("release run plan refused before execution: %w", err)
	}
	return nil
}

func loadReleaseArtifacts() (agentbench.CalibrationManifest, agentbench.NoiseFloorManifest, agentbench.ReleaseGatePolicy, error) {
	var calibration agentbench.CalibrationManifest
	if err := readAgentBenchArtifact(benchAgentCalibrationPath, &calibration); err != nil {
		return calibration, agentbench.NoiseFloorManifest{}, agentbench.ReleaseGatePolicy{}, fmt.Errorf("load calibration: %w", err)
	}
	var noise agentbench.NoiseFloorManifest
	if err := readAgentBenchArtifact(benchAgentNoiseFloorPath, &noise); err != nil {
		return calibration, noise, agentbench.ReleaseGatePolicy{}, fmt.Errorf("load noise floor: %w", err)
	}
	var policy agentbench.ReleaseGatePolicy
	if err := readAgentBenchArtifact(benchAgentGatePolicyPath, &policy); err != nil {
		return calibration, noise, policy, fmt.Errorf("load release policy: %w", err)
	}
	return calibration, noise, policy, nil
}

func readAgentBenchArtifact(path string, out any) error {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("artifact contains more than one JSON value")
		}
		return err
	}
	return nil
}

func writeAgentBenchArtifact(path string, artifact any) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("--out is required")
	}
	blob, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return fmt.Errorf("encode artifact: %w", err)
	}
	if err := os.WriteFile(filepath.Clean(path), append(blob, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func rollupMetricDelta(metric string, a, b agentbench.Rollup) (float64, string, error) {
	switch metric {
	case "path coverage", "path_coverage":
		if !a.Accuracy.PathCoverageDefined || !b.Accuracy.PathCoverageDefined {
			return 0, "", fmt.Errorf("path coverage is not defined in both journals")
		}
		return b.Accuracy.PathCoverage - a.Accuracy.PathCoverage, "path coverage", nil
	case "schema_conformance":
		if !a.Accuracy.SchemaConformanceDefined || !b.Accuracy.SchemaConformanceDefined {
			return 0, "", fmt.Errorf("schema conformance is not defined in both journals")
		}
		return b.Accuracy.SchemaConformance - a.Accuracy.SchemaConformance, "schema conformance", nil
	case "task_success_rate":
		av, aok := a.Failures.TaskSuccessRate()
		bv, bok := b.Failures.TaskSuccessRate()
		if !aok || !bok {
			return 0, "", fmt.Errorf("task success rate is not defined in both journals")
		}
		return bv - av, "task success rate", nil
	default:
		return 0, "", fmt.Errorf("unsupported pre-registered agent metric %q", metric)
	}
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
	// Reject a malformed manifest HERE, naming the field, rather than letting
	// the regeneration fence report it as an ordinary task-set mismatch. A
	// manifest whose taskSetSha256 is captured help text (2026-08-19) refuses
	// against every task set, so the fence's message sends the reader looking
	// for a task-set change that never happened.
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("gold manifest %s is malformed: %w", path, err)
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
// checkScoringProvenance refuses a scoring pass whose own numbers could not be
// traced back to a commit, and prints the identities of every binary that
// determines them before hours are spent.
//
// THE INCIDENT (2026-08-16). A long-horizon arm journaled durationMs=0 for all
// 14 records while the ledger held the durations. The read code was correct and
// present in the tree; the vornikctl running it was 27 commits stale and
// predated the fix. The arm key pinned the DAEMON — which was current — and the
// scoring contract version, which matched, so nothing refused and nothing warned.
// The stale binary was reached through a bare `vornikctl` on $PATH.
//
// Two halves, matching the two ways that failed.
//
// It REFUSES a harness that is dirty or unstamped, because such a run cannot be
// reproduced from any commit and a benchmark whose own provenance is unknown has
// no business publishing a figure. The override exists because a developer
// smoke-testing harness changes has a legitimate dirty tree — but it must be
// asked for by name, so the unreproducibility is a decision someone made rather
// than a default nobody saw.
//
// It PRINTS the harness and daemon revisions together, because the two coming
// from different commits is legitimate (scoring an old release with the current
// harness is the point of release comparison) and only a human can say whether
// this particular pairing was intended. Nothing put them side by side before.
func checkScoringProvenance(cmd *cobra.Command) error {
	harness := agentbench.HarnessBuild()
	daemon := agentbench.BinaryBuild(benchAgentDaemonBinary)

	w := cmd.ErrOrStderr()
	_, _ = fmt.Fprintf(w, "provenance: harness %s", harness)
	if daemon != "" {
		_, _ = fmt.Fprintf(w, ", daemon %s", daemon)
	}
	_, _ = fmt.Fprintln(w)

	if err := scoringProvenanceError(harness, benchAgentAllowUnreproducible); err != nil {
		return err
	}
	if !agentbench.HarnessBuildTrustworthy(harness) {
		_, _ = fmt.Fprintf(w, "WARNING: scoring with an unreproducible harness (%s) "+
			"because --i-know-this-is-unreproducible was given; these figures cannot "+
			"be regenerated from any commit and must not be published\n", harness)
	}
	return nil
}

// scoringProvenanceError is the decision checkScoringProvenance makes, split
// from the printing so it can be tested without a build stamp of its own — a
// test binary's VCS metadata is not something a test may depend on.
func scoringProvenanceError(harness string, allowUnreproducible bool) error {
	if agentbench.HarnessBuildTrustworthy(harness) || allowUnreproducible {
		return nil
	}
	return fmt.Errorf("refusing to score with harness build %q: a run whose own scoring "+
		"binary cannot be traced to a commit cannot be reproduced, and a stale one "+
		"silently journals zeros for metrics it is too old to read (which is exactly "+
		"how durationMs was lost on 2026-08-16).\n"+
		"  Build the harness from the tree you mean to score with, or pass "+
		"--i-know-this-is-unreproducible to accept unpublishable figures", harness)
}

// printSchemaAccuracy prints conformance WITH the denominator it was computed
// over, and the reliability half beside it.
//
// The rollup has carried SchemaJudged and SchemaNoOutput since 2026-08-16
// precisely so a reader can tell "0.912 conformance" over 58% of steps from
// 0.912 over all of them — but this printer showed only the ratio, so the
// figure that reached a terminal was the flattering one. A struct field nobody
// prints is not published.
func printSchemaAccuracy(w *tabwriter.Writer, r agentbench.Rollup) {
	a := r.Accuracy
	terminal := a.SchemaJudged + a.SchemaNoOutput

	if a.SchemaConformanceDefined {
		if terminal > 0 {
			_, _ = fmt.Fprintf(w, "schema conformance\t%.3f  (over %d of %d terminal steps)\n",
				a.SchemaConformance, a.SchemaJudged, terminal)
		} else {
			_, _ = fmt.Fprintf(w, "schema conformance\t%.3f\n", a.SchemaConformance)
		}
	} else if terminal > 0 {
		// Undefined is a RESULT, not an absence: every terminal step produced
		// nothing a schema could apply to. Printing nothing here would read as
		// "not measured".
		_, _ = fmt.Fprintf(w, "schema conformance\tundefined (0 of %d terminal steps produced output)\n",
			terminal)
	}

	if a.SchemaNoOutput > 0 {
		pct := 0.0
		if terminal > 0 {
			pct = 100 * float64(a.SchemaNoOutput) / float64(terminal)
		}
		_, _ = fmt.Fprintf(w, "no output at all\t%d (%.1f%%)\n", a.SchemaNoOutput, pct)
		// Sorted so two runs of the same arm print the same order — an
		// unstable diff invites reading noise as change.
		for _, cause := range sortedCauses(a.SchemaNoOutputByOutcome) {
			_, _ = fmt.Fprintf(w, "  %s\t%d\n", cause, a.SchemaNoOutputByOutcome[cause])
		}
	}
}

// sortedCauses orders a cause breakdown by count descending, then name, so the
// remedy with the most steps behind it reads first.
func sortedCauses(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if m[out[i]] != m[out[j]] {
			return m[out[i]] > m[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

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

// verifyModelWindow compares the context window the arm is CONFIGURED for
// against the one the serving endpoint actually reports.
//
// WHY THIS IS A PREFLIGHT AND NOT A NOTE. context_size drives the agent's
// whole compaction budget: entrypoint.sh allows
// (context - max_tokens - 2048) * 3 bytes/token * 80%. A model with no
// agent_llm.model_limits entry silently inherits the daemon-wide global, and
// nothing reports the substitution. That produced the 2026-07-12 overflow
// incident and then 14 of the 73 failures in the 2026-08-16 long-horizon arm,
// where Qwen/Qwen3.8-27B-FP8 ran against a declared 100000 and a real 32768.
//
// A misconfigured window does not fail loudly at config-read time. It fails
// mid-run, as overflows, hours in — which is exactly the class of fault a
// preflight exists to convert into a refusal.
//
// Opt-in by environment, mirroring VORNIK_BENCH_DSN: without an endpoint and
// model there is nothing to probe, and a bench run against a provider this
// check cannot reach must not be blocked by it. Skipping is reported, never
// silent — an unrun check that looks like a passed one is how the scoring
// harness went 27 commits stale unnoticed.
func verifyModelWindow(ctx context.Context, cmd *cobra.Command) error {
	endpoint := strings.TrimSpace(os.Getenv("VORNIK_BENCH_MODEL_ENDPOINT"))
	model := strings.TrimSpace(os.Getenv("VORNIK_BENCH_MODEL"))
	if endpoint == "" || model == "" {
		cmd.PrintErrln("  ..  model window: not checked (set VORNIK_BENCH_MODEL_ENDPOINT and " +
			"VORNIK_BENCH_MODEL to verify the arm's context_size against the endpoint)")
		return nil
	}

	configured := 0
	if raw := strings.TrimSpace(os.Getenv("VORNIK_BENCH_MODEL_CONTEXT")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("VORNIK_BENCH_MODEL_CONTEXT=%q is not an integer", raw)
		}
		configured = n
	}

	discovered, err := agentbench.DiscoverModelWindow(ctx, endpoint, os.Getenv("VORNIK_BENCH_MODEL_API_KEY"), model)
	if err != nil {
		// A probe that cannot reach the endpoint says nothing about the
		// configuration. Report it and continue rather than blocking a run on
		// a diagnostic's own failure.
		cmd.PrintErrf("  ..  model window: probe failed (%v) — configured value unverified\n", err)
		return nil
	}

	v := agentbench.CheckConfiguredWindow(model, configured, discovered)
	if v.Fatal {
		return fmt.Errorf("model window: %s", v.Message)
	}
	switch v.Verdict {
	case agentbench.WindowOK:
		cmd.PrintErrf("  ok  model window: %s\n", v.Message)
	default:
		cmd.PrintErrf("  !!  model window: %s\n", v.Message)
	}
	return nil
}
