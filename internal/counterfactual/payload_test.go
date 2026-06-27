package counterfactual_test

// Ported from internal/blackbox/counterfactual_overrides_test.go.
// These tests verify the CE-side decoder (ExtractCounterfactualPayload)
// produces the same behaviour as the original blackbox decoder.

import (
	"encoding/json"
	"testing"

	"vornik.io/vornik/internal/counterfactual"
)

// TestPayload_OriginalTools covers the not-in-original gate's read path:
// extraction of the original_tools array + the fail-open semantics when the
// set is unknown.
func TestPayload_OriginalTools(t *testing.T) {
	payload := json.RawMessage(`{"context":{"counterfactual":{"original_tools":["read_file","broker_buy",""]}}}`)
	o := counterfactual.ExtractPayload(payload)
	if !o.IsReplay {
		t.Fatal("expected IsReplay=true")
	}
	if !o.OriginalToolsKnown() {
		t.Fatal("expected OriginalToolsKnown=true")
	}
	if !o.WasCalledInOriginal("read_file") {
		t.Error("read_file should be in the original set")
	}
	if o.WasCalledInOriginal("telegram_send") {
		t.Error("telegram_send should NOT be in the original set")
	}
	// Empty-string entries are dropped.
	if _, ok := o.OriginalTools[""]; ok {
		t.Error("empty tool name should be dropped from the set")
	}
}

func TestPayload_OriginalTools_AbsentIsUnknown(t *testing.T) {
	// Replay block with NO original_tools key → provenance unknown.
	// OriginalToolsKnown() is false; WasCalledInOriginal returns true
	// (no info to block on at this layer). The MCP gate branches on
	// OriginalToolsKnown FIRST and fails CLOSED on the unknown case.
	o := counterfactual.ExtractPayload(json.RawMessage(`{"context":{"counterfactual":{}}}`))
	if o.OriginalToolsKnown() {
		t.Fatal("expected OriginalToolsKnown=false when original_tools absent")
	}
	if o.OriginalToolsRecorded {
		t.Fatal("absent key must leave OriginalToolsRecorded=false")
	}
	if !o.WasCalledInOriginal("any_tool_at_all") {
		t.Error("WasCalledInOriginal returns true when the set is unknown")
	}
}

// TestPayload_OriginalTools_RecordedButEmpty is the FIX 2 representability
// regression: original_tools present but empty means the recording succeeded
// and the original called ZERO tools. That is KNOWN provenance.
func TestPayload_OriginalTools_RecordedButEmpty(t *testing.T) {
	o := counterfactual.ExtractPayload(json.RawMessage(`{"context":{"counterfactual":{"original_tools":[]}}}`))
	if !o.OriginalToolsRecorded {
		t.Fatal("empty array still PRESENT → OriginalToolsRecorded must be true")
	}
	if !o.OriginalToolsKnown() {
		t.Fatal("recorded-but-empty must read as KNOWN, not unknown")
	}
	if len(o.OriginalTools) != 0 {
		t.Errorf("recorded set should be empty; got %v", o.OriginalTools)
	}
	if o.WasCalledInOriginal("anything") {
		t.Error("nothing was called in a zero-tool original")
	}
}

// TestExtractCounterfactualPayload_ZeroOnEmpty — nil/empty payload returns
// the zero value, NOT an error.
func TestExtractCounterfactualPayload_ZeroOnEmpty(t *testing.T) {
	cases := []struct {
		name string
		raw  json.RawMessage
	}{
		{"nil bytes", nil},
		{"empty bytes", json.RawMessage{}},
		{"empty object", json.RawMessage(`{}`)},
		{"no context", json.RawMessage(`{"foo":"bar"}`)},
		{"context but no counterfactual", json.RawMessage(`{"context":{"prompt":"hi"}}`)},
		{"context not a map", json.RawMessage(`{"context":"hi"}`)},
		{"malformed", json.RawMessage(`{not json`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := counterfactual.ExtractPayload(c.raw)
			if got.IsReplay || got.RouterLevelModel != "" ||
				len(got.ModelByRole) != 0 || len(got.PromptByRole) != 0 ||
				len(got.ToolResultOverride) != 0 {
				t.Errorf("expected zero value, got %+v", got)
			}
		})
	}
}

// TestExtractCounterfactualPayload_EmptyBlockMarksReplay — a payload with
// context.counterfactual = {} is still a replay task (the block exists).
func TestExtractCounterfactualPayload_EmptyBlockMarksReplay(t *testing.T) {
	raw := json.RawMessage(`{"context":{"counterfactual":{}}}`)
	o := counterfactual.ExtractPayload(raw)
	if !o.IsReplay {
		t.Error("empty counterfactual block should still flag IsReplay=true")
	}
}

// TestPayload_ResolveToolResult — operator-supplied stubs distinguish
// "explicit empty stub" (ok=true, v="") from "no override" (ok=false).
func TestPayload_ResolveToolResult(t *testing.T) {
	o := counterfactual.Payload{
		ToolResultOverride: map[string]string{
			"mcp__broker__place_order": `{"status":"stubbed"}`,
		},
	}
	if v, ok := o.ResolveToolResult("mcp__broker__place_order"); !ok || v == "" {
		t.Errorf("matched tool should return (stub, true); got (%q, %v)", v, ok)
	}
	if _, ok := o.ResolveToolResult("mcp__broker__cancel"); ok {
		t.Error("unmatched tool should return ok=false")
	}
	empty := counterfactual.Payload{}
	if _, ok := empty.ResolveToolResult("any"); ok {
		t.Error("empty overrides should return ok=false")
	}
}

// TestExtractCounterfactualPayload_AllFields — the full shape round-trips
// correctly.
func TestExtractCounterfactualPayload_AllFields(t *testing.T) {
	raw := json.RawMessage(`{
		"context": {
			"counterfactual": {
				"model_override_all_roles": "claude-opus-4",
				"role_model_override": {"researcher": "gpt-4o", "lead": "claude-sonnet-4-6"},
				"role_prompt_override": {"researcher": "Override prompt for researcher."},
				"tool_result_override": {"mcp__broker__place_order": "{\"status\":\"stubbed\"}"}
			}
		}
	}`)
	o := counterfactual.ExtractPayload(raw)
	if !o.IsReplay {
		t.Error("populated counterfactual block should flag IsReplay=true")
	}
	if o.RouterLevelModel != "claude-opus-4" {
		t.Errorf("router-level: %q", o.RouterLevelModel)
	}
	if got := o.ModelByRole["researcher"]; got != "gpt-4o" {
		t.Errorf("role model researcher: %q", got)
	}
	if got := o.ModelByRole["lead"]; got != "claude-sonnet-4-6" {
		t.Errorf("role model lead: %q", got)
	}
	if got := o.PromptByRole["researcher"]; got != "Override prompt for researcher." {
		t.Errorf("role prompt researcher: %q", got)
	}
	if got := o.ToolResultOverride["mcp__broker__place_order"]; got != `{"status":"stubbed"}` {
		t.Errorf("tool result override: %q", got)
	}
}

// TestPayload_ResolveModel_Precedence — per-role wins over router-level;
// missing role falls through to router-level; neither set returns empty.
func TestPayload_ResolveModel_Precedence(t *testing.T) {
	o := counterfactual.Payload{
		RouterLevelModel: "claude-opus-4",
		ModelByRole:      map[string]string{"researcher": "gpt-4o"},
	}
	if got := o.ResolveModel("researcher"); got != "gpt-4o" {
		t.Errorf("per-role should win: %q", got)
	}
	if got := o.ResolveModel("lead"); got != "claude-opus-4" {
		t.Errorf("unmatched role should fall back to router-level: %q", got)
	}
	empty := counterfactual.Payload{}
	if got := empty.ResolveModel("any"); got != "" {
		t.Errorf("empty overrides should return empty model: %q", got)
	}
}

// TestPayload_HasModelOverride mirrors the executor's "should I override?"
// branch.
func TestPayload_HasModelOverride(t *testing.T) {
	o := counterfactual.Payload{RouterLevelModel: "claude-opus-4"}
	if !o.HasModelOverride("any") {
		t.Error("router-level should report HasModelOverride=true for any role")
	}
	empty := counterfactual.Payload{}
	if empty.HasModelOverride("any") {
		t.Error("empty overrides should report HasModelOverride=false")
	}
}

// TestPayload_ResolvePrompt — role-keyed only; missing role returns empty.
func TestPayload_ResolvePrompt(t *testing.T) {
	o := counterfactual.Payload{
		PromptByRole: map[string]string{"lead": "Override prompt."},
	}
	if got := o.ResolvePrompt("lead"); got != "Override prompt." {
		t.Errorf("matched role: %q", got)
	}
	if got := o.ResolvePrompt("other"); got != "" {
		t.Errorf("unmatched role should be empty: %q", got)
	}
}

// TestExtractCounterfactualPayload_IgnoresNonStringValues — the writer always
// emits strings; defensively drop non-string entries rather than blowing up.
func TestExtractCounterfactualPayload_IgnoresNonStringValues(t *testing.T) {
	raw := json.RawMessage(`{
		"context": {
			"counterfactual": {
				"role_model_override": {"researcher": "gpt-4o", "lead": 42, "skip": null}
			}
		}
	}`)
	o := counterfactual.ExtractPayload(raw)
	if o.ModelByRole["researcher"] != "gpt-4o" {
		t.Errorf("string entry lost: %q", o.ModelByRole["researcher"])
	}
	if _, ok := o.ModelByRole["lead"]; ok {
		t.Errorf("non-string entry should be dropped, got %+v", o.ModelByRole)
	}
	if _, ok := o.ModelByRole["skip"]; ok {
		t.Errorf("null entry should be dropped, got %+v", o.ModelByRole)
	}
}

// TestPayload_BudgetOverride — BudgetOverride fields decode correctly.
func TestPayload_BudgetOverride(t *testing.T) {
	raw := json.RawMessage(`{
		"context": {
			"counterfactual": {
				"budget_override": {
					"max_iters_per_step": 5,
					"step_timeout_seconds": 120,
					"max_tokens": 4096
				}
			}
		}
	}`)
	o := counterfactual.ExtractPayload(raw)
	if o.Budget.IsZero() {
		t.Error("budget override should not be zero")
	}
	if o.Budget.MaxItersPerStep != 5 {
		t.Errorf("MaxItersPerStep: got %d, want 5", o.Budget.MaxItersPerStep)
	}
	if o.Budget.StepTimeoutSeconds != 120 {
		t.Errorf("StepTimeoutSeconds: got %d, want 120", o.Budget.StepTimeoutSeconds)
	}
	if o.Budget.MaxTokens != 4096 {
		t.Errorf("MaxTokens: got %d, want 4096", o.Budget.MaxTokens)
	}
}

func TestBudgetOverride_IsZero(t *testing.T) {
	zero := counterfactual.BudgetOverride{}
	if !zero.IsZero() {
		t.Error("zero BudgetOverride must report IsZero=true")
	}
	nonZero := counterfactual.BudgetOverride{MaxItersPerStep: 1}
	if nonZero.IsZero() {
		t.Error("non-zero BudgetOverride must report IsZero=false")
	}
}

// TestPayload_ExcludedChunks — excluded_chunks slice decodes correctly.
func TestPayload_ExcludedChunks(t *testing.T) {
	raw := json.RawMessage(`{
		"context": {
			"counterfactual": {
				"excluded_chunks": ["chunk-1", "chunk-2", ""]
			}
		}
	}`)
	o := counterfactual.ExtractPayload(raw)
	if len(o.ExcludedChunks) != 2 {
		t.Errorf("ExcludedChunks: got %v", o.ExcludedChunks)
	}
	if !o.IsChunkExcluded("chunk-1") {
		t.Error("chunk-1 should be excluded")
	}
	if o.IsChunkExcluded("chunk-99") {
		t.Error("chunk-99 should not be excluded")
	}
	if o.IsChunkExcluded("") {
		t.Error("empty chunk ID should not be excluded")
	}
}
