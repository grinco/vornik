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
// Flow: look up trigger → check status=open → try a deterministic recipe →
// otherwise ask the CONFIG ASSISTANT for a single-workflow edit →
// MarkGenerated stamps the proposal_id on the trigger → respond with
// {proposal_id, trigger}.
//
// The producer changed on 2026-09-16 (WP9 step 3): it was the memetic
// architect, which is deleted. mapHealingProducerError distinguishes "not
// wired", "the assistant declined" and "the bridge refused the shape",
// because the operator's next action differs for each — and before the
// cutover the third of those silently fell back to the architect.
func (s *Server) AdminHealingTriggerGenerateCandidate(w http.ResponseWriter, r *http.Request, id string) {
	if !s.requireAdminGate(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST only")
		return
	}
	// Generous timeout — the assistant runs a synchronous model call.
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	// One producer, two presentations (healing_candidate_producer.go). This
	// handler decides only how to RENDER an outcome; the UI button renders the
	// same outcomes differently. The duplicate orchestration that used to live
	// here and in the UI is what let the 2026-09-16 cutover repoint one copy
	// and not the other.
	out, err := s.GenerateHealingCandidateForTrigger(ctx, id)
	if err != nil {
		s.respondGenerateCandidateError(w, id, err)
		return
	}
	if updated, gerr := s.healingTriggerRepo.Get(ctx, id); gerr == nil {
		respondJSON(w, http.StatusOK, healingTriggerToJSON(updated))
		return
	}
	// Best-effort: return what we know rather than lose the proposal id.
	respondJSON(w, http.StatusOK, map[string]string{
		"id":          id,
		"status":      string(persistence.HealingTriggerStatusGeneratedCandidate),
		"proposal_id": out.Proposal.ID,
	})
}

// respondGenerateCandidateError maps a producer failure to the HTTP answer that
// says which KIND of failure it was — the operator's next action differs for
// each, which is why they are not collapsed into one 500.
func (s *Server) respondGenerateCandidateError(w http.ResponseWriter, id string, err error) {
	var stamp *TriggerStampError
	switch {
	case errors.Is(err, ErrHealingTriggerRepoUnavailable):
		respondError(w, http.StatusServiceUnavailable, "BLACKBOX_DISABLED",
			"workflow-healing trigger repository not wired on this deployment")
	case errors.Is(err, ErrHealingTriggerNotFound):
		respondError(w, http.StatusNotFound, "NOT_FOUND", "no trigger with id "+id)
	case errors.Is(err, ErrHealingTriggerNotOpen):
		respondError(w, http.StatusConflict, "TRIGGER_NOT_OPEN", err.Error())
	case errors.As(err, &stamp):
		// The proposal EXISTS. Surface both halves so the caller does not lose
		// the producer's work to a bookkeeping failure.
		respondError(w, http.StatusInternalServerError, "TRIGGER_STAMP_FAILED", stamp.Error())
	case errors.Is(err, errHealingProducerUnavailable):
		respondError(w, http.StatusServiceUnavailable, "HEALING_PRODUCER_DISABLED",
			"the config assistant is not wired and no deterministic recipe applied; no healing candidate can be generated")
	default:
		// Producer-specific failures (assistant declined, bridge refused the
		// shape) keep their own mapping.
		s.mapHealingProducerError(w, id, err)
	}
}

// persistHealingCandidate writes a workflow_healing_candidates row
// that LINKS the trigger to the architect's WorkflowProposal. It is
// best-effort and side-effect-isolated: a nil repo, a nil proposal, or
// an insert error never propagates to the caller (the proposal and
// trigger stamp are already durable). The candidate genome hash is
// derived from the proposal's ProposalYAML; the baseline hash is stamped
// here, against the workflow as it is RIGHT NOW, because that is the file
// the proposal was written against and the only thing that makes the
// promoter's currency gate meaningful. (It used to be left empty on the
// stated grounds that the trial runner stamped it at trial time; the trial
// runner never did, and every candidate that reached trial_passed carried
// an empty baseline as a result — observed 2026-09-16.)
func (s *Server) persistHealingCandidate(ctx context.Context, t *persistence.HealingTrigger, proposal *persistence.WorkflowProposal) {
	if s.healingCandidateRepo == nil || t == nil || proposal == nil {
		return
	}
	// Shared constructor, so the surfaces cannot drift. The CLASS comes from
	// the proposal's provenance rather than the call site: the first cutover
	// deploy filed an assistant genome labelled "architect", which named a
	// producer that no longer exists on the row an operator reads before
	// trusting a candidate (observed 2026-09-16).
	cand := workflowhealing.CandidateFromWorkflowProposal(t, proposal)
	if s.projectRegistry != nil {
		workflowhealing.StampBaselineGenome(cand, s.projectRegistry.GetWorkflow(t.WorkflowID))
	}
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
	if len(executionIDs) == 0 {
		return nil
	}
	// Single query over the whole run set (execution_id IN (...)) instead of
	// one List per execution (E5, audit 2026-07-03).
	rows, err := s.stepOutcomeRepo.List(ctx, persistence.ExecutionStepOutcomeFilter{ExecutionIDs: executionIDs})
	if err != nil {
		return nil
	}
	return rows
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
