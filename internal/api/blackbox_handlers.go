package api

// Autonomy Black Box admin endpoints — Phase A + C surface.
// See https://docs.vornik.io
//
//   GET  /api/v1/admin/blackbox/traces/{task_id}
//   POST /api/v1/admin/blackbox/replay              — Phase C
//   GET  /api/v1/admin/blackbox/scorecard/{a}/{b}   — Phase C
//   GET  /api/v1/admin/blackbox/sideeffects         — Phase C
//
// Same admin gate matrix as /admin/audit (admin.enabled +
// admin-key allowlist).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/contracts"
)

// WithBlackBoxService wires the trace-assembly service behind
// the admin endpoints. Nil keeps the endpoints at 503 with a
// "not configured" message — single-process / non-Postgres
// deployments can leave it unwired.
func WithBlackBoxService(svc BlackBoxTraceService) ServerOption {
	return func(s *Server) {
		s.blackboxService = svc
	}
}

// WithBlackBoxEngine wires the Phase C counterfactual replay
// engine. Nil → /replay returns 503. Caller is responsible for
// constructing the engine from the production
// CounterfactualRepo + taskcreate.Creator.
func WithBlackBoxEngine(eng BlackBoxReplayEngine) ServerOption {
	return func(s *Server) {
		s.blackboxEngine = eng
	}
}

// WithBlackBoxReplaySafety supplies the replay-safe allow-list
// classifier that backs /sideeffects + the counterfactual MCP gate's
// deny-by-default decision. Nil → introspection endpoint returns 503;
// gate treats nil as CE/Community edition — all tools allowed in replay.
func WithBlackBoxReplaySafety(c contracts.ReplaySafetyClassifier) ServerOption {
	return func(s *Server) {
		s.blackboxReplaySafety = c
	}
}

// AdminBlackBoxTraces handles GET /api/v1/admin/blackbox/traces/{task_id}.
//
// 503 BLACKBOX_DISABLED when the service isn't wired.
// 400 BAD_REQUEST when the path is malformed.
// 404 TASK_NOT_FOUND when the task isn't in the audit tables.
// 200 + trace JSON on success.
func (s *Server) AdminBlackBoxTraces(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminGate(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	if s.blackboxService == nil {
		respondError(w, http.StatusServiceUnavailable, "BLACKBOX_DISABLED",
			"blackbox trace service not wired on this deployment")
		return
	}
	const prefix = "/api/v1/admin/blackbox/traces/"
	taskID := strings.TrimPrefix(r.URL.Path, prefix)
	taskID = strings.TrimSpace(taskID)
	if taskID == "" || strings.Contains(taskID, "/") {
		respondError(w, http.StatusBadRequest, "BAD_REQUEST",
			"task_id required: GET /api/v1/admin/blackbox/traces/{task_id}")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	trace, _, err := s.blackboxService.AssembleCached(ctx, taskID)
	if err != nil {
		if errors.Is(err, contracts.ErrBlackBoxTaskNotFound) {
			respondError(w, http.StatusNotFound, "TASK_NOT_FOUND",
				"no audit data for task "+taskID)
			return
		}
		respondError(w, http.StatusInternalServerError, "INTERNAL", "trace assembly failed: "+err.Error())
		return
	}
	respondJSON(w, http.StatusOK, trace)
}

// replayRequest is the JSON body shape for POST .../replay.
// Mirrors blackbox.Plan but kept separate so the wire shape can
// evolve without changing the package-internal type.
type replayRequest struct {
	OriginalTaskID string `json:"original_task_id"`
	Variable       string `json:"variable"`
	Value          string `json:"value"`
	Role           string `json:"role,omitempty"`
	Label          string `json:"label"`
}

// replayResponse is the JSON body shape for the 201 success path.
// task_id + execution_id are the handles operators poll on; the
// status surface (existing GET /api/v1/tasks/{id}) covers the rest.
type replayResponse struct {
	TaskID                 string `json:"task_id"`
	OriginalTaskID         string `json:"original_task_id"`
	Variable               string `json:"variable"`
	Label                  string `json:"label"`
	StampWarning           string `json:"stamp_warning,omitempty"`
	SideEffectingToolsHint string `json:"side_effecting_tools_hint,omitempty"`
}

// AdminBlackBoxReplay handles POST /api/v1/admin/blackbox/replay.
//
// 503 BLACKBOX_DISABLED — engine not wired (e.g. SQLite deployment).
// 400 BAD_REQUEST       — malformed JSON or plan validation failure.
// 404 TASK_NOT_FOUND    — original task missing.
// 501 NOT_IMPLEMENTED   — variable in design but not v1 (budget /
//
//	policy / tool_result / memory_chunk_excluded).
//
// 201 + replayResponse  — counterfactual task created.
func (s *Server) AdminBlackBoxReplay(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminGate(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST only")
		return
	}
	if s.blackboxEngine == nil {
		respondError(w, http.StatusServiceUnavailable, "BLACKBOX_DISABLED",
			"counterfactual replay engine not wired on this deployment")
		return
	}

	body, err := readLimitedBody(w, r, 1<<16) // 64 KiB cap is generous for a Plan
	if err != nil {
		return
	}
	var req replayRequest
	if err := json.Unmarshal(body, &req); err != nil {
		respondError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid JSON: "+err.Error())
		return
	}
	plan := BlackBoxReplayPlan{
		OriginalTaskID: strings.TrimSpace(req.OriginalTaskID),
		Variable:       strings.TrimSpace(req.Variable),
		Value:          req.Value,
		Role:           strings.TrimSpace(req.Role),
		Label:          strings.TrimSpace(req.Label),
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	task, err := s.blackboxEngine.Apply(ctx, plan)
	stampWarn := ""
	if err != nil {
		// Distinguish "not yet implemented" from validation +
		// missing-original + everything else.
		if errors.Is(err, contracts.ErrBlackBoxVariableNotImplemented) {
			respondError(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", err.Error())
			return
		}
		if errors.Is(err, contracts.ErrBlackBoxMissingOriginal) {
			respondError(w, http.StatusNotFound, "TASK_NOT_FOUND", err.Error())
			return
		}
		// Stamp-after-success — the engine returns BOTH the task
		// and the error so we surface the warning without
		// pretending the replay failed.
		if task != nil && strings.Contains(err.Error(), "stamp failed") {
			stampWarn = err.Error()
		} else {
			respondError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
			return
		}
	}

	resp := replayResponse{
		TaskID:         task.ID,
		OriginalTaskID: plan.OriginalTaskID,
		Variable:       plan.Variable,
		Label:          plan.Label,
		StampWarning:   stampWarn,
		SideEffectingToolsHint: "DENY-BY-DEFAULT: only replay-safe (allow-listed) tools the original " +
			"trace called run live during replay; every other tool is STUBBED (synthesized " +
			"'skipped:not_replay_safe' response) so a counterfactual cannot fire broker orders, send " +
			"messages, or write files. GET /api/v1/admin/blackbox/sideeffects for the replay-safe allow-list.",
	}
	respondJSON(w, http.StatusCreated, resp)
}

// AdminBlackBoxScorecard handles
// GET /api/v1/admin/blackbox/scorecard/{a}/{b}.
//
// Loads two assembled traces (via the cached assembler) and
// returns the Scorecard. The "warmth" of the cache governs
// latency — first call is ~10ms per trace, repeats are sub-ms.
//
// 400 BAD_REQUEST   — path missing or malformed.
// 404 TASK_NOT_FOUND — either task missing.
// 200 + scorecard JSON on success.
func (s *Server) AdminBlackBoxScorecard(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminGate(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	if s.blackboxService == nil {
		respondError(w, http.StatusServiceUnavailable, "BLACKBOX_DISABLED",
			"blackbox trace service not wired on this deployment")
		return
	}
	const prefix = "/api/v1/admin/blackbox/scorecard/"
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	parts := strings.SplitN(strings.TrimSpace(rest), "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		respondError(w, http.StatusBadRequest, "BAD_REQUEST",
			"two task_ids required: GET /api/v1/admin/blackbox/scorecard/{a}/{b}")
		return
	}
	aID, bID := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	traceA, _, err := s.blackboxService.AssembleCached(ctx, aID)
	if err != nil {
		if errors.Is(err, contracts.ErrBlackBoxTaskNotFound) {
			respondError(w, http.StatusNotFound, "TASK_NOT_FOUND", "trace1: "+aID)
			return
		}
		respondError(w, http.StatusInternalServerError, "INTERNAL", "assemble trace1: "+err.Error())
		return
	}
	traceB, _, err := s.blackboxService.AssembleCached(ctx, bID)
	if err != nil {
		if errors.Is(err, contracts.ErrBlackBoxTaskNotFound) {
			respondError(w, http.StatusNotFound, "TASK_NOT_FOUND", "trace2: "+bID)
			return
		}
		respondError(w, http.StatusInternalServerError, "INTERNAL", "assemble trace2: "+err.Error())
		return
	}
	sc, _ := s.blackboxService.Compare(traceA, traceB)
	respondJSON(w, http.StatusOK, sc)
}

// AdminBlackBoxSideEffects handles GET /api/v1/admin/blackbox/sideeffects.
// Returns the active replay-safe allow-list as JSON for operator
// inspection.
//
// Enforcement is ENFORCED at the MCP gate
// (internal/api/mcp_counterfactual_gate.go) under a DENY-BY-DEFAULT
// policy (Phase C inversion, 2026-06-17): a counterfactual replay
// short-circuits every tool NOT on this allow-list with a synthesized
// "skipped" (not_replay_safe) envelope. The list is now tunable via
// the `blackbox.replay_safe_tools` CONFIG key (seeded from the curated
// default at boot, swapped in-process via ReplaySafetyClassifier.Replace
// on `vornikctl daemon reload`).
func (s *Server) AdminBlackBoxSideEffects(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminGate(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	if s.blackboxReplaySafety == nil {
		respondError(w, http.StatusServiceUnavailable, "BLACKBOX_DISABLED",
			"replay-safety classifier not wired on this deployment")
		return
	}
	// snapshotable is a local interface for the optional Snapshot() method
	// present on the concrete EE classifier but not on the CE seam interface.
	type snapshotable interface {
		Snapshot() []string
	}
	var tools []string
	if snap, ok := s.blackboxReplaySafety.(snapshotable); ok {
		tools = snap.Snapshot()
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"replay_safe_tools": tools,
		"enforcement":       "enforced",
		"policy":            "deny_by_default",
		"config_tunable":    true,
		"note": "DENY-BY-DEFAULT: only tools on this replay-safe allow-list run live " +
			"during a counterfactual replay; every other tool is short-circuited with a " +
			"synthesized 'skipped' (not_replay_safe) response. Tune the list via the " +
			"blackbox.replay_safe_tools config key + `vornikctl daemon reload`.",
	})
}
