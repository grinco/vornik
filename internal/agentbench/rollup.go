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

	ToolCallValidity        float64 `json:"toolCallValidity"`
	ToolCallValidityDefined bool    `json:"toolCallValidityDefined"`
	UnknownToolCalls        int     `json:"unknownToolCalls"`
	ArgumentErrors          int     `json:"argumentErrors"`

	// PathCoverage is the tool-grant probe's headline, present only when a gold
	// set existed for the run.
	PathCoverage        float64 `json:"pathCoverage"`
	PathCoverageDefined bool    `json:"pathCoverageDefined"`
	CoreMisses          int     `json:"coreMisses"`
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
	r.Efficiency.WallClockMSPerTask = wallClock / n
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
	}
	if toolValidityWeight > 0 {
		r.Accuracy.ToolCallValidity = toolValiditySum / toolValidityWeight
		r.Accuracy.ToolCallValidityDefined = true
	}
	if coverageN > 0 {
		r.Accuracy.PathCoverage = coverageSum / coverageN
		r.Accuracy.PathCoverageDefined = true
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
