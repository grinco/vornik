package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentbench"
	"vornik.io/vornik/internal/quality"
)

func writeAgentBenchJSON(t *testing.T, dir, name string, v any) string {
	t.Helper()
	path := filepath.Join(dir, name)
	blob, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func resetAgentFlags() {
	benchAgentProject = ""
	benchAgentBenchProject = ""
	benchAgentSwarm = ""
	benchAgentDatabase = ""
	benchAgentConfirmWipe = ""
	benchAgentTaskSetHash = ""
	benchAgentGoldPath = ""
	benchAgentPreRegPath = ""
	benchAgentCalibrationPath = ""
	benchAgentNoiseFloorPath = ""
	benchAgentGatePolicyPath = ""
	benchAgentCalibrationOutPath = ""
	benchAgentNoiseFloorOutPath = ""
	benchAgentReleaseArms = nil
	benchAgentReleaseRationale = ""
	benchAgentReleasePreRegOut = ""
	benchAgentJSON = false
}

// Guard-ordering law (§5.2), asserted at the CLI boundary too: the run below is
// invalid in four ways and the wipe-confirmation error is the one that must
// surface.
func TestBenchAgent_ScopeGuardFiresBeforeAnythingElse(t *testing.T) {
	resetAgentFlags()
	// Shipped-denylist name, not a deployment's own — see membench/guard.go.
	benchAgentDatabase = "production"
	benchAgentConfirmWipe = ""
	benchAgentProject = ""
	benchAgentBenchProject = "bench"
	benchAgentSwarm = "ibkr-trader"
	// No pre-registration and no task-set hash either — both would error.

	err := runBenchAgentRun(benchAgentRunCmd, nil)
	if err == nil {
		t.Fatal("a run invalid in four ways was authorised")
	}
	if !strings.Contains(err.Error(), "--i-know-this-wipes") {
		t.Errorf("the destructive-target guard must fire first; got: %v", err)
	}
}

func TestBenchAgent_RunRefusesWithoutPreRegistration(t *testing.T) {
	resetAgentFlags()
	benchAgentDatabase = "agentbench_local"
	benchAgentConfirmWipe = "agentbench_local"
	benchAgentProject = "bench"
	benchAgentBenchProject = "bench"
	benchAgentSwarm = "bench"

	err := runBenchAgentRun(benchAgentRunCmd, nil)
	if err == nil {
		t.Fatal("ran without a pre-registration")
	}
	if !strings.Contains(err.Error(), "press release") {
		t.Errorf("refusal does not explain why it exists: %v", err)
	}
}

func TestBenchAgent_RunRefusesAnEmptyPreRegistration(t *testing.T) {
	dir := t.TempDir()
	resetAgentFlags()
	benchAgentDatabase = "agentbench_local"
	benchAgentConfirmWipe = "agentbench_local"
	benchAgentProject = "bench"
	benchAgentBenchProject = "bench"
	benchAgentSwarm = "bench"
	benchAgentPreRegPath = writeAgentBenchJSON(t, dir, "prereg.json", agentbench.PreRegistration{
		Arms: []string{"only-one"},
	})

	err := runBenchAgentRun(benchAgentRunCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "two arms") {
		t.Errorf("want a validation refusal naming the single-arm problem, got: %v", err)
	}
}

func TestBenchAgent_GoldRefusesAnUnchangedTaskSet(t *testing.T) {
	dir := t.TempDir()
	resetAgentFlags()
	benchAgentDatabase = "agentbench_local"
	benchAgentConfirmWipe = "agentbench_local"
	benchAgentProject = "bench"
	benchAgentBenchProject = "bench"
	benchAgentSwarm = "bench"
	// A realistically SHAPED digest. It was "abc123" until 2026-08-19, which
	// exercised the fence while exercising nothing that cares what a digest
	// looks like — the blind spot that let captured help text ship in this
	// field for five days.
	const taskSet = "9b6fffe10fe0fdb6ead82e94bea62a48a9511a38ef2ef7cefe24a97797c98df9"
	benchAgentTaskSetHash = taskSet
	benchAgentGoldPath = writeAgentBenchJSON(t, dir, "gold.json", agentbench.GoldManifest{
		TaskSetSHA256: taskSet,
	})

	err := runBenchAgentGold(benchAgentGoldCmd, nil)
	if err == nil {
		t.Fatal("regenerated gold against an unchanged task set")
	}
	if !strings.Contains(err.Error(), "delete the pinned manifest") {
		t.Errorf("refusal does not name the reviewable escape hatch: %v", err)
	}
}

func TestBenchAgent_GoldRequiresATaskSetHash(t *testing.T) {
	resetAgentFlags()
	benchAgentDatabase = "agentbench_local"
	benchAgentConfirmWipe = "agentbench_local"
	benchAgentProject = "bench"
	benchAgentBenchProject = "bench"
	benchAgentSwarm = "bench"

	err := runBenchAgentGold(benchAgentGoldCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "--task-set-hash") {
		t.Errorf("want a refusal demanding the hash the fence compares against, got: %v", err)
	}
}

// A degraded run's figures must not be readable without the warning, so the
// warning goes to stderr BEFORE the table reaches stdout.
func TestBenchAgent_RollupWarnsBeforeItPrints(t *testing.T) {
	dir := t.TempDir()
	resetAgentFlags()

	j := agentbench.Journal{
		Manifest: agentbench.RunManifest{
			Arm:                 agentbench.ArmFields{Name: "baseline"},
			Untrustworthy:       true,
			UntrustworthyReason: "daemon restarted mid-run",
			PreRegistrationHash: "h",
		},
		Records: []agentbench.ExecutionRecord{
			{TaskID: "t1", Succeeded: true, CostUSD: 1.0},
		},
	}
	path := writeAgentBenchJSON(t, dir, "journal.json", j)

	cmd := *benchAgentRollupCmd
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)

	if err := runBenchAgentRollup(&cmd, []string{path}); err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if !strings.Contains(errOut.String(), "daemon restarted mid-run") {
		t.Errorf("the untrustworthy reason was not surfaced: %q", errOut.String())
	}
	if !strings.Contains(out.String(), "COST") {
		t.Errorf("the table was not printed: %q", out.String())
	}
}

// $/success is printed beside $/attempt and the success rate, because the figure
// alone invites exactly the "cost of our successes" misreading it exists to
// avoid.
func TestBenchAgent_RollupPrintsAllThreeCostFigures(t *testing.T) {
	dir := t.TempDir()
	resetAgentFlags()

	j := agentbench.Journal{
		Manifest: agentbench.RunManifest{
			Arm:                 agentbench.ArmFields{Name: "baseline"},
			PreRegistrationHash: "h",
			Power: agentbench.PowerCheck{
				SigmaD: 0.01, SigmaN: 12, TargetDelta: 0.05,
				RequiredPairs: 1, AvailablePairs: 20, Adequate: true,
			},
		},
		Records: []agentbench.ExecutionRecord{
			{TaskID: "t1", Succeeded: true, CostUSD: 1.0},
			{TaskID: "t2", Succeeded: true, CostUSD: 1.0},
			{TaskID: "t3", Succeeded: false, ErrorText: "criteria not met", CostUSD: 2.0},
		},
	}
	path := writeAgentBenchJSON(t, dir, "journal.json", j)

	cmd := *benchAgentRollupCmd
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})

	if err := runBenchAgentRollup(&cmd, []string{path}); err != nil {
		t.Fatalf("rollup: %v", err)
	}
	got := out.String()
	for _, want := range []string{"$4.0000", "$1.3333", "$2.0000", "failed-run spend included"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

// Request precision is printed apart from EFFICIENCY and labelled, because it
// improves when the lead asks for less.
func TestBenchAgent_RollupSeparatesTheDiagnostic(t *testing.T) {
	dir := t.TempDir()
	resetAgentFlags()

	j := agentbench.Journal{
		Manifest: agentbench.RunManifest{
			Arm:                 agentbench.ArmFields{Name: "baseline"},
			PreRegistrationHash: "h",
			Power: agentbench.PowerCheck{
				SigmaD: 0.01, SigmaN: 12, TargetDelta: 0.05,
				RequiredPairs: 1, AvailablePairs: 20, Adequate: true,
			},
		},
		Records: []agentbench.ExecutionRecord{{
			TaskID: "t1", Succeeded: true,
			Verdicts: []agentbench.Verdict{{
				Probe: "tool-grant", RequestPrecision: 0.2, RequestPrecisionDefined: true,
				Escalations: 5,
			}},
		}},
	}
	path := writeAgentBenchJSON(t, dir, "journal.json", j)

	cmd := *benchAgentRollupCmd
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})

	if err := runBenchAgentRollup(&cmd, []string{path}); err != nil {
		t.Fatalf("rollup: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "not an optimisation target") {
		t.Errorf("request precision was not labelled as a diagnostic:\n%s", got)
	}
	if !strings.Contains(got, "read against escalations (5)") {
		t.Errorf("the diagnostic was printed without the counterweight that makes it "+
			"readable:\n%s", got)
	}
}

func TestBenchAgent_CompareRefusesIncomparableArms(t *testing.T) {
	dir := t.TempDir()
	resetAgentFlags()

	mk := func(name, binary string) string {
		j := agentbench.Journal{Manifest: agentbench.RunManifest{
			Arm: agentbench.ArmFields{Name: name, BinarySHA256: binary, HarnessVersion: "1"},
		}}
		return writeAgentBenchJSON(t, dir, name+".json", j)
	}
	a := mk("baseline", "aaaa")
	b := mk("variant", "bbbb")

	cmd := *benchAgentCompareCmd
	cmd.SetOut(&bytes.Buffer{})
	err := runBenchAgentCompare(&cmd, []string{a, b})
	if err == nil {
		t.Fatal("compared two runs built from different binaries")
	}
	if !strings.Contains(err.Error(), "binary_sha256") {
		t.Errorf("refusal does not name the differing axis: %v", err)
	}
}

func TestBenchAgent_CompareUsesThePreRegisteredGradedMetric(t *testing.T) {
	dir := t.TempDir()
	arm := agentbench.ArmFields{
		HarnessVersion: agentbench.HarnessVersion, BinarySHA256: "bin", ConfigSHA256: "cfg",
		Models: map[string]string{"worker": "model"}, ContextPolicy: "policy",
		TaskSetSHA256: "tasks", ScoringPolicySHA256: "scoring", Probes: []string{"probe"},
	}
	mk := func(name string, scores []agentbench.TaskScore) string {
		j := agentbench.Journal{Manifest: agentbench.RunManifest{
			Arm: arm, PreRegistration: agentbench.PreRegistration{Metric: agentbench.PinnedCaseValidationMetric},
			Power: agentbench.PowerCheck{ResolvableDelta: .01},
		}, TaskScores: scores}
		j.Manifest.Arm.Name = name
		return writeAgentBenchJSON(t, dir, name+".json", j)
	}
	score := func(task string, value float64) agentbench.TaskScore {
		return agentbench.TaskScore{TaskID: task, Repeat: 1, Kind: quality.ScoreKindPinnedCaseValidation, Score: value}
	}
	a := mk("baseline", []agentbench.TaskScore{score("a", .2), score("b", .8)})
	b := mk("variant", []agentbench.TaskScore{score("a", .6), score("b", .6)})

	cmd := *benchAgentCompareCmd
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := runBenchAgentCompare(&cmd, []string{a, b}); err != nil {
		t.Fatalf("compare: %v", err)
	}
	for _, want := range []string{"pinned_case_validation_score", "signed mean delta = 0.1000", "pairs = 2", "sigma_d"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

// The digest must be order-independent: the fence compares it, so a reordered
// file must not read as a changed task set and permit a gold rebuild.
func TestBenchAgent_TaskSetHashIsOrderIndependent(t *testing.T) {
	dir := t.TempDir()
	resetAgentFlags()

	a := writeAgentBenchJSON(t, dir, "a.json", []agentbench.TaskSpec{
		{ID: "t1", Workflow: "w", Prompt: "one"},
		{ID: "t2", Workflow: "w", Prompt: "two"},
	})
	b := writeAgentBenchJSON(t, dir, "b.json", []agentbench.TaskSpec{
		{ID: "t2", Workflow: "w", Prompt: "two"},
		{ID: "t1", Workflow: "w", Prompt: "one"},
	})

	run := func(path string) string {
		cmd := *benchAgentTaskSetHashCmd
		var out bytes.Buffer
		cmd.SetOut(&out)
		if err := runBenchAgentTaskSetHash(&cmd, []string{path}); err != nil {
			t.Fatalf("hash %s: %v", path, err)
		}
		return out.String()
	}
	if run(a) != run(b) {
		t.Error("reordering the file changed the digest — the fence would allow a " +
			"gold rebuild against an unchanged task set")
	}
}

// The same prompt on a different workflow is a different experiment.
func TestBenchAgent_TaskSetHashCoversTheWorkflow(t *testing.T) {
	dir := t.TempDir()
	resetAgentFlags()

	a := writeAgentBenchJSON(t, dir, "a.json", []agentbench.TaskSpec{{ID: "t1", Workflow: "w1", Prompt: "same"}})
	b := writeAgentBenchJSON(t, dir, "b.json", []agentbench.TaskSpec{{ID: "t1", Workflow: "w2", Prompt: "same"}})

	run := func(path string) string {
		cmd := *benchAgentTaskSetHashCmd
		var out bytes.Buffer
		cmd.SetOut(&out)
		if err := runBenchAgentTaskSetHash(&cmd, []string{path}); err != nil {
			t.Fatalf("hash: %v", err)
		}
		return out.String()
	}
	if run(a) == run(b) {
		t.Error("the workflow is not covered by the digest")
	}
}

func TestBenchAgent_TaskSetHashRefusesDuplicateIDs(t *testing.T) {
	dir := t.TempDir()
	resetAgentFlags()
	path := writeAgentBenchJSON(t, dir, "dup.json", []agentbench.TaskSpec{
		{ID: "t1", Workflow: "w", Prompt: "one"},
		{ID: "t1", Workflow: "w", Prompt: "different"},
	})

	cmd := *benchAgentTaskSetHashCmd
	cmd.SetOut(&bytes.Buffer{})
	err := runBenchAgentTaskSetHash(&cmd, []string{path})
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("a duplicate task id was digested silently, covering only one: %v", err)
	}
}

func TestBenchAgent_ReleaseArtifactCommandsWriteHashedEvidence(t *testing.T) {
	dir := t.TempDir()
	resetAgentFlags()
	tiers := map[string]agentbench.TaskTier{
		"trip": agentbench.TaskTierTripwire, "gate-a": agentbench.TaskTierGate, "gate-b": agentbench.TaskTierGate,
	}
	arm := agentbench.ArmFields{HarnessVersion: agentbench.HarnessVersion, BinarySHA256: "bin",
		ConfigSHA256: "cfg", Models: map[string]string{"worker": "model"},
		AgentImages: map[string]string{"worker": "sha256:image"}, ContextPolicy: "policy",
		TaskSetSHA256: "tasks", TierPolicySHA256: "tiers", ScoringPolicySHA256: "scores",
		Probes: []string{"schema-following"}}
	mk := func(runID string, values map[string]float64) agentbench.Journal {
		j := agentbench.Journal{Manifest: agentbench.RunManifest{RunID: runID, Arm: arm,
			ArmKey: arm.Key(), TaskTiers: tiers}}
		for repeat := 1; repeat <= 10; repeat++ {
			j.TaskRuns = append(j.TaskRuns, agentbench.TaskRun{TaskID: "trip", Repeat: repeat, Succeeded: true})
			for _, id := range []string{"gate-a", "gate-b"} {
				passed := repeat%2 == 0
				j.TaskRuns = append(j.TaskRuns, agentbench.TaskRun{TaskID: id, Repeat: repeat,
					Succeeded: passed, ErrorText: map[bool]string{false: "criteria not met"}[passed]})
				j.TaskScores = append(j.TaskScores, agentbench.TaskScore{TaskID: id, Repeat: repeat,
					Kind: quality.ScoreKindPinnedCaseValidation, Score: values[id]})
			}
		}
		return j
	}
	aPath := writeAgentBenchJSON(t, dir, "a.json", mk("a", map[string]float64{"gate-a": .4, "gate-b": .6}))
	bPath := writeAgentBenchJSON(t, dir, "b.json", mk("b", map[string]float64{"gate-a": .5, "gate-b": .4}))

	benchAgentCalibrationOutPath = filepath.Join(dir, "calibration.json")
	var out bytes.Buffer
	cmd := *benchAgentCalibrateCmd
	cmd.SetOut(&out)
	if err := runBenchAgentCalibrate(&cmd, []string{aPath}); err != nil {
		t.Fatalf("calibrate: %v", err)
	}
	if _, err := os.Stat(benchAgentCalibrationOutPath); err != nil || !strings.Contains(out.String(), "sha256") {
		t.Fatalf("calibration artifact/output missing: stat=%v output=%q", err, out.String())
	}

	benchAgentNoiseFloorOutPath = filepath.Join(dir, "noise.json")
	out.Reset()
	cmd = *benchAgentNoiseFloorCmd
	cmd.SetOut(&out)
	if err := runBenchAgentNoiseFloor(&cmd, []string{aPath, bPath}); err != nil {
		t.Fatalf("noise floor: %v", err)
	}
	if _, err := os.Stat(benchAgentNoiseFloorOutPath); err != nil || !strings.Contains(out.String(), "sha256") {
		t.Fatalf("noise artifact/output missing: stat=%v output=%q", err, out.String())
	}

	benchAgentCalibrationPath = benchAgentCalibrationOutPath
	benchAgentNoiseFloorPath = benchAgentNoiseFloorOutPath
	benchAgentGatePolicyPath = writeAgentBenchJSON(t, dir, "policy.json", agentbench.ReleaseGatePolicy{
		Metric: agentbench.PinnedCaseValidationMetric, MaxScoreRegression: .5,
		MaxStepNoOutputRateIncrease: .02,
	})
	benchAgentReleaseArms = []string{"baseline", "candidate"}
	benchAgentReleaseRationale = "refuse an agent-quality release regression"
	benchAgentReleasePreRegOut = filepath.Join(dir, "release-prereg.json")
	out.Reset()
	cmd = *benchAgentReleasePreRegCmd
	cmd.SetOut(&out)
	if err := runBenchAgentReleasePreRegistration(&cmd, nil); err != nil {
		t.Fatalf("release preregistration: %v", err)
	}
	var pre agentbench.PreRegistration
	if err := readAgentBenchArtifact(benchAgentReleasePreRegOut, &pre); err != nil {
		t.Fatalf("read release preregistration: %v", err)
	}
	if !pre.ReleaseGateEnabled() || !strings.Contains(out.String(), "required paired gate tasks") {
		t.Fatalf("derived release preregistration/output = %+v / %q", pre, out.String())
	}
}

func TestBenchAgent_GateRequiresAllCommittedArtifacts(t *testing.T) {
	resetAgentFlags()
	cmd := *benchAgentGateCmd
	cmd.SetOut(&bytes.Buffer{})
	err := runBenchAgentGate(&cmd, []string{"baseline.json", "candidate.json"})
	if err == nil || !strings.Contains(err.Error(), "--calibration") {
		t.Fatalf("gate reached journals without artifact inputs: %v", err)
	}
}

func TestBenchAgent_AgentImageObservationFailureMakesJournalUntrustworthy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		observed map[string]string
		err      error
	}{
		{name: "inspect or ledger error", err: errors.New("inspect failed")},
		{name: "no immutable ids", observed: map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			journal := agentbench.Journal{}
			applyObservedAgentImages(&journal, tc.observed, tc.err)
			if !journal.Manifest.Untrustworthy ||
				!strings.Contains(journal.Manifest.UntrustworthyReason, "AGENT_IMAGE_PROVENANCE_MISSING") {
				t.Fatalf("missing provenance was not made explicit: %+v", journal.Manifest)
			}
		})
	}
	journal := agentbench.Journal{}
	applyObservedAgentImages(&journal, map[string]string{"worker": "sha256:aaa+sha256:bbb"}, nil)
	if !journal.Manifest.Untrustworthy || !strings.Contains(journal.Manifest.UntrustworthyReason, "AGENT_IMAGE_DRIFT") ||
		journal.Manifest.Arm.AgentImages["worker"] == "" {
		t.Fatalf("image drift was not retained and refused: %+v", journal.Manifest)
	}
}

func TestBenchAgent_ShippedReleasePolicyValidates(t *testing.T) {
	path := "../agentbench/policies/dev-swarm-release-v1.json"
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Skip("release policy absent in CE tree")
	}
	var policy agentbench.ReleaseGatePolicy
	if err := readAgentBenchArtifact(path, &policy); err != nil {
		t.Fatalf("load shipped release policy: %v", err)
	}
	if err := policy.Validate(); err != nil {
		t.Fatalf("shipped release policy: %v", err)
	}
}

// The shipped starter set must stay loadable and unique — it is the input to
// every first run.
func TestBenchAgent_ShippedTaskSetLoads(t *testing.T) {
	// A task set is bound to a swarm SEMANTICALLY, not just by role names
	// resolving. Pairing companion-* workflows with dev-swarm loaded clean and
	// failed every step at run time, so the sets are split and each is pinned to
	// the workflows its swarm actually implements.
	// The release denominator, not the total task count, must stay above the
	// pair count its measured noise floor demands. Exploratory and tripwire
	// tasks are deliberately excluded from this count.
	const devSwarmMinGateTasks = 20

	sets := map[string][]string{
		"../agentbench/tasksets/dev-swarm-tasks-v1.json":       {"simple-workflow", "dev-pipeline"},
		"../agentbench/tasksets/companion-swarm-tasks-v1.json": {"companion-research-gather", "companion-data-validation", "companion-test-coverage-audit"},
	}

	seen := map[string]bool{}
	for path, allowed := range sets {
		// The task sets are EE-only assets: the CE export prunes
		// internal/agentbench/tasksets (design 7 — a task definition or gold set
		// reaching the public artifact would let anyone tune against the
		// benchmark). This test ships to CE, so it must skip rather than fail
		// there; failing would make the CE export refuse to publish over an
		// asset it deliberately removed.
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			t.Skipf("%s absent — expected in a CE tree, which prunes EE task sets", path)
		}
		tasks, err := loadTaskSet(path)
		if err != nil {
			t.Fatalf("load %s: %v", path, err)
		}
		if err := agentbench.ValidateTaskTiers(tasks); err != nil {
			t.Fatalf("tier policy %s: %v", path, err)
		}
		ok := map[string]bool{}
		for _, w := range allowed {
			ok[w] = true
		}
		gateTasks := 0
		for _, task := range tasks {
			if task.Tier == agentbench.TaskTierGate {
				gateTasks++
			}
		}
		if strings.Contains(path, "dev-swarm") && gateTasks < devSwarmMinGateTasks {
			t.Errorf("%s has %d gate tasks; the measured sigma_d needs at least %d pairs to "+
				"resolve a 5pp effect, so exploratory tasks cannot fill the shortfall",
				path, gateTasks, devSwarmMinGateTasks)
		}
		for _, task := range tasks {
			if task.ID == "" || task.Workflow == "" || task.Prompt == "" {
				t.Errorf("%s: task %+v is missing a required field", path, task)
			}
			if !ok[task.Workflow] {
				t.Errorf("%s: task %q uses workflow %q, which its swarm does not implement",
					path, task.ID, task.Workflow)
			}
			if seen[task.ID] {
				t.Errorf("duplicate task id %q across sets", task.ID)
			}
			seen[task.ID] = true
		}
	}
	if len(seen) != 40 {
		t.Errorf("total tasks across both sets = %d, want 40", len(seen))
	}
}

// A scored arm that runs ONE BATCH of the task set must still describe the
// WHOLE set on every axis, or `bench agent rollup` refuses to merge the
// batches — it rejects journals whose arms disagree, which is correct and is
// exactly what made a 7-hour arm unresumable (design §12.13).
//
// All THREE task-derived axes matter, not just the task-set digest: scoring
// policy and tier policy are computed from the same slice, so a batch would
// differ on all three.
func TestBuildArm_BatchDescribesTheWholeTaskSet(t *testing.T) {
	full := []agentbench.TaskSpec{
		{ID: "a", Workflow: "simple-workflow", Prompt: "one", Tier: "exploratory"},
		{ID: "b", Workflow: "simple-workflow", Prompt: "two", Tier: "exploratory"},
		{ID: "c", Workflow: "simple-workflow", Prompt: "three", Tier: "exploratory"},
	}
	batch := full[:1] // the first batch only

	prevPol, prevArm := benchAgentContextPol, benchAgentArm
	benchAgentContextPol, benchAgentArm = "suppression=none;advert=gated", "test-arm"
	t.Cleanup(func() { benchAgentContextPol, benchAgentArm = prevPol, prevArm })

	whole, err := buildArmOver(full, full, nil)
	if err != nil {
		t.Fatal(err)
	}
	batched, err := buildArmOver(batch, full, nil)
	if err != nil {
		t.Fatal(err)
	}

	if batched.TaskSetSHA256 != whole.TaskSetSHA256 {
		t.Errorf("task-set digest differs: batch %s, whole %s",
			batched.TaskSetSHA256, whole.TaskSetSHA256)
	}
	if batched.ScoringPolicySHA256 != whole.ScoringPolicySHA256 {
		t.Errorf("scoring-policy digest differs: batch %s, whole %s",
			batched.ScoringPolicySHA256, whole.ScoringPolicySHA256)
	}
	if batched.TierPolicySHA256 != whole.TierPolicySHA256 {
		t.Errorf("tier-policy digest differs: batch %s, whole %s",
			batched.TierPolicySHA256, whole.TierPolicySHA256)
	}
}

// Without an axis set, the arm describes exactly the tasks it ran. This is the
// unbatched path every existing caller uses, and it must not change.
func TestBuildArm_UnbatchedDescribesWhatItRan(t *testing.T) {
	tasks := []agentbench.TaskSpec{
		{ID: "a", Workflow: "simple-workflow", Prompt: "one", Tier: "exploratory"},
		{ID: "b", Workflow: "simple-workflow", Prompt: "two", Tier: "exploratory"},
	}
	prevPol, prevArm := benchAgentContextPol, benchAgentArm
	benchAgentContextPol, benchAgentArm = "suppression=none;advert=gated", "test-arm"
	t.Cleanup(func() { benchAgentContextPol, benchAgentArm = prevPol, prevArm })

	// nil axis set => axes come from the tasks themselves.
	got, err := buildArmOver(tasks, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want, err := buildArmOver(tasks, tasks, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.TaskSetSHA256 != want.TaskSetSHA256 {
		t.Errorf("nil axis set must equal self-described: %s vs %s",
			got.TaskSetSHA256, want.TaskSetSHA256)
	}
}
