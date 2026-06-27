package hallucination

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ctxWithFetched(urls []string) *GroundingContext {
	gc := &GroundingContext{
		FetchedURLs:        map[string]struct{}{},
		ArtifactNames:      map[string]struct{}{},
		KnownTaskIDs:       map[string]struct{}{},
		KnownProjectIDs:    map[string]struct{}{},
		KnownArtifactNames: map[string]struct{}{},
		ToolCallNames:      []string{"web_fetch"},
	}
	for _, u := range urls {
		gc.FetchedURLs[u] = struct{}{}
	}
	return gc
}

// TestUrlNotFetched_FlagsHallucinatedURL — the headline case
// the rule exists for: a worker writes "see https://x.com" but
// no tool ever fetched x.com. Must surface as High because the
// executor blocks on High and this is the strongest signal of
// fabrication we have.
func TestUrlNotFetched_FlagsHallucinatedURL(t *testing.T) {
	gc := ctxWithFetched([]string{"https://example.com/jobs/123"})
	text := "I scraped the listings from https://hallucinated.example.org/feed and scored them."
	d := NewDefault()
	sigs := d.Scan(text, gc)
	require.Len(t, sigs, 1)
	assert.Equal(t, "url_not_fetched", sigs[0].Detector)
	assert.Equal(t, SeverityHigh, sigs[0].Severity)
	assert.Equal(t, "https://hallucinated.example.org/feed", sigs[0].ClaimValue)
	assert.True(t, d.ShouldBlock(sigs), "high-severity url claim must block the step")
}

// TestUrlNotFetched_PrefixMatchClearsClaim — a model that says
// "https://example.com" while the audit shows
// "https://example.com/jobs/123" hasn't hallucinated; it's
// summarising the host. The fuzzy-match in matchesFetchedURL
// is what makes this work.
func TestUrlNotFetched_PrefixMatchClearsClaim(t *testing.T) {
	gc := ctxWithFetched([]string{"https://example.com/jobs/123"})
	text := "Scraped https://example.com for new postings."
	d := NewDefault()
	sigs := d.Scan(text, gc)
	for _, s := range sigs {
		assert.NotEqual(t, "url_not_fetched", s.Detector,
			"prefix-match should clear https://example.com against https://example.com/jobs/123")
	}
}

// TestUrlNotFetched_NoFetcherCalled_RuleSilent — when the step
// never invoked a fetcher, mentioning a URL is most likely the
// agent quoting back a URL from its prompt. Tripping the rule
// here would generate retry storms on perfectly good summaries.
// The rule must stay silent.
func TestUrlNotFetched_NoFetcherCalled_RuleSilent(t *testing.T) {
	gc := ctxWithFetched(nil)
	gc.ToolCallNames = []string{"file_read", "memory_search"} // no fetcher
	text := "The user's earlier link was https://nope.example.com."
	d := NewDefault()
	sigs := d.Scan(text, gc)
	for _, s := range sigs {
		assert.NotEqual(t, "url_not_fetched", s.Detector,
			"no fetcher in the audit means URL claims must NOT be flagged")
	}
}

// TestUrlNotFetched_TrailingPunctuation — extractURLs must
// strip sentence-final punctuation from URLs, otherwise prose
// like "fetched https://x.com." (with a period) loops false-
// positive against an audit entry of "https://x.com" (no
// period).
func TestUrlNotFetched_TrailingPunctuation(t *testing.T) {
	gc := ctxWithFetched([]string{"https://example.com/jobs"})
	text := "Confirmed: scrape returned 12 listings from https://example.com/jobs."
	d := NewDefault()
	sigs := d.Scan(text, gc)
	for _, s := range sigs {
		assert.NotEqual(t, "url_not_fetched", s.Detector,
			"trailing period must not flip the membership check")
	}
}

// TestTaskIDNotFound_FlagsInventedID — the dispatcher / chat
// case where the model fabricates a task_<...>_<...> string.
// The shape is regex-pinnable enough that ordinary prose won't
// match.
func TestTaskIDNotFound_FlagsInventedID(t *testing.T) {
	gc := &GroundingContext{
		KnownTaskIDs: map[string]struct{}{
			"task_20260502105358_fa493fbdd9ce7ab0": {},
		},
	}
	text := "Your earlier task task_20260999999999_deadbeefcafebabe finished successfully."
	d := NewDefault()
	sigs := d.Scan(text, gc)
	require.Len(t, sigs, 1)
	assert.Equal(t, "task_id_not_found", sigs[0].Detector)
	assert.Equal(t, SeverityHigh, sigs[0].Severity)
}

// TestTaskIDNotFound_AllowsKnownID — the model citing a real
// task ID must not trip the rule. Critical because chat
// answers routinely reference "task X completed".
func TestTaskIDNotFound_AllowsKnownID(t *testing.T) {
	taskID := "task_20260502105358_fa493fbdd9ce7ab0"
	gc := &GroundingContext{KnownTaskIDs: map[string]struct{}{taskID: {}}}
	text := "Your task " + taskID + " just completed."
	d := NewDefault()
	sigs := d.Scan(text, gc)
	for _, s := range sigs {
		assert.NotEqual(t, "task_id_not_found", s.Detector)
	}
}

// TestProjectIDNotFound_FlagsInventedProject — dispatcher
// quotes a project that doesn't exist. The pattern matcher
// looks for "project 'name'" / "project_id=name" forms.
func TestProjectIDNotFound_FlagsInventedProject(t *testing.T) {
	gc := &GroundingContext{
		KnownProjectIDs: map[string]struct{}{"janka": {}, "snake": {}},
	}
	text := "Switched to project 'saascotorial' for this turn."
	d := NewDefault()
	sigs := d.Scan(text, gc)
	require.Len(t, sigs, 1)
	assert.Equal(t, "project_id_not_found", sigs[0].Detector)
	assert.Equal(t, "saascotorial", sigs[0].ClaimValue)
}

// TestProjectIDNotFound_NoSnapshot_RuleSilent — without a
// registry snapshot the rule has no ground truth and must not
// fire. Tests, dev mode, and degraded prod paths all hit this.
func TestProjectIDNotFound_NoSnapshot_RuleSilent(t *testing.T) {
	gc := &GroundingContext{} // empty KnownProjectIDs
	text := "Switched to project 'whatever'."
	d := NewDefault()
	sigs := d.Scan(text, gc)
	assert.Empty(t, sigs)
}

// TestArtifactNotProduced_FlagsInventedFile — the writer-role
// failure mode: agent says "wrote scan-eu-2026.md" but didn't.
// The rule's negative space is "step_artifact_names + tool
// outputs"; absence from BOTH triggers a Warn.
func TestArtifactNotProduced_FlagsInventedFile(t *testing.T) {
	gc := &GroundingContext{
		ArtifactNames: map[string]struct{}{
			"actual-output.md": {},
		},
		ToolOutputs: "no other artifact mentioned here",
	}
	text := "Wrote findings to fabricated-report.md and concluded the scan."
	d := NewDefault()
	sigs := d.Scan(text, gc)
	require.Len(t, sigs, 1)
	assert.Equal(t, "artifact_not_produced", sigs[0].Detector)
	assert.Equal(t, SeverityWarn, sigs[0].Severity,
		"warn rather than block — extension matching has false positives")
	assert.False(t, d.ShouldBlock(sigs), "warn-only signals must not block")
}

// TestArtifactNotProduced_AllowedViaToolOutput — the model
// quoting a name returned by list_artifacts shouldn't trip
// the rule, even if the step itself didn't produce it.
func TestArtifactNotProduced_AllowedViaToolOutput(t *testing.T) {
	gc := &GroundingContext{
		ArtifactNames: map[string]struct{}{},
		ToolOutputs:   "Found 3 artifact(s):\n  - prior-scan.md (output)",
	}
	text := "Latest scan is prior-scan.md from yesterday."
	d := NewDefault()
	sigs := d.Scan(text, gc)
	for _, s := range sigs {
		assert.NotEqual(t, "artifact_not_produced", s.Detector)
	}
}

// TestNumericClaimMismatch_FlagsCountNotInOutputs — agent says
// "found 12 listings" but no instance of "12" appears in the
// audited tool outputs for this step. Soft signal (Warn), since
// the count could be downstream filtering, not fabrication.
func TestNumericClaimMismatch_FlagsCountNotInOutputs(t *testing.T) {
	gc := &GroundingContext{ToolOutputs: "scrape returned the following: <html>...</html>"}
	text := "Found 12 listings matching Janka's profile."
	d := NewDefault()
	sigs := d.Scan(text, gc)
	require.NotEmpty(t, sigs)
	any := false
	for _, s := range sigs {
		if s.Detector == "numeric_claim_mismatch" {
			any = true
			assert.Equal(t, SeverityWarn, s.Severity)
			assert.Equal(t, "12", s.ClaimValue)
		}
	}
	assert.True(t, any, "expected numeric_claim_mismatch among signals")
}

// TestTAIndicatorClaim_FlagsFabricatedRSI — strategist's prose
// claims RSI=29.5 but the actual RSI tool call returned 41.3.
// The claim is well outside the 5% tolerance band (any anchor
// in the [39.2, 43.4] range), so the rule must fire. Captures
// the production failure where the model's rationale named an
// indicator value to support an entry the indicator didn't
// actually support.
func TestTAIndicatorClaim_FlagsFabricatedRSI(t *testing.T) {
	gc := &GroundingContext{
		ToolCallNames: []string{"mcp__ta__rsi", "mcp__broker__get_quote"},
		ToolOutputs:   `{"values": [42.1, 41.7, 41.3], "latest": 41.3}`,
	}
	text := "AAPL: RSI(14)=29.5 below oversold threshold; entering long."
	d := NewDefault()
	sigs := d.Scan(text, gc)
	any := false
	for _, s := range sigs {
		if s.Detector == "ta_indicator_claim_unsupported" {
			any = true
			assert.Equal(t, SeverityWarn, s.Severity)
			assert.Contains(t, s.ClaimValue, "RSI")
		}
	}
	assert.True(t, any, "expected ta_indicator_claim_unsupported among signals")
}

// TestTAIndicatorClaim_AcceptsRoundedValue — the strategist
// rounds tool outputs in prose ("RSI=29.5" when the tool
// returned 29.47). Tolerance must be loose enough to accept
// honest rounding; otherwise the rule turns into a false-
// positive factory on every legitimate strategist output.
func TestTAIndicatorClaim_AcceptsRoundedValue(t *testing.T) {
	gc := &GroundingContext{
		ToolCallNames: []string{"mcp__ta__rsi"},
		ToolOutputs:   `{"latest": 29.47}`,
	}
	text := "AAPL: RSI(14)=29.5 below threshold."
	d := NewDefault()
	sigs := d.Scan(text, gc)
	for _, s := range sigs {
		if s.Detector == "ta_indicator_claim_unsupported" {
			t.Fatalf("rounding within 5%% must not trip: %+v", s)
		}
	}
}

// TestTAIndicatorClaim_QuietWhenNoTATool — the rule must NOT
// fire when no ta-class tool was called this step. The agent
// might be replaying findings from a prior step's
// PreviousStepResult or referencing pre-trained knowledge in
// a non-trading workflow; either way the rule has no
// grounding to compare against and must stay quiet.
func TestTAIndicatorClaim_QuietWhenNoTATool(t *testing.T) {
	gc := &GroundingContext{
		ToolCallNames: []string{"mcp__broker__get_quote"},
		ToolOutputs:   `{"last": 277, "ask": 277}`,
	}
	text := "AAPL: RSI(14)=29.5 below threshold."
	d := NewDefault()
	sigs := d.Scan(text, gc)
	for _, s := range sigs {
		if s.Detector == "ta_indicator_claim_unsupported" {
			t.Fatalf("rule must stay quiet without a TA tool call: %+v", s)
		}
	}
}

// TestTAIndicatorClaim_QuietWhenAllOutputsNull — the upstream
// ta MCP server returns null for insufficient-history symbols.
// Flagging in that state is a false positive (the strategist
// shouldn't be claiming values when null came back, but the
// observability problem is the ta service, not the strategist).
func TestTAIndicatorClaim_QuietWhenAllOutputsNull(t *testing.T) {
	gc := &GroundingContext{
		ToolCallNames: []string{"mcp__ta__rsi"},
		ToolOutputs:   `{"values": [null, null], "latest": null}`,
	}
	text := "AAPL: RSI(14)=29.5 below threshold."
	d := NewDefault()
	sigs := d.Scan(text, gc)
	for _, s := range sigs {
		if s.Detector == "ta_indicator_claim_unsupported" {
			t.Fatalf("rule must stay quiet when all TA outputs are null: %+v", s)
		}
	}
}

// TestTAIndicatorClaim_HandlesPriceAnchored — SMA / ATR / EMA
// outputs are dollar values and the strategist commonly
// writes them with a $ prefix. The relative-tolerance band
// scales with the anchor magnitude so a $266 SMA rounded to
// $267 grounds, but a $266 SMA misquoted as $200 trips.
func TestTAIndicatorClaim_HandlesPriceAnchored(t *testing.T) {
	gc := &GroundingContext{
		ToolCallNames: []string{"mcp__ta__sma"},
		ToolOutputs:   `{"latest": 266.89}`,
	}
	d := NewDefault()

	// Within tolerance: rounded prose claim grounds.
	sigsClean := d.Scan("price $277 above SMA(50)=$267.00 by 3.7%.", gc)
	for _, s := range sigsClean {
		if s.Detector == "ta_indicator_claim_unsupported" {
			t.Fatalf("$267 within tolerance of $266.89 must ground: %+v", s)
		}
	}

	// Way outside tolerance: trips.
	sigsBad := d.Scan("price $277 above SMA(50)=$200.00 — entering.", gc)
	any := false
	for _, s := range sigsBad {
		if s.Detector == "ta_indicator_claim_unsupported" {
			any = true
		}
	}
	assert.True(t, any, "expected SMA mismatch from $200 vs ground $266.89")
}

// TestHighestSeverity_OrderingHolds — pin the rank order.
// Future code paths may depend on "any High beats every Warn"
// for retry decisions; a test pins the ordering so a refactor
// that flips the constants doesn't quietly invert behaviour.
func TestHighestSeverity_OrderingHolds(t *testing.T) {
	assert.Equal(t, SeverityHigh, HighestSeverity([]Signal{
		{Severity: SeverityWarn},
		{Severity: SeverityHigh},
		{Severity: SeverityInfo},
	}))
	assert.Equal(t, SeverityWarn, HighestSeverity([]Signal{
		{Severity: SeverityWarn},
		{Severity: SeverityInfo},
	}))
	assert.Equal(t, Severity(""), HighestSeverity(nil))
}

// TestSignal_Truncates — the JSONB column on
// execution_step_outcomes can hold many signals; the
// constructor must truncate ClaimValue / Sentence so a single
// pathological model output doesn't bloat the row.
func TestSignal_Truncates(t *testing.T) {
	long := make([]byte, MaxClaimValueBytes+200)
	for i := range long {
		long[i] = 'a'
	}
	s := NewSignal("x", SeverityInfo, "url", string(long), string(long), "", "")
	assert.LessOrEqual(t, len(s.ClaimValue), MaxClaimValueBytes+4) // ascii + ellipsis
	assert.LessOrEqual(t, len(s.Sentence), MaxSentenceBytes+4)
}

// TestExtractURLs_StopsAtBackslash — production false-positive:
// when an agent's prose contains a JSON-escaped URL like
// `"url": "https://x.com\"`, the v1 regex captured the
// trailing `\\` and the membership check then missed the
// audit's un-escaped form. Backslash must terminate the URL
// match.
func TestExtractURLs_StopsAtBackslash(t *testing.T) {
	in := `the model wrote {"input":"{\"url\":\"https://www.bbc.com/news\"}"} into its summary`
	urls := extractURLs(in)
	require.NotEmpty(t, urls)
	for _, u := range urls {
		assert.NotContains(t, u, `\`,
			"captured URL %q must not contain a backslash; the regex needs to terminate at the JSON escape", u)
	}
}

// TestUrlNotFetched_HandlesJSONEscapedURLs — end-to-end
// counterpart of TestExtractURLs_StopsAtBackslash:
// productionprose like `"url": "https://x.com"` (with
// JSON-escape-rendered quotes) must NOT trip the rule when the
// underlying URL was actually fetched. Pre-fix, the captured
// claim ended in `\\` and didn't match the audit's clean
// `https://x.com`, producing 31 phantom signals on a single
// step.
func TestUrlNotFetched_HandlesJSONEscapedURLs(t *testing.T) {
	gc := ctxWithFetched([]string{"https://www.bbc.com/news"})
	text := `Tool result: {"input": "{\"url\": \"https://www.bbc.com/news\"}"}`
	d := NewDefault()
	sigs := d.Scan(text, gc)
	for _, s := range sigs {
		assert.NotEqual(t, "url_not_fetched", s.Detector,
			"JSON-escaped URL whose unescaped form is in the audit must not flag")
	}
}

// TestScan_NilOrEmptyInputs_NoSignal — defensive: the scan
// path runs in two hot loops (per-step and per-chat-turn). A
// nil context must short-circuit cleanly.
func TestScan_NilOrEmptyInputs_NoSignal(t *testing.T) {
	d := NewDefault()
	assert.Nil(t, d.Scan("", &GroundingContext{}))
	assert.Nil(t, d.Scan("any text", nil))
	var nilDetector *Detector
	assert.Nil(t, nilDetector.Scan("text", &GroundingContext{}))
}

// TestHallucinatedToolFormat_FlagsXMLFragmentInName pins the
// regression that motivated the rule (snake task
// task_20260502222422_b6c8a5ae8750fa64): the agent emitted a
// Codex-style <arg_value>...</arg_value> tool call as plain
// text, the parser captured the entire shell command + closing
// tag as the tool name, the runtime returned 404 for every
// retry, and 75 iterations later the task failed with no
// detector signal. The rule must fire so the operator sees
// "model hallucinated a tool-call format" instead of having to
// dig through journals.
func TestHallucinatedToolFormat_FlagsXMLFragmentInName(t *testing.T) {
	gc := &GroundingContext{
		ToolCallNames: []string{
			`grep -n "case '1':\|case '2':" project/index.html</arg_value>`,
		},
	}
	d := NewDefault()
	sigs := d.Scan("", gc)
	require.Len(t, sigs, 1)
	s := sigs[0]
	assert.Equal(t, "hallucinated_tool_format", s.Detector)
	assert.Equal(t, SeverityHigh, s.Severity)
	assert.Contains(t, s.ClaimValue, "</arg_value>")
}

// TestHallucinatedToolFormat_FlagsRunShellArgVariant pins the
// 2026-05-03 recurrence (task_20260503231658_5d6a07a4d2a2ca05):
// agent emitted a chained `grep ... || grep ... || echo ...`
// shell pipeline with a trailing </arg_value> as the tool name —
// what should have been the `command` argument value of run_shell
// landed in the name slot. Distinct from the earlier regression
// because of the multi-stage `||` chain and shell redirects
// (`2>/dev/null`). The rule must fire on either signal (the XML
// closing tag OR the embedded shell control characters).
func TestHallucinatedToolFormat_FlagsRunShellArgVariant(t *testing.T) {
	gc := &GroundingContext{
		ToolCallNames: []string{
			`grep -n "canvas.*500" project/index.html 2>/dev/null || grep -n "background.*image\|img.*src" project/index.html 2>/dev/null || echo "No background image references found"</arg_value>`,
		},
	}
	d := NewDefault()
	sigs := d.Scan("", gc)
	require.Len(t, sigs, 1, "rule must fire exactly once for this name")
	s := sigs[0]
	assert.Equal(t, "hallucinated_tool_format", s.Detector)
	assert.Equal(t, SeverityHigh, s.Severity)
}

// TestHallucinatedToolFormat_FlagsShellCommandAsName covers
// the related variant where no XML leaks but the model packs
// an entire shell pipeline into the name field. Real tool
// names are short single-token identifiers — whitespace alone
// is enough signal.
func TestHallucinatedToolFormat_FlagsShellCommandAsName(t *testing.T) {
	gc := &GroundingContext{
		ToolCallNames: []string{
			"set -e; mkdir -p artifacts/out; cat > artifacts/out/audit-latest.md",
		},
	}
	sigs := d_default().Scan("", gc)
	require.Len(t, sigs, 1)
	assert.Equal(t, "hallucinated_tool_format", sigs[0].Detector)
}

// TestHallucinatedToolFormat_FlagsXMLWrapperInToolInput — when
// the model gets the tool name right (`run_shell`) but emits its
// own XML tool-call wrapper INSIDE the arguments blob, the runtime
// dispatches the call and the wrapper survives into the audit
// row. Without scanning ToolCallInputs the rule misses this whole
// class. The 2026-05-03 incident
// (task_20260503231658_5d6a07a4d2a2ca05) is a real-world hit on
// this path: name=run_shell, arguments contained the embedded
// `…</arg_value>` fragment.
func TestHallucinatedToolFormat_FlagsXMLWrapperInToolInput(t *testing.T) {
	gc := &GroundingContext{
		ToolCallNames: []string{"run_shell"},
		ToolCallInputs: []string{
			`{"command":"grep -n \"canvas.*500\" project/index.html 2>/dev/null || echo none</arg_value>"}`,
		},
	}
	d := NewDefault()
	sigs := d.Scan("", gc)
	require.Len(t, sigs, 1)
	s := sigs[0]
	assert.Equal(t, "hallucinated_tool_format", s.Detector)
	assert.Equal(t, SeverityHigh, s.Severity)
	assert.Equal(t, "tool_args_format", s.ClaimType)
	assert.Equal(t, "</arg_value>", s.ClaimValue)
}

// TestHallucinatedToolFormat_FlagsTokenizerSpecialInToolInput —
// a model that mis-decodes its own template can spill ChatML
// markers into the arguments string. Should fire High the same
// way the prose schema_leakage rule does.
func TestHallucinatedToolFormat_FlagsTokenizerSpecialInToolInput(t *testing.T) {
	gc := &GroundingContext{
		ToolCallNames: []string{"file_read"},
		ToolCallInputs: []string{
			`{"path":"/etc/hosts<|im_end|>"}`,
		},
	}
	sigs := d_default().Scan("", gc)
	require.Len(t, sigs, 1)
	assert.Equal(t, "hallucinated_tool_format", sigs[0].Detector)
	assert.Equal(t, "<|im_end|>", sigs[0].ClaimValue)
}

// TestHallucinatedToolFormat_LegitimateToolInputsPass — well-
// formed JSON arguments must not false-fire even when they
// happen to mention HTML-shaped content (a code-search query,
// a template snippet). The wrapper list is specific enough
// that bare `<div>` or `<span>` won't trigger.
func TestHallucinatedToolFormat_LegitimateToolInputsPass(t *testing.T) {
	gc := &GroundingContext{
		ToolCallNames: []string{"run_shell", "file_read"},
		ToolCallInputs: []string{
			`{"command":"grep -n '<div class=\"canvas\">' project/index.html"}`,
			`{"path":"src/components/Modal.tsx"}`,
			`{"query":"<button onClick"}`,
		},
	}
	sigs := d_default().Scan("", gc)
	for _, s := range sigs {
		assert.NotEqual(t, "hallucinated_tool_format", s.Detector,
			"rule must not fire on legitimate inputs — got: %s", s.ClaimValue)
	}
}

// TestHallucinatedToolFormat_DedupsRepeatedShape — a
// degenerate loop emits the same malformed name dozens of
// times. Operator wants ONE signal per shape, not 50.
func TestHallucinatedToolFormat_DedupsRepeatedShape(t *testing.T) {
	bad := `grep -n "x" project/index.html</arg_value>`
	gc := &GroundingContext{
		ToolCallNames: []string{bad, bad, bad, bad, bad},
	}
	sigs := d_default().Scan("", gc)
	assert.Len(t, sigs, 1, "duplicates collapse to a single signal per malformed name")
}

// TestHallucinatedToolFormat_LegitimateToolNamesPass — the
// rule must NOT false-positive on real tool names, including
// the vornik MCP namespacing pattern (mcp__server__tool).
func TestHallucinatedToolFormat_LegitimateToolNamesPass(t *testing.T) {
	gc := &GroundingContext{
		ToolCallNames: []string{
			"file_read",
			"run_shell",
			"grep",
			"glob",
			"memory_search",
			"mcp__broker__get_account_summary",
			"mcp__news__news_recent",
			"mcp__ta__sma",
		},
	}
	sigs := d_default().Scan("", gc)
	for _, s := range sigs {
		assert.NotEqual(t, "hallucinated_tool_format", s.Detector,
			"rule must not fire on legitimate names — got: %s", s.ClaimValue)
	}
}

func d_default() *Detector { return NewDefault() }

// TestSchemaLeakage_FlagsTokenizerSpecial — the headline case:
// a model emits its own ChatML / Codex template marker as
// visible content. Never legitimate, must fire High.
func TestSchemaLeakage_FlagsTokenizerSpecial(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"chatml im_start", "Sure, here's the plan.<|im_start|>assistant\nrun the script", "<|im_start|>"},
		{"tool_call special", "I'll proceed.<|tool_call|>{\"name\":\"x\"}", "<|tool_call|>"},
		{"endoftext", "The summary is ready.<|endoftext|>", "<|endoftext|>"},
		{"llama eot_id", "Done.<|eot_id|>", "<|eot_id|>"},
	}
	d := NewDefault()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sigs := d.Scan(tc.text, &GroundingContext{})
			require.Len(t, sigs, 1, "expected one schema_leakage signal")
			s := sigs[0]
			assert.Equal(t, "schema_leakage", s.Detector)
			assert.Equal(t, SeverityHigh, s.Severity)
			assert.Equal(t, "tokenizer_special", s.ClaimType)
			assert.Equal(t, tc.want, s.ClaimValue)
		})
	}
}

// TestSchemaLeakage_FlagsToolCallWrappersInProse — Claude /
// Codex XML tool-call shapes leaking into message text.
// Outside any code block; must fire.
func TestSchemaLeakage_FlagsToolCallWrappersInProse(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"function_calls open", "Now I'll execute: <function_calls> grep ...", "<function_calls>"},
		{"invoke open", "Step one: <invoke name=\"grep\"> done", "<invoke"},
		{"arg_value close", "ran grep with </arg_value> for the patterns", "</arg_value>"},
		{"parameter open", "I'll set <parameter name=\"path\">x</parameter> next", "<parameter name="},
	}
	d := NewDefault()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sigs := d.Scan(tc.text, &GroundingContext{})
			// Find the schema_leakage signal among any others
			// (parameter+/parameter together would surface two —
			// at least one must match).
			var got *Signal
			for i := range sigs {
				if sigs[i].Detector == "schema_leakage" && sigs[i].ClaimValue == tc.want {
					got = &sigs[i]
					break
				}
			}
			require.NotNil(t, got, "expected schema_leakage signal for %q, got %+v", tc.want, sigs)
			assert.Equal(t, SeverityHigh, got.Severity)
			assert.Equal(t, "tool_call_wrapper", got.ClaimType)
		})
	}
}

// TestSchemaLeakage_IgnoresWrappersInsideCodeBlocks — operators
// frequently ask agents to explain or generate Claude tool-use
// shapes; those legitimate samples sit inside code fences and
// must not trip the rule.
func TestSchemaLeakage_IgnoresWrappersInsideCodeBlocks(t *testing.T) {
	d := NewDefault()

	// Fenced code: must NOT fire.
	fenced := "Here's the Claude tool-use format:\n```xml\n<function_calls>\n  <invoke name=\"grep\">\n    <parameter name=\"q\">x</parameter>\n  </invoke>\n</function_calls>\n```\nThat's how it looks."
	for _, s := range d.Scan(fenced, &GroundingContext{}) {
		assert.NotEqual(t, "schema_leakage", s.Detector,
			"fenced-code XML wrapper must not fire schema_leakage; got %s=%q", s.ClaimType, s.ClaimValue)
	}

	// Inline backticks: must NOT fire.
	inline := "Use the `<function_calls>` element to wrap calls."
	for _, s := range d.Scan(inline, &GroundingContext{}) {
		assert.NotEqual(t, "schema_leakage", s.Detector,
			"inline-backtick XML wrapper must not fire; got %s=%q", s.ClaimType, s.ClaimValue)
	}
}

// TestSchemaLeakage_TokenizerSpecialFiresEvenInCode —
// distinct from XML wrappers: tokenizer-only markers like
// `<|im_start|>` are never legitimate prose content, even
// inside a code block, because they're not user-typed text —
// the operator seeing them in any output is conclusive
// evidence of decoder leakage.
func TestSchemaLeakage_TokenizerSpecialFiresEvenInCode(t *testing.T) {
	text := "Example trace:\n```\n<|im_start|>user\nhi\n```"
	sigs := NewDefault().Scan(text, &GroundingContext{})
	var got *Signal
	for i := range sigs {
		if sigs[i].Detector == "schema_leakage" {
			got = &sigs[i]
			break
		}
	}
	require.NotNil(t, got, "tokenizer special inside code block must still fire")
	assert.Equal(t, "<|im_start|>", got.ClaimValue)
}

// TestSchemaLeakage_DedupsRepeatedShape — degenerate decoding
// loops emit the same fragment dozens of times. Operator
// wants ONE signal per shape.
func TestSchemaLeakage_DedupsRepeatedShape(t *testing.T) {
	text := "<|im_start|>once<|im_start|>twice<|im_start|>thrice"
	sigs := NewDefault().Scan(text, &GroundingContext{})
	count := 0
	for _, s := range sigs {
		if s.Detector == "schema_leakage" && s.ClaimValue == "<|im_start|>" {
			count++
		}
	}
	assert.Equal(t, 1, count, "duplicate occurrences collapse to a single signal per shape")
}

// TestSchemaLeakage_LegitimateProseSilent — narrative prose,
// HTML samples, ordinary code references must not fire. The
// rule is high-precision by design.
func TestSchemaLeakage_LegitimateProseSilent(t *testing.T) {
	cases := []string{
		"I scanned the listings and found 12 candidates.",
		"The HTML uses <div class=\"foo\"> for the wrapper.",
		"We'll invoke the grep tool next.",
		"See <https://example.com> for details.",
	}
	d := NewDefault()
	for _, text := range cases {
		t.Run(text[:min(40, len(text))], func(t *testing.T) {
			for _, s := range d.Scan(text, &GroundingContext{}) {
				assert.NotEqual(t, "schema_leakage", s.Detector,
					"rule must not fire on legitimate prose; got %s=%q", s.ClaimType, s.ClaimValue)
			}
		})
	}
}
