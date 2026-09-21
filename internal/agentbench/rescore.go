package agentbench

import (
	"context"
	"fmt"
	"strconv"
)

// TraceReader re-reads an execution's traces from the ledger.
type TraceReader interface {
	AssembleTraces(ctx context.Context, executionID string) ([]Trace, error)
}

// Rescore re-runs the probes over a completed journal's traces under the
// CURRENT scoring contract, without re-running a single agent.
//
// Why this exists. HarnessVersion is bumped whenever a probe's DEFINITION
// changes, which correctly makes every earlier figure incomparable — and, until
// now, meant re-running the pass to get a comparable one. A pass costs hours
// and, on a prepaid allowance, days of waiting for a quota reset. The evidence
// a probe reads is already in the ledger, so the honest fix is to re-score the
// evidence rather than re-buy it. Introduced when v3's substitution rule
// stranded a v2 chunk that had cost ~9% of a monthly allowance.
//
// What it does NOT touch: cost, tokens, success, error text, and every arm axis
// describing the RUN — binary, config, models, policy, task set, gold. Those are
// facts about an execution that happened, and re-scoring does not make it happen
// differently. Only the harness version moves, because only the scoring did.
//
// REFUSES on any execution whose traces are gone. A ledger has a retention
// window, and a re-score that quietly dropped the expired executions would
// return a smaller, cleaner-looking journal that reads exactly like a complete
// one — the same class of silent-truncation error the abort path is marked for.
func Rescore(ctx context.Context, j Journal, traces TraceReader, probes []Probe, gold *GoldManifest) (Journal, error) {
	return RescoreWithTasks(ctx, j, traces, probes, gold, nil)
}

// RescoreWithTasks is Rescore plus the task set, which is what the TASK SCORES
// need: a score's policy (producer step, verifier step, kind) lives in the task
// spec, not in the journal.
//
// Task scores used to pass through verbatim while the harness stamp moved,
// which was harmless only while every contract change was a PROBE change. The
// 2026-09-17 extras rule changed pinned_case_validation, so a v5 journal
// rescored to v6 kept its v5 metric under a v6 claim — verified on the
// 2026.9.4 arm, where dp-01-nilguard rescored 5->6 and kept
// 0.000/invalid_evidence while the current scorer on the same stored snapshot
// returns 1.000 with three extras counted.
//
// A journal that carries task scores is therefore re-scored WITH them or not at
// all: supplying no task set is refused rather than silently passing them on.
func RescoreWithTasks(ctx context.Context, j Journal, traces TraceReader, probes []Probe, gold *GoldManifest, tasks []TaskSpec) (Journal, error) {
	if traces == nil {
		return Journal{}, fmt.Errorf("re-scoring needs a trace store")
	}
	if len(probes) == 0 {
		return Journal{}, fmt.Errorf("re-scoring needs at least one probe")
	}
	if j.Manifest.Arm.HarnessVersion == HarnessVersion {
		return Journal{}, fmt.Errorf("journal %q was already scored by harness %s: "+
			"re-scoring it would change nothing and overwrite the original",
			j.Manifest.RunID, HarnessVersion)
	}
	if reason := rescoreInputGap(j.Manifest.Arm.HarnessVersion, len(j.TaskScores) > 0); reason != "" {
		return Journal{}, fmt.Errorf("journal %q was scored by harness %s and cannot be "+
			"re-scored by harness %s: %s",
			j.Manifest.RunID, j.Manifest.Arm.HarnessVersion, HarnessVersion, reason)
	}

	out := j
	out.Manifest.Arm.HarnessVersion = HarnessVersion
	out.Records = make([]ExecutionRecord, 0, len(j.Records))

	r := &Runner{Probes: probes}
	for _, rec := range j.Records {
		if rec.ExecutionID == "" {
			// A record with no execution — an excluded task, or a submission
			// that never produced one. It carries no verdicts to re-score, so
			// it passes through rather than failing the whole re-score.
			out.Records = append(out.Records, rec)
			continue
		}
		found, err := traces.AssembleTraces(ctx, rec.ExecutionID)
		if err != nil {
			return Journal{}, fmt.Errorf("re-score %s (task %s): %w — the ledger no longer "+
				"holds this execution, so the pass cannot be re-scored in full",
				rec.ExecutionID, rec.TaskID, err)
		}
		if len(found) == 0 {
			return Journal{}, fmt.Errorf("re-score %s (task %s): no traces in the ledger; "+
				"re-scoring would silently drop it and report a smaller journal as complete",
				rec.ExecutionID, rec.TaskID)
		}
		rec.Verdicts = r.score(ctx, RunConfig{Gold: gold}, TaskSpec{ID: rec.TaskID}, found)
		out.Records = append(out.Records, rec)
	}

	// Re-derived, not carried over: the old reason was a judgement made under
	// the old scoring, and the new scoring may reach a different one.
	out.Manifest.Untrustworthy = false
	out.Manifest.UntrustworthyReason = ""
	if reason := untrustworthyReason(out.Records); reason != "" {
		out.Manifest.Untrustworthy = true
		out.Manifest.UntrustworthyReason = reason
	}
	// The arm key is denormalised in the manifest; leaving the old one would
	// let a reader believe a v3 journal is comparable with v2 figures.
	out.Manifest.ArmKey = out.Manifest.Arm.Key()
	out.Manifest.ArmPartial = out.Manifest.Arm.Partial()
	// Task scores: recomputed from the stored execution snapshots, never copied.
	if len(j.TaskScores) > 0 {
		scores, err := rescoreTaskScores(ctx, j, traces, tasks)
		if err != nil {
			return Journal{}, err
		}
		out.TaskScores = scores
	}

	return out, nil
}

// rescoreTaskScores recomputes every journalled task score from the execution
// snapshot it was originally scored from, using the same selection rule the
// runner applies: the newest execution that reached the verifier wins, and
// failing that the root supplies the fail-closed verdict.
//
// Split out of RescoreWithTasks for the complexity ratchet, and it reads better
// here: the probe half and the task-score half share only the journal.
func rescoreTaskScores(ctx context.Context, j Journal, traces TraceReader, tasks []TaskSpec) ([]TaskScore, error) {
	specs := make(map[string]TaskSpec, len(tasks))
	for _, t := range tasks {
		specs[t.ID] = t
	}
	stateStore, ok := traces.(ExecutionStateStore)
	if !ok {
		return nil, fmt.Errorf("journal %q carries %d task score(s) but the trace store "+
			"cannot read execution state snapshots, so they cannot be recomputed",
			j.Manifest.RunID, len(j.TaskScores))
	}
	out := make([]TaskScore, 0, len(j.TaskScores))
	for _, ts := range j.TaskScores {
		spec, known := specs[ts.TaskID]
		if !known || spec.Scoring == nil {
			return nil, fmt.Errorf("task %q has a journalled score but no scoring policy in "+
				"the supplied task set: its score defines the release metric and cannot be "+
				"carried forward unverified", ts.TaskID)
		}
		snapshot, err := rescoreSnapshotFor(ctx, stateStore, ts, spec)
		if err != nil {
			return nil, err
		}
		next, err := ScoreTask(ts.TaskID, ts.Repeat, spec.Scoring, ts.ExecutionIDs, snapshot)
		if err != nil {
			return nil, fmt.Errorf("re-score task %s repeat %d: %w", ts.TaskID, ts.Repeat, err)
		}
		out = append(out, next)
	}
	return out, nil
}

// rescoreSnapshotFor picks the execution snapshot a task score is computed
// from, mirroring Runner.scoreTaskRepeat.
func rescoreSnapshotFor(ctx context.Context, store ExecutionStateStore, ts TaskScore, spec TaskSpec) ([]byte, error) {
	var snapshot []byte
	for i, id := range ts.ExecutionIDs {
		candidate, err := store.StateSnapshot(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("re-score task %s repeat %d from execution %s: %w",
				ts.TaskID, ts.Repeat, id, err)
		}
		if i == 0 {
			snapshot = candidate
		}
		hasVerifier, err := snapshotHasStepResult(candidate, spec.Scoring.VerifierStep)
		if err != nil {
			return nil, fmt.Errorf("re-score task %s repeat %d from execution %s: "+
				"decode state snapshot: %w", ts.TaskID, ts.Repeat, id, err)
		}
		if hasVerifier {
			snapshot = candidate
		}
	}
	return snapshot, nil
}

// MinRescorableHarness is the oldest harness whose journals the CURRENT
// harness can re-score.
//
// It exists because "already at this version" was the only refusal, and
// everything older was re-stamped — which is correct exactly while a harness
// bump changes the SCORING RULE over inputs every journal still carries. It
// stops being correct the moment a bump changes the INPUTS.
//
// That is what D3' does. Harness 7 assembles its numerator from the per-visit
// result bodies in `execution_step_outcomes`, and a harness-6 journal does not
// carry them: they live in the ledger, and the bench database is wiped by the
// next arm. Re-stamping such a journal would produce a harness-7 number from
// inputs harness 7 never saw — worse than refusing, because the figure would
// look comparable with real harness-7 figures and would not be.
//
// The previous design text asserted this refusal already existed. It did not;
// `review-20260919-43d1` F6 caught the assertion, and this is the refusal it
// was asserting.
const MinRescorableHarness = "8"

// minRescorableHarnessReason is WHY the floor sits where it does, declared
// beside it so that whoever raises one is looking at the other.
//
// It exists because the refusal used to hard-code the 6->7 rationale into a
// sentence parameterised by version: "harness N assembles the pinned-case
// numerator from the per-visit result bodies, which a journal at harness M
// does not carry". True of a harness-6 journal, FALSE of a harness-7 one —
// harness 7 is the version that introduced per-visit accumulation, so a
// harness-7 journal carries exactly what the message said it lacked. Raising
// the floor to 8 would have refused correctly, by version, for a stated reason
// that is a lie, and sent the reader looking for a storage problem that does
// not exist.
//
// So the reason is a property of the FLOOR, not of the refusal. Raise the
// floor, change this string: the test asserts the refusal carries it verbatim,
// which is what makes forgetting visible.
const minRescorableHarnessReason = "harness 8 bounds every agent container's memory, so a " +
	"journal written before it recorded a run that could consume the whole host; re-stamping " +
	"it as harness 8 would claim a resource envelope the run never had"

// rescoreInputGap reports why a journal at the given harness version cannot be
// re-scored by the current one, or "" when it can.
//
// It names the missing INPUT rather than the version gap, because "harness 6
// journals cannot be re-scored" tells an operator nothing they can act on,
// while "the per-visit bodies are not in the journal" tells them the figure
// they wanted is not recoverable and a fresh arm is the only route to it.
//
// THE GAP IS SCOPED TO TASK SCORES, and that scoping is the whole correctness
// of it. Probe verdicts are re-derived from TRACES, which the ledger still
// holds and which no harness bump has changed the shape of — so a probe-only
// journal crosses the v7 boundary perfectly well, and refusing it would throw
// away the one thing re-scoring is genuinely for (applying a corrected probe
// to a pass that already cost money to run). It is the pinned-case numerator
// that needs per-visit bodies, and only a journal carrying task scores has
// one.
//
// A first draft refused on version alone, which made every historical journal
// unrescorable and three existing tests fail for a reason that was not true of
// them.
func rescoreInputGap(journalHarness string, carriesTaskScores bool) string {
	if !carriesTaskScores {
		return ""
	}
	if journalHarness == "" {
		return "the journal declares no harness version, so what it was scored " +
			"against — and therefore what re-scoring its task scores would mean — is unknown"
	}
	// NUMERIC, not lexicographic. The versions are decimal strings, so a
	// string compare says "10" < "7" and would silently start re-scoring the
	// very journals this refuses the moment the counter reaches two digits —
	// a bug that cannot be caught by any test written before then unless the
	// test is written now. It is.
	journal, jErr := strconv.Atoi(journalHarness)
	floor, fErr := strconv.Atoi(MinRescorableHarness)
	if jErr != nil || fErr != nil {
		return "harness version " + journalHarness + " is not a number, so it cannot be " +
			"compared against the re-scorable floor (" + MinRescorableHarness + ")"
	}
	if journal < floor {
		// States the boundary FIRST (which crossing refused, and at what
		// floor) and the reason SECOND, from the floor's own declaration —
		// so the sentence stays true when the floor moves.
		// "must not", not "cannot": at some boundaries the inputs are absent
		// and at others they are present but describe a different run. Both
		// refuse; only the first is an impossibility, and claiming the wrong
		// one is how the previous message came to be false.
		return "journal harness " + journalHarness + " is below the re-scorable floor (" +
			MinRescorableHarness + ") for current harness " + HarnessVersion + ": " +
			minRescorableHarnessReason + ". Its probe verdicts could still be re-derived " +
			"from traces, but its TASK SCORES must not be re-stamped under this harness — " +
			"run a fresh arm instead"
	}
	return ""
}
