package agentbench

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/membench"
	"vornik.io/vornik/internal/quality"
)

type fakeTasks struct {
	outcomes map[string]TaskOutcome
	err      error
	calls    []string
}

func (f *fakeTasks) Run(_ context.Context, spec TaskSpec) (TaskOutcome, error) {
	f.calls = append(f.calls, spec.ID)
	if f.err != nil {
		return TaskOutcome{}, f.err
	}
	o, ok := f.outcomes[spec.ID]
	if !ok {
		return TaskOutcome{TaskID: spec.ID, Succeeded: true, Executions: []string{spec.ID + "-e1"}}, nil
	}
	return o, nil
}

type fakeTraces struct {
	rec        ExecutionRecord
	traces     []Trace
	err        error
	execs      []string
	execsErr   error
	askedTasks []string
	states     map[string][]byte
	stateErr   error
}

func (f *fakeTraces) StateSnapshot(_ context.Context, executionID string) ([]byte, error) {
	if f.stateErr != nil {
		return nil, f.stateErr
	}
	return f.states[executionID], nil
}

func (f *fakeTraces) Executions(_ context.Context, taskID string) ([]string, error) {
	f.askedTasks = append(f.askedTasks, taskID)
	if f.execsErr != nil {
		return nil, f.execsErr
	}
	if f.execs != nil {
		return f.execs, nil
	}
	return []string{taskID + "-e1"}, nil
}

func (f *fakeTraces) Assemble(_ context.Context, _, _ string) (ExecutionRecord, []Trace, error) {
	if f.err != nil {
		return ExecutionRecord{}, nil, f.err
	}
	return f.rec, f.traces, nil
}

func validScope() membench.RunScope {
	return membench.RunScope{
		Database:         "agentbench_local",
		Confirmation:     "agentbench_local",
		ProjectID:        "bench",
		BenchmarkProject: "bench",
		SwarmID:          "bench",
	}
}

func validConfig() RunConfig {
	return RunConfig{
		RunID:           "r1",
		Arm:             baseArm(),
		Scope:           validScope(),
		PreRegistration: validPreReg(),
		Power:           PowerCheck{SigmaD: 0.02, SigmaN: 12, AvailablePairs: 20, Adequate: true},
		Tasks:           []TaskSpec{{ID: "t1", Name: "first"}},
		Repeats:         1,
	}
}

// The guard is the first statement: no task may be submitted before the scope
// is authorised.
func TestRunner_GuardRunsBeforeAnyTaskIsSubmitted(t *testing.T) {
	tasks := &fakeTasks{}
	r := &Runner{Tasks: tasks, Traces: &fakeTraces{}}

	cfg := validConfig()
	cfg.Scope.Confirmation = "" // invalid

	if _, err := r.Run(context.Background(), cfg); err == nil {
		t.Fatal("ran without authorisation")
	}
	if len(tasks.calls) != 0 {
		t.Errorf("submitted %d task(s) before the guard authorised the run", len(tasks.calls))
	}
}

func TestRunner_RefusesAnInvalidPreRegistration(t *testing.T) {
	tasks := &fakeTasks{}
	r := &Runner{Tasks: tasks, Traces: &fakeTraces{}}

	cfg := validConfig()
	cfg.PreRegistration.Arms = []string{"only-one"}

	if _, err := r.Run(context.Background(), cfg); err == nil {
		t.Fatal("ran on a pre-registration that commits to no comparison")
	}
	if len(tasks.calls) != 0 {
		t.Error("submitted tasks despite an invalid pre-registration")
	}
}

// A run with nothing in it would journal a clean sheet and report it as a pass.
func TestRunner_RefusesAnEmptyTaskSet(t *testing.T) {
	r := &Runner{Tasks: &fakeTasks{}, Traces: &fakeTraces{}}
	cfg := validConfig()
	cfg.Tasks = nil

	if _, err := r.Run(context.Background(), cfg); err == nil {
		t.Fatal("ran an empty task set")
	}
}

func TestRunner_RefusesInvalidScoringPolicyBeforeSubmittingAnything(t *testing.T) {
	tasks := &fakeTasks{}
	r := &Runner{Tasks: tasks, Traces: &fakeTraces{}}
	cfg := validConfig()
	cfg.Tasks[0].Scoring = &quality.ScoringPolicy{
		Kind: quality.ScoreKind("future"), ProducerStep: "analyze", VerifierStep: "test",
	}
	if _, err := r.Run(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("want unsupported scoring policy refusal, got %v", err)
	}
	if len(tasks.calls) != 0 {
		t.Fatal("spent a benchmark task before validating its scoring contract")
	}
}

// Verdicts are scored in-run and journaled, because tool_audit_log expires.
func TestRunner_ScoresProbesInRunAndJournalsTheVerdicts(t *testing.T) {
	traces := &fakeTraces{
		rec: ExecutionRecord{CostUSD: 0.25, PromptTokens: 100, CompletionTokens: 20},
		traces: []Trace{{
			ExecutionID: "t1-e1", StepID: "s1", Role: "worker",
			Requested: []string{"a", "b"}, Accepted: []string{"a"}, Invoked: []string{"a"},
			Outcomes: []StepOutcome{{StepID: "s1", Role: "worker", Outcome: OutcomeOK, Attempt: 1}},
			Calls:    []ToolCall{{Name: "a", Role: "worker"}},
		}},
	}
	r := &Runner{
		Tasks:  &fakeTasks{},
		Traces: traces,
		Probes: []Probe{GrantProbe{}, SchemaProbe{}, ToolUseProbe{}},
	}

	cfg := validConfig()
	cfg.Gold = &GoldManifest{Entries: []Gold{{TaskID: "t1", Paths: [][]string{{"a"}}}}}

	j, err := r.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(j.Records) != 1 {
		t.Fatalf("records = %d, want 1", len(j.Records))
	}
	if got := len(j.Records[0].Verdicts); got != 3 {
		t.Fatalf("verdicts = %d, want one per probe", got)
	}
	if j.Records[0].CostUSD != 0.25 {
		t.Errorf("the store's cost was dropped: %+v", j.Records[0])
	}
	if j.Manifest.PreRegistrationHash == "" {
		t.Error("the pre-registration hash was not journaled")
	}
}

func TestRunner_JournalsOneTaskScorePerRepeat(t *testing.T) {
	traces := &fakeTraces{states: map[string][]byte{"t1-e1": pinnedSnapshot("passed", "failed")}}
	r := &Runner{Tasks: &fakeTasks{}, Traces: traces}
	cfg := validConfig()
	cfg.Tasks[0].Scoring = pinnedPolicy()
	cfg.Repeats = 2

	j, err := r.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(j.TaskScores) != 2 || j.TaskScores[0].Repeat != 1 || j.TaskScores[1].Repeat != 2 {
		t.Fatalf("task scores = %#v", j.TaskScores)
	}
	if j.TaskScores[0].Score != .5 {
		t.Fatalf("score = %v, want .5", j.TaskScores[0].Score)
	}
}

func TestRunner_ScoresTheNewestExecutionThatReachedTheVerifier(t *testing.T) {
	traces := &fakeTraces{
		execs: []string{"root-failed", "retry-verified"},
		states: map[string][]byte{
			"root-failed":    []byte(`{"stepResults":{}}`),
			"retry-verified": pinnedSnapshot("passed", "manual"),
		},
	}
	tasks := &fakeTasks{outcomes: map[string]TaskOutcome{
		"t1": {TaskID: "ledger-t1", Succeeded: true},
	}}
	r := &Runner{Tasks: tasks, Traces: traces}
	cfg := validConfig()
	cfg.Tasks[0].Scoring = pinnedPolicy()

	j, err := r.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(j.TaskScores) != 1 || j.TaskScores[0].Score != 1 {
		t.Fatalf("task scores = %#v", j.TaskScores)
	}
	if got := j.TaskScores[0].ExecutionIDs; len(got) != 2 || got[0] != "root-failed" || got[1] != "retry-verified" {
		t.Fatalf("execution provenance = %v", got)
	}
}

func TestRunner_MarksUnreadableScoreSnapshotUntrustworthy(t *testing.T) {
	traces := &fakeTraces{stateErr: errors.New("ledger unavailable")}
	r := &Runner{Tasks: &fakeTasks{}, Traces: traces}
	cfg := validConfig()
	cfg.Tasks[0].Scoring = pinnedPolicy()

	j, err := r.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !j.Manifest.Untrustworthy || !strings.Contains(j.Manifest.UntrustworthyReason, "ledger unavailable") {
		t.Fatalf("corrupt score run remained publishable: %#v", j.Manifest)
	}
}

// Without gold, the two probes whose ground truth is configuration still run.
// That is the whole point of them needing no recording.
func TestRunner_RunsGoldFreeProbesWithNoGoldSet(t *testing.T) {
	traces := &fakeTraces{
		traces: []Trace{{
			ExecutionID: "t1-e1", StepID: "s1", Role: "worker",
			Outcomes: []StepOutcome{{StepID: "s1", Role: "worker", Outcome: OutcomeOK, Attempt: 1}},
			Calls:    []ToolCall{{Name: "a", Role: "worker"}},
		}},
	}
	r := &Runner{
		Tasks:  &fakeTasks{},
		Traces: traces,
		Probes: []Probe{GrantProbe{}, SchemaProbe{}, ToolUseProbe{}},
	}

	cfg := validConfig() // no Gold
	j, err := r.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	got := map[string]bool{}
	for _, v := range j.Records[0].Verdicts {
		got[v.Probe] = true
	}
	if got["tool-grant"] {
		t.Error("the gold-dependent probe scored without gold")
	}
	if !got["schema-following"] || !got["tool-use"] {
		t.Errorf("a gold-free probe was skipped: %v — a run with no gold must still "+
			"produce these", got)
	}
}

// An exclusion nobody can see is indistinguishable from a task that was never in
// the set.
func TestRunner_RecordsAnExcludedTaskRatherThanSkippingIt(t *testing.T) {
	tasks := &fakeTasks{}
	r := &Runner{Tasks: tasks, Traces: &fakeTraces{}, Probes: []Probe{GrantProbe{}}}

	cfg := validConfig()
	cfg.Gold = &GoldManifest{Entries: []Gold{{
		TaskID: "t1", Excluded: true,
		ExcludedReason: "the unrestricted-ceiling arm never passed this task",
	}}}

	j, err := r.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(tasks.calls) != 0 {
		t.Error("submitted a task that has no ground truth")
	}
	if len(j.Records) != 1 {
		t.Fatalf("records = %d, want the exclusion recorded", len(j.Records))
	}
	if !strings.Contains(j.Records[0].ErrorText, "excluded from gold") {
		t.Errorf("exclusion not recorded readably: %q", j.Records[0].ErrorText)
	}
	// And it classifies as harness, so it never counts against the agent.
	if got := ClassifyFailure(false, j.Records[0].ErrorText); got != FailureHarness {
		t.Errorf("an excluded task classified as %q, want harness", got)
	}
}

// A trace we could not assemble is OUR failure, not the agent's.
func TestRunner_TraceAssemblyFailureIsAHarnessFailure(t *testing.T) {
	r := &Runner{
		Tasks:  &fakeTasks{},
		Traces: &fakeTraces{err: errors.New("db closed")},
		Probes: []Probe{SchemaProbe{}},
	}

	j, err := r.Run(context.Background(), validConfig())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	rec := j.Records[0]
	if got := ClassifyFailure(rec.Succeeded, rec.ErrorText); got != FailureHarness {
		t.Errorf("classified as %q, want harness — counting our own failure against the "+
			"agent inflates exactly the number this benchmark exists to report honestly", got)
	}
	if len(j.TaskRuns) != 1 || j.TaskRuns[0].Succeeded ||
		ClassifyFailure(j.TaskRuns[0].Succeeded, j.TaskRuns[0].ErrorText) != FailureHarness {
		t.Fatalf("task-level calibration evidence hid the trace failure: %+v", j.TaskRuns)
	}
}

func TestRunner_SubmissionFailureIsAHarnessFailure(t *testing.T) {
	r := &Runner{
		Tasks:  &fakeTasks{err: errors.New("daemon unreachable")},
		Traces: &fakeTraces{},
		Probes: []Probe{SchemaProbe{}},
	}

	j, err := r.Run(context.Background(), validConfig())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := ClassifyFailure(false, j.Records[0].ErrorText); got != FailureHarness {
		t.Errorf("classified as %q, want harness", got)
	}
}

// A run that mostly broke the harness describes the harness, not the system.
// Marked rather than discarded: throwing it away would discard the evidence of
// why it is untrustworthy.
func TestRunner_MarksARunThatMostlyFailedInTheHarness(t *testing.T) {
	r := &Runner{
		Tasks:  &fakeTasks{err: errors.New("daemon unreachable")},
		Traces: &fakeTraces{},
		Probes: []Probe{SchemaProbe{}},
	}

	cfg := validConfig()
	cfg.Tasks = []TaskSpec{{ID: "t1"}, {ID: "t2"}, {ID: "t3"}}

	j, err := r.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !j.Manifest.Untrustworthy {
		t.Fatal("a run where every execution failed in the harness was presented as a result")
	}
	if len(j.Records) != 3 {
		t.Errorf("records = %d — the evidence was discarded along with the verdict", len(j.Records))
	}
	if err := j.CheckReadable(); err == nil {
		t.Error("an untrustworthy run reported itself readable")
	}
}

func TestRunner_RepeatsEachTask(t *testing.T) {
	tasks := &fakeTasks{}
	r := &Runner{Tasks: tasks, Traces: &fakeTraces{}, Probes: []Probe{SchemaProbe{}}}

	cfg := validConfig()
	cfg.Repeats = 3

	if _, err := r.Run(context.Background(), cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(tasks.calls) != 3 {
		t.Errorf("submitted %d times, want 3", len(tasks.calls))
	}
}

func TestRunner_JournalsOneTaskRunPerRepeatRatherThanPerExecution(t *testing.T) {
	tasks := &fakeTasks{outcomes: map[string]TaskOutcome{"t1": {
		TaskID: "ledger-task", Succeeded: true, Executions: []string{"e1", "e2"},
	}}}
	r := &Runner{Tasks: tasks, Traces: &fakeTraces{}, Probes: []Probe{SchemaProbe{}}}
	cfg := validConfig()
	cfg.Tasks[0].Tier = TaskTierTripwire
	cfg.Repeats = 2

	j, err := r.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(j.TaskRuns) != 2 {
		t.Fatalf("task runs = %d, want 2 despite %d execution records", len(j.TaskRuns), len(j.Records))
	}
	for i, run := range j.TaskRuns {
		if run.TaskID != "t1" || run.Repeat != i+1 || len(run.ExecutionIDs) != 2 {
			t.Fatalf("task run %d = %+v", i, run)
		}
	}
	if j.Manifest.TaskTiers["t1"] != TaskTierTripwire {
		t.Fatalf("journal lost tier map: %+v", j.Manifest.TaskTiers)
	}
}

func TestRunner_RefusesWithoutCollaborators(t *testing.T) {
	r := &Runner{Probes: []Probe{SchemaProbe{}}}
	if _, err := r.Run(context.Background(), validConfig()); err == nil {
		t.Fatal("ran with no task runner and no trace store")
	}
}

// Two arms must submit in the same order or a paired comparison does not line up.
func TestSortTasks_IsDeterministic(t *testing.T) {
	got := SortTasks([]TaskSpec{{ID: "c"}, {ID: "a"}, {ID: "b"}})
	if got[0].ID != "a" || got[1].ID != "b" || got[2].ID != "c" {
		t.Errorf("order = %v, want a b c", []string{got[0].ID, got[1].ID, got[2].ID})
	}
}

// Gold records what a TASK needed; grants are recorded per (execution, step).
// Scoring each step against the whole task's path asks every step to have
// granted everything, which is why the first validated run produced path
// coverage 0.000 with a core miss on all six steps — five never called
// grant_step_tools at all.
func TestRunner_ScoresGrantsPerExecutionNotPerStep(t *testing.T) {
	traces := &fakeTraces{
		traces: []Trace{
			{ExecutionID: "e1", StepID: "plan", Role: "lead",
				Requested: []string{"a"}, Accepted: []string{"a"}, Invoked: []string{"a"}},
			{ExecutionID: "e1", StepID: "implement", Role: "coder",
				Invoked: []string{"b"}}, // never called grant_step_tools
			{ExecutionID: "e1", StepID: "review", Role: "reviewer",
				Requested: []string{"b"}, Accepted: []string{"b"}, Invoked: []string{"b"}},
		},
	}
	r := &Runner{Tasks: &fakeTasks{}, Traces: traces, Probes: []Probe{GrantProbe{}}}

	cfg := validConfig()
	cfg.Gold = &GoldManifest{Entries: []Gold{{TaskID: "t1", Paths: [][]string{{"a", "b"}}}}}

	j, err := r.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	var grants []Verdict
	for _, v := range j.Records[0].Verdicts {
		if v.Probe == grantProbeName {
			grants = append(grants, v)
		}
	}
	if len(grants) != 1 {
		t.Fatalf("grant verdicts = %d, want exactly one per execution", len(grants))
	}
	// Merged across steps, {a} and {b} together cover the task's path.
	if grants[0].PathCoverage != 1.0 {
		t.Errorf("path coverage = %v, want 1.0 — the execution as a whole was granted "+
			"everything the task needed", grants[0].PathCoverage)
	}
	if grants[0].CoreMiss {
		t.Error("core miss reported when the execution covered the whole path")
	}
}

func TestMergeTraces_UnionsAcrossSteps(t *testing.T) {
	got := MergeTraces([]Trace{
		{ExecutionID: "e1", Accepted: []string{"a"}, Escalations: 1, ToolBudget: 10, ToolCallsUsed: 3},
		{ExecutionID: "e1", Accepted: []string{"a", "b"}, Escalations: 2, ToolBudget: 40, ToolCallsUsed: 5},
	})
	if len(got.Accepted) != 2 {
		t.Errorf("accepted = %v, want deduped [a b]", got.Accepted)
	}
	if got.Escalations != 3 {
		t.Errorf("escalations = %d, want 3 summed", got.Escalations)
	}
	if got.ToolBudget != 40 || got.ToolCallsUsed != 8 {
		t.Errorf("budget=%d used=%d, want 40 and 8", got.ToolBudget, got.ToolCallsUsed)
	}
}

func infraRec(msg string) []ExecutionRecord {
	return []ExecutionRecord{{TaskID: "t", Succeeded: false, ErrorText: msg}}
}

// A spent allowance makes every remaining call fail identically. Continuing
// burns a prepaid quota — measured in DAYS to reset, not dollars — to journal a
// wall of failures that say nothing about the system under test.
func TestUpdateInfraStreak(t *testing.T) {
	const quota = "quota exceeded for this billing period"

	t.Run("infra failures accumulate", func(t *testing.T) {
		streak := 0
		for i := 1; i <= 3; i++ {
			streak = updateInfraStreak(streak, infraRec(quota))
			if streak != i {
				t.Fatalf("after %d infra failures streak = %d", i, streak)
			}
		}
	})

	t.Run("any success resets it", func(t *testing.T) {
		streak := updateInfraStreak(2, []ExecutionRecord{{TaskID: "t", Succeeded: true}})
		if streak != 0 {
			t.Errorf("streak = %d after a success; the provider is demonstrably answering", streak)
		}
	})

	// Otherwise an alternating infra/task pattern — which is still a provider
	// problem — would never trip the breaker.
	t.Run("a task failure neither advances nor clears it", func(t *testing.T) {
		streak := updateInfraStreak(2, infraRec("schema validation failed: missing field"))
		if streak != 2 {
			t.Errorf("streak = %d, want it held at 2", streak)
		}
	})
}

// One blip is weather; three in a row is a wall. A run that gave up on the
// first would be useless.
func TestConsecutiveInfraFailuresBeforeAbort_IsNotOne(t *testing.T) {
	if consecutiveInfraFailuresBeforeAbort < 2 {
		t.Fatalf("breaker trips at %d: a single provider blip would abort every run",
			consecutiveInfraFailuresBeforeAbort)
	}
}
