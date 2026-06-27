package api

// Workflow-healing candidate admin endpoints — Self-Healing Workflow
// Genome v1 (LLD § Admin API). These sit on top of the candidate ledger
// the generate-candidate flow writes (healing_triggers_handlers.go) and
// expose the operator's trial + promotion lifecycle:
//
//   GET  /api/v1/admin/workflow-healing/candidates/{id}
//   POST /api/v1/admin/workflow-healing/candidates/{id}/run-trial
//   POST /api/v1/admin/workflow-healing/candidates/{id}/promote
//   POST /api/v1/admin/workflow-healing/candidates/{id}/reject
//
// Safety invariants honoured here (LLD non-negotiables):
//   - Promotion is ALWAYS a manual operator action and runs the gate +
//     the memetic apply path via the Promoter; it REFUSES anything not in
//     trial_passed (the Promoter enforces this; the handler maps the
//     refusal to 409). Nothing auto-promotes.
//   - run-trial is operator-triggered (this endpoint). There is no
//     background auto-trial loop.
//   - Every new dependency is nil-guarded → 503 when unwired, never a
//     panic.
//
// Admin-audit rows record the REDACTED principal
// (apiKeyPrincipalFromContext), never the raw bearer key.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// HealingTrialRunner is the narrow seam over the trial runner the
// run-trial endpoint drives. Production wires it to a thin adapter over
// *workflowhealing.TrialRunner (the concrete runner returns its own
// TrialResult; the adapter flattens it to HealingTrialOutcome so the api
// package stays free of a workflowhealing import). Tests supply a fake.
type HealingTrialRunner interface {
	// RunTrial executes a trial of the candidate in the given mode
	// (static | replay) against the evidence set and persists the
	// result, returning the verdict + scorecard JSON. Mirrors
	// workflowhealing.TrialRunner.RunTrial's contract: a missing
	// candidate surfaces ErrHealingCandidateNotFound; a terminal
	// candidate surfaces ErrHealingCandidateTerminal; an unsupported
	// mode surfaces ErrHealingTrialMode; a live concurrent trial
	// surfaces ErrHealingTrialRunning.
	RunTrial(ctx context.Context, candidateID, mode string, evidenceIDs []string) (*HealingTrialOutcome, error)
	// RunTrialAsync opens the trial (pending row, trial_running) and
	// returns its id immediately; the evaluation finishes in a
	// detached goroutine. Used for replay trials, whose real replays
	// run minutes past any HTTP window. Same sentinel contract as
	// RunTrial.
	RunTrialAsync(ctx context.Context, candidateID, mode string, evidenceIDs []string) (trialID string, err error)
}

// HealingTrialOutcome is the api-package projection of a trial run. The
// verdict + scorecard JSON are the operator-facing payload; the scorecard
// is opaque JSON (the LLD HealingScorecard) the UI renders.
type HealingTrialOutcome struct {
	Mode          string `json:"mode"`
	Verdict       string `json:"verdict"`
	ScorecardJSON string `json:"-"`
}

// HealingCandidatePromoter is the narrow seam over the promoter the
// promote/reject endpoints drive. Production wires it to a thin adapter
// over *workflowhealing.Promoter; tests supply a fake. The promoter
// enforces the trial_passed precondition and runs the memetic apply path
// — the handler only maps its sentinel errors to HTTP statuses.
type HealingCandidatePromoter interface {
	// Promote runs the promotion gate + the memetic apply path for the
	// candidate's linked proposal and stamps the candidate promoted.
	// Refuses anything not in trial_passed.
	Promote(ctx context.Context, candidateID, promotedBy string) (*persistence.HealingCandidate, error)
	// Reject flips a non-terminal candidate to rejected without touching
	// production.
	Reject(ctx context.Context, candidateID string) (*persistence.HealingCandidate, error)
}

// Sentinel errors the seams return so the handler can map them to HTTP
// statuses without importing workflowhealing. The service-layer adapters
// translate the workflowhealing sentinels into these.
var (
	// ErrHealingCandidateNotFound → 404.
	ErrHealingCandidateNotFound = errors.New("healing candidate not found")
	// ErrHealingCandidateTerminal → 409 (promoted/rejected already).
	ErrHealingCandidateTerminal = errors.New("healing candidate is terminal")
	// ErrHealingCandidateNotPromotable → 409 (not trial_passed). This is
	// the gate that enforces "nothing promotes without trial_passed".
	ErrHealingCandidateNotPromotable = errors.New("healing candidate is not promotable (requires trial_passed)")
	// ErrHealingTrialMode → 400 (unsupported trial mode).
	ErrHealingTrialMode = errors.New("unsupported trial mode")
	// ErrHealingTrialRunning → 409 (a trial is already in flight for
	// this candidate; wait for its verdict before starting another).
	ErrHealingTrialRunning = errors.New("a trial is already running for this candidate")
)

// WithHealingTrialRepository wires the trial ledger so the candidate GET
// endpoint can render the candidate's trial history. Nil leaves trials
// empty in the response.
func WithHealingTrialRepository(repo persistence.WorkflowHealingTrialRepository) ServerOption {
	return func(s *Server) {
		s.healingTrialRepo = repo
	}
}

// WithHealingTrialRunner wires the operator-triggered trial runner behind
// the run-trial endpoint. Nil keeps run-trial at 503.
func WithHealingTrialRunner(r HealingTrialRunner) ServerOption {
	return func(s *Server) {
		s.healingTrialRunner = r
	}
}

// WithHealingCandidatePromoter wires the promote/reject actions. Nil keeps
// those endpoints at 503.
func WithHealingCandidatePromoter(p HealingCandidatePromoter) ServerOption {
	return func(s *Server) {
		s.healingPromoter = p
	}
}

// HealingCandidateJSON is the wire shape of a candidate row. Times are
// RFC3339; the proposal diff/motivation are denormalised copies sourced
// from the linked WorkflowProposal at generation time.
type HealingCandidateJSON struct {
	ID                  string `json:"id"`
	TriggerID           string `json:"trigger_id"`
	ProjectID           string `json:"project_id"`
	WorkflowID          string `json:"workflow_id"`
	ProposalID          string `json:"proposal_id"`
	BaselineGenomeHash  string `json:"baseline_genome_hash,omitempty"`
	CandidateGenomeHash string `json:"candidate_genome_hash,omitempty"`
	CandidateClass      string `json:"candidate_class"`
	ProposalDiff        string `json:"proposal_diff,omitempty"`
	Motivation          string `json:"motivation,omitempty"`
	ExpectedEffect      string `json:"expected_effect,omitempty"`
	RiskLevel           string `json:"risk_level"`
	Status              string `json:"status"`
	CreatedAt           string `json:"created_at"`
	PromotedAt          string `json:"promoted_at,omitempty"`
	PromotedBy          string `json:"promoted_by,omitempty"`
}

// HealingTrialJSON is the wire shape of a trial row. The summary +
// scorecard blobs are emitted as raw JSON so the UI/CLI can render the
// LLD scorecard without a second decode step.
type HealingTrialJSON struct {
	ID                   string          `json:"id"`
	CandidateID          string          `json:"candidate_id"`
	Mode                 string          `json:"mode"`
	EvidenceExecutionIDs []string        `json:"evidence_execution_ids,omitempty"`
	BaselineSummary      json.RawMessage `json:"baseline_summary,omitempty"`
	CandidateSummary     json.RawMessage `json:"candidate_summary,omitempty"`
	Scorecard            json.RawMessage `json:"scorecard,omitempty"`
	Verdict              string          `json:"verdict"`
	StartedAt            string          `json:"started_at"`
	FinishedAt           string          `json:"finished_at,omitempty"`
}

// HealingCandidateDetailResponse is the GET {id} payload: the candidate
// plus its trial history (newest first).
type HealingCandidateDetailResponse struct {
	Candidate HealingCandidateJSON `json:"candidate"`
	Trials    []HealingTrialJSON   `json:"trials"`
}

// adminHealingCandidatesItem routes
// /api/v1/admin/workflow-healing/candidates/...:
//   - /{id}             (GET)
//   - /{id}/run-trial   (POST)
//   - /{id}/promote     (POST)
//   - /{id}/reject      (POST)
func (s *Server) adminHealingCandidatesItem(w http.ResponseWriter, r *http.Request) {
	const prefix = "/api/v1/admin/workflow-healing/candidates/"
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	for _, action := range []string{"run-trial", "promote", "reject"} {
		if strings.HasSuffix(rest, "/"+action) {
			id := strings.TrimSuffix(rest, "/"+action)
			if id == "" || strings.Contains(id, "/") {
				respondError(w, http.StatusBadRequest, "BAD_REQUEST", "malformed candidate id")
				return
			}
			switch action {
			case "run-trial":
				s.AdminHealingCandidateRunTrial(w, r, id)
			case "promote":
				s.AdminHealingCandidatePromote(w, r, id)
			case "reject":
				s.AdminHealingCandidateReject(w, r, id)
			}
			return
		}
	}
	// Bare /{id} → GET detail.
	if rest == "" || strings.Contains(rest, "/") {
		respondError(w, http.StatusBadRequest, "BAD_REQUEST", "malformed candidate id")
		return
	}
	s.AdminHealingCandidateGet(w, r, rest)
}

// AdminHealingCandidateGet handles GET .../candidates/{id}. Returns the
// candidate row plus its trial history.
func (s *Server) AdminHealingCandidateGet(w http.ResponseWriter, r *http.Request, id string) {
	if !s.requireAdminGate(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	if s.healingCandidateRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "BLACKBOX_DISABLED",
			"workflow-healing candidate repository not wired on this deployment")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	cand, err := s.healingCandidateRepo.Get(ctx, id)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "no candidate with id "+id)
			return
		}
		respondError(w, http.StatusInternalServerError, "INTERNAL", "candidate lookup failed: "+err.Error())
		return
	}
	out := HealingCandidateDetailResponse{
		Candidate: healingCandidateToJSON(cand),
		Trials:    []HealingTrialJSON{},
	}
	// Trial history is best-effort: a nil repo or a list error leaves the
	// trials slice empty rather than failing the whole response.
	if s.healingTrialRepo != nil {
		trials, terr := s.healingTrialRepo.ListByCandidate(ctx, id)
		if terr != nil {
			s.logger.Warn().Err(terr).Str("candidate_id", id).Msg("healing candidate trial history list failed")
		} else {
			for _, tr := range trials {
				out.Trials = append(out.Trials, healingTrialToJSON(tr))
			}
		}
	}
	respondJSON(w, http.StatusOK, out)
}

// healingRunTrialRequest is the run-trial body. mode defaults to static
// when omitted (the cheapest, always-available signal); evidence_execution_ids
// override the trigger's evidence set for replay trials.
type healingRunTrialRequest struct {
	Mode                 string   `json:"mode"`
	EvidenceExecutionIDs []string `json:"evidence_execution_ids"`
}

// AdminHealingCandidateRunTrial handles POST .../candidates/{id}/run-trial.
// Operator-triggered: kicks the trial runner synchronously and returns the
// verdict. There is no background trial loop (LLD non-negotiable #5).
func (s *Server) AdminHealingCandidateRunTrial(w http.ResponseWriter, r *http.Request, id string) {
	if !s.requireAdminGate(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST only")
		return
	}
	if s.healingTrialRunner == nil {
		respondError(w, http.StatusServiceUnavailable, "BLACKBOX_DISABLED",
			"workflow-healing trial runner not wired on this deployment")
		return
	}
	var body healingRunTrialRequest
	// Empty body is fine — defaults to a static trial.
	if r.Body != nil {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
		if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
				"request body must be JSON {mode, evidence_execution_ids}: "+err.Error())
			return
		}
	}
	mode := strings.TrimSpace(body.Mode)
	if mode == "" {
		mode = string(persistence.HealingTrialModeStatic)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	// Replay trials run real (non-production) re-executions that take
	// minutes to tens of minutes — far past any handler window. They
	// run ASYNC: the trial row opens now (202), the verdict lands on
	// the candidate's trial history when the detached run finishes.
	// Static is a fast deterministic check and stays synchronous.
	if mode == string(persistence.HealingTrialModeReplay) {
		trialID, err := s.healingTrialRunner.RunTrialAsync(ctx, id, mode, body.EvidenceExecutionIDs)
		if err != nil {
			s.respondHealingTrialError(w, err, id, mode)
			return
		}
		s.auditHealingCandidate(ctx, r, "blackbox-candidate.trial-run", id, map[string]any{
			"mode":     mode,
			"trial_id": trialID,
			"verdict":  string(persistence.HealingTrialPending),
		})
		respondJSON(w, http.StatusAccepted, map[string]any{
			"candidate_id": id,
			"mode":         mode,
			"trial_id":     trialID,
			"verdict":      string(persistence.HealingTrialPending),
			"note":         "replay trial started; poll the candidate's trial history for the verdict",
		})
		return
	}

	outcome, err := s.healingTrialRunner.RunTrial(ctx, id, mode, body.EvidenceExecutionIDs)
	if err != nil {
		s.respondHealingTrialError(w, err, id, mode)
		return
	}
	s.auditHealingCandidate(ctx, r, "blackbox-candidate.trial-run", id, map[string]any{
		"mode":    outcome.Mode,
		"verdict": outcome.Verdict,
	})
	resp := map[string]any{
		"candidate_id": id,
		"mode":         outcome.Mode,
		"verdict":      outcome.Verdict,
	}
	if outcome.ScorecardJSON != "" {
		resp["scorecard"] = json.RawMessage(outcome.ScorecardJSON)
	}
	respondJSON(w, http.StatusOK, resp)
}

// respondHealingTrialError maps the trial-runner sentinels onto HTTP
// statuses; shared by the sync (static) and async (replay) paths.
func (s *Server) respondHealingTrialError(w http.ResponseWriter, err error, id, mode string) {
	switch {
	case errors.Is(err, ErrHealingCandidateNotFound):
		respondError(w, http.StatusNotFound, "NOT_FOUND", "no candidate with id "+id)
	case errors.Is(err, ErrHealingCandidateTerminal):
		respondError(w, http.StatusConflict, "CANDIDATE_TERMINAL",
			"candidate is in a terminal state; cannot run a trial")
	case errors.Is(err, ErrHealingTrialRunning):
		respondError(w, http.StatusConflict, "TRIAL_RUNNING",
			"a trial is already running for this candidate; wait for its verdict")
	case errors.Is(err, ErrHealingTrialMode):
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"unsupported trial mode "+strconv.Quote(mode)+"; use static or replay")
	default:
		respondError(w, http.StatusInternalServerError, "INTERNAL", "trial run failed: "+err.Error())
	}
}

// AdminHealingCandidatePromote handles POST .../candidates/{id}/promote.
// Runs the gate + the memetic apply path via the Promoter. REFUSES (409)
// when the candidate is not trial_passed — nothing promotes without a
// passing trial.
func (s *Server) AdminHealingCandidatePromote(w http.ResponseWriter, r *http.Request, id string) {
	if !s.requireAdminGate(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST only")
		return
	}
	if s.healingPromoter == nil {
		respondError(w, http.StatusServiceUnavailable, "BLACKBOX_DISABLED",
			"workflow-healing promoter not wired on this deployment")
		return
	}
	// Promotion writes WORKFLOW.md, git-commits, and hot-reloads config —
	// give it room.
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	promotedBy := apiKeyPrincipalFromContext(r.Context())
	if promotedBy == "" {
		promotedBy = "anonymous-admin"
	}
	cand, err := s.healingPromoter.Promote(ctx, id, promotedBy)
	if err != nil {
		switch {
		case errors.Is(err, ErrHealingCandidateNotFound):
			respondError(w, http.StatusNotFound, "NOT_FOUND", "no candidate with id "+id)
		case errors.Is(err, ErrHealingCandidateNotPromotable):
			respondError(w, http.StatusConflict, "CANDIDATE_NOT_PROMOTABLE",
				"candidate has not cleared a trial; promotion requires status trial_passed")
		case errors.Is(err, ErrHealingCandidateTerminal):
			respondError(w, http.StatusConflict, "CANDIDATE_TERMINAL",
				"candidate is already promoted or rejected")
		default:
			respondError(w, http.StatusInternalServerError, "PROMOTE_FAILED", "promotion failed: "+err.Error())
		}
		return
	}
	s.auditHealingCandidate(ctx, r, "blackbox-candidate.promoted", id, map[string]any{
		"workflow_id": cand.WorkflowID,
		"proposal_id": cand.ProposalID,
		"trigger_id":  cand.TriggerID,
	})
	respondJSON(w, http.StatusOK, healingCandidateToJSON(cand))
}

// AdminHealingCandidateReject handles POST .../candidates/{id}/reject.
// Flips a non-terminal candidate to rejected without touching production.
func (s *Server) AdminHealingCandidateReject(w http.ResponseWriter, r *http.Request, id string) {
	if !s.requireAdminGate(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST only")
		return
	}
	if s.healingPromoter == nil {
		respondError(w, http.StatusServiceUnavailable, "BLACKBOX_DISABLED",
			"workflow-healing promoter not wired on this deployment")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	cand, err := s.healingPromoter.Reject(ctx, id)
	if err != nil {
		switch {
		case errors.Is(err, ErrHealingCandidateNotFound):
			respondError(w, http.StatusNotFound, "NOT_FOUND", "no candidate with id "+id)
		case errors.Is(err, ErrHealingCandidateTerminal):
			respondError(w, http.StatusConflict, "CANDIDATE_TERMINAL",
				"candidate is already promoted or rejected")
		default:
			respondError(w, http.StatusInternalServerError, "INTERNAL", "reject failed: "+err.Error())
		}
		return
	}
	s.auditHealingCandidate(ctx, r, "blackbox-candidate.rejected", id, map[string]any{
		"workflow_id": cand.WorkflowID,
		"trigger_id":  cand.TriggerID,
	})
	respondJSON(w, http.StatusOK, healingCandidateToJSON(cand))
}

// auditHealingCandidate writes an admin-audit row with the REDACTED
// principal (apiKeyPrincipalFromContext, never the raw bearer key). Nil
// audit repo is a no-op; a marshal/insert failure is best-effort and never
// blocks the operator action.
func (s *Server) auditHealingCandidate(ctx context.Context, r *http.Request, action, candidateID string, after map[string]any) {
	if s.adminAuditRepo == nil {
		return
	}
	principal := apiKeyPrincipalFromContext(r.Context())
	if principal == "" {
		principal = "anonymous-admin"
	}
	if after == nil {
		after = map[string]any{}
	}
	after["candidate_id"] = candidateID
	afterJSON, _ := json.Marshal(after)
	_ = s.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
		Principal: principal,
		Source:    "api",
		Action:    action,
		Target:    candidateID,
		After:     string(afterJSON),
		IP:        clientIPFromRequest(r),
		UserAgent: r.UserAgent(),
	})
}

func healingCandidateToJSON(c *persistence.HealingCandidate) HealingCandidateJSON {
	if c == nil {
		return HealingCandidateJSON{}
	}
	out := HealingCandidateJSON{
		ID:                  c.ID,
		TriggerID:           c.TriggerID,
		ProjectID:           c.ProjectID,
		WorkflowID:          c.WorkflowID,
		ProposalID:          c.ProposalID,
		BaselineGenomeHash:  c.BaselineGenomeHash,
		CandidateGenomeHash: c.CandidateGenomeHash,
		CandidateClass:      string(c.CandidateClass),
		ProposalDiff:        c.ProposalDiff,
		Motivation:          c.Motivation,
		ExpectedEffect:      c.ExpectedEffect,
		RiskLevel:           string(c.RiskLevel),
		Status:              string(c.Status),
		CreatedAt:           c.CreatedAt.UTC().Format(time.RFC3339),
		PromotedBy:          c.PromotedBy,
	}
	if c.PromotedAt != nil {
		out.PromotedAt = c.PromotedAt.UTC().Format(time.RFC3339)
	}
	return out
}

func healingTrialToJSON(t *persistence.HealingTrial) HealingTrialJSON {
	if t == nil {
		return HealingTrialJSON{}
	}
	out := HealingTrialJSON{
		ID:                   t.ID,
		CandidateID:          t.CandidateID,
		Mode:                 string(t.Mode),
		EvidenceExecutionIDs: t.EvidenceExecutionIDs,
		Verdict:              string(t.Verdict),
		StartedAt:            t.StartedAt.UTC().Format(time.RFC3339),
	}
	out.BaselineSummary = rawJSONOrNil(t.BaselineSummary)
	out.CandidateSummary = rawJSONOrNil(t.CandidateSummary)
	out.Scorecard = rawJSONOrNil(t.Scorecard)
	if t.FinishedAt != nil {
		out.FinishedAt = t.FinishedAt.UTC().Format(time.RFC3339)
	}
	return out
}

// rawJSONOrNil returns the blob as raw JSON, or nil when empty / the empty
// object so the wire shape omits noise.
func rawJSONOrNil(blob string) json.RawMessage {
	blob = strings.TrimSpace(blob)
	if blob == "" || blob == "{}" {
		return nil
	}
	return json.RawMessage(blob)
}
