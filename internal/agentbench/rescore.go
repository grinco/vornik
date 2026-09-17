package agentbench

import (
	"context"
	"fmt"
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
