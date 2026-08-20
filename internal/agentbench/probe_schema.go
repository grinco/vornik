package agentbench

import (
	"context"
	"sort"
)

// Schema-following probe (§3.2, second probe).
//
// WHY THIS IS THE CHEAPEST PROBE IN THE DESIGN. Its ground truth is the role's
// DECLARED output schema — configuration, not a recording — so unlike the
// tool-grant probe it needs no unrestricted-ceiling pass, no gold manifest and
// no operator review of a gold set. It can gate from the first run.
//
// WHY IT MATTERS MORE THAN ITS COST SUGGESTS. Schema conformance is a gate for
// most roles rather than a score: a step whose output does not parse, or parses
// into the wrong shape, does not degrade — it fails, retries, and burns a round
// trip per attempt. That makes it simultaneously a quality metric and a cost
// metric, which is why the verdict reports both conformance and the retries it
// took to get there.
//
// RELATIONSHIP TO THE LIVE METRIC. internal/executor already exports
// model_schema_violation_rate (schema_violation / total_terminal, per model).
// That is production telemetry over whatever traffic happened to arrive. This is
// a controlled measurement over a fixed task set, per role, per arm — the two
// answer different questions and neither replaces the other.
//
// KNOWN MISATTRIBUTION (measured 2026-08-20). Both this probe and that gauge
// attribute a schema_violation to the MODEL, and a substantial share of them are
// harness-side losses. Audit of all 198 "missing required keys" rungs in the
// bench DB, classified by whether the step's OWN response artifact contained the
// key the step was failed for:
//
//	48  valid JSON, key at top level          - the answer existed and was lost
//	22  fenced JSON, merge Pass 2 recoverable  - ditto
//	18  embedded {...}, merge Pass 3 recoverable - ditto
//	29  key present only as prose              - model did not emit the contract
//	81  key genuinely absent                   - model non-compliance
//
// RE-AUDITED on two markers (same day, after the first fix failed to hold). The
// second marker is tool_calls_used: it comes from result.json's metrics block, so
// a NULL means the whole result was lost rather than one field being dropped.
// Across all 200 rows:
//
//	artifact had key + metrics NULL     90   the whole result was emptied
//	artifact had key + metrics present    0   <- the telling zero
//	key absent + metrics NULL             5   emptied, key not recoverable
//	key absent + metrics present        105   genuine model non-compliance
//
// The ZERO is what identifies the stage. Metrics are assembled into base_result
// BEFORE the structured merge runs, so a merge-stage failure would drop the answer
// and keep the metrics — that row would be non-empty. It is empty in 200 rows, so
// the merge was never the operative cause, and the loss is always the whole result
// going away.
//
// Consequently merge_structured_result — written first, for a real but unobserved
// mechanism — did not close this: bench arm 6 ran it and still produced a rung
// with the artifact holding `analysis` and NULL metrics. The operative cause is
// the unguarded jq chain in write_result's tail, where command substitution of a
// failed jq yields "" and the next line writes it to result.json. Fixed by
// guard_result_update; guarded by test-entrypoint-result-never-emptied.sh.
//
// So the harness-side share is 90/200 (45%) and model non-compliance is 105/200
// (52.5%) — not the "up to 44%" an artifact-only audit suggested, and attributed
// to a different stage than that audit concluded.
//
// CONSEQUENCE FOR COMPARISONS ACROSS THAT FIX. Runs before it under-report
// schema conformance by up to the share above, so a conformance improvement
// measured across the boundary is partly the harness being fixed rather than the
// model improving. The 198 historical rows cannot be retro-labelled. Treat any
// pre-fix schema-violation figure as an upper bound on model non-compliance, not
// a measurement of it.

// Step outcomes, mirroring the taxonomy owned by the migration that created
// execution_step_outcomes. TestOutcomeConstants_MatchTheMigrationTaxonomy fails
// if the two drift.
const (
	OutcomeOK                 = "ok"
	OutcomePendingValidation  = "pending_validation"
	OutcomeParseError         = "parse_error"
	OutcomeSchemaViolation    = "schema_violation"
	OutcomeRefused            = "refused"
	OutcomeIterationExhausted = "iteration_exhausted"
	OutcomeDegenerateLoop     = "degenerate_loop"
	OutcomeDownstreamRejected = "downstream_rejected"
	OutcomeGateFailed         = "gate_failed"
	OutcomeTimeout            = "timeout"
	OutcomeCancelled          = "cancelled"
	OutcomeFailed             = "failed"
)

// AllStepOutcomes is the vocabulary this package scores.
func AllStepOutcomes() []string {
	return []string{
		OutcomeOK, OutcomePendingValidation, OutcomeParseError, OutcomeSchemaViolation,
		OutcomeRefused, OutcomeIterationExhausted, OutcomeDegenerateLoop,
		OutcomeDownstreamRejected, OutcomeGateFailed, OutcomeTimeout,
		OutcomeCancelled, OutcomeFailed,
	}
}

// StepOutcome is one recorded step result from execution_step_outcomes.
type StepOutcome struct {
	StepID  string `json:"stepId"`
	Role    string `json:"role"`
	Model   string `json:"model"`
	Outcome string `json:"outcome"`
	// Attempt is 1 for a first emission; higher values are retries after a
	// shape failure.
	Attempt    int    `json:"attempt"`
	ErrorClass string `json:"errorClass,omitempty"`
	// DurationMS is the step's wall clock, from execution_step_outcomes.
	// PER-STEP rather than only summed per execution because the speed-aware
	// timeout fit regresses duration on completion tokens and tool calls, and
	// that regression is per step — an execution-level total cannot separate a
	// slow model from a slow tool. Zero means the ledger row had no duration,
	// which is distinct from a step that genuinely took no time.
	DurationMS int64 `json:"durationMs,omitempty"`
}

// RoleConformance is one role's schema-following record.
type RoleConformance struct {
	Terminal         int     `json:"terminal"`
	Conforming       int     `json:"conforming"`
	Conformance      float64 `json:"conformance"`
	ParseErrors      int     `json:"parseErrors"`
	SchemaViolations int     `json:"schemaViolations"`
}

// SchemaVerdict is the schema probe's vector.
type SchemaVerdict struct {
	Probe       string `json:"probe"`
	ExecutionID string `json:"executionId"`
	TaskID      string `json:"taskId"`

	// Terminal counts steps that resolved. pending_validation is excluded: it
	// has not answered yet, and counting it either way invents a result the
	// ledger has not recorded.
	Terminal int `json:"terminal"`

	// Judged is the conformance denominator: resolved steps that produced
	// output a schema could be applied to. NoOutput is the remainder — crashed,
	// timed out, refused, exhausted — which is a reliability fact rather than a
	// schema one.
	Judged   int `json:"judged"`
	NoOutput int `json:"noOutput"`

	// NoOutputByOutcome breaks NoOutput down by the outcome that caused it.
	//
	// NoOutput on its own is not actionable. The 2026-08-16 long-horizon arm
	// reported 32 of 57 terminal steps (56.1%) producing nothing, which is the
	// single largest reliability fact in the run — and the journal could not say
	// whether that was containers exiting non-zero, steps timing out, or agents
	// exhausting their iterations. Those have three different fixes, and the
	// bare count picks none of them.
	//
	// Keyed by the raw outcome string rather than a closed enum: the taxonomy
	// grows, and a cause this map cannot name would otherwise vanish into the
	// remainder it exists to explain.
	NoOutputByOutcome map[string]int `json:"noOutputByOutcome,omitempty"`

	// NoOutputByErrorClass breaks NoOutput down by ERROR CLASS.
	//
	// NoOutputByOutcome answers "what outcome did the ledger record", which
	// for a crashed container is always `failed`. The cause lives in
	// error_class, and until 2026-08-16 nothing wrote a useful one: the
	// long-horizon arm's 73 failures were all container_non_zero_exit, and
	// the real split — plausibility 32, degenerate loop 23, context overflow
	// 14, iteration cap 4 — had to be recovered by hand from error_detail
	// prose. Getting that recovery wrong is easy: error_detail carries the
	// container log, whose lines mention context_size, so a keyword query
	// mis-attributes iteration-cap failures as context overflows.
	//
	// Keyed by raw class string, for the same reason its sibling is keyed by
	// raw outcome: a cause this map cannot name would vanish into the
	// remainder it exists to explain. Steps with no class recorded bucket as
	// "unclassified" rather than being dropped, so the shares always sum to
	// NoOutput.
	//
	// Additive to the journal — does not affect arm comparability.
	NoOutputByErrorClass map[string]int `json:"noOutputByErrorClass,omitempty"`

	// SchemaConformance is conforming / JUDGED — did the step end up conformant,
	// however many attempts it took, over the steps that produced output at all.
	SchemaConformance        float64 `json:"schemaConformance"`
	SchemaConformanceDefined bool    `json:"schemaConformanceDefined"`

	// FirstEmissionConformance is the honest headline: conformant on attempt 1.
	// A role that always gets there on attempt 3 is not "conformant" in any
	// sense a cost model cares about.
	FirstEmissionConformance        float64 `json:"firstEmissionConformance"`
	FirstEmissionConformanceDefined bool    `json:"firstEmissionConformanceDefined"`

	// RetriesToValid is the wasted round trips: every non-first attempt that
	// followed a shape failure.
	RetriesToValid int `json:"retriesToValid"`

	// ParseErrors and SchemaViolations are kept apart because they need
	// different fixes — unparseable output and wrong-shaped output are not the
	// same defect, and retry.go already distinguishes them for its corrective
	// hints.
	ParseErrors      int `json:"parseErrors"`
	SchemaViolations int `json:"schemaViolations"`

	// DownstreamRejected and GateFailed are schema-VALID outputs that were
	// still unusable — the role obeyed its contract and produced the wrong
	// thing. Neither counts as a schema failure.
	DownstreamRejected int `json:"downstreamRejected"`
	GateFailed         int `json:"gateFailed"`

	// ByRole breaks the above down, because schema following is gated per role
	// and a blended figure hides which role needs the fix.
	ByRole map[string]RoleConformance `json:"byRole"`
}

// SchemaProbe scores schema following.
type SchemaProbe struct{}

// Name implements Probe.
func (SchemaProbe) Name() string { return "schema-following" }

// isShapeFailure reports the two outcomes that mean "the output did not conform".
func isShapeFailure(outcome string) bool {
	return outcome == OutcomeParseError || outcome == OutcomeSchemaViolation
}

// isJudgeable reports whether a step produced output a schema could be applied
// to.
//
// FOUND BY HAND-CHECKING THE FIRST REAL RUN. A step whose container exited
// non-zero, timed out, was cancelled, refused, or exhausted its iterations
// emitted NOTHING. Counting those as conformant — which the first implementation
// did, because they are simply "not a shape failure" — inflates conformance with
// steps that never produced a thing to conform. On the first live run that
// turned a true 2/7 into a reported 3/8.
//
// They are not counted as violations either: a crashed container is a
// reliability fact, not a schema fact, and folding it in would make this metric
// mean two things at once. They leave the denominator and are reported as
// NoOutput.
func isJudgeable(outcome string) bool {
	switch outcome {
	case OutcomeOK, OutcomeParseError, OutcomeSchemaViolation,
		OutcomeDownstreamRejected, OutcomeGateFailed:
		// downstream_rejected and gate_failed DID produce schema-valid output —
		// it was rejected for other reasons — so they belong in the denominator
		// as conforming, and are tracked separately besides.
		return true
	default:
		return false
	}
}

// isTerminalOutcome reports whether an outcome has resolved.
func isTerminalOutcome(outcome string) bool {
	return outcome != "" && outcome != OutcomePendingValidation
}

// ScoreSchema scores schema following for one execution.
//
// Takes no Gold: the ground truth is the declared schema, and demanding a gold
// manifest here would impose the tool-grant probe's whole generation pass on a
// measurement that does not need it.
func (p SchemaProbe) ScoreSchema(trace Trace, task TaskRef) SchemaVerdict {
	v := SchemaVerdict{
		Probe:       p.Name(),
		ExecutionID: trace.ExecutionID,
		TaskID:      task.ID,
		ByRole:      map[string]RoleConformance{},
	}

	// THE UNIT IS THE STEP, NOT THE ROW. A step that violated twice and then
	// conformed IS conformant — it produced usable output — and counting each
	// attempt as its own outcome would report that step as 33% conformant,
	// conflating "did it get there" with "how expensively". Those are the two
	// separate numbers this verdict reports: SchemaConformance resolves each
	// step to its final attempt, RetriesToValid carries the cost.
	final := p.tallyRows(&v, trace.Outcomes)
	conforming := p.tallySteps(&v, final)

	if v.Judged > 0 {
		v.SchemaConformance = float64(conforming) / float64(v.Judged)
		v.SchemaConformanceDefined = true
	}
	return v
}

// tallyRows walks every recorded row: per-EVENT counters (shape failures,
// retries, downstream rejections, first-emission conformance) and, as it goes,
// resolves each step to its last terminal attempt.
func (p SchemaProbe) tallyRows(v *SchemaVerdict, outcomes []StepOutcome) map[string]StepOutcome {
	final := map[string]StepOutcome{}
	firstEmissions, firstConforming := 0, 0

	for _, o := range outcomes {
		switch o.Outcome {
		case OutcomeDownstreamRejected:
			v.DownstreamRejected++
		case OutcomeGateFailed:
			v.GateFailed++
		}

		switch {
		case o.Attempt > 1 && isJudgeable(o.Outcome):
			// Every attempt past the first exists because an earlier one did
			// not conform. That is the cost of poor schema following, in round
			// trips.
			v.RetriesToValid++
		case o.Attempt <= 1 && isJudgeable(o.Outcome):
			firstEmissions++
			if !isShapeFailure(o.Outcome) {
				firstConforming++
			}
		}

		countShapeFailure(o.Outcome, &v.ParseErrors, &v.SchemaViolations)

		// A later terminal attempt supersedes an earlier one; a non-terminal
		// row never supersedes a resolved result.
		if !isTerminalOutcome(o.Outcome) {
			continue
		}
		if prev, ok := final[o.StepID]; !ok || o.Attempt >= prev.Attempt {
			final[o.StepID] = o
		}
	}

	if firstEmissions > 0 {
		v.FirstEmissionConformance = float64(firstConforming) / float64(firstEmissions)
		v.FirstEmissionConformanceDefined = true
	}
	return final
}

// tallySteps scores the resolved steps — the unit conformance is measured in —
// and fills the per-role breakdown. Returns the conforming step count.
func (p SchemaProbe) tallySteps(v *SchemaVerdict, final map[string]StepOutcome) int {
	conforming := 0
	roleTotals := map[string]RoleConformance{}

	for _, o := range final {
		v.Terminal++
		if !isJudgeable(o.Outcome) {
			v.NoOutput++
			if v.NoOutputByOutcome == nil {
				v.NoOutputByOutcome = map[string]int{}
			}
			v.NoOutputByOutcome[o.Outcome]++
			if v.NoOutputByErrorClass == nil {
				v.NoOutputByErrorClass = map[string]int{}
			}
			class := o.ErrorClass
			if class == "" {
				// Named rather than dropped: a step whose cause nothing
				// recorded is itself a finding, and silently omitting it
				// would make the breakdown's shares not sum to NoOutput.
				class = "unclassified"
			}
			v.NoOutputByErrorClass[class]++
			continue
		}
		v.Judged++
		rc := roleTotals[o.Role]
		rc.Terminal++
		if isShapeFailure(o.Outcome) {
			countShapeFailure(o.Outcome, &rc.ParseErrors, &rc.SchemaViolations)
		} else {
			conforming++
			rc.Conforming++
		}
		roleTotals[o.Role] = rc
	}

	for role, rc := range roleTotals {
		if rc.Terminal > 0 {
			rc.Conformance = float64(rc.Conforming) / float64(rc.Terminal)
		}
		v.ByRole[role] = rc
	}
	return conforming
}

// countShapeFailure increments whichever counter the outcome names, keeping the
// parse/violation split in one place so the two call sites cannot disagree.
func countShapeFailure(outcome string, parseErrors, violations *int) {
	switch outcome {
	case OutcomeParseError:
		*parseErrors++
	case OutcomeSchemaViolation:
		*violations++
	}
}

// Score implements Probe by projecting the schema verdict onto the shared
// Verdict shape, so a runner can hold every probe uniformly.
//
// The projection is deliberately lossy and the full vector stays available via
// ScoreSchema — collapsing schema following to one number is exactly what the
// per-role breakdown exists to prevent, so the journal records both.
func (p SchemaProbe) Score(_ context.Context, task TaskRef, _ Gold, trace Trace) (Verdict, error) {
	s := p.ScoreSchema(trace, task)
	return Verdict{
		Probe:       p.Name(),
		ExecutionID: trace.ExecutionID,
		TaskID:      task.ID,
		Schema:      &s,
	}, nil
}

// WorstRoles lists roles below a conformance threshold, worst first. Schema
// following gates per role, so the actionable output is which roles fail, not
// an average that no single role experiences.
func (v SchemaVerdict) WorstRoles(threshold float64) []string {
	var out []string
	for role, rc := range v.ByRole {
		if rc.Terminal > 0 && rc.Conformance < threshold {
			out = append(out, role)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := v.ByRole[out[i]], v.ByRole[out[j]]
		if a.Conformance != b.Conformance {
			return a.Conformance < b.Conformance
		}
		return out[i] < out[j]
	})
	return out
}
