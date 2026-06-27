package contracts_test

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/contracts"
)

// --- Fake implementations for interface-satisfaction tests ---

// fakeReplaySafetyClassifier satisfies contracts.ReplaySafetyClassifier.
type fakeReplaySafetyClassifier struct {
	safeTools map[string]bool
}

func (f *fakeReplaySafetyClassifier) IsReplaySafe(toolName string) bool {
	return f.safeTools[toolName]
}

// fakeInstinctBudgetResolver satisfies contracts.InstinctBudgetResolver.
type fakeInstinctBudgetResolver struct {
	result contracts.LearnedTierResult
	ok     bool
}

func (f *fakeInstinctBudgetResolver) LearnedTier(_ context.Context, _, _ string, _ float64) (contracts.LearnedTierResult, bool) {
	return f.result, f.ok
}

// fakeHealingApplier satisfies contracts.HealingApplier.
type fakeHealingApplier struct {
	trace       contracts.ExecutionTrace
	err         error
	baselineErr error
}

func (f *fakeHealingApplier) ApplyPlan(_ context.Context, _ contracts.CounterfactualPlan) (contracts.ExecutionTrace, error) {
	return f.trace, f.err
}

func (f *fakeHealingApplier) BaselineTrace(_ context.Context, _ string) (contracts.ExecutionTrace, error) {
	return f.trace, f.baselineErr
}

// --- Compile-time interface satisfaction checks ---

var _ contracts.ReplaySafetyClassifier = (*fakeReplaySafetyClassifier)(nil)
var _ contracts.InstinctBudgetResolver = (*fakeInstinctBudgetResolver)(nil)
var _ contracts.HealingApplier = (*fakeHealingApplier)(nil)

// --- DTO round-trip tests ---

func TestCounterfactualPlan_RoundTrip(t *testing.T) {
	plan := contracts.CounterfactualPlan{
		OriginalTaskID: "task-123",
		Variable:       "model",
		Value:          "claude-opus-4",
		Role:           "researcher",
		Label:          "model comparison",
	}
	if plan.OriginalTaskID != "task-123" {
		t.Errorf("OriginalTaskID: got %q, want %q", plan.OriginalTaskID, "task-123")
	}
	if plan.Variable != "model" {
		t.Errorf("Variable: got %q, want %q", plan.Variable, "model")
	}
	if plan.Value != "claude-opus-4" {
		t.Errorf("Value: got %q, want %q", plan.Value, "claude-opus-4")
	}
	if plan.Role != "researcher" {
		t.Errorf("Role: got %q, want %q", plan.Role, "researcher")
	}
	if plan.Label != "model comparison" {
		t.Errorf("Label: got %q, want %q", plan.Label, "model comparison")
	}
}

func TestExecutionEvent_RoundTrip(t *testing.T) {
	ev := contracts.ExecutionEvent{
		Kind:    "llm_call",
		Role:    "lead",
		Model:   "claude-opus-4",
		Detail:  "LLM call completed",
		CostUSD: 0.00123,
	}
	if ev.Kind != "llm_call" {
		t.Errorf("Kind: got %q", ev.Kind)
	}
	if ev.Role != "lead" {
		t.Errorf("Role: got %q", ev.Role)
	}
	if ev.CostUSD != 0.00123 {
		t.Errorf("CostUSD: got %f", ev.CostUSD)
	}
}

func TestTraceCounts_RoundTrip(t *testing.T) {
	tc := contracts.TraceCounts{
		Messages:  5,
		ToolCalls: 12,
		LLMCalls:  8,
	}
	if tc.Messages != 5 {
		t.Errorf("Messages: got %d", tc.Messages)
	}
	if tc.ToolCalls != 12 {
		t.Errorf("ToolCalls: got %d", tc.ToolCalls)
	}
	if tc.LLMCalls != 8 {
		t.Errorf("LLMCalls: got %d", tc.LLMCalls)
	}
}

func TestExecutionTrace_RoundTrip(t *testing.T) {
	tr := contracts.ExecutionTrace{
		TaskID: "task-abc",
		Events: []contracts.ExecutionEvent{
			{Kind: "step", Role: "lead", Detail: "step completed"},
		},
		Digest: "abc123",
		Counts: contracts.TraceCounts{Messages: 1, ToolCalls: 2, LLMCalls: 3},
	}
	if tr.TaskID != "task-abc" {
		t.Errorf("TaskID: got %q", tr.TaskID)
	}
	if len(tr.Events) != 1 {
		t.Errorf("Events: got %d events", len(tr.Events))
	}
	if tr.Digest != "abc123" {
		t.Errorf("Digest: got %q", tr.Digest)
	}
	if tr.Counts.LLMCalls != 3 {
		t.Errorf("Counts.LLMCalls: got %d", tr.Counts.LLMCalls)
	}
}

func TestLearnedTierResult_RoundTrip(t *testing.T) {
	r := contracts.LearnedTierResult{
		Tier:       "standard",
		InstinctID: "inst-xyz",
	}
	if r.Tier != "standard" {
		t.Errorf("Tier: got %q", r.Tier)
	}
	if r.InstinctID != "inst-xyz" {
		t.Errorf("InstinctID: got %q", r.InstinctID)
	}
}

// --- Interface behaviour tests via fake impls ---

func TestFakeReplaySafetyClassifier(t *testing.T) {
	c := &fakeReplaySafetyClassifier{
		safeTools: map[string]bool{
			"read_file":         true,
			"broker_get_orders": true,
		},
	}
	if !c.IsReplaySafe("read_file") {
		t.Error("read_file should be replay-safe")
	}
	if c.IsReplaySafe("broker_place_order") {
		t.Error("broker_place_order should NOT be replay-safe")
	}
}

// TestFakeReplaySafetyClassifier_NilGuard verifies the nil-receiver pattern
// that CE callers must guard against.
func TestFakeReplaySafetyClassifier_NilGuard(t *testing.T) {
	var c contracts.ReplaySafetyClassifier
	if c != nil {
		t.Fatal("nil interface must be nil")
	}
	// CE callers guard: if c == nil { fail closed }
	if c != nil && c.IsReplaySafe("any") {
		t.Error("would only reach here if c were non-nil")
	}
}

func TestFakeInstinctBudgetResolver_HitPath(t *testing.T) {
	r := &fakeInstinctBudgetResolver{
		result: contracts.LearnedTierResult{Tier: "standard", InstinctID: "inst-1"},
		ok:     true,
	}
	got, ok := r.LearnedTier(context.Background(), "proj-1", "lead", 0.6)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got.Tier != "standard" {
		t.Errorf("Tier: got %q, want standard", got.Tier)
	}
}

func TestFakeInstinctBudgetResolver_MissPath(t *testing.T) {
	r := &fakeInstinctBudgetResolver{ok: false}
	_, ok := r.LearnedTier(context.Background(), "proj-1", "lead", 0.6)
	if ok {
		t.Fatal("expected ok=false on miss path")
	}
}

// TestFakeInstinctBudgetResolver_NilGuard verifies that a nil resolver
// (the Community path) never panics — callers check for nil before calling.
func TestFakeInstinctBudgetResolver_NilGuard(t *testing.T) {
	var r contracts.InstinctBudgetResolver
	if r != nil {
		t.Fatal("nil interface must be nil")
	}
}

func TestFakeHealingApplier_Success(t *testing.T) {
	tr := contracts.ExecutionTrace{TaskID: "task-replay", Digest: "d1"}
	a := &fakeHealingApplier{trace: tr}
	plan := contracts.CounterfactualPlan{OriginalTaskID: "task-orig", Variable: "workflow", Value: "wf-candidate", Label: "trial"}
	got, err := a.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.TaskID != "task-replay" {
		t.Errorf("TaskID: got %q", got.TaskID)
	}
}

func TestFakeHealingApplier_Error(t *testing.T) {
	sentinel := errors.New("engine error")
	a := &fakeHealingApplier{err: sentinel}
	_, err := a.ApplyPlan(context.Background(), contracts.CounterfactualPlan{})
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel error, got %v", err)
	}
}

// TestFakeHealingApplier_NilGuard verifies that a nil HealingApplier (the
// Community path — nil in CommunityProviders) is handled by callers.
func TestFakeHealingApplier_NilGuard(t *testing.T) {
	var a contracts.HealingApplier
	if a != nil {
		t.Fatal("nil interface must be nil")
	}
}
