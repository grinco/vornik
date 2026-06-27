package api

// Workflow-healing trigger admin endpoints — Black Box Phase B.
//
//   GET  /api/v1/admin/workflow-healing/triggers
//   POST /api/v1/admin/workflow-healing/triggers/bulk-dismiss
//   POST /api/v1/admin/workflow-healing/triggers/{id}/dismiss
//   POST /api/v1/admin/workflow-healing/triggers/{id}/generate-candidate
//
// generate-candidate runs the architect via the existing
// /workflow-architect/propose machinery, then stamps the
// resulting proposal_id on the trigger. Mirrors the UI handler's
// semantics so scripted automation gets parity with the admin UI.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/workflowhealing"
)

// HealingTriggerJSON is the wire shape. Times as RFC3339 strings
// so the CLI/UI can render without time-format gymnastics.
type HealingTriggerJSON struct {
	ID                   string   `json:"id"`
	ProjectID            string   `json:"project_id"`
	WorkflowID           string   `json:"workflow_id"`
	TriggerClass         string   `json:"trigger_class"`
	MetricName           string   `json:"metric_name"`
	BaselineStart        string   `json:"baseline_start"`
	BaselineEnd          string   `json:"baseline_end"`
	ComparisonStart      string   `json:"comparison_start"`
	ComparisonEnd        string   `json:"comparison_end"`
	BaselineValue        float64  `json:"baseline_value"`
	ComparisonValue      float64  `json:"comparison_value"`
	ThresholdValue       float64  `json:"threshold_value"`
	EvidenceExecutionIDs []string `json:"evidence_execution_ids"`
	Status               string   `json:"status"`
	CreatedAt            string   `json:"created_at"`
	ResolvedAt           string   `json:"resolved_at,omitempty"`
	ProposalID           string   `json:"proposal_id,omitempty"`
}

// HealingTriggerListResponse wraps the list response.
type HealingTriggerListResponse struct {
	Entries []HealingTriggerJSON `json:"entries"`
}

// WithHealingTriggerRepository wires the trigger ledger behind
// the admin endpoints. Nil keeps the endpoints at 503.
func WithHealingTriggerRepository(repo persistence.WorkflowHealingTriggerRepository) ServerOption {
	return func(s *Server) {
		s.healingTriggerRepo = repo
	}
}

// WithHealingCandidateRepository wires the Self-Healing Workflow
// Genome v1 candidate ledger (migration 87). When set, the
// generate-candidate endpoint persists a candidate row linked to the
// architect's WorkflowProposal in addition to stamping the trigger.
// Nil keeps the pre-genome behaviour (trigger stamp only).
func WithHealingCandidateRepository(repo persistence.WorkflowHealingCandidateRepository) ServerOption {
	return func(s *Server) {
		s.healingCandidateRepo = repo
	}
}

// AdminHealingTriggersList handles GET /api/v1/admin/workflow-healing/triggers.
// Query params: status, project, workflow, class, limit.
func (s *Server) AdminHealingTriggersList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminGate(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	if s.healingTriggerRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "BLACKBOX_DISABLED",
			"workflow-healing trigger repository not wired on this deployment")
		return
	}
	q := r.URL.Query()
	filter := persistence.HealingTriggerListFilter{
		ProjectID:    strings.TrimSpace(q.Get("project")),
		WorkflowID:   strings.TrimSpace(q.Get("workflow")),
		Status:       persistence.HealingTriggerStatus(q.Get("status")),
		TriggerClass: persistence.HealingTriggerClass(q.Get("class")),
	}
	if v := q.Get("limit"); v != "" {
		if n, err := parseLimit(v, 1, 500); err == nil {
			filter.PageSize = n
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := s.healingTriggerRepo.List(ctx, filter)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL", "healing-triggers list failed: "+err.Error())
		return
	}
	out := HealingTriggerListResponse{Entries: make([]HealingTriggerJSON, 0, len(rows))}
	for _, t := range rows {
		out.Entries = append(out.Entries, healingTriggerToJSON(t))
	}
	respondJSON(w, http.StatusOK, out)
}

// AdminHealingTriggerDismiss handles POST /api/v1/admin/workflow-healing/triggers/{id}/dismiss.
func (s *Server) AdminHealingTriggerDismiss(w http.ResponseWriter, r *http.Request, id string) {
	if !s.requireAdminGate(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST only")
		return
	}
	if s.healingTriggerRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "BLACKBOX_DISABLED",
			"workflow-healing trigger repository not wired on this deployment")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.healingTriggerRepo.Dismiss(ctx, id); err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "no open trigger with id "+id)
			return
		}
		respondError(w, http.StatusInternalServerError, "INTERNAL", "dismiss failed: "+err.Error())
		return
	}
	t, err := s.healingTriggerRepo.Get(ctx, id)
	if err != nil {
		// Dismiss succeeded but read-back failed — best-effort 200 with empty body
		respondJSON(w, http.StatusOK, map[string]string{"id": id, "status": "dismissed"})
		return
	}
	respondJSON(w, http.StatusOK, healingTriggerToJSON(t))
}

// adminHealingTriggersItem routes /api/v1/admin/workflow-healing/triggers/...
// Handles:
//   - /triggers/bulk-dismiss              (POST, single-segment after prefix)
//   - /triggers/{id}/dismiss              (POST)
//   - /triggers/{id}/generate-candidate   (POST)
func (s *Server) adminHealingTriggersItem(w http.ResponseWriter, r *http.Request) {
	const prefix = "/api/v1/admin/workflow-healing/triggers/"
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	// Single-segment is reserved for bulk operations.
	if rest == "bulk-dismiss" {
		s.AdminHealingTriggersBulkDismiss(w, r)
		return
	}
	if strings.HasSuffix(rest, "/dismiss") {
		id := strings.TrimSuffix(rest, "/dismiss")
		if id == "" || strings.Contains(id, "/") {
			respondError(w, http.StatusBadRequest, "BAD_REQUEST", "malformed trigger id")
			return
		}
		s.AdminHealingTriggerDismiss(w, r, id)
		return
	}
	if strings.HasSuffix(rest, "/generate-candidate") {
		id := strings.TrimSuffix(rest, "/generate-candidate")
		if id == "" || strings.Contains(id, "/") {
			respondError(w, http.StatusBadRequest, "BAD_REQUEST", "malformed trigger id")
			return
		}
		s.AdminHealingTriggerGenerateCandidate(w, r, id)
		return
	}
	respondError(w, http.StatusNotFound, "NOT_FOUND", "no such trigger action")
}

// HealingTriggerBulkDismissRequest is the wire shape for
// POST /triggers/bulk-dismiss. The handler dismisses each ID in
// order, aggregating per-ID failures rather than aborting the
// batch.
type HealingTriggerBulkDismissRequest struct {
	IDs []string `json:"ids"`
}

// HealingTriggerBulkDismissResponse summarises the outcome. Errors
// are returned per-ID so the caller can retry only the failures.
type HealingTriggerBulkDismissResponse struct {
	Dismissed int                  `json:"dismissed"`
	Failures  []BulkDismissFailure `json:"failures,omitempty"`
}

// BulkDismissFailure pairs an ID with the reason its dismiss
// failed (most common: ErrNotFound when the row is already
// terminal).
type BulkDismissFailure struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

// AdminHealingTriggersBulkDismiss handles
// POST /api/v1/admin/workflow-healing/triggers/bulk-dismiss.
func (s *Server) AdminHealingTriggersBulkDismiss(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminGate(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST only")
		return
	}
	if s.healingTriggerRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "BLACKBOX_DISABLED",
			"workflow-healing trigger repository not wired on this deployment")
		return
	}
	var body HealingTriggerBulkDismissRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32*1024))
	if err := dec.Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"request body must be JSON {ids: [...]}: "+err.Error())
		return
	}
	if len(body.IDs) == 0 {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "ids must be non-empty")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	out := HealingTriggerBulkDismissResponse{}
	for _, id := range body.IDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if err := s.healingTriggerRepo.Dismiss(ctx, id); err != nil {
			out.Failures = append(out.Failures, BulkDismissFailure{ID: id, Error: err.Error()})
			continue
		}
		out.Dismissed++
	}
	respondJSON(w, http.StatusOK, out)
}

// AdminHealingTriggerGenerateCandidate handles
// POST /api/v1/admin/workflow-healing/triggers/{id}/generate-candidate.
//
// Flow: look up trigger → check status=open → call architect.Propose
// with the trigger's workflow_id → MarkGenerated stamps the
// proposal_id on the trigger → respond with {proposal_id, trigger}.
//
// Sentinel errors from the architect map to the same HTTP
// statuses as POST /workflow-architect/propose for caller
// consistency (mapArchitectError).
func (s *Server) AdminHealingTriggerGenerateCandidate(w http.ResponseWriter, r *http.Request, id string) {
	if !s.requireAdminGate(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST only")
		return
	}
	if s.healingTriggerRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "BLACKBOX_DISABLED",
			"workflow-healing trigger repository not wired on this deployment")
		return
	}
	// Generous timeout — architect runs a synchronous LLM call.
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	t, err := s.healingTriggerRepo.Get(ctx, id)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "no trigger with id "+id)
			return
		}
		respondError(w, http.StatusInternalServerError, "INTERNAL", "trigger lookup failed: "+err.Error())
		return
	}
	if t.Status != persistence.HealingTriggerStatusOpen {
		respondError(w, http.StatusConflict, "TRIGGER_NOT_OPEN",
			"only open triggers can generate candidates; current status: "+string(t.Status))
		return
	}

	// Self-Healing Workflow Genome v1, part 2: try a deterministic structural
	// recipe BEFORE paying for the LLM architect. A recipe candidate is
	// promotable through the same proposal apply path (ProposalFromRecipeResult
	// synthesizes a pending proposal). When no recipe applies — or the recipe
	// deps aren't wired — fall through to the architect.
	if proposal, cand, ok := s.tryRecipeCandidate(ctx, t); ok {
		if err := s.workflowProposals.Insert(ctx, proposal); err != nil {
			// Couldn't persist the recipe proposal — fall back to the
			// architect rather than fail the request.
			s.logger.Warn().Err(err).Str("trigger_id", id).
				Msg("healing recipe proposal insert failed; falling back to architect")
		} else {
			if err := s.healingTriggerRepo.MarkGenerated(ctx, id, proposal.ID); err != nil {
				respondError(w, http.StatusInternalServerError, "TRIGGER_STAMP_FAILED",
					fmt.Sprintf("recipe proposal %s created but trigger stamp failed: %s", proposal.ID, err.Error()))
				return
			}
			// Candidate persistence is best-effort (mirrors the architect path):
			// the proposal + trigger stamp are already durable.
			if s.healingCandidateRepo != nil {
				if err := s.healingCandidateRepo.Insert(ctx, cand); err != nil {
					s.logger.Warn().Err(err).Str("trigger_id", id).Str("proposal_id", proposal.ID).
						Msg("healing recipe candidate persist failed; proposal + trigger stamp are durable")
				}
			}
			s.logger.Info().Str("trigger_id", id).Str("proposal_id", proposal.ID).
				Str("candidate_class", string(cand.CandidateClass)).
				Msg("healing candidate generated by deterministic recipe (architect not called)")
			if updated, gerr := s.healingTriggerRepo.Get(ctx, id); gerr == nil {
				respondJSON(w, http.StatusOK, healingTriggerToJSON(updated))
				return
			}
			respondJSON(w, http.StatusOK, map[string]string{
				"id":          id,
				"status":      string(persistence.HealingTriggerStatusGeneratedCandidate),
				"proposal_id": proposal.ID,
			})
			return
		}
	}

	if s.workflowArchitect == nil {
		respondError(w, http.StatusServiceUnavailable, "ARCHITECT_DISABLED",
			"workflow-architect not wired and no deterministic recipe applied")
		return
	}
	proposalAny, err := proposeHealingTriggerCandidate(ctx, s.workflowArchitect, t.WorkflowID, t.EvidenceExecutionIDs)
	if err != nil {
		s.mapArchitectError(w, t.WorkflowID, err)
		return
	}
	proposal, ok := proposalAny.(*persistence.WorkflowProposal)
	if !ok || proposal == nil || proposal.ID == "" {
		respondError(w, http.StatusInternalServerError, "INTERNAL",
			"architect returned no proposal (low confidence?)")
		return
	}
	if err := s.healingTriggerRepo.MarkGenerated(ctx, id, proposal.ID); err != nil {
		// Proposal exists in the proposals tree but the trigger
		// stamp failed — surface BOTH so the caller doesn't lose
		// the architect's work.
		respondError(w, http.StatusInternalServerError, "TRIGGER_STAMP_FAILED",
			fmt.Sprintf("proposal %s created but trigger stamp failed: %s", proposal.ID, err.Error()))
		return
	}
	// Self-Healing Workflow Genome v1: persist a candidate row LINKING
	// to the proposal the architect just stamped (we do NOT duplicate
	// the architect call or the proposal — proposal_diff/motivation are
	// denormalised copies; the proposal remains the apply-path source of
	// truth). Best-effort: a candidate-insert failure must not lose the
	// proposal or the trigger stamp, so we log and continue rather than
	// rolling back. Nil repo (pre-genome / SQLite deployment) skips
	// silently.
	s.persistHealingCandidate(ctx, t, proposal)
	// Read back the updated trigger for a single-shot response.
	updated, err := s.healingTriggerRepo.Get(ctx, id)
	if err != nil {
		// Best-effort: return what we know.
		respondJSON(w, http.StatusOK, map[string]string{
			"id":          id,
			"status":      string(persistence.HealingTriggerStatusGeneratedCandidate),
			"proposal_id": proposal.ID,
		})
		return
	}
	respondJSON(w, http.StatusOK, healingTriggerToJSON(updated))
}

// persistHealingCandidate writes a workflow_healing_candidates row
// that LINKS the trigger to the architect's WorkflowProposal. It is
// best-effort and side-effect-isolated: a nil repo, a nil proposal, or
// an insert error never propagates to the caller (the proposal and
// trigger stamp are already durable). The candidate genome hash is
// derived from the proposal's ProposalYAML; the baseline hash is left
// empty here (the trial runner stamps it against the live genome at
// trial time, when it loads the current workflow definition).
func (s *Server) persistHealingCandidate(ctx context.Context, t *persistence.HealingTrigger, proposal *persistence.WorkflowProposal) {
	if s.healingCandidateRepo == nil || t == nil || proposal == nil {
		return
	}
	// Shared constructor — the UI generate-candidate path persists the
	// SAME row shape (workflowhealing.CandidateFromArchitectProposal),
	// so the two surfaces cannot drift.
	cand := workflowhealing.CandidateFromArchitectProposal(t, proposal)
	if err := s.healingCandidateRepo.Insert(ctx, cand); err != nil {
		s.logger.Warn().
			Err(err).
			Str("trigger_id", t.ID).
			Str("proposal_id", proposal.ID).
			Str("workflow_id", t.WorkflowID).
			Msg("healing candidate persist failed; proposal + trigger stamp are durable")
		return
	}
	s.logger.Info().
		Str("candidate_id", cand.ID).
		Str("trigger_id", t.ID).
		Str("proposal_id", proposal.ID).
		Msg("healing candidate persisted")
}

// tryRecipeCandidate attempts deterministic-recipe generation for a trigger:
// it loads the baseline genome from the registry, tallies per-step failures
// across the trigger's evidence executions, and asks the retry-budget builder
// for a candidate. Returns ok=false (the architect-fallback signal) when the
// recipe deps aren't wired, the workflow can't be loaded, or no recipe
// applies. Read-only — the caller owns persistence.
func (s *Server) tryRecipeCandidate(ctx context.Context, t *persistence.HealingTrigger) (*persistence.WorkflowProposal, *persistence.HealingCandidate, bool) {
	if t == nil || s.projectRegistry == nil || s.workflowProposals == nil || s.stepOutcomeRepo == nil {
		return nil, nil, false
	}
	baseline := s.projectRegistry.GetWorkflow(t.WorkflowID)
	if baseline == nil {
		return nil, nil, false
	}
	rows := s.evidenceStepOutcomes(ctx, t.EvidenceExecutionIDs)

	// Prefer the verifier-insertion recipe when the evidence shows
	// verifier violations (verifier_warn): inserting an explicit
	// verification checkpoint targets that failure mode directly. Falls
	// through to the retry-budget recipe when it doesn't apply (no
	// offending step, anchor lacks on_success, or the verifier role isn't
	// declared in the genome so validation fails).
	verifierFailures := workflowhealing.VerifierFailuresByStep(rows)
	if proposal, cand, err := workflowhealing.BuildVerifierInsertionCandidate(
		baseline, t, verifierFailures, defaultHealingVerifierRole, time.Now()); err == nil {
		return proposal, cand, true
	}

	failures := workflowhealing.FailuresByStep(rows)
	proposal, cand, err := workflowhealing.BuildRetryBudgetCandidate(baseline, t, failures, time.Now())
	if err != nil {
		// Both deterministic recipes declined → architect fallback.
		return nil, nil, false
	}
	return proposal, cand, true
}

// defaultHealingVerifierRole is the role assigned to a verifier step the
// verifier-insertion recipe synthesizes. When a workflow's swarm doesn't
// declare this role the generated genome fails validation and the recipe
// no-ops to the retry-budget recipe / architect, so this is a safe
// default rather than a hard requirement.
const defaultHealingVerifierRole = "verifier"

// evidenceStepOutcomes fetches all step-outcome rows for the given executions,
// concatenated. A per-execution query error is skipped (best-effort: a partial
// tally still yields a usable offending-step signal).
func (s *Server) evidenceStepOutcomes(ctx context.Context, executionIDs []string) []*persistence.ExecutionStepOutcome {
	var rows []*persistence.ExecutionStepOutcome
	for i := range executionIDs {
		execID := executionIDs[i]
		got, err := s.stepOutcomeRepo.List(ctx, persistence.ExecutionStepOutcomeFilter{ExecutionID: &execID})
		if err != nil {
			continue
		}
		rows = append(rows, got...)
	}
	return rows
}

type workflowArchitectWithEvidence interface {
	ProposeWithEvidence(ctx context.Context, workflowID string, evidenceRunIDs []string) (any, error)
}

func proposeHealingTriggerCandidate(ctx context.Context, arch WorkflowArchitect, workflowID string, evidenceRunIDs []string) (any, error) {
	if withEvidence, ok := arch.(workflowArchitectWithEvidence); ok {
		return withEvidence.ProposeWithEvidence(ctx, workflowID, evidenceRunIDs)
	}
	return arch.Propose(ctx, workflowID)
}

func healingTriggerToJSON(t *persistence.HealingTrigger) HealingTriggerJSON {
	out := HealingTriggerJSON{
		ID:                   t.ID,
		ProjectID:            t.ProjectID,
		WorkflowID:           t.WorkflowID,
		TriggerClass:         string(t.TriggerClass),
		MetricName:           t.MetricName,
		BaselineStart:        t.BaselineStart.UTC().Format(time.RFC3339),
		BaselineEnd:          t.BaselineEnd.UTC().Format(time.RFC3339),
		ComparisonStart:      t.ComparisonStart.UTC().Format(time.RFC3339),
		ComparisonEnd:        t.ComparisonEnd.UTC().Format(time.RFC3339),
		BaselineValue:        t.BaselineValue,
		ComparisonValue:      t.ComparisonValue,
		ThresholdValue:       t.ThresholdValue,
		EvidenceExecutionIDs: t.EvidenceExecutionIDs,
		Status:               string(t.Status),
		CreatedAt:            t.CreatedAt.UTC().Format(time.RFC3339),
		ProposalID:           t.ProposalID,
	}
	if t.ResolvedAt != nil {
		out.ResolvedAt = t.ResolvedAt.UTC().Format(time.RFC3339)
	}
	return out
}
