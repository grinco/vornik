package hallucination

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// TestParseVerdict_PlainJSON — the canonical happy path.
// Models that obey the prompt return clean JSON; the parser
// returns the verdict as-is.
func TestParseVerdict_PlainJSON(t *testing.T) {
	in := `{"decision":"pass","confidence":0.9,"summary":"all claims grounded","signals":[]}`
	v := parseVerdict(in)
	require.NotNil(t, v)
	assert.Equal(t, persistence.JudgeVerdictPass, v.Decision)
	assert.Equal(t, 0.9, v.Confidence)
}

// TestParseVerdict_CodeFenced — small models routinely wrap
// their JSON in markdown code fences. The parser must strip
// them so a perfectly-good verdict isn't lost to formatting.
func TestParseVerdict_CodeFenced(t *testing.T) {
	in := "```json\n{\"decision\":\"fail\",\"confidence\":0.7,\"summary\":\"URL X not in audit\"}\n```"
	v := parseVerdict(in)
	require.NotNil(t, v)
	assert.Equal(t, persistence.JudgeVerdictFail, v.Decision)
}

// TestParseVerdict_PrefatoryText — some models say "Here is my
// verdict: {...}". The parser strips prefatory text by skipping
// to the first '{'.
func TestParseVerdict_PrefatoryText(t *testing.T) {
	in := "Sure, here's the analysis. {\"decision\":\"abstain\",\"confidence\":0.0,\"summary\":\"no audit\"}"
	v := parseVerdict(in)
	require.NotNil(t, v)
	assert.Equal(t, persistence.JudgeVerdictAbstain, v.Decision)
}

// TestParseVerdict_InvalidDecision — a verdict with a decision
// outside the {pass,fail,abstain} set is malformed; parser
// returns nil so the caller falls back to abstain. Without this
// guard a typo'd "passes" would land in the DB and pollute
// dashboards.
func TestParseVerdict_InvalidDecision(t *testing.T) {
	in := `{"decision":"approved","confidence":1.0}`
	v := parseVerdict(in)
	assert.Nil(t, v)
}

// TestParseVerdict_Garbage — a model that fails to produce
// JSON at all returns nil; the runner falls back to abstain
// with an error string. No partial parsing — better to abstain
// than misclassify.
func TestParseVerdict_Garbage(t *testing.T) {
	in := "I cannot determine this without more context."
	v := parseVerdict(in)
	assert.Nil(t, v)
}

// TestStubJudge_ReturnsConfiguredVerdict — sanity check on
// the test stub itself, since runner tests rely on it being
// transparent.
func TestStubJudge_ReturnsConfiguredVerdict(t *testing.T) {
	want := &Verdict{Decision: persistence.JudgeVerdictPass, Confidence: 1}
	stub := &StubJudge{Out: want}
	got, metrics, err := stub.Evaluate(context.Background(), JudgeInput{})
	require.NoError(t, err)
	assert.Same(t, want, got)
	require.NotNil(t, metrics, "stub must always return non-nil metrics so callers don't have to nil-check")
	assert.Equal(t, 0, metrics.PromptTokens, "stub default metrics carry zero tokens")
}

// TestStubJudge_HonoursConfiguredMetrics — runner tests rely on
// being able to configure both the verdict and the metrics
// returned, so the cost-recording path can be exercised
// without a real LLM.
func TestStubJudge_HonoursConfiguredMetrics(t *testing.T) {
	stub := &StubJudge{
		Out: &Verdict{Decision: persistence.JudgeVerdictPass},
		Metrics: &JudgeMetrics{
			Model:            "openai.gpt-oss-120b-1:0",
			PromptTokens:     500,
			CompletionTokens: 200,
		},
	}
	_, m, err := stub.Evaluate(context.Background(), JudgeInput{})
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Equal(t, 500, m.PromptTokens)
	assert.Equal(t, 200, m.CompletionTokens)
	assert.Equal(t, "openai.gpt-oss-120b-1:0", m.Model)
}

// TestAbstainVerdict_HasReason — the abstain helper preserves
// the operator-actionable reason in the summary so dashboards
// can group "abstained because LLM error" separately from
// "abstained because no evidence".
func TestAbstainVerdict_HasReason(t *testing.T) {
	v := abstainVerdict("LLM error: timeout")
	assert.Equal(t, persistence.JudgeVerdictAbstain, v.Decision)
	assert.Contains(t, v.Summary, "timeout")
}

// TestParseVerdict_ReasoningModelThinkBlock — reasoning models
// (nvidia.nemotron-nano-9b-v2, deepseek r1, qwen reasoning
// variants) emit a <think>…</think> block before the answer.
// The block routinely contains JSON-like punctuation that would
// confuse the "find first {" extraction. parseVerdict must
// strip the block before parsing.
func TestParseVerdict_ReasoningModelThinkBlock(t *testing.T) {
	in := "<think>The user wants me to decide pass or fail. Looking at the audit {1: success}, all claims are grounded. So I'll go with pass.</think>\n\n{\"decision\":\"pass\",\"confidence\":0.85,\"summary\":\"grounded\",\"signals\":[]}"
	v := parseVerdict(in)
	require.NotNil(t, v, "expected verdict, got nil")
	assert.Equal(t, persistence.JudgeVerdictPass, v.Decision)
	assert.Equal(t, 0.85, v.Confidence)
}

func TestParseVerdict_MultipleThinkBlocks(t *testing.T) {
	in := "<think>first thought</think>\nsome text\n<think>second {with} braces</think>\n{\"decision\":\"fail\",\"confidence\":0.7,\"summary\":\"x\",\"signals\":[]}"
	v := parseVerdict(in)
	require.NotNil(t, v)
	assert.Equal(t, persistence.JudgeVerdictFail, v.Decision)
}

func TestParseVerdict_UnclosedThinkBlock(t *testing.T) {
	// Truncated response — the model started thinking but ran out
	// of tokens before emitting the answer. parseVerdict should
	// return nil so the caller falls back to abstain (rather than
	// trying to interpret the partial think block as JSON).
	in := "<think>Reasoning about the audit {1: ok"
	v := parseVerdict(in)
	assert.Nil(t, v, "unclosed think with no answer must yield nil verdict")
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
		if got := stripThinkBlocks(c.in); got != c.want {
			t.Errorf("stripThinkBlocks(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
