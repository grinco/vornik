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
	return out, nil
}
