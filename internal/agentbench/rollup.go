package agentbench

// Customer rollup (§4).
//
// The layers measure mechanism; customers ask about cost, efficiency, accuracy
// and success rate. This is the mapping, and it is part of the design rather
// than a reporting afterthought because three of the four have a dishonest form
// that is easier to compute than the honest one.
//
// READS THE JOURNAL, NEVER THE LIVE TABLE. Probe verdicts are journaled at run
// time (§3.2) precisely so a rollup survives tool_audit_log's 30-day retention.
// Every input here is a recorded value; nothing re-queries the ledger.

// ExecutionRecord is one execution's journaled result.
type ExecutionRecord struct {
	TaskID      string `json:"taskId"`
	ExecutionID string `json:"executionId"`

	Succeeded bool   `json:"succeeded"`
	ErrorText string `json:"errorText,omitempty"`

	CostUSD          float64 `json:"costUsd"`
	PromptTokens     int64   `json:"promptTokens"`
	CompletionTokens int64   `json:"completionTokens"`
	DurationMS       int64   `json:"durationMs"`
	ToolCalls        int     `json:"toolCalls"`

	Verdicts []Verdict `json:"verdicts,omitempty"`
}

// EfficiencyRollup is resource spent per unit of work — the only one of the
// four customer metrics we can differentiate on, because the other three are
// mostly facts about the model.
type EfficiencyRollup struct {
	TokensPerTask      float64 `json:"tokensPerTask"`
	ToolCallsPerTask   float64 `json:"toolCallsPerTask"`
	WallClockMSPerTask float64 `json:"wallClockMsPerTask"`
	// WallClockDefined distinguishes "the steps took no measurable time" from
	// "no duration was recorded". Every journal written before 2026-08-16 is the
	// SECOND case: ExecutionRecord.DurationMS was declared and never populated,
	// so the rollup emitted a confident 0 ms/task. Re-scoring such a journal
	// cannot recover the number — the durations were never journaled — so the
	// flag is how an old rollup admits it rather than reporting a zero someone
	// might compare against a real one.
	WallClockDefined bool `json:"wallClockDefined"`

	// GrantPrecision is over-granting: schema tokens advertised and never used.
	GrantPrecision        float64 `json:"grantPrecision"`
	GrantPrecisionDefined bool    `json:"grantPrecisionDefined"`

	// Escalations and SchemaRetries are both wasted round trips, from the two
	// different causes this benchmark can tell apart.
	Escalations   int `json:"escalations"`
	SchemaRetries int `json:"schemaRetries"`
}

// AccuracyRollup is correctness on verifiable criteria. No judge appears in any
// of these: each is a fact recorded by the executor.
type AccuracyRollup struct {
	SchemaConformance        float64 `json:"schemaConformance"`
	SchemaConformanceDefined bool    `json:"schemaConformanceDefined"`
	// SchemaJudged and SchemaNoOutput are conformance's two halves.
	//
	// Judged is the denominator: terminal steps that produced output a schema
	// could be applied to. NoOutput is the remainder — crashed, timed out,
	// refused, exhausted. The probe is right to keep them apart (a crashed
	// container is a reliability fact, not a schema fact), but the ROLLUP
	// published only the ratio, so the reliability half vanished.
	//
	// Measured 2026-08-16 on the qwen-local-fixed arm: 825 terminal steps, 479
	// judged, 346 — 41.9% — producing no output at all. "Conformance 0.912"
	// reads as near-perfect until you know it describes 58% of the steps.
	// Publishing both makes the number interpretable instead of flattering.
	SchemaJudged   int `json:"schemaJudged"`
	SchemaNoOutput int `json:"schemaNoOutput"`

	// SchemaNoOutputByOutcome is the reliability half, broken down by cause.
	//
	// The count alone says how much of the run produced nothing; it does not say
	// what to fix. On the 2026-08-16 long-horizon arm that count was 56.1% of
	// terminal steps — the run's largest single fact — and container exits,
	// timeouts and iteration exhaustion have three different remedies.
	SchemaNoOutputByOutcome map[string]int `json:"schemaNoOutputByOutcome,omitempty"`

	ToolCallValidity        float64 `json:"toolCallValidity"`
	ToolCallValidityDefined bool    `json:"toolCallValidityDefined"`
	UnknownToolCalls        int     `json:"unknownToolCalls"`
	ArgumentErrors          int     `json:"argumentErrors"`

	// PathCoverage is the tool-grant probe's headline, present only when a gold
	// set existed for the run.
	PathCoverage        float64 `json:"pathCoverage"`
	PathCoverageDefined bool    `json:"pathCoverageDefined"`
	// PathCoverageN is HOW MANY executions the coverage figure averages.
	//
	// Its denominator is not the task set and never was: GrantProbe refuses to
	// score an execution that made no grant request (probe.go), deliberately,
	// because scoring those 0.0 would drag every real measurement to zero. So
	// coverage measures executions that USED the grant feature, which is a
	// self-selected subset.
	//
	// Measured 2026-08-16 on the qwen-local-fixed arm: 16 scored out of 139
	// executions (11.5%), and every dp-* task contributed ZERO — that whole
	// workflow family never requests a grant, so it cannot appear in the
	// headline accuracy number at all.
	//
	// PathCoverageDefined said "there is a number"; it could not say "from how
	// many". Publishing 0.548 without the count invites reading it as a
	// property of the task set rather than of a fraction of it.
	PathCoverageN int `json:"pathCoverageN"`
	CoreMisses    int `json:"coreMisses"`
	// CoreShellCovered counts core requirements met ONLY because a shell was
	// granted, and it is why CoreMisses must never be quoted alone.
	//
	// Substitution (harness v3) is right: a lead that granted run_shell and
	// withheld git_status blocked the agent from nothing, and the v2 rule that
	// called that a hard failure was wrong five times out of five. But it
	// leaves one blind spot — a lead that lazily grants a shell for every step
	// scores a perfect zero on core misses while making the worst possible
	// grant decision.
	//
	// That laziness IS caught, by grant precision, which fell to 0.20 and 0.40
	// on exactly those executions. Keeping the two metrics separate is
	// deliberate: core miss asks "could the agent do the work", precision asks
	// "was the grant tight". This counter is the bridge, so a clean core-miss
	// sheet earned entirely through shell grants cannot be read as a tight
	// policy.
	CoreShellCovered int `json:"coreShellCovered"`
	// CoreSubstituted counts core requirements met by ANY equivalent tool,
	// shell or peer. CoreShellCovered is a subset of it.
	CoreSubstituted int `json:"coreSubstituted"`
}

// Rollup is one arm's customer-facing figures.
type Rollup struct {
	Arm       string `json:"arm"`
	Attempted int    `json:"attempted"`

	TotalCostUSD      float64 `json:"totalCostUsd"`
	CostPerAttemptUSD float64 `json:"costPerAttemptUsd"`
	// CostPerSuccessUSD is TOTAL spend over successes — failed-run spend
	// included. The mean cost of the runs that worked is the number this
	// deliberately is not.
	CostPerSuccessUSD     float64 `json:"costPerSuccessUsd"`
	CostPerSuccessDefined bool    `json:"costPerSuccessDefined"`

	Failures   SuccessBreakdown `json:"failures"`
	Efficiency EfficiencyRollup `json:"efficiency"`
	Accuracy   AccuracyRollup   `json:"accuracy"`

	// RequestPrecisionDiagnosticOnly is set whenever a request-precision figure
	// was observed, and exists so a reader cannot mistake it for a component of
	// efficiency. It improves when the lead asks for less, so optimising it in
	// isolation produces a worse agent (§3.2).
	RequestPrecisionDiagnosticOnly bool `json:"requestPrecisionDiagnosticOnly"`
	// RequestPrecision is carried for diagnosis, never rolled up.
	RequestPrecision        float64 `json:"requestPrecision,omitempty"`
	RequestPrecisionDefined bool    `json:"requestPrecisionDefined,omitempty"`
}

// BuildRollup computes one arm's customer figures from its journaled records.
func BuildRollup(arm string, records []ExecutionRecord) Rollup {
	r := Rollup{Arm: arm, Attempted: len(records)}
	if len(records) == 0 {
		return r
	}

	var tokens, toolCalls, wallClock float64
	var grantSum, grantN float64
	var reqSum, reqN float64
	var schemaSum, schemaWeight float64
	var toolValiditySum, toolValidityWeight float64
	var coverageSum, coverageN float64

	for _, rec := range records {
		r.TotalCostUSD += rec.CostUSD
		r.Failures.Add(ClassifyFailure(rec.Succeeded, rec.ErrorText))

		tokens += float64(rec.PromptTokens + rec.CompletionTokens)
		toolCalls += float64(rec.ToolCalls)
		wallClock += float64(rec.DurationMS)

		for _, v := range rec.Verdicts {
			accumulateGrant(&r, v, &grantSum, &grantN, &reqSum, &reqN, &coverageSum, &coverageN)
			accumulateSchema(&r, v, &schemaSum, &schemaWeight)
			accumulateToolUse(&r, v, &toolValiditySum, &toolValidityWeight)
		}
	}

	n := float64(len(records))
	r.CostPerAttemptUSD = r.TotalCostUSD / n
	if r.Failures.Succeeded > 0 {
		r.CostPerSuccessUSD = r.TotalCostUSD / float64(r.Failures.Succeeded)
		r.CostPerSuccessDefined = true
	}

	r.Efficiency.TokensPerTask = tokens / n
	r.Efficiency.ToolCallsPerTask = toolCalls / n
	if wallClock > 0 {
		r.Efficiency.WallClockMSPerTask = wallClock / n
		r.Efficiency.WallClockDefined = true
	}
	if grantN > 0 {
		r.Efficiency.GrantPrecision = grantSum / grantN
		r.Efficiency.GrantPrecisionDefined = true
	}

	if reqN > 0 {
		r.RequestPrecision = reqSum / reqN
		r.RequestPrecisionDefined = true
		r.RequestPrecisionDiagnosticOnly = true
	}
	if schemaWeight > 0 {
		r.Accuracy.SchemaConformance = schemaSum / schemaWeight
		r.Accuracy.SchemaConformanceDefined = true
		r.Accuracy.SchemaJudged = int(schemaWeight)
	}
	if toolValidityWeight > 0 {
		r.Accuracy.ToolCallValidity = toolValiditySum / toolValidityWeight
		r.Accuracy.ToolCallValidityDefined = true
	}
	if coverageN > 0 {
		r.Accuracy.PathCoverage = coverageSum / coverageN
		r.Accuracy.PathCoverageDefined = true
		r.Accuracy.PathCoverageN = int(coverageN)
	}
	return r
}

// accumulateGrant folds one verdict's grant-probe components into the rollup.
// Split out of BuildRollup so each probe's accumulation reads on its own.
func accumulateGrant(r *Rollup, v Verdict, grantSum, grantN, reqSum, reqN, coverageSum, coverageN *float64) {
	if v.GrantPrecisionDefined {
		*grantSum += v.GrantPrecision
		*grantN++
	}
	if v.RequestPrecisionDefined {
		*reqSum += v.RequestPrecision
		*reqN++
	}
	r.Efficiency.Escalations += v.Escalations
	if v.CoreMiss {
		r.Accuracy.CoreMisses++
	}
	for _, via := range v.CoreSubstitutions {
		r.Accuracy.CoreSubstituted++
		if via == shellTool {
			r.Accuracy.CoreShellCovered++
		}
	}
	if v.Probe == grantProbeName {
		*coverageSum += v.PathCoverage
		*coverageN++
	}
}

// accumulateSchema folds one verdict's schema components in, weighting
// conformance by terminal steps: two executions with different step counts must
// not count equally per step.
func accumulateSchema(r *Rollup, v Verdict, sum, weight *float64) {
	s := v.Schema
	if s == nil {
		return
	}
	r.Efficiency.SchemaRetries += s.RetriesToValid
	// Accumulated even when conformance is undefined: an execution whose steps
	// ALL produced no output contributes nothing to the ratio but is exactly
	// what a reader needs to see. Skipping it here would hide the worst cases.
	r.Accuracy.SchemaNoOutput += s.NoOutput
	for outcome, n := range s.NoOutputByOutcome {
		if r.Accuracy.SchemaNoOutputByOutcome == nil {
			r.Accuracy.SchemaNoOutputByOutcome = map[string]int{}
		}
		r.Accuracy.SchemaNoOutputByOutcome[outcome] += n
	}
	if s.SchemaConformanceDefined && s.Judged > 0 {
		*sum += s.SchemaConformance * float64(s.Judged)
		*weight += float64(s.Judged)
	}
}

// accumulateToolUse folds one verdict's tool-use components in, weighting
// validity by call count for the same reason.
func accumulateToolUse(r *Rollup, v Verdict, sum, weight *float64) {
	tu := v.ToolUse
	if tu == nil {
		return
	}
	r.Accuracy.UnknownToolCalls += tu.UnknownTool
	r.Accuracy.ArgumentErrors += tu.ArgumentError
	if tu.CallValidityDefined && tu.Calls > 0 {
		*sum += tu.CallValidity * float64(tu.Calls)
		*weight += float64(tu.Calls)
	}
}
