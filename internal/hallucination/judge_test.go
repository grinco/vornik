package hallucination

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

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

// TestParseVerdict_KeepsSignalsAndStampsRecordedAt pins what the
// hallucination wrapper adds on top of llmjudge.ParseVerdict (the
// 2026-09-13 extraction): the per-claim Signals survive the round trip
// through the shared parser's raw JSON, and each gets a RecordedAt stamp
// because the LLM never carries one. The tolerant-extraction cases
// (fences, prose, <think> blocks) live in internal/llmjudge with the code.
func TestParseVerdict_KeepsSignalsAndStampsRecordedAt(t *testing.T) {
	in := "<think>{not json}</think>\n```json\n{\"decision\":\"fail\",\"confidence\":0.7,\"summary\":\"URL X not in audit\",\"signals\":[{\"detector\":\"judge\",\"severity\":\"warn\",\"claim_type\":\"url\",\"claim_value\":\"https://x\",\"detail\":\"not in audit\"}]}\n```"
	v := parseVerdict(in)
	require.NotNil(t, v)
	assert.Equal(t, persistence.JudgeVerdictFail, v.Decision)
	assert.Equal(t, 0.7, v.Confidence)
	assert.Equal(t, "URL X not in audit", v.Summary)
	require.Len(t, v.Signals, 1)
	assert.Equal(t, "https://x", v.Signals[0].ClaimValue)
	assert.False(t, v.Signals[0].RecordedAt.IsZero(), "RecordedAt must be stamped on parse")
}

// TestParseVerdict_MalformedSignalsIsNil: a verdict whose head parses but
// whose signals are the wrong shape is rejected whole, as it was before the
// extraction — better to abstain than to persist a half-read verdict.
func TestParseVerdict_MalformedSignalsIsNil(t *testing.T) {
	in := `{"decision":"pass","confidence":0.9,"summary":"ok","signals":"not-a-list"}`
	assert.Nil(t, parseVerdict(in))
	assert.Nil(t, parseVerdict("I cannot determine this without more context."))
	assert.Nil(t, parseVerdict(`{"decision":"approved","confidence":1.0}`))
}
