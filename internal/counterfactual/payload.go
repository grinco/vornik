// Package counterfactual provides the CE-side counterfactual payload decoder.
//
// The payload decoding (ExtractPayload / Payload and its accessor methods)
// is PURE JSON-unmarshal + field access — commodity infrastructure, no
// heuristics, no IP. It is re-homed here from internal/blackbox so CE packages
// (executor, api/mcp_counterfactual_gate) can import it without importing IP.
//
// Design reference: https://docs.vornik.io §2.1
// ("What STAYS CE: Counterfactual payload decoding")
//
// This package MUST NOT import internal/enterprise, internal/blackbox, or
// internal/instinct. It may import only stdlib and other neutral CE packages.
package counterfactual

import (
	"encoding/json"
)

// Payload is the materialized counterfactual block from a task
// payload. The zero value (all fields nil/empty) is the expected shape for
// non-counterfactual tasks, so call sites can read unconditionally and branch
// on whether a specific field is populated.
type Payload struct {
	// IsReplay records whether the task carries a context.counterfactual block
	// at all. True even when the block is empty — used by the MCP handler to
	// gate side-effect enforcement on replay tasks regardless of which variable
	// triggered the counterfactual.
	IsReplay bool
	// RouterLevelModel applies to every chat call when no per-role override
	// exists for the named role. Empty means no router-level override was set.
	RouterLevelModel string
	// ModelByRole gives per-role model overrides. Nil-safe to index (a
	// non-existent role returns the zero string).
	ModelByRole map[string]string
	// PromptByRole gives per-role system prompt overrides.
	PromptByRole map[string]string
	// ToolResultOverride gives operator-supplied stub results for specific tools
	// (Phase C v2 VariableToolResult). The MCP handler returns the override
	// verbatim when an agent calls the named tool, bypassing the real tool. Key
	// is the fully-qualified tool name (mcp__server__tool); value is the
	// response string the agent receives.
	ToolResultOverride map[string]string
	// Budget caps overlaid by VariableBudget. Zero values mean "no override;
	// use native config". Each cap targets a different enforcement point in
	// the executor.
	Budget BudgetOverride
	// ExcludedChunks is the set of memory chunk IDs the operator has asked
	// the recall path to drop (Phase C v2 VariableMemoryChunkExcluded). Stored
	// as a slice for stable iteration; the recall handler filters hits whose
	// ChunkID matches any entry. Empty / nil = no exclusion.
	ExcludedChunks []string
	// OriginalTools is the set of fully-qualified tool names that were called
	// in the ORIGINAL trace, recorded at replay-construction time. The MCP gate
	// uses this to block any tool a counterfactual tries to call that the
	// original never called.
	OriginalTools map[string]struct{}
	// OriginalToolsRecorded is true when the original_tools key was PRESENT in
	// the payload (even as an empty array). This is distinct from set
	// membership: a task that called zero tools records an empty set but is
	// still fully "known" — the gate must not treat it as "unknown provenance"
	// and fail closed on every call.
	OriginalToolsRecorded bool
}

// BudgetOverride is the per-task counterfactual budget override.
// Each field is independently optional; the executor uses each where it's
// enforced today. A zero value across the struct is equivalent to no override.
type BudgetOverride struct {
	// MaxItersPerStep caps the visit count for any one step. Zero = use
	// workflow's MaxStepVisits.
	MaxItersPerStep int
	// StepTimeoutSeconds caps the per-step wallclock. Zero = use the workflow
	// YAML's step.Timeout (or the executor default).
	StepTimeoutSeconds int
	// MaxTokens caps the VORNIK_LLM_MAX_TOKENS environment variable passed
	// into the agent container. Zero = use the role-level model limit / global
	// default.
	MaxTokens int
}

// IsZero reports whether any budget cap was set. Used by the executor to skip
// the override path entirely for tasks without a budget block.
func (b BudgetOverride) IsZero() bool {
	return b.MaxItersPerStep == 0 && b.StepTimeoutSeconds == 0 && b.MaxTokens == 0
}

// OriginalToolsKnown reports whether the original tool set was recorded at
// replay-construction time. True even for a zero-tool original (empty recorded
// set) — recorded-ness is presence of the original_tools key, NOT set size.
// The MCP gate branches on this to distinguish "unknown provenance, fail
// closed" (false) from "known, apply not-in-original rules" (true).
func (o Payload) OriginalToolsKnown() bool {
	return o.OriginalToolsRecorded
}

// WasCalledInOriginal reports whether the named tool was called in the original
// trace. Only meaningful when OriginalToolsKnown() is true; callers must check
// that first. When the set is unknown this returns true (no information to
// block on — the gate's original_tools_unknown branch handles the fail-closed
// posture).
func (o Payload) WasCalledInOriginal(toolName string) bool {
	if !o.OriginalToolsKnown() {
		return true
	}
	_, ok := o.OriginalTools[toolName]
	return ok
}

// HasModelOverride reports whether any model override applies to the named role
// (per-role first, then router-level fallback).
func (o Payload) HasModelOverride(role string) bool {
	return o.ResolveModel(role) != ""
}

// ResolveModel returns the effective model override for the role.
// Precedence: per-role > router-level > "". Callers treat an empty return as
// "no override; use native resolution".
func (o Payload) ResolveModel(role string) string {
	if v, ok := o.ModelByRole[role]; ok && v != "" {
		return v
	}
	return o.RouterLevelModel
}

// ResolvePrompt returns the per-role prompt override. Prompt overrides are
// role-keyed only — there's no "all roles" prompt override because a single
// prompt across all roles makes no sense.
func (o Payload) ResolvePrompt(role string) string {
	return o.PromptByRole[role]
}

// ResolveToolResult returns the operator-supplied stub for a specific tool
// call when one is set. Empty string + false signals the MCP handler to fall
// through to the replay-safety classifier. The two-return form distinguishes
// "explicit empty stub" from "no override".
func (o Payload) ResolveToolResult(toolName string) (string, bool) {
	v, ok := o.ToolResultOverride[toolName]
	return v, ok
}

// IsChunkExcluded reports whether a memory chunk ID is in the exclude list.
// Linear scan — the list is bounded (operator-supplied, typically <10 items)
// so a map allocation per check isn't worth the overhead.
func (o Payload) IsChunkExcluded(chunkID string) bool {
	if chunkID == "" {
		return false
	}
	for _, id := range o.ExcludedChunks {
		if id == chunkID {
			return true
		}
	}
	return false
}

// ExtractPayload decodes context.counterfactual.* from a task
// payload. Returns the zero value on missing block, malformed JSON, or any
// structural mismatch — defensive because the executor calls this on every
// step and an extraction failure must not abort the run.
//
// Expected payload shape (written by mutatePayloadForVariable):
//
//	{
//	  "context": {
//	    "counterfactual": {
//	      "model_override_all_roles": "claude-opus-4",
//	      "role_model_override": {"researcher": "gpt-4o"},
//	      "role_prompt_override": {"lead": "..."}
//	    }
//	  }
//	}
func ExtractPayload(payload json.RawMessage) Payload {
	var out Payload
	if len(payload) == 0 {
		return out
	}
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		return out
	}
	ctxMap, _ := m["context"].(map[string]any)
	if ctxMap == nil {
		return out
	}
	cf, _ := ctxMap["counterfactual"].(map[string]any)
	if cf == nil {
		return out
	}
	// Presence of the counterfactual block flags the task as a replay
	// regardless of which (if any) variable was mutated.
	out.IsReplay = true
	if v, ok := cf["model_override_all_roles"].(string); ok {
		out.RouterLevelModel = v
	}
	if rm, ok := cf["role_model_override"].(map[string]any); ok {
		out.ModelByRole = toStringMap(rm)
	}
	if rp, ok := cf["role_prompt_override"].(map[string]any); ok {
		out.PromptByRole = toStringMap(rp)
	}
	if tr, ok := cf["tool_result_override"].(map[string]any); ok {
		out.ToolResultOverride = toStringMap(tr)
	}
	if b, ok := cf["budget_override"].(map[string]any); ok {
		out.Budget = budgetFromMap(b)
	}
	if ec, ok := cf["excluded_chunks"].([]any); ok {
		out.ExcludedChunks = toStringSlice(ec)
	}
	// Presence of the original_tools key (even an empty array) means the
	// recording lookup succeeded — provenance is KNOWN. Set membership is
	// tracked separately so a zero-tool original is representable as
	// "recorded but empty" rather than collapsing to "unknown".
	if ot, ok := cf["original_tools"].([]any); ok {
		out.OriginalToolsRecorded = true
		out.OriginalTools = toStringSet(ot)
	}
	return out
}

// toStringSet narrows a JSON-decoded []any to a set, dropping non-string /
// empty entries. Returns nil for an empty result. The caller tracks
// recorded-ness separately (OriginalToolsRecorded) so a nil set no longer
// implies "unknown" — an empty-but-recorded set means the original called zero
// tools.
func toStringSet(in []any) map[string]struct{} {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(in))
	for _, v := range in {
		if s, ok := v.(string); ok && s != "" {
			out[s] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// toStringSlice narrows a JSON-decoded []any to []string, dropping non-string
// entries. Empty/zero-length results return nil so IsChunkExcluded fast-paths.
func toStringSlice(in []any) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// budgetFromMap narrows a JSON-decoded map[string]any to BudgetOverride. JSON
// numbers come back as float64; we truncate to int and drop negatives. Missing
// / non-numeric fields stay zero so callers can compare against IsZero cleanly.
func budgetFromMap(m map[string]any) BudgetOverride {
	var b BudgetOverride
	if v, ok := m["max_iters_per_step"].(float64); ok && v > 0 {
		b.MaxItersPerStep = int(v)
	}
	if v, ok := m["step_timeout_seconds"].(float64); ok && v > 0 {
		b.StepTimeoutSeconds = int(v)
	}
	if v, ok := m["max_tokens"].(float64); ok && v > 0 {
		b.MaxTokens = int(v)
	}
	return b
}

// toStringMap narrows a map[string]any to map[string]string, dropping
// non-string entries. JSON-decoded maps come in as any-typed values; the
// override fields are documented as string-keyed strings so non-strings are
// operator error and silently ignored is fine.
func toStringMap(m map[string]any) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if s, ok := v.(string); ok && s != "" {
			out[k] = s
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
