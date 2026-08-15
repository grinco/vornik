package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentbench"
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
	benchAgentTaskSetHash = "abc123"
	benchAgentGoldPath = writeAgentBenchJSON(t, dir, "gold.json", agentbench.GoldManifest{
		TaskSetSHA256: "abc123",
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

// The shipped starter set must stay loadable and unique — it is the input to
// every first run.
func TestBenchAgent_ShippedTaskSetLoads(t *testing.T) {
	// A task set is bound to a swarm SEMANTICALLY, not just by role names
	// resolving. Pairing companion-* workflows with dev-swarm loaded clean and
	// failed every step at run time, so the sets are split and each is pinned to
	// the workflows its swarm actually implements.
	// The dev-swarm set must stay above the pair count its own measured noise
	// floor demands. sigma_d = 0.0604 (n=10, §12.5) needs 12 pairs to resolve a
	// 5pp conformance effect; the set carries 18 so the gate has margin and a
	// task can be retired without silently dropping under the floor.
	const devSwarmMinTasks = 12

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
		ok := map[string]bool{}
		for _, w := range allowed {
			ok[w] = true
		}
		if strings.Contains(path, "dev-swarm") && len(tasks) < devSwarmMinTasks {
			t.Errorf("%s has %d tasks; the measured sigma_d needs at least %d pairs to "+
				"resolve a 5pp effect, so a smaller set cannot gate what it claims to",
				path, len(tasks), devSwarmMinTasks)
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
	if len(seen) != 28 {
		t.Errorf("total tasks across both sets = %d, want 28", len(seen))
	}
}
