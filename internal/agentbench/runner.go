package agentbench

import (
	"context"
	"fmt"
	"sort"
	"time"

	"vornik.io/vornik/internal/membench"
)

// The run executor (§10 step 7).
//
// Submits the task set, waits for each task, assembles its trace from the
// ledger, scores every probe WHILE THE TRACE STILL EXISTS, and journals the
// verdicts. The 30-day retention on tool_audit_log is why scoring happens here
// rather than in a later pass.
//
// The two collaborators are interfaces because the run logic must be testable
// without a daemon or a database. A runner that could only be exercised against
// live infrastructure would be tested rarely and therefore wrongly.

// TaskSpec is one benchmark task to run.
type TaskSpec struct {
	ID       string
	Name     string
	Workflow string
	Prompt   string
}

// TaskOutcome is what the daemon reported for a submitted task.
type TaskOutcome struct {
	TaskID    string
	Succeeded bool
	ErrorText string
	// Executions lists the execution ids this task produced, so traces can be
	// assembled for each.
	Executions []string
}

// TaskRunner submits a task and waits for it to reach a terminal state.
type TaskRunner interface {
	Run(ctx context.Context, spec TaskSpec) (TaskOutcome, error)
}

// TraceStore assembles the recorded evidence for one execution.
//
// Returns one Trace per step: grants are scoped to (execution_id, step_id), and
// collapsing them to one trace per execution would average a lead's per-step
// decisions into a number no decision corresponds to.
type TraceStore interface {
	Assemble(ctx context.Context, taskID, executionID string) (ExecutionRecord, []Trace, error)
	// Executions lists what a task produced. The runner asks the STORE rather
	// than trusting the submitter: the companion status surface does not report
	// execution ids, and a submitter that cannot name them would otherwise leave
	// the run silently unmeasured.
	Executions(ctx context.Context, taskID string) ([]string, error)
}

// RunConfig is everything one arm's run needs.
type RunConfig struct {
	RunID string
	Arm   ArmFields
	Scope membench.RunScope

	PreRegistration PreRegistration
	Power           PowerCheck

	Tasks []TaskSpec
	// Gold is optional. Without it the gold-dependent probes are skipped and
	// the run still produces schema-following and tool-use verdicts — which is
	// the whole point of those two needing no recording.
	Gold *GoldManifest
	// Repeats is how many times each task runs. Repeats reduce a task's
	// contribution to sigma_d; they do not add pairs.
	Repeats int

	// DaemonBuild is the commit the daemon under test was built from, recorded
	// beside the harness's own so a journal shows both halves of what produced
	// its numbers. Optional: an unidentifiable daemon is still worth running,
	// it is just worth saying so. See RunManifest.DaemonBuild.
	DaemonBuild string
}

// Runner executes one arm.
type Runner struct {
	Tasks  TaskRunner
	Traces TraceStore
	Probes []Probe
	// Now is injected so a journal's timing is reproducible under test.
	Now func() time.Time
}

// Run executes the arm and returns its journal.
//
// THE GUARD IS THE FIRST STATEMENT, before any task is submitted or any store is
// touched. An operator who is shown an error and fixes it must not be walked
// toward the wipe by a sequence of lesser complaints.
func (r *Runner) Run(ctx context.Context, cfg RunConfig) (Journal, error) {
	if err := membench.CheckRunScope(cfg.Scope); err != nil {
		return Journal{}, err
	}
	if err := cfg.PreRegistration.Validate(); err != nil {
		return Journal{}, err
	}
	if r.Tasks == nil || r.Traces == nil {
		return Journal{}, fmt.Errorf("runner needs both a task runner and a trace store")
	}
	if len(cfg.Tasks) == 0 {
		return Journal{}, fmt.Errorf("refusing to run an empty task set: a run with nothing " +
			"in it would journal a clean sheet and report it as a pass")
	}

	preRegHash, err := cfg.PreRegistration.Hash()
	if err != nil {
		return Journal{}, err
	}

	j := Journal{Manifest: RunManifest{
		RunID:               cfg.RunID,
		Arm:                 cfg.Arm,
		ArmKey:              cfg.Arm.Key(),
		ArmPartial:          cfg.Arm.Partial(),
		HarnessBuild:        HarnessBuild(),
		DaemonBuild:         cfg.DaemonBuild,
		PreRegistrationHash: preRegHash,
		PreRegistration:     cfg.PreRegistration,
		Power:               cfg.Power,
	}}

	repeats := cfg.Repeats
	if repeats < 1 {
		repeats = 1
	}

	// Consecutive infra failures mean the provider is down or the allowance is
	// spent, and every remaining task will fail the same way. Continuing burns
	// a paid allowance to journal a wall of failures that say nothing about the
	// system under test — and on a prepaid plan the cost of that is measured in
	// DAYS until reset, not dollars. Stop, and say why.
	infraStreak := 0

	for _, spec := range cfg.Tasks {
		if infraStreak >= consecutiveInfraFailuresBeforeAbort {
			break
		}
		if gold, ok := r.goldFor(cfg.Gold, spec.ID); ok && gold.Excluded {
			// Recorded, not silently skipped: an exclusion nobody can see is
			// indistinguishable from a task that was never in the set.
			j.Records = append(j.Records, ExecutionRecord{
				TaskID:    spec.ID,
				Succeeded: false,
				ErrorText: "excluded from gold: " + gold.ExcludedReason,
			})
			continue
		}
		for i := 0; i < repeats; i++ {
			records := r.runOnce(ctx, cfg, spec)
			j.Records = append(j.Records, records...)
			infraStreak = updateInfraStreak(infraStreak, records)
			if infraStreak >= consecutiveInfraFailuresBeforeAbort {
				break
			}
		}
	}
	if infraStreak >= consecutiveInfraFailuresBeforeAbort {
		// Marked, never silently truncated: a short journal that does not say it
		// was cut short reads as a complete pass over a smaller task set.
		j.Manifest.Untrustworthy = true
		j.Manifest.UntrustworthyReason = fmt.Sprintf(
			"aborted after %d consecutive infra failures: the provider was unavailable "+
				"or the allowance was spent, so the remaining tasks were not attempted",
			infraStreak)
		return j, nil
	}

	// A run whose executions mostly failed to yield evidence is not a result.
	// Marking rather than discarding keeps the evidence of WHY it is
	// untrustworthy, which a refusal would throw away along with the data.
	if reason := untrustworthyReason(j.Records); reason != "" {
		j.Manifest.Untrustworthy = true
		j.Manifest.UntrustworthyReason = reason
	}
	return j, nil
}

// runOnce runs a task and returns one record per execution it produced.
func (r *Runner) runOnce(ctx context.Context, cfg RunConfig, spec TaskSpec) []ExecutionRecord {
	outcome, err := r.Tasks.Run(ctx, spec)
	if err != nil {
		// The harness could not get an answer. That is OUR failure, and
		// ClassifyFailure files it as such rather than against the agent.
		return []ExecutionRecord{{
			TaskID:    spec.ID,
			Succeeded: false,
			ErrorText: fmt.Sprintf("could not assemble trace: submitting %q failed: %v", spec.ID, err),
		}}
	}

	// A submitter that can name its executions is believed; otherwise the ledger
	// is asked. Falling through to "no executions" would journal a task as
	// scored while measuring nothing.
	// The LEDGER's task id, which is the daemon's, not the benchmark's spec id.
	// Every ledger table keys on it; passing the spec id would match no rows and
	// report a run with zero cost and zero traces as clean.
	ledgerTaskID := outcome.TaskID
	if ledgerTaskID == "" {
		ledgerTaskID = spec.ID
	}

	executions := outcome.Executions
	if len(executions) == 0 {
		found, err := r.Traces.Executions(ctx, ledgerTaskID)
		if err != nil {
			return []ExecutionRecord{{
				TaskID:    spec.ID,
				Succeeded: false,
				ErrorText: fmt.Sprintf("could not assemble trace: listing executions for %q failed: %v", spec.ID, err),
			}}
		}
		executions = found
	}
	if len(executions) == 0 {
		return []ExecutionRecord{{
			TaskID:    spec.ID,
			Succeeded: false,
			ErrorText: fmt.Sprintf("could not assemble trace: task %q produced no executions", spec.ID),
		}}
	}

	var out []ExecutionRecord
	for _, execID := range executions {
		rec, traces, err := r.Traces.Assemble(ctx, ledgerTaskID, execID)
		if err != nil {
			out = append(out, ExecutionRecord{
				TaskID:      spec.ID,
				ExecutionID: execID,
				Succeeded:   false,
				ErrorText:   fmt.Sprintf("could not assemble trace for %s: %v", execID, err),
			})
			continue
		}
		// Labelled with the BENCHMARK's id — that is what pairs across arms —
		// while the query above used the ledger's.
		rec.TaskID = spec.ID
		rec.ExecutionID = execID
		// The daemon's verdict on the task wins over anything the store
		// inferred: the store sees rows, not whether the work was accepted.
		rec.Succeeded = outcome.Succeeded
		if !outcome.Succeeded && rec.ErrorText == "" {
			rec.ErrorText = outcome.ErrorText
		}
		rec.Verdicts = r.score(ctx, cfg, spec, traces)
		out = append(out, rec)
	}
	return out
}

// score runs every probe over every step trace, in-run, because the evidence
// expires.
//
// A probe that errors on a trace is SKIPPED rather than failing the record: the
// commonest cause is a gold entry that does not apply, and dropping the whole
// execution would discard the other probes' verdicts with it.
func (r *Runner) score(ctx context.Context, cfg RunConfig, spec TaskSpec, traces []Trace) []Verdict {
	gold, _ := r.goldFor(cfg.Gold, spec.ID)
	ref := TaskRef{ID: spec.ID, Name: spec.Name}

	// GRANTS ARE SCORED PER EXECUTION, everything else per step.
	//
	// Gold records the tools a TASK needed; grants are recorded per (execution,
	// step). Scoring each step against the whole task's path is a category
	// error — it asks every step to have granted everything the task needed —
	// and the first validated run showed the consequence: path coverage 0.000
	// with a core miss on all six steps, because five of them never called
	// grant_step_tools at all.
	merged := MergeTraces(traces)

	var out []Verdict
	for _, p := range r.Probes {
		if p.Name() == grantProbeName {
			v, err := p.Score(ctx, ref, gold, merged)
			if err == nil {
				out = append(out, v)
			}
			continue
		}
		for _, tr := range traces {
			v, err := p.Score(ctx, ref, gold, tr)
			if err != nil {
				continue
			}
			out = append(out, v)
		}
	}
	return out
}

// MergeTraces folds an execution's per-step traces into one.
//
// Grant decisions across an execution's steps together answer "did this
// execution end up with the tools the task needed", which is the question gold
// can actually answer. The per-step traces stay available for the probes whose
// unit genuinely is the step.
func MergeTraces(traces []Trace) Trace {
	if len(traces) == 0 {
		return Trace{}
	}
	out := Trace{ExecutionID: traces[0].ExecutionID}
	for _, t := range traces {
		out.Requested = append(out.Requested, t.Requested...)
		out.Accepted = append(out.Accepted, t.Accepted...)
		out.Refused = append(out.Refused, t.Refused...)
		out.Invoked = append(out.Invoked, t.Invoked...)
		out.Calls = append(out.Calls, t.Calls...)
		out.Outcomes = append(out.Outcomes, t.Outcomes...)
		out.Escalations += t.Escalations
		out.ToolCallsUsed += t.ToolCallsUsed
		if t.ToolBudget > out.ToolBudget {
			out.ToolBudget = t.ToolBudget
		}
		out.Stalled = out.Stalled || t.Stalled
	}
	out.Requested = dedupe(out.Requested)
	out.Accepted = dedupe(out.Accepted)
	out.Refused = dedupe(out.Refused)
	out.Invoked = dedupe(out.Invoked)
	return out
}

func (r *Runner) goldFor(m *GoldManifest, taskID string) (Gold, bool) {
	if m == nil {
		return Gold{}, false
	}
	return m.Lookup(taskID)
}

// untrustworthyMajority is the share of harness failures past which a run's
// figures stop describing the system under test and start describing the
// harness.
const untrustworthyMajority = 0.5

func untrustworthyReason(records []ExecutionRecord) string {
	if len(records) == 0 {
		return "no executions were recorded"
	}
	harness := 0
	for _, rec := range records {
		if ClassifyFailure(rec.Succeeded, rec.ErrorText) == FailureHarness {
			harness++
		}
	}
	if float64(harness)/float64(len(records)) > untrustworthyMajority {
		return fmt.Sprintf("%d of %d executions failed in the harness rather than the "+
			"system under test, so these figures describe the benchmark", harness, len(records))
	}
	return ""
}

// SortTasks orders a task set deterministically, so two arms submit in the same
// order and a paired comparison lines up.
func SortTasks(tasks []TaskSpec) []TaskSpec {
	out := append([]TaskSpec(nil), tasks...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// consecutiveInfraFailuresBeforeAbort is how many infra failures in a row end
// the run.
//
// Three, not one: a single provider blip is normal and a run that gave up on it
// would be useless. Three in a row is a wall, not weather.
const consecutiveInfraFailuresBeforeAbort = 3

// updateInfraStreak advances or resets the consecutive-infra counter for one
// task's records.
//
// ANY success resets it. The streak is about "is the provider answering at
// all", and a task that completed proves it is — even if a sibling execution in
// the same task failed on infra.
func updateInfraStreak(streak int, records []ExecutionRecord) int {
	sawInfra := false
	for _, rec := range records {
		if rec.Succeeded {
			return 0
		}
		if ClassifyFailure(rec.Succeeded, rec.ErrorText) == FailureInfra {
			sawInfra = true
		}
	}
	if !sawInfra {
		// A task failure is not evidence the provider is gone, so it neither
		// advances the streak nor clears it: an alternating infra/task pattern
		// is still a provider problem.
		return streak
	}
	return streak + 1
}
