package agentbench

import "testing"

// $/successful task must carry FAILED-run spend in the numerator. Reporting the
// mean cost of the runs that happened to work is the standard way this metric is
// fudged, and it is the first thing a technical buyer probes.
func TestRollup_CostPerSuccessIncludesFailedRunSpend(t *testing.T) {
	r := BuildRollup("arm-a", []ExecutionRecord{
		{TaskID: "t1", Succeeded: true, CostUSD: 1.00},
		{TaskID: "t2", Succeeded: true, CostUSD: 1.00},
		{TaskID: "t3", Succeeded: false, ErrorText: "criteria not met", CostUSD: 2.00},
	})

	if r.TotalCostUSD != 4.00 {
		t.Fatalf("total = %v, want 4.00", r.TotalCostUSD)
	}
	// The naive figure is 1.00 (mean of the two that worked). The honest one is
	// 4.00/2 = 2.00: the failure's spend is real money that bought nothing.
	if r.CostPerSuccessUSD != 2.00 {
		t.Errorf("cost per success = %v, want 2.00 — failed-run spend belongs in the "+
			"numerator, or the figure describes a world where failures are free",
			r.CostPerSuccessUSD)
	}
	if want := 4.00 / 3.0; r.CostPerAttemptUSD != want {
		t.Errorf("cost per attempt = %v, want %v", r.CostPerAttemptUSD, want)
	}
}

func TestRollup_CostPerSuccessUndefinedWithNoSuccesses(t *testing.T) {
	r := BuildRollup("arm-a", []ExecutionRecord{
		{TaskID: "t1", Succeeded: false, ErrorText: "criteria not met", CostUSD: 3.00},
	})
	if r.CostPerSuccessDefined {
		t.Error("reported a cost per success with zero successes — that is a division " +
			"by zero dressed as a number")
	}
	// The spend still happened and is still reported.
	if r.TotalCostUSD != 3.00 {
		t.Errorf("total = %v, want 3.00", r.TotalCostUSD)
	}
}

// A blended success rate absorbs a provider outage, a context overflow and an
// agent that could not do the work into one number that answers no question.
func TestRollup_SuccessIsBrokenOutByFailureClass(t *testing.T) {
	r := BuildRollup("arm-a", []ExecutionRecord{
		{TaskID: "t1", Succeeded: true},
		{TaskID: "t2", Succeeded: false, ErrorText: "acceptance criteria not met"},
		{TaskID: "t3", Succeeded: false, ErrorText: "curl: (7) Failed to connect"},
		{TaskID: "t4", Succeeded: false, ErrorText: "maximum context length is 202752 tokens"},
		{TaskID: "t5", Succeeded: false, ErrorText: `task "t5" has no recorded paths`},
	})

	if got := r.Failures.ByClass[FailureTask]; got != 1 {
		t.Errorf("task failures = %d, want 1", got)
	}
	if got := r.Failures.ByClass[FailureInfra]; got != 1 {
		t.Errorf("infra failures = %d, want 1", got)
	}
	if got := r.Failures.ByClass[FailureContextOverflow]; got != 1 {
		t.Errorf("context overflow = %d, want 1 — it is the policy under test failing, "+
			"not the provider", got)
	}
	if got := r.Failures.ByClass[FailureHarness]; got != 1 {
		t.Errorf("harness failures = %d, want 1", got)
	}

	// 1 success over 4 non-harness attempts, not 1 over 5.
	rate, defined := r.Failures.TaskSuccessRate()
	if !defined || rate != 0.25 {
		t.Errorf("success rate = %v (defined %v), want 0.25", rate, defined)
	}
}

// Request precision improves when the lead asks for LESS, even at the cost of
// escalation. Rolling it into a headline efficiency figure would reward exactly
// the wrong behaviour in the material customers skim hardest.
func TestRollup_EfficiencyExcludesRequestPrecision(t *testing.T) {
	r := BuildRollup("arm-a", []ExecutionRecord{
		{
			TaskID: "t1", Succeeded: true, PromptTokens: 1000, CompletionTokens: 200,
			Verdicts: []Verdict{{
				Probe: "tool-grant", GrantPrecision: 0.5, GrantPrecisionDefined: true,
				RequestPrecision: 0.1, RequestPrecisionDefined: true, Escalations: 3,
			}},
		},
	})

	if !r.Efficiency.GrantPrecisionDefined || r.Efficiency.GrantPrecision != 0.5 {
		t.Errorf("grant precision = %v, want 0.5", r.Efficiency.GrantPrecision)
	}
	if r.Efficiency.Escalations != 3 {
		t.Errorf("escalations = %d, want 3", r.Efficiency.Escalations)
	}
	// The diagnostic is carried, but flagged so no reader mistakes it for a
	// component of efficiency.
	if !r.RequestPrecisionDiagnosticOnly {
		t.Error("request precision was not marked diagnostic-only")
	}
	if r.Efficiency.TokensPerTask != 1200 {
		t.Errorf("tokens per task = %v, want 1200", r.Efficiency.TokensPerTask)
	}
}

func TestRollup_SchemaConformanceAggregatesAcrossExecutions(t *testing.T) {
	// Weighted by JUDGED steps — the conformance denominator — not by every
	// resolved step: a step that crashed produced nothing to conform and must
	// not pull the average toward either answer.
	sv1 := SchemaVerdict{SchemaConformance: 1.0, SchemaConformanceDefined: true, Terminal: 2, Judged: 2, RetriesToValid: 0}
	sv2 := SchemaVerdict{SchemaConformance: 0.5, SchemaConformanceDefined: true, Terminal: 2, Judged: 2, RetriesToValid: 3}

	r := BuildRollup("arm-a", []ExecutionRecord{
		{TaskID: "t1", Succeeded: true, Verdicts: []Verdict{{Probe: "schema-following", Schema: &sv1}}},
		{TaskID: "t2", Succeeded: true, Verdicts: []Verdict{{Probe: "schema-following", Schema: &sv2}}},
	})

	// Weighted by terminal steps, not a mean of means: two executions with
	// different step counts must not count equally per step.
	if want := 0.75; r.Accuracy.SchemaConformance != want {
		t.Errorf("schema conformance = %v, want %v", r.Accuracy.SchemaConformance, want)
	}
	if r.Efficiency.SchemaRetries != 3 {
		t.Errorf("schema retries = %d, want 3 — retries are the token cost of poor "+
			"schema following", r.Efficiency.SchemaRetries)
	}
}

func TestRollup_ToolUseValidityAggregates(t *testing.T) {
	tv1 := ToolUseVerdict{Calls: 4, CallValidity: 1.0, CallValidityDefined: true, UnknownTool: 0}
	tv2 := ToolUseVerdict{Calls: 4, CallValidity: 0.5, CallValidityDefined: true, UnknownTool: 2}

	r := BuildRollup("arm-a", []ExecutionRecord{
		{TaskID: "t1", Succeeded: true, Verdicts: []Verdict{{Probe: "tool-use", ToolUse: &tv1}}},
		{TaskID: "t2", Succeeded: true, Verdicts: []Verdict{{Probe: "tool-use", ToolUse: &tv2}}},
	})

	if want := 0.75; r.Accuracy.ToolCallValidity != want {
		t.Errorf("call validity = %v, want %v", r.Accuracy.ToolCallValidity, want)
	}
	if r.Accuracy.UnknownToolCalls != 2 {
		t.Errorf("unknown tool calls = %d, want 2 — an invented tool name is the "+
			"single most legible function-calling failure", r.Accuracy.UnknownToolCalls)
	}
}

func TestRollup_EmptyRunReportsNothingRatherThanZeroes(t *testing.T) {
	r := BuildRollup("arm-a", nil)
	if r.CostPerSuccessDefined || r.Efficiency.GrantPrecisionDefined {
		t.Error("an empty run reported defined metrics — nothing ran, so nothing is measured")
	}
	if r.Attempted != 0 {
		t.Errorf("attempted = %d, want 0", r.Attempted)
	}
}

// Substitution (v3) is right, but it leaves one blind spot: a lead that lazily
// grants a shell for every step scores a PERFECT zero on core misses while
// making the worst possible grant decision. This counter is what stops a clean
// core-miss sheet being read as a tight policy.
func TestBuildRollup_CountsCoreRequirementsCoveredOnlyByAShell(t *testing.T) {
	records := []ExecutionRecord{{
		TaskID: "t1", Succeeded: true,
		Verdicts: []Verdict{{
			Probe: grantProbeName, PathCoverage: 0.8,
			GrantPrecision: 0.4, GrantPrecisionDefined: true,
			CoreSubstitutions: map[string]string{
				"git_status":      "run_shell", // the lazy-shell case
				"read_many_files": "file_read", // a genuine peer
			},
		}},
	}}

	r := BuildRollup("baseline", records)

	if r.Accuracy.CoreMisses != 0 {
		t.Errorf("core misses = %d, want 0", r.Accuracy.CoreMisses)
	}
	if r.Accuracy.CoreShellCovered != 1 {
		t.Errorf("shell-covered = %d, want 1 (only git_status came from the shell)",
			r.Accuracy.CoreShellCovered)
	}
	if r.Accuracy.CoreSubstituted != 2 {
		t.Errorf("substituted = %d, want 2", r.Accuracy.CoreSubstituted)
	}
}

// A grant that names the tools itself must not look like shell laundering.
func TestBuildRollup_ExactGrantsCountAsNoSubstitution(t *testing.T) {
	records := []ExecutionRecord{{
		TaskID: "t1", Succeeded: true,
		Verdicts: []Verdict{{Probe: grantProbeName, PathCoverage: 1}},
	}}

	r := BuildRollup("baseline", records)

	if r.Accuracy.CoreShellCovered != 0 || r.Accuracy.CoreSubstituted != 0 {
		t.Errorf("an exact grant reported substitutions: shell=%d total=%d",
			r.Accuracy.CoreShellCovered, r.Accuracy.CoreSubstituted)
	}
}
