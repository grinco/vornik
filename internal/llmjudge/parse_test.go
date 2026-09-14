package llmjudge

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// The ParseVerdict / StripThinkBlocks cases below moved from
// internal/hallucination/judge_test.go with the code (2026-09-13
// extraction). The inputs are unchanged; only the call shape is — the
// shared parser returns the head fields plus the raw JSON instead of the
// hallucination package's Verdict.

// TestParseVerdict_PlainJSON — the canonical happy path.
// Models that obey the prompt return clean JSON; the parser
// returns the verdict as-is.
func TestParseVerdict_PlainJSON(t *testing.T) {
	in := `{"decision":"pass","confidence":0.9,"summary":"all claims grounded","signals":[]}`
	decision, confidence, summary, raw, ok := ParseVerdict(in)
	require.True(t, ok)
	assert.Equal(t, persistence.JudgeVerdictPass, decision)
	assert.Equal(t, DecisionPass, decision)
	assert.Equal(t, 0.9, confidence)
	assert.Equal(t, "all claims grounded", summary)
	assert.JSONEq(t, in, string(raw), "raw must be the extracted object so callers can read their own fields")
}

// TestParseVerdict_CodeFenced — small models routinely wrap
// their JSON in markdown code fences. The parser must strip
// them so a perfectly-good verdict isn't lost to formatting.
func TestParseVerdict_CodeFenced(t *testing.T) {
	in := "```json\n{\"decision\":\"fail\",\"confidence\":0.7,\"summary\":\"URL X not in audit\"}\n```"
	decision, _, _, raw, ok := ParseVerdict(in)
	require.True(t, ok)
	assert.Equal(t, persistence.JudgeVerdictFail, decision)
	assert.True(t, json.Valid(raw))
}

// TestParseVerdict_PrefatoryText — some models say "Here is my
// verdict: {...}". The parser strips prefatory text by skipping
// to the first '{'.
func TestParseVerdict_PrefatoryText(t *testing.T) {
	in := "Sure, here's the analysis. {\"decision\":\"abstain\",\"confidence\":0.0,\"summary\":\"no audit\"}"
	decision, _, _, _, ok := ParseVerdict(in)
	require.True(t, ok)
	assert.Equal(t, persistence.JudgeVerdictAbstain, decision)
}

// TestParseVerdict_InvalidDecision — a verdict with a decision
// outside the {pass,fail,abstain} set is malformed; parser
// returns ok=false so the caller falls back to abstain. Without
// this guard a typo'd "passes" would land in the DB and pollute
// dashboards.
func TestParseVerdict_InvalidDecision(t *testing.T) {
	in := `{"decision":"approved","confidence":1.0}`
	_, _, _, raw, ok := ParseVerdict(in)
	assert.False(t, ok)
	assert.Nil(t, raw)
}

// TestParseVerdict_Garbage — a model that fails to produce
// JSON at all returns ok=false; the runner falls back to abstain
// with an error string. No partial parsing — better to abstain
// than misclassify.
func TestParseVerdict_Garbage(t *testing.T) {
	_, _, _, _, ok := ParseVerdict("I cannot determine this without more context.")
	assert.False(t, ok)
	_, _, _, _, ok = ParseVerdict("")
	assert.False(t, ok)
}

// TestParseVerdict_ReasoningModelThinkBlock — reasoning models
// (nvidia.nemotron-nano-9b-v2, deepseek r1, qwen reasoning
// variants) emit a <think>…</think> block before the answer.
// The block routinely contains JSON-like punctuation that would
// confuse the "find first {" extraction. ParseVerdict must
// strip the block before parsing.
func TestParseVerdict_ReasoningModelThinkBlock(t *testing.T) {
	in := "<think>The user wants me to decide pass or fail. Looking at the audit {1: success}, all claims are grounded. So I'll go with pass.</think>\n\n{\"decision\":\"pass\",\"confidence\":0.85,\"summary\":\"grounded\",\"signals\":[]}"
	decision, confidence, _, _, ok := ParseVerdict(in)
	require.True(t, ok, "expected verdict, got !ok")
	assert.Equal(t, persistence.JudgeVerdictPass, decision)
	assert.Equal(t, 0.85, confidence)
}

func TestParseVerdict_MultipleThinkBlocks(t *testing.T) {
	in := "<think>first thought</think>\nsome text\n<think>second {with} braces</think>\n{\"decision\":\"fail\",\"confidence\":0.7,\"summary\":\"x\",\"signals\":[]}"
	decision, _, _, _, ok := ParseVerdict(in)
	require.True(t, ok)
	assert.Equal(t, persistence.JudgeVerdictFail, decision)
}

func TestParseVerdict_UnclosedThinkBlock(t *testing.T) {
	// Truncated response — the model started thinking but ran out
	// of tokens before emitting the answer. ParseVerdict should
	// return ok=false so the caller falls back to abstain (rather
	// than trying to interpret the partial think block as JSON).
	in := "<think>Reasoning about the audit {1: ok"
	_, _, _, _, ok := ParseVerdict(in)
	assert.False(t, ok, "unclosed think with no answer must yield no verdict")
}

func TestStripThinkBlocks(t *testing.T) {
	cases := []struct{ in, want string }{
		{"hello", "hello"},
		{"<think>hidden</think> visible", " visible"},
		{"<think>a</think><think>b</think>final", "final"},
		{"<think>unclosed and ran out", ""},
		{"prefix <think>middle</think> suffix", "prefix  suffix"},
	}
	for _, c := range cases {
		if got := StripThinkBlocks(c.in); got != c.want {
			t.Errorf("StripThinkBlocks(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestAbstainSummary_HasReason — the abstain helper preserves
// the operator-actionable reason in the summary so dashboards
// can group "abstained because LLM error" separately from
// "abstained because no evidence".
func TestAbstainSummary_HasReason(t *testing.T) {
	assert.Equal(t, "judge abstained: LLM error: timeout", AbstainSummary("LLM error: timeout"))
}

func TestValidDecision(t *testing.T) {
	for _, d := range []string{DecisionPass, DecisionFail, DecisionAbstain} {
		assert.True(t, ValidDecision(d), d)
	}
	for _, d := range []string{"", "approved", "PASS", "passes"} {
		assert.False(t, ValidDecision(d), d)
	}
}
