package hallucination

import (
	"regexp"
	"strconv"
	"strings"
)

// Concrete claim extractors. Each returns the literal value
// PLUS the surrounding sentence so the Signal can carry both
// the value and human-readable context. Sentence is the model's
// own prose around the claim — UI surfaces it directly.

// urlRE matches http(s) URLs. Deliberately permissive on
// trailing punctuation because models often write "https://x.com."
// with a sentence-final period that isn't part of the URL.
// Backslash is excluded because models routinely embed
// JSON-escaped strings in their prose ("url": "https://x.com\"
// — those trailing backslashes are escape syntax, not part of
// the URL, and capturing them produces phantom mismatches
// against the audit which holds the un-escaped form.
// We strip trailing punctuation in extractURLs.
var urlRE = regexp.MustCompile(`https?://[^\s\)<>"'\]\\]+`)

// taskIDRE matches the executor's GenerateID format:
// "task_<14-digit-stamp>_<16-hex>". Tight enough that ordinary
// prose won't accidentally match.
var taskIDRE = regexp.MustCompile(`\btask_\d{14}_[0-9a-f]{16}\b`)

// artifactNameRE matches typical artifact filenames. Conservative —
// requires a known artifact-class extension so prose like "the
// document" or filenames inside code fences (which aren't
// artifact citations) don't trip the rule.
var artifactNameRE = regexp.MustCompile(`\b[A-Za-z0-9_\-]+\.(md|pdf|json|csv|txt|html|patch|diff)\b`)

// numericClaimRE matches "found N", "scraped N", "scored N",
// "returned N" — the patterns that show up in result.json
// summaries when the agent reports counts. Used as a soft
// signal because the artifacts may legitimately have a different
// count (extras filtered, duplicates removed, etc.).
var numericClaimRE = regexp.MustCompile(`(?i)\b(?:found|scraped|scored|returned|matched|fetched|listed|produced|wrote)\s+(\d+)\s+(?:listings?|jobs?|results?|matches?|items?|rows?|records?|entries|files?|artifacts?)\b`)

// extractURLs pulls URLs out of arbitrary text and trims
// trailing punctuation. Returns lowercased canonical forms so
// callers can do membership checks against
// GroundingContext.FetchedURLs without a per-call ToLower.
func extractURLs(text string) []string {
	matches := urlRE.FindAllString(text, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		// Strip a trailing run of punctuation chars that almost
		// always comes from sentence boundaries, not the URL.
		m = strings.TrimRight(m, ".,;:!?)\"'")
		if m == "" {
			continue
		}
		out = append(out, m)
	}
	return out
}

// findSentence returns the sentence-like fragment containing
// the given offset, capped at MaxSentenceBytes. Sentence
// boundaries are loose ("." / "!" / "?" / "\n" / start/end of
// text) — the goal is "enough surrounding prose to make the
// signal readable in the UI", not a perfect NLP boundary.
func findSentence(text string, idx int) string {
	if idx < 0 || idx >= len(text) {
		return ""
	}
	start := idx
	for start > 0 && !isSentenceBoundary(text[start-1]) {
		start--
	}
	end := idx
	for end < len(text) && !isSentenceBoundary(text[end]) {
		end++
	}
	if end < len(text) {
		end++ // include the boundary char so the rendered sentence ends naturally.
	}
	s := strings.TrimSpace(text[start:end])
	if len(s) > MaxSentenceBytes {
		s = s[:MaxSentenceBytes] + "…"
	}
	return s
}

func isSentenceBoundary(b byte) bool {
	return b == '.' || b == '!' || b == '?' || b == '\n'
}

// urlNotFetchedRule emits SeverityHigh when the agent's prose
// quotes a URL that doesn't appear anywhere in the step's
// tool_audit input/output. Only fires when at least one
// fetch-class tool was called — if the step never touched a
// fetcher, citing a URL might be referencing pre-existing
// knowledge (e.g., a prompt that named the URL the agent should
// summarise) and the rule must not fire.
//
// Implementation note: the case-insensitive comparison matches
// extractURLs's lowercase canonical form. URL fragments and
// trailing slashes are normalised by trimming so a model that
// quotes "https://Example.com" and an audit entry of
// "https://example.com/" both ground the claim correctly.
func urlNotFetchedRule(text string, gc *GroundingContext) []Signal {
	if gc == nil {
		return nil
	}
	hasFetcher := false
	for _, n := range gc.ToolCallNames {
		if isFetchTool(n) {
			hasFetcher = true
			break
		}
	}
	if !hasFetcher {
		return nil
	}

	var out []Signal
	for _, raw := range extractURLs(text) {
		key := strings.ToLower(strings.TrimRight(raw, "/"))
		if matchesFetchedURL(key, gc.FetchedURLs) {
			continue
		}
		// Find the position so we can pull a sentence. Search the
		// raw form (case-preserving) since urlRE matched it
		// without case folding.
		idx := strings.Index(text, raw)
		out = append(out, NewSignal(
			"url_not_fetched",
			SeverityHigh,
			"url",
			raw,
			findSentence(text, idx),
			"tool_audit (urls in input/output)",
			"Agent prose cites a URL that does not appear in any tool_audit input or output for this step. Either the agent fabricated the URL or did not actually fetch it.",
		))
	}
	return out
}

// isFetchTool reports whether a tool name is a fetcher. Used by
// urlNotFetchedRule to avoid false positives on steps that
// legitimately quote a URL passed in via the prompt without
// having fetched anything (e.g. a writer summarising a
// research output).
func isFetchTool(name string) bool {
	if strings.HasPrefix(name, "mcp__") {
		// MCP tools include scrapers (web_fetch, web_scrape,
		// http_get) — the qualified prefix is what we treat as
		// fetcher. The narrower form
		// mcp__<server>__web_fetch is what scrapers use; if a
		// future MCP server names a non-fetcher web_*,
		// urlNotFetchedRule overcounts but only as Info-level
		// noise (the URL is in the audit either way).
		return true
	}
	switch name {
	case "web_fetch", "web_scrape", "http_get", "fetch":
		return true
	}
	return false
}

// matchesFetchedURL is a forgiving membership check. Treats two
// URLs as matching if either is a prefix of the other, after
// trim of trailing slash. Catches the common case of a model
// quoting "https://example.com/jobs" while the audit shows
// "https://example.com/jobs/12345" (or vice versa) — both
// indicate the agent actually fetched something on the host.
func matchesFetchedURL(claim string, fetched map[string]struct{}) bool {
	if _, ok := fetched[claim]; ok {
		return true
	}
	for f := range fetched {
		ft := strings.TrimRight(f, "/")
		if strings.HasPrefix(ft, claim) || strings.HasPrefix(claim, ft) {
			return true
		}
	}
	return false
}

// taskIDNotFoundRule emits SeverityHigh when the agent's prose
// references a task ID that's neither (a) the agent's own task,
// (b) one of the dispatcher's "known recent tasks" snapshot,
// nor (c) referenced in any tool input/output for this step.
// The agent's own task ID and execution ID are added to
// KnownTaskIDs by the caller before Scan, so a writer agent
// citing "the task" by ID doesn't trip the rule.
func taskIDNotFoundRule(text string, gc *GroundingContext) []Signal {
	if gc == nil {
		return nil
	}
	matches := taskIDRE.FindAllStringIndex(text, -1)
	if len(matches) == 0 {
		return nil
	}
	var out []Signal
	for _, m := range matches {
		id := text[m[0]:m[1]]
		if _, ok := gc.KnownTaskIDs[id]; ok {
			continue
		}
		if strings.Contains(gc.ToolOutputs, id) {
			continue
		}
		out = append(out, NewSignal(
			"task_id_not_found",
			SeverityHigh,
			"task_id",
			id,
			findSentence(text, m[0]),
			"known_task_ids + tool_audit_outputs",
			"Agent referenced a task ID that does not exist in the recent tasks snapshot and does not appear in any tool output for this turn.",
		))
	}
	return out
}

// projectIDNotFoundRule emits SeverityHigh for project IDs the
// dispatcher cites that aren't in the registry. Distinct from
// taskID because project IDs don't have a regex-pinnable form
// (operator-chosen names) — we instead look for any quoted
// identifier the model claims is a project_id. Implementation
// is a best-effort: scans for "project '<name>'" /
// "project \"<name>\"" / "project_id=<name>" patterns and
// validates the name against KnownProjectIDs.
//
// Empty KnownProjectIDs is "no registry snapshot wired" — the
// rule no-ops in that case rather than flagging every project
// the model mentions.
func projectIDNotFoundRule(text string, gc *GroundingContext) []Signal {
	if gc == nil || len(gc.KnownProjectIDs) == 0 {
		return nil
	}
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`project ['"]([A-Za-z0-9_-]+)['"]`),
		regexp.MustCompile(`project_id\s*[:=]\s*['"]?([A-Za-z0-9_-]+)['"]?`),
	}
	seen := map[string]struct{}{}
	var out []Signal
	for _, re := range patterns {
		for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
			if len(m) < 4 {
				continue
			}
			name := text[m[2]:m[3]]
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
			if _, ok := gc.KnownProjectIDs[name]; ok {
				continue
			}
			out = append(out, NewSignal(
				"project_id_not_found",
				SeverityHigh,
				"project_id",
				name,
				findSentence(text, m[0]),
				"registry_known_project_ids",
				"Dispatcher claimed a project that is not in the registry snapshot for this user.",
			))
		}
	}
	return out
}

// artifactNotProducedRule emits SeverityWarn when the model
// cites an artifact filename that isn't in the producing set
// (executor) or known set (dispatcher). Warn rather than High
// because artifact-class extensions match general filenames in
// prose ("see config.json" inside an explanation of source
// code), and a too-aggressive rule generates noise.
//
// On the executor path, ArtifactNames is the just-produced set
// — a step that says "wrote out.md" but didn't trips here.
// On the dispatcher path, KnownArtifactNames is fed from the
// project's recent artifact list and the rule catches
// "see <invented>.md".
func artifactNotProducedRule(text string, gc *GroundingContext) []Signal {
	if gc == nil {
		return nil
	}
	known := unionStringSets(gc.ArtifactNames, gc.KnownArtifactNames)
	if len(known) == 0 {
		return nil // no ground truth to compare against
	}
	matches := artifactNameRE.FindAllStringIndex(text, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	var out []Signal
	for _, m := range matches {
		name := text[m[0]:m[1]]
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		if _, ok := known[name]; ok {
			continue
		}
		// Skip if the name appears inside a tool output —
		// that means the agent saw it via list_artifacts /
		// read_artifact / similar. Prevents flagging legit
		// references the rule's narrow KnownArtifactNames
		// snapshot didn't cover.
		if strings.Contains(gc.ToolOutputs, name) {
			continue
		}
		out = append(out, NewSignal(
			"artifact_not_produced",
			SeverityWarn,
			"artifact_name",
			name,
			findSentence(text, m[0]),
			"step_artifact_names + tool_audit_outputs",
			"Agent referenced an artifact filename that the step did not produce and that does not appear in any tool output for this turn.",
		))
	}
	return out
}

// numericClaimMismatchRule emits SeverityWarn when the agent
// claims "found N" but the producing artifact contains a
// markedly different count. v1 implementation just looks for
// the claimed N value's substring in tool_outputs; if missing,
// it's a soft signal. A future iteration will join against the
// artifact's actual entry count.
//
// Warn-only because numeric prose is easy to miscount even
// when the underlying work was correct (e.g. "found 12
// listings, 3 duplicates" -> the artifact has 9, not 12, but
// the claim is supported by context). Run primarily so the
// signal lands in the UI for an operator's eyeball check, not
// to trigger retries.
func numericClaimMismatchRule(text string, gc *GroundingContext) []Signal {
	if gc == nil || gc.ToolOutputs == "" {
		return nil
	}
	matches := numericClaimRE.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return nil
	}
	var out []Signal
	for _, m := range matches {
		// m[2..3] is the (\d+) capture group.
		if len(m) < 4 {
			continue
		}
		n := text[m[2]:m[3]]
		if strings.Contains(gc.ToolOutputs, n) {
			continue
		}
		out = append(out, NewSignal(
			"numeric_claim_mismatch",
			SeverityWarn,
			"numeric",
			n,
			findSentence(text, m[0]),
			"tool_audit_outputs",
			"Agent reported a count that does not appear anywhere in the step's tool outputs. Worth a sanity check.",
		))
	}
	return out
}

// hallucinatedToolFormatRule flags audit entries whose tool name
// looks like the model emitted a fictitious tool-call shape — a
// Codex-style XML fragment leaked into the name (`</arg_value>`),
// or a whole shell command stuffed into the name field (spaces,
// pipes, semicolons). Both shapes mean the model invented a
// tool-call format that the runtime can't parse, so the call
// returns 404 and the agent loops with no progress. Surfacing
// this as a high-severity hallucination signal lets the operator
// see "agent hallucinated a non-existent tool format" rather
// than stare at a degenerate-loop / iteration-cap failure with
// no diagnostic.
//
// Real tool names are short, alphanumeric + underscore +
// double-underscore namespacing (file_read, run_shell,
// mcp__broker__place_order). Whitespace, quotes, pipes, or
// closing-tag fragments in the name are unambiguous tells.
//
// Independent of the prose `text` argument — operates purely
// over gc.ToolCallNames, so it fires even when the agent's
// final-turn message is empty or laconic.
func hallucinatedToolFormatRule(text string, gc *GroundingContext) []Signal {
	if gc == nil {
		return nil
	}
	if len(gc.ToolCallNames) == 0 && len(gc.ToolCallInputs) == 0 {
		return nil
	}
	var out []Signal
	seen := make(map[string]struct{}, len(gc.ToolCallNames)+len(gc.ToolCallInputs))
	for _, name := range gc.ToolCallNames {
		if !isHallucinatedToolName(name) {
			continue
		}
		// Dedup on the literal name — a degenerate loop emits
		// the same malformed name repeatedly and a single
		// signal per shape is enough for the operator.
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, NewSignal(
			"hallucinated_tool_format",
			SeverityHigh,
			"tool_format",
			name,
			"",
			"tool_audit_log",
			"Agent emitted a tool call whose name contains shell syntax or XML fragments — the model hallucinated a tool-call format the runtime can't parse, and every retry returns 404 silently. Inspect the agent's prompt template / function-calling shape.",
		))
	}

	// Same XML/tokenizer-leak shapes also appear inside tool
	// argument JSON when the model only got the wrapper half-right
	// — name parses fine (e.g. `run_shell`), but the argument
	// string contains `<arg_value>…</arg_value>` or a tokenizer
	// special token. The runtime happily dispatches the call;
	// the wrapper survives into the audit row and shows up here.
	for _, input := range gc.ToolCallInputs {
		token, ok := schemaWrapperInToolInput(input)
		if !ok {
			continue
		}
		key := "input:" + token
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, NewSignal(
			"hallucinated_tool_format",
			SeverityHigh,
			"tool_args_format",
			token,
			truncateForSignal(input, 240),
			"tool_audit_log",
			"Tool argument contains a tool-call XML wrapper or tokenizer special token — the model emitted its own tool-call shape inside the arguments blob instead of (or in addition to) using the structured call. Inspect the agent's function-calling configuration.",
		))
	}
	return out
}

// schemaWrapperInToolInput reports the first XML wrapper or
// tokenizer special found in the raw tool-input JSON, or false
// when the input is well-formed. Reuses the package-level lists
// so adding a new format upstream covers both prose-leak and
// argument-leak detection in one place.
func schemaWrapperInToolInput(input string) (string, bool) {
	if input == "" {
		return "", false
	}
	for _, w := range toolCallXMLWrappers {
		if strings.Contains(input, w) {
			return w, true
		}
	}
	for _, t := range tokenizerSpecialTokens {
		if strings.Contains(input, t) {
			return t, true
		}
	}
	return "", false
}

// truncateForSignal trims a string to n runes for the Detail
// field of a Signal. Tool inputs can be large JSON blobs and
// the signal store doesn't need the full body — operators just
// need enough context to recognise the shape.
func truncateForSignal(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// isHallucinatedToolName reports whether `name` matches one of
// the malformed shapes produced when a model invents a tool-call
// format. Used by hallucinatedToolFormatRule.
func isHallucinatedToolName(name string) bool {
	if name == "" {
		return false
	}
	// XML closing-tag leak — Codex/Claude-style <tag>...</tag>
	// emitted as plain text instead of as a structured tool
	// call. The </ fragment sticks to the end of the name.
	if strings.Contains(name, "</") {
		return true
	}
	// Whitespace, pipes, or shell separators in the NAME mean
	// the model packed an entire command-line into the name
	// field. Real tool names are single-token identifiers.
	if strings.ContainsAny(name, " \t|;\"'`<>") {
		return true
	}
	return false
}

// schemaLeakageRule flags model control-format tokens leaking
// into the agent's prose response. Two distinct classes:
//
//  1. Tokenizer special tokens — `<|im_start|>`, `<|tool_call|>`,
//     `<|endoftext|>`, etc. These never appear in legitimate
//     human prose; they're tokenizer-only markers a working
//     decoder strips before emitting text. Their presence is
//     conclusive evidence the model is mis-decoding its own
//     template as content. Flagged unconditionally — even inside
//     code blocks, since a code sample mentioning these would be
//     extraordinarily unusual outside an LLM-internals discussion
//     and an operator-visible signal is the right default.
//
//  2. Tool-call XML wrappers — `<function_calls>`, `</invoke>`,
//     `<parameter name=`, `<arg_value>`, etc. Could legitimately
//     appear in code samples ("here's the Claude tool-use
//     shape"), so the rule strips fenced-code (```…```) and
//     inline-backtick (`…`) regions before searching. Flagged
//     when present in raw prose.
//
// Independent of which model emitted the leak: ChatML / Codex /
// Claude / Mistral all have their own variants and operators
// shouldn't have to know which model is in use to recognise the
// failure. The token list deliberately spans formats.
//
// Each unique shape produces one signal even if the model
// repeated it dozens of times — degenerate decoding loops emit
// the same fragment many times and the operator only needs the
// shape once to triage.
func schemaLeakageRule(text string, gc *GroundingContext) []Signal {
	if text == "" {
		return nil
	}
	stripped := stripCodeRegions(text)
	seen := make(map[string]struct{})
	var out []Signal

	// Class 1: tokenizer specials. Searched in raw text — code
	// blocks don't grant a free pass for these.
	for _, tok := range tokenizerSpecialTokens {
		if !strings.Contains(text, tok) {
			continue
		}
		if _, dup := seen[tok]; dup {
			continue
		}
		seen[tok] = struct{}{}
		out = append(out, NewSignal(
			"schema_leakage",
			SeverityHigh,
			"tokenizer_special",
			tok,
			findContaining(text, tok),
			"agent_prose",
			"Agent prose contains a tokenizer special token. The model emitted a template marker as visible content — its decoder is leaking control format. The user is seeing raw template artifacts.",
		))
	}

	// Class 2: tool-call XML wrappers. Searched in code-stripped
	// text so legitimate inline code samples don't false-trip.
	for _, tok := range toolCallXMLWrappers {
		if !strings.Contains(stripped, tok) {
			continue
		}
		if _, dup := seen[tok]; dup {
			continue
		}
		seen[tok] = struct{}{}
		out = append(out, NewSignal(
			"schema_leakage",
			SeverityHigh,
			"tool_call_wrapper",
			tok,
			findContaining(stripped, tok),
			"agent_prose",
			"Agent prose contains a tool-call XML wrapper outside any code block. The model emitted its own tool-call format as message content — its function-calling shape is mis-configured or the prompt template is leaking.",
		))
	}

	return out
}

// tokenizerSpecialTokens is the cross-format set of control
// tokens that should never appear in legitimate prose. Spans
// ChatML (Qwen, Yi, etc.), Codex/Mistral action markers, and
// FIM completion markers. Adding a new format = append here.
var tokenizerSpecialTokens = []string{
	// ChatML-family
	"<|im_start|>", "<|im_end|>",
	"<|user|>", "<|assistant|>", "<|system|>",
	// Tool-call control tokens
	"<|tool_call|>", "<|/tool_call|>",
	"<|begin_of_action|>", "<|end_of_action|>",
	"<|tool_response|>", "<|/tool_response|>",
	// FIM (fill-in-middle) markers — should never escape into
	// chat output, but do appear when a code model is asked
	// general questions and falls back on a completion shape.
	"<|fim_prefix|>", "<|fim_middle|>", "<|fim_suffix|>",
	// End-of-text / response delimiters
	"<|endoftext|>", "<|endofresponse|>", "<|eot_id|>",
}

// toolCallXMLWrappers is the cross-format set of XML-shaped
// tool-call wrappers. The patterns are specific enough that
// they don't false-fire on common HTML/XML in code samples
// (`<div>`, `<input>`, etc.) but loose enough to catch
// attribute variations — `<invoke name="x">` and `<invoke>`
// both match the `<invoke` prefix.
var toolCallXMLWrappers = []string{
	// Claude tool-use format
	"<function_calls>", "</function_calls>",
	"<invoke", "</invoke>",
	"<parameter name=", "</parameter>",
	// Codex / generic XML tool-call format
	"<arg_value>", "</arg_value>",
	"<tool_call>", "</tool_call>",
	"<tool_use>", "</tool_use>",
}

// stripCodeRegions removes fenced-code (```...```) and
// inline-backtick (`...`) content from text so XML-wrapper
// search doesn't false-fire on legitimate code samples.
// Tokenizer specials are searched in the raw text, not this
// stripped form — code blocks shouldn't grant immunity for
// `<|im_start|>` and friends.
func stripCodeRegions(text string) string {
	stripped := codeFencedRE.ReplaceAllString(text, "")
	stripped = codeInlineRE.ReplaceAllString(stripped, "")
	return stripped
}

// codeFencedRE matches a triple-backtick fenced block,
// non-greedy across lines. (?s) enables DOTALL so `.` spans
// newlines.
var codeFencedRE = regexp.MustCompile("(?s)```.*?```")

// codeInlineRE matches an inline-backtick run. Backtick
// interior is forbidden via [^`]* so a stray triple-backtick
// sequence the fenced-pass missed doesn't get re-matched
// here.
var codeInlineRE = regexp.MustCompile("`[^`]*`")

// findContaining returns a short window around the first
// occurrence of needle in haystack — enough context for the
// signal's "sentence" field without dumping the whole prose.
func findContaining(haystack, needle string) string {
	i := strings.Index(haystack, needle)
	if i < 0 {
		return needle
	}
	start := i - 40
	if start < 0 {
		start = 0
	}
	end := i + len(needle) + 40
	if end > len(haystack) {
		end = len(haystack)
	}
	return haystack[start:end]
}

// taIndicatorClaimRE matches "RSI=29.5", "RSI(14)=29.5",
// "SMA(50)=$266.89", "ATR(14): $3.10", "MACD at 0.42", and
// related forms used by the trading strategist's prose
// rationale. Capture groups: (indicator, optional period,
// numeric value). The trailing-numeric capture allows a
// dollar-sign prefix because price-anchored indicators
// (SMA, EMA, ATR, BBands) are commonly written in dollars
// while ratio indicators (RSI, MACD) are not.
//
// Period is optional: "RSI=" (no period) is valid and we
// fall back to "any RSI tool call" for grounding lookup.
//
// The full set we support today: RSI, SMA, EMA, ATR, MACD.
// BBands is intentionally excluded — its output is multi-
// valued (upper/middle/lower), and a single-value claim
// pattern would generate too many false positives.
var taIndicatorClaimRE = regexp.MustCompile(`(?i)\b(RSI|SMA|EMA|ATR|MACD)(?:\((\d+)\))?\s*(?:[=:]|\bat\b|\bof\b)\s*\$?(\d+(?:\.\d+)?)`)

// taLatestRE pulls "latest": <num> values out of the ta
// MCP server's JSON outputs (`{"values": [...], "latest": N}`).
var taLatestRE = regexp.MustCompile(`"latest"\s*:\s*(-?\d+(?:\.\d+)?)`)

// taIndicatorTolerance is how close a claimed value must be
// to a value present in tool_outputs to count as grounded.
// 5% is loose enough to absorb rounding the strategist
// inevitably does ("RSI=29.5" when the tool returned 29.47)
// but tight enough that a fabricated value (RSI=72 when the
// tool returned 41) trips. Applied as a relative percentage
// for non-zero anchors and as an absolute floor of 0.5 for
// near-zero anchors so MACD claims around zero don't get
// graded against an unrealistic relative threshold.
const taIndicatorTolerance = 0.05

// taIndicatorClaimRule emits SeverityWarn when the
// strategist's prose claims an indicator value that doesn't
// appear approximately in any tool output AND a ta-class
// tool was actually called this step. Catches the trading-
// specific hallucination class where the model writes
// "RSI(14)=29.5" to support an entry decision without
// actually having queried the indicator — the operator's
// risk-officer can rubber-stamp such proposals because the
// prose looks authoritative.
//
// Conservative on false positives:
//   - Skipped entirely when no ta-class tool was called
//     (the agent might be referencing pre-existing knowledge
//     in a non-trading workflow, or replaying findings from
//     a prior step's PreviousStepResult).
//   - Tolerance-grounded: a claim within 5% of any value in
//     tool_outputs counts as grounded — strategists round.
//   - Per-claim Warn rather than Block: numeric prose around
//     trading is easy to misformat without bad intent.
func taIndicatorClaimRule(text string, gc *GroundingContext) []Signal {
	if gc == nil || gc.ToolOutputs == "" {
		return nil
	}
	hasTATool := false
	for _, n := range gc.ToolCallNames {
		if strings.HasPrefix(n, "mcp__ta__") {
			hasTATool = true
			break
		}
	}
	if !hasTATool {
		return nil
	}
	groundValues := taExtractGroundValues(gc.ToolOutputs)
	if len(groundValues) == 0 {
		// All TA outputs were null (insufficient history,
		// service hiccup). Flagging without a baseline gives
		// false positives; the upstream ta service returning
		// null is its own observability problem.
		return nil
	}
	matches := taIndicatorClaimRE.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return nil
	}
	var out []Signal
	for _, m := range matches {
		if len(m) < 8 {
			continue
		}
		indicator := strings.ToUpper(text[m[2]:m[3]])
		valueStr := text[m[6]:m[7]]
		claimed, err := strconv.ParseFloat(valueStr, 64)
		if err != nil {
			continue
		}
		if !taValueGrounded(claimed, groundValues) {
			out = append(out, NewSignal(
				"ta_indicator_claim_unsupported",
				SeverityWarn,
				"ta_indicator",
				indicator+"="+valueStr,
				findSentence(text, m[0]),
				"tool_audit_outputs",
				"Strategist claimed a technical indicator value that does not match any value in this step's TA tool outputs (within 5%). Possible fabricated rationale.",
			))
		}
	}
	return out
}

// taExtractGroundValues pulls every numeric value the TA
// tools returned in this step (the "latest" field of the
// ta MCP server's JSON output shape).
func taExtractGroundValues(toolOutputs string) []float64 {
	var out []float64
	for _, m := range taLatestRE.FindAllStringSubmatch(toolOutputs, -1) {
		if len(m) < 2 {
			continue
		}
		v, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			continue
		}
		out = append(out, v)
	}
	return out
}

// taValueGrounded reports whether `claimed` lies within the
// rule's tolerance of any value in `ground`. Relative-
// tolerance band for non-zero anchors plus a small absolute
// floor for near-zero anchors so MACD-style indicators that
// hover around zero don't get graded against an unrealistic
// relative threshold.
func taValueGrounded(claimed float64, ground []float64) bool {
	for _, g := range ground {
		anchor := g
		if anchor < 0 {
			anchor = -anchor
		}
		tol := anchor * taIndicatorTolerance
		if tol < 0.5 {
			tol = 0.5
		}
		diff := claimed - g
		if diff < 0 {
			diff = -diff
		}
		if diff <= tol {
			return true
		}
	}
	return false
}

// unionStringSets returns the union of two map-as-set values.
// Helper used wherever a rule wants to grant credit if either
// the executor-side or dispatcher-side known set vouches for
// a claim.
func unionStringSets(a, b map[string]struct{}) map[string]struct{} {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		out[k] = struct{}{}
	}
	for k := range b {
		out[k] = struct{}{}
	}
	return out
}
