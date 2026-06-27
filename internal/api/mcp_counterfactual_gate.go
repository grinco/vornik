package api

// Counterfactual-aware MCP gate (Phase C v2 — deny-by-default,
// 2026-06-17). Intercepts tool calls when the caller's task is a
// counterfactual replay, in this precedence:
//
//   1. operator-supplied tool_result stub wins — the agent gets
//      the exact string the operator provided
//   2. unknown provenance (original tool set never recorded) →
//      fail closed, stub with original_tools_unknown
//   3. tool not in the original trace → stub with not_in_original
//      (we can't fabricate a recorded output)
//   4. tool NOT on the replay-safe allow-list → stub with
//      not_replay_safe so the new run can't fire broker orders /
//      send messages / write files
//   5. otherwise (replay-safe AND in-original, no override) — fall
//      through and execute normally
//
// The replay-safe gate (4) is deny-by-default: only tools the
// operator has marked read-only/idempotent run live; everything else
// is stubbed. This replaced the old side-effect deny-list, which
// failed OPEN when a new MCP server shipped an unlisted side-effecting
// tool. Tasks without a context.counterfactual block pass through
// unchanged.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"vornik.io/vornik/internal/counterfactual"
	"vornik.io/vornik/internal/persistence"
)

// counterfactualGateResult signals what the gate decided.
type counterfactualGateResult struct {
	// HandledLocally is true when the gate produced a response
	// without calling the real MCP server. Caller must skip the
	// downstream Execute() call.
	HandledLocally bool
	// Text is the response body to return to the agent when
	// HandledLocally is true. Always a JSON-string the bridge
	// will surface as the tool result.
	Text string
}

// cfGateEntry is the immutable snapshot of a task's counterfactual
// payload overrides cached by the gate after the first successful
// taskRepo.Get. A task payload's replay-ness, recorded original-tool
// set, and operator stubs do not change after creation, so a single
// snapshot serves the task for the rest of the process's life. See
// Server.cfGateCache for why it survives DB blips.
type cfGateEntry struct {
	overrides counterfactual.Payload
}

// applyCounterfactualMCPGate consults the task's payload (when
// X-Task-ID was supplied) and short-circuits the call if:
//   - the task is a counterfactual replay AND
//   - the operator supplied a stub for this tool, OR
//   - the tool was not in the original trace (or provenance is
//     unknown — fail closed), OR
//   - the tool is NOT on the replay-safe allow-list (deny-by-default).
//
// Blast-radius containment (FIX 1): the only failure that 503s the
// caller is "no cached snapshot AND the live Get failed". A task we
// have resolved once is served from cache on any later Get error, so
// a long-running agent's calls survive a DB blip after its first
// successful call — and a non-replay task never blocks real dispatch
// on a transient DB error.
//
// Returns the decision + any sentinel errors that should bubble
// up unchanged. nil error + HandledLocally=false signals the
// caller to proceed with the real Execute() call.
func (s *Server) applyCounterfactualMCPGate(ctx context.Context, taskID, toolName string) (counterfactualGateResult, error) {
	if taskID == "" || s.taskRepo == nil {
		return counterfactualGateResult{}, nil
	}
	task, err := s.taskRepo.Get(ctx, taskID)
	if err != nil {
		// Get failed (transient DB error, or the row was archived/
		// purged out from under a still-running agent). Fall back to
		// the cached snapshot if we ever resolved this task before —
		// replay-ness is immutable, so a stale snapshot is still
		// authoritative. No snapshot => we cannot prove the call is
		// safe => fail closed (the caller 503s).
		if cached, ok := s.loadCFGateEntry(taskID); ok {
			s.logger.Warn().Err(err).Str("task_id", taskID).
				Msg("counterfactual gate: task lookup failed; serving cached provenance")
			return s.evaluateCounterfactualGate(cached.overrides, toolName)
		}
		if errors.Is(err, persistence.ErrNotFound) {
			return counterfactualGateResult{}, fmt.Errorf("counterfactual gate: task %s not found", taskID)
		}
		return counterfactualGateResult{}, fmt.Errorf("counterfactual gate: task lookup: %w", err)
	}
	if task == nil {
		return counterfactualGateResult{}, nil
	}
	overrides := counterfactual.ExtractPayload(task.Payload)
	// Cache the immutable snapshot so future Get errors for this task
	// can still be evaluated rather than blocking all dispatch.
	s.storeCFGateEntry(taskID, cfGateEntry{overrides: overrides})
	return s.evaluateCounterfactualGate(overrides, toolName)
}

// loadCFGateEntry returns the cached snapshot for a task, if any.
func (s *Server) loadCFGateEntry(taskID string) (cfGateEntry, bool) {
	v, ok := s.cfGateCache.Load(taskID)
	if !ok {
		return cfGateEntry{}, false
	}
	e, ok := v.(cfGateEntry)
	return e, ok
}

// storeCFGateEntry records the immutable snapshot for a task.
func (s *Server) storeCFGateEntry(taskID string, e cfGateEntry) {
	s.cfGateCache.Store(taskID, e)
}

// evaluateCounterfactualGate applies the replay-safety rules to an
// already-resolved set of payload overrides. Pure decision logic,
// shared by the live-Get path and the cached-fallback path so both
// enforce identically.
func (s *Server) evaluateCounterfactualGate(overrides counterfactual.Payload, toolName string) (counterfactualGateResult, error) {
	if !overrides.IsReplay {
		return counterfactualGateResult{}, nil
	}
	// Operator-supplied stub wins, even over the side-effect
	// classifier — the operator chose this stub on purpose.
	if stub, ok := overrides.ResolveToolResult(toolName); ok {
		return counterfactualGateResult{HandledLocally: true, Text: stub}, nil
	}
	// When no replay-safety classifier is wired (CE / Community edition
	// without the EE blackbox module), treat all tools as allowed —
	// replay-safety enforcement is an EE capability. Nil classifier =
	// enforcement OFF; every tool passes through.
	if s.blackboxReplaySafety == nil {
		return counterfactualGateResult{}, nil
	}
	// Fail-CLOSED contract (reversed from the original fail-open
	// design in commit a799e3f2): when the original tool set was
	// never recorded (provenance unknown) we cannot prove a newly
	// named tool is harmless — a candidate workflow could introduce
	// a side-effecting tool — so block it with the
	// original_tools_unknown response. The operator-stub branch above
	// still lets an operator deliberately allow a tool.
	//
	// Note (FIX 2): a zero-tool original is "recorded but empty", so
	// OriginalToolsKnown() is true for it and we fall through to the
	// not-in-original branch (which blocks every unstubbed tool as
	// not_in_original) rather than the unknown branch — the empty
	// recording is real information, not missing provenance.
	if !overrides.OriginalToolsKnown() {
		return counterfactualGateResult{
			HandledLocally: true,
			Text:           synthesizeOriginalToolsUnknownResponse(toolName),
		}, nil
	}
	// Not-in-original block: a counterfactual is a re-run of the
	// original with ONE variable changed; it must not invoke a tool
	// the original never called (the LLD's "we can't fabricate a
	// recorded output" rule, § "Counterfactual Replay Engine"). The
	// provenance-known check above already returned for the unknown
	// case, so WasCalledInOriginal is the only remaining condition.
	if !overrides.WasCalledInOriginal(toolName) {
		return counterfactualGateResult{
			HandledLocally: true,
			Text:           synthesizeNotInOriginalResponse(toolName),
		}, nil
	}
	// Deny-by-default replay-safety gate: even a tool the original
	// called is short-circuited unless it is on the replay-safe
	// allow-list (read-only / idempotent). This is the Phase C
	// inversion of the old side-effect deny-list — an unrecognised
	// tool is stubbed, never fired, so a new MCP server's
	// side-effecting tool can't slip through for lack of a deny-list
	// entry. Operators opt a tool into live replay via
	// blackbox.replay_safe_tools.
	if !s.blackboxReplaySafety.IsReplaySafe(toolName) {
		return counterfactualGateResult{
			HandledLocally: true,
			Text:           synthesizeNotReplaySafeResponse(toolName),
		}, nil
	}
	return counterfactualGateResult{}, nil
}

func synthesizeOriginalToolsUnknownResponse(toolName string) string {
	body, err := json.Marshal(map[string]any{
		"counterfactual_replay": true,
		"skipped":               "original_tools_unknown",
		"tool":                  toolName,
		"note":                  "This tool was not invoked because the original replay tool set is unavailable.",
	})
	if err != nil {
		return `{"counterfactual_replay":true,"skipped":"original_tools_unknown"}`
	}
	return string(body)
}

// synthesizeNotInOriginalResponse returns the body the agent
// receives when the gate blocks a tool the original trace never
// called. JSON-string so the bridge surfaces it as a tool result;
// structured fields signal "no recording exists" without lying
// about success.
func synthesizeNotInOriginalResponse(toolName string) string {
	body, err := json.Marshal(map[string]any{
		"counterfactual_replay": true,
		"skipped":               "not_in_original",
		"tool":                  toolName,
		"note": "This tool was not invoked because the task is a " +
			"counterfactual replay and the tool was not called in the " +
			"original trace, so no recorded output exists to reproduce. " +
			"Treat as 'tool unavailable in this counterfactual'.",
	})
	if err != nil {
		return `{"counterfactual_replay":true,"skipped":"not_in_original"}`
	}
	return string(body)
}

// synthesizeNotReplaySafeResponse returns the body the agent receives
// when the deny-by-default gate suppresses a tool that is not on the
// replay-safe allow-list. JSON-string so the bridge surfaces it as a
// tool result the agent can parse; structured fields signal "no work
// happened" without lying about success.
func synthesizeNotReplaySafeResponse(toolName string) string {
	body, err := json.Marshal(map[string]any{
		"counterfactual_replay": true,
		"skipped":               "not_replay_safe",
		"tool":                  toolName,
		"note": "This tool was not invoked because the task is a " +
			"counterfactual replay and the tool is not on the replay-safe " +
			"allow-list (only read-only/idempotent tools run live in a " +
			"replay). Treat as 'tool succeeded but produced no real-world effect'.",
	})
	if err != nil {
		// JSON encoding of fixed-shape map is impossible to fail
		// in practice; fall back to a static string.
		return `{"counterfactual_replay":true,"skipped":"not_replay_safe"}`
	}
	return string(body)
}
