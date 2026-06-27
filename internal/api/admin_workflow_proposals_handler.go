package api

// Admin workflow-proposal review endpoints — Slice 3a of the
// memetic-workflows arc. Read + decide surface for the architect
// proposals the operator has to approve before any apply path
// runs. Slice 4 will add the apply / Slice 5 the rollback paths;
// here we cap at "see them, approve or reject".
//
// Routes (registered in routes.go):
//   GET  /api/v1/admin/workflow-proposals          — list
//   GET  /api/v1/admin/workflow-proposals/{id}     — show
//   POST /api/v1/admin/workflow-proposals/{id}/decide — approve|reject
//
// Same admin gate matrix as the rest of /admin/* (disabled → 404,
// no key → 401, non-admin → 403, repo missing → 503).
//
// Path routing: net/http's mux is prefix-based, so we register the
// list endpoint at the exact path and the per-id endpoint via a
// trailing-slash router that branches on the suffix.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// workflowProposalJSON is the wire shape. Mirrors
// persistence.WorkflowProposal but with explicit JSON tags +
// RFC3339-formatted timestamps so the CLI's table renderer and
// the UI's React form don't have to round-trip Go time.Time.
type workflowProposalJSON struct {
	ID             string   `json:"id"`
	WorkflowID     string   `json:"workflow_id"`
	Status         string   `json:"status"`
	ProposalYAML   string   `json:"proposal_yaml"`
	Motivation     string   `json:"motivation"`
	EvidenceRunIDs []string `json:"evidence_run_ids"`
	Confidence     float32  `json:"confidence"`
	ArchitectModel string   `json:"architect_model"`
	CreatedAt      string   `json:"created_at"`
	DecidedAt      string   `json:"decided_at,omitempty"`
	DecidedBy      string   `json:"decided_by,omitempty"`
	AppliedAt      string   `json:"applied_at,omitempty"`
	AppliedCommit  string   `json:"applied_commit,omitempty"`
	RollbackCommit string   `json:"rollback_commit,omitempty"`
	Notes          string   `json:"notes,omitempty"`
}

func toWorkflowProposalJSON(p *persistence.WorkflowProposal) workflowProposalJSON {
	out := workflowProposalJSON{
		ID:             p.ID,
		WorkflowID:     p.WorkflowID,
		Status:         string(p.Status),
		ProposalYAML:   p.ProposalYAML,
		Motivation:     p.Motivation,
		EvidenceRunIDs: p.EvidenceRunIDs,
		Confidence:     p.Confidence,
		ArchitectModel: p.ArchitectModel,
		CreatedAt:      p.CreatedAt.UTC().Format(time.RFC3339Nano),
		DecidedBy:      p.DecidedBy,
		AppliedCommit:  p.AppliedCommit,
		RollbackCommit: p.RollbackCommit,
		Notes:          p.Notes,
	}
	if p.DecidedAt != nil {
		out.DecidedAt = p.DecidedAt.UTC().Format(time.RFC3339Nano)
	}
	if p.AppliedAt != nil {
		out.AppliedAt = p.AppliedAt.UTC().Format(time.RFC3339Nano)
	}
	if out.EvidenceRunIDs == nil {
		// Render as [] not null so the CLI iterator doesn't have
		// to special-case nil.
		out.EvidenceRunIDs = []string{}
	}
	return out
}

type workflowProposalListResponse struct {
	Proposals []workflowProposalJSON `json:"proposals"`
}

// adminWorkflowProposalsAuthorised gates every handler in this file.
// Returns true when the caller is admitted; false when it already
// wrote the error response.
//
// D4 (audit 2026-06-10): previously returned the admin key string with
// "" as the failure sentinel, using an inline IsAdminKey check that
// 401'd the trusted local operator on auth-OFF deployments. It now
// routes through requireAdminGate (auth off → admitted; admin key →
// admitted; session-admin → admitted; otherwise 401/403/404). Callers
// that need the operator identity for decided_by/applied_by/reverted_by
// stamping read APIKeyFromContext directly (empty under auth-off, which
// is acceptable for a single-operator deployment with no key identity).
func (s *Server) adminWorkflowProposalsAuthorised(w http.ResponseWriter, r *http.Request) bool {
	if !s.adminConfig.Enabled {
		http.NotFound(w, r)
		return false
	}
	if s.workflowProposals == nil {
		respondError(w, http.StatusServiceUnavailable, "WORKFLOW_PROPOSALS_DISABLED",
			"workflow-proposals not wired on this deployment")
		return false
	}
	return s.requireAdminGate(w, r)
}

// AdminWorkflowProposalsList handles GET
// /api/v1/admin/workflow-proposals. Query params:
//   - status=<csv>: pending,approved,rejected,applied,rolled_back,regressed
//   - workflow=<id>: filter to one workflow
//   - limit=<int>: cap (default 50, max 500)
func (s *Server) AdminWorkflowProposalsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "only GET is supported")
		return
	}
	if !s.adminWorkflowProposalsAuthorised(w, r) {
		return
	}

	q := r.URL.Query()
	filter := persistence.WorkflowProposalFilter{
		WorkflowID: q.Get("workflow"),
		PageSize:   50,
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			filter.PageSize = n
		}
	}
	if raw := q.Get("status"); raw != "" {
		for _, s := range strings.Split(raw, ",") {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			filter.Statuses = append(filter.Statuses, persistence.WorkflowProposalStatus(s))
		}
	}

	got, err := s.workflowProposals.List(r.Context(), filter)
	if err != nil {
		s.logger.Warn().Err(err).Msg("workflow-proposals list failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	out := workflowProposalListResponse{Proposals: make([]workflowProposalJSON, 0, len(got))}
	for _, p := range got {
		out.Proposals = append(out.Proposals, toWorkflowProposalJSON(p))
	}
	respondJSON(w, http.StatusOK, out)
}

// AdminWorkflowProposalsItem routes per-id GET / POST-decide
// against the trailing path segment. Registered at
// /api/v1/admin/workflow-proposals/ (note the slash).
func (s *Server) AdminWorkflowProposalsItem(w http.ResponseWriter, r *http.Request) {
	if !s.adminWorkflowProposalsAuthorised(w, r) {
		return
	}
	// Strip the route prefix to get "{id}" or "{id}/decide".
	const prefix = "/api/v1/admin/workflow-proposals/"
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	if rest == "" {
		http.NotFound(w, r)
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	if id == "" {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 1 {
		// /workflow-proposals/{id}
		if r.Method != http.MethodGet {
			respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "only GET is supported on this path")
			return
		}
		s.adminWorkflowProposalsGet(w, r, id)
		return
	}
	switch parts[1] {
	case "decide":
		if r.Method != http.MethodPost {
			respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "only POST is supported on /decide")
			return
		}
		s.adminWorkflowProposalsDecide(w, r, id)
	case "apply":
		if r.Method != http.MethodPost {
			respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "only POST is supported on /apply")
			return
		}
		s.adminWorkflowProposalsApply(w, r, id)
	case "rollback":
		if r.Method != http.MethodPost {
			respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "only POST is supported on /rollback")
			return
		}
		s.adminWorkflowProposalsRollback(w, r, id)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) adminWorkflowProposalsGet(w http.ResponseWriter, r *http.Request, id string) {
	got, err := s.workflowProposals.Get(r.Context(), id)
	if err != nil {
		s.mapProposalReadError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, toWorkflowProposalJSON(got))
}

// decideBody is the request body for POST .../decide.
// status MUST be approved|rejected — Decide() refuses other
// transitions at the repository layer, but pre-validating here
// gives a clearer error than the generic 422 the repo would
// return.
type decideBody struct {
	Status string `json:"status"`
	Notes  string `json:"notes"`
}

func (s *Server) adminWorkflowProposalsDecide(w http.ResponseWriter, r *http.Request, id string) {
	var body decideBody
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"request body must be JSON: "+err.Error())
		return
	}
	body.Status = strings.TrimSpace(body.Status)
	body.Notes = strings.TrimSpace(body.Notes)
	switch persistence.WorkflowProposalStatus(body.Status) {
	case persistence.WorkflowProposalStatusApproved,
		persistence.WorkflowProposalStatusRejected:
		// ok
	default:
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"status must be 'approved' or 'rejected'")
		return
	}

	// The deciding operator's identity: the admin API key
	// fingerprint OR (when admin-keyed-by-name is wired) the
	// operator name behind the key. For Slice 3 we use the key
	// directly — it's already a stable per-operator identifier
	// in the admin allowlist, and the decision audit lives in
	// admin_audit so we don't lose the speaker.
	decidedBy := APIKeyFromContext(r.Context())

	err := s.workflowProposals.Decide(r.Context(), id,
		persistence.WorkflowProposalStatus(body.Status),
		decidedBy, body.Notes)
	if err != nil {
		s.mapProposalDecideError(w, err)
		return
	}

	// Read-after-write so the CLI / UI gets the updated row
	// without a second round-trip. Best-effort — if the read
	// fails we still consider the decide successful.
	got, err := s.workflowProposals.Get(r.Context(), id)
	if err != nil {
		// The decision landed; the read-back is the only thing that
		// failed. We can't run the rejection write-back without the
		// row, so skip it (it'll re-fire if the operator re-rejects,
		// but rejection is terminal so in practice this is rare).
		respondJSON(w, http.StatusOK, map[string]any{"id": id, "status": body.Status})
		return
	}

	// Consumer B — on a rejection, record the declined proposal back to
	// the instinct layer as an 'architect-reject' contradiction so the
	// architect learns to stop re-proposing it. Gated at wiring time
	// (instinct.enabled && instinct.consumers.architect_priors): when
	// the gate is off the recorder is nil and this is a no-op. Strictly
	// best-effort — the operator's rejection already succeeded, so a
	// write-back failure is logged and swallowed, never surfaced.
	if got.Status == persistence.WorkflowProposalStatusRejected && s.proposalRejectionRecorder != nil {
		if rerr := s.proposalRejectionRecorder.RecordRejection(r.Context(), got); rerr != nil {
			s.logger.Warn().Err(rerr).Str("proposal_id", id).
				Msg("instinct rejection write-back failed (rejection itself succeeded)")
		}
	}

	respondJSON(w, http.StatusOK, toWorkflowProposalJSON(got))
}

// adminWorkflowProposalsApply runs the Slice 4 apply path on the
// proposal. Error mapping:
//   - applier not wired                       → 503
//   - persistence.ErrNotFound                  → 404
//   - memetic.ErrProposalNotApproved           → 409
//   - persistence.ErrInvalidProposalTransition → 409
//   - other                                    → 500
func (s *Server) adminWorkflowProposalsApply(w http.ResponseWriter, r *http.Request, id string) {
	if s.workflowApplier == nil {
		respondError(w, http.StatusServiceUnavailable, "WORKFLOW_APPLIER_DISABLED",
			"workflow apply path not wired on this deployment")
		return
	}
	appliedBy := APIKeyFromContext(r.Context())
	got, err := s.workflowApplier.Apply(r.Context(), id, appliedBy)
	if err != nil {
		s.mapApplyError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, got)
}

// adminWorkflowProposalsRollback is the Slice 5 entry point.
// Wired in Slice 5; for Slice 4 it surfaces 503 so the route
// exists but the action isn't yet available.
func (s *Server) adminWorkflowProposalsRollback(w http.ResponseWriter, r *http.Request, id string) {
	if s.workflowRollbacker == nil {
		respondError(w, http.StatusServiceUnavailable, "WORKFLOW_ROLLBACK_DISABLED",
			"workflow rollback not wired on this deployment")
		return
	}
	revertedBy := APIKeyFromContext(r.Context())
	got, err := s.workflowRollbacker.Rollback(r.Context(), id, revertedBy)
	if err != nil {
		s.mapRollbackError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, got)
}

func (s *Server) mapApplyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, persistence.ErrNotFound):
		respondError(w, http.StatusNotFound, "NOT_FOUND", "proposal not found")
	case errors.Is(err, persistence.ErrInvalidProposalTransition):
		respondError(w, http.StatusConflict, "INVALID_TRANSITION",
			"proposal is no longer in the right state to apply")
	default:
		// memetic.ErrProposalNotApproved is in the memetic
		// package which the api package now imports (for the
		// architect handler) — check by string match here to
		// avoid a circular-import risk. The applier wraps the
		// sentinel in a formatted message including "must be
		// approved", which is the stable surface.
		if strings.Contains(err.Error(), "must be approved") {
			respondError(w, http.StatusConflict, "PROPOSAL_NOT_APPROVED", err.Error())
			return
		}
		s.logger.Warn().Err(err).Msg("workflow-proposals apply failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
	}
}

func (s *Server) mapRollbackError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, persistence.ErrNotFound):
		respondError(w, http.StatusNotFound, "NOT_FOUND", "proposal not found")
	case errors.Is(err, persistence.ErrInvalidProposalTransition):
		respondError(w, http.StatusConflict, "INVALID_TRANSITION",
			"proposal is not in the applied state and cannot be rolled back")
	default:
		// Catch the memetic.ErrProposalNotApplied wrapped-error
		// path via stable substring. "must be applied" comes
		// from the sentinel's message verbatim.
		if strings.Contains(err.Error(), "must be applied") {
			respondError(w, http.StatusConflict, "PROPOSAL_NOT_APPLIED", err.Error())
			return
		}
		s.logger.Warn().Err(err).Msg("workflow-proposals rollback failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
	}
}

func (s *Server) mapProposalReadError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, persistence.ErrNotFound):
		respondError(w, http.StatusNotFound, "NOT_FOUND", "proposal not found")
	default:
		s.logger.Warn().Err(err).Msg("workflow-proposals read failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
	}
}

func (s *Server) mapProposalDecideError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, persistence.ErrNotFound):
		respondError(w, http.StatusNotFound, "NOT_FOUND", "proposal not found")
	case errors.Is(err, persistence.ErrInvalidProposalTransition):
		// 409 Conflict — the row exists but isn't in the right
		// state to be decided. Typical cause: another operator
		// already approved/rejected it.
		respondError(w, http.StatusConflict, "INVALID_TRANSITION",
			"proposal is no longer pending; another operator may have already decided")
	default:
		s.logger.Warn().Err(err).Msg("workflow-proposals decide failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
	}
}
