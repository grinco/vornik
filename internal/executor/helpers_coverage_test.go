package executor

import (
	"encoding/json"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// TestExtractTaskType_HappyPath — the payload's taskType field
// drives WhenTaskType filters on declarative verifiers. Pin the
// JSON-path so a future payload schema change surfaces here
// rather than as silent verifier no-ops.
func TestExtractTaskType_HappyPath(t *testing.T) {
	payload := map[string]any{"taskType": "research", "context": map[string]any{"prompt": "x"}}
	body, _ := json.Marshal(payload)
	task := &persistence.Task{Payload: body}
	if got := extractTaskType(task); got != "research" {
		t.Errorf("extractTaskType = %q, want research", got)
	}
}

// TestExtractTaskType_EmptyTaskOrPayload — defensive paths. nil
// task / nil payload / malformed JSON all return empty string so
// the WhenTaskType filter just doesn't match (correct behaviour
// for "task type unknown").
func TestExtractTaskType_EmptyTaskOrPayload(t *testing.T) {
	cases := []*persistence.Task{
		nil,
		{Payload: nil},
		{Payload: []byte{}},
		{Payload: []byte("not json")},
		{Payload: []byte(`{"no":"taskType"}`)},
		{Payload: []byte(`{"taskType":42}`)}, // wrong type
	}
	for i, task := range cases {
		if got := extractTaskType(task); got != "" {
			t.Errorf("case %d: extractTaskType = %q, want empty", i, got)
		}
	}
}

// TestExecutorOptionSetters — every option setter on the
// Executor takes a value and stashes it. Drive all 20+ through
// NewWithOptions in one pass for cheap coverage. None should
// panic on a nil/zero arg.
func TestExecutorOptionSetters(t *testing.T) {
	opts := []Option{
		WithWarmPool(nil),
		WithToolAuditRepository(nil),
		WithConversationalLifecycle(nil, nil, nil),
		WithLLMUsageRepository(nil),
		WithStepOutcomeRepository(nil),
		WithSecrets(nil, nil),
		WithTradingOrderRepo(nil),
		WithHallucinationDetector(nil),
		WithHallucinationMetrics(nil),
		WithJudgeRunner(nil),
		WithCompletionNotifier(nil),
		WithCircuitBreaker(nil),
		WithConfig(nil),
		WithTracer(nil),
		WithArtifactStore(nil),
		WithMemoryIndexer(nil),
		WithIngestQueue(nil),
		WithIngestEnqueueFallbackRecorder(nil),
		WithPricing(nil),
	}
	e := NewWithOptions(nil, nil, nil, nil, nil, opts...)
	if e == nil {
		t.Fatal("NewWithOptions returned nil after option chain")
	}
}
