package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"vornik.io/vornik/internal/persistence"
)

// Control-plane proposal ledger — operator REST surface (LLD 2026-07-07-
// control-plane-design, Phase 1). A proposal is a human-gated suggested
// change (config diff / model swap); Phase 1 only writes DRAFT + approve/
// reject — there is NO apply path here, so nothing mutates daemon config.
//
// Gated by requireOperatorScope (CE-safe; NOT under /api/v1/admin/, so it
// works in Community and is not stripped by the EE split). Driven by
// `vornikctl operator ...`.

type operatorProposeRequest struct {
	ProjectID   string `json:"projectId"`
	Kind        string `json:"kind"`
	BlastRadius string `json:"blastRadius"`
	Title       string `json:"title"`
	Diff        string `json:"diff"`
	Rationale   string `json:"rationale"`
	Evidence    string `json:"evidence"`
	ProposedBy  string `json:"proposedBy"`
}

type operatorDecideRequest struct {
	Decision string `json:"decision"` // "approve" | "reject"
	Actor    string `json:"actor"`
}

// proposalJSON is the wire shape (mirrors the model, camelCase).
type proposalJSON struct {
	ID          string  `json:"id"`
	ProjectID   string  `json:"projectId,omitempty"`
	Kind        string  `json:"kind"`
	BlastRadius string  `json:"blastRadius"`
	Title       string  `json:"title"`
	Diff        string  `json:"diff,omitempty"`
	Rationale   string  `json:"rationale,omitempty"`
	Evidence    string  `json:"evidence,omitempty"`
	Status      string  `json:"status"`
	ProposedBy  string  `json:"proposedBy,omitempty"`
	Approver    string  `json:"approver,omitempty"`
	CreatedAt   string  `json:"createdAt"`
	DecidedAt   *string `json:"decidedAt,omitempty"`
}

func toProposalJSON(p *persistence.ControlPlaneProposal) proposalJSON {
	j := proposalJSON{
		ID: p.ID, ProjectID: p.ProjectID, Kind: p.Kind, BlastRadius: p.BlastRadius,
		Title: p.Title, Diff: p.Diff, Rationale: p.Rationale, Evidence: p.Evidence,
		Status: p.Status, ProposedBy: p.ProposedBy, Approver: p.Approver,
		CreatedAt: p.CreatedAt.UTC().Format(rfc3339),
	}
	if p.DecidedAt != nil {
		s := p.DecidedAt.UTC().Format(rfc3339)
		j.DecidedAt = &s
	}
	return j
}

const rfc3339 = "2006-01-02T15:04:05Z07:00"

// OperatorProposals handles the collection routes:
//
//	POST /api/v1/operator/proposals        — propose (writes DRAFT)
//	GET  /api/v1/operator/proposals        — list (?project=&status=)
func (s *Server) OperatorProposals(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperatorScope(w, r) {
		return
	}
	if s.proposalStore == nil {
		respondError(w, http.StatusServiceUnavailable, "PROPOSALS_UNAVAILABLE", "proposal ledger not wired on this daemon")
		return
	}
	switch r.Method {
	case http.MethodPost:
		s.operatorPropose(w, r)
	case http.MethodGet:
		s.operatorListProposals(w, r)
	default:
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET or POST")
	}
}

// OperatorProposalItem handles the per-id routes:
//
//	GET  /api/v1/operator/proposals/{id}
//	POST /api/v1/operator/proposals/{id}/decide  {"decision":"approve|reject","actor":"..."}
func (s *Server) OperatorProposalItem(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperatorScope(w, r) {
		return
	}
	if s.proposalStore == nil {
		respondError(w, http.StatusServiceUnavailable, "PROPOSALS_UNAVAILABLE", "proposal ledger not wired on this daemon")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/operator/proposals/")
	if rest == "" || rest == r.URL.Path {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "expected /api/v1/operator/proposals/{id}[/decide]")
		return
	}
	if id, ok := strings.CutSuffix(rest, "/decide"); ok {
		s.operatorDecide(w, r, id)
		return
	}
	if strings.Contains(rest, "/") {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "unknown proposal sub-route")
		return
	}
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
		return
	}
	p, err := s.proposalStore.GetByID(r.Context(), rest)
	if err != nil {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "proposal not found")
		return
	}
	respondJSON(w, http.StatusOK, toProposalJSON(p))
}

func (s *Server) operatorPropose(w http.ResponseWriter, r *http.Request) {
	var req operatorProposeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "INVALID_BODY", "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Title) == "" || !validProposalKind(req.Kind) || !validProposalScope(req.BlastRadius) {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "title required; kind in {config,model,scaffold}; blastRadius in {model,project,swarm,daemon}")
		return
	}
	proposedBy := strings.TrimSpace(req.ProposedBy)
	if proposedBy == "" {
		proposedBy = "operator"
	}
	p := &persistence.ControlPlaneProposal{
		ID:          persistence.GenerateID("cpp"),
		ProjectID:   strings.TrimSpace(req.ProjectID),
		Kind:        req.Kind,
		BlastRadius: req.BlastRadius,
		Title:       strings.TrimSpace(req.Title),
		Diff:        req.Diff,
		Rationale:   req.Rationale,
		Evidence:    req.Evidence,
		Status:      persistence.ProposalStatusDraft,
		ProposedBy:  proposedBy,
	}
	if err := s.proposalStore.Create(r.Context(), p); err != nil {
		respondError(w, http.StatusBadRequest, "PROPOSE_FAILED", err.Error())
		return
	}
	respondJSON(w, http.StatusCreated, toProposalJSON(p))
}

func (s *Server) operatorListProposals(w http.ResponseWriter, r *http.Request) {
	f := persistence.ProposalListFilter{
		ProjectID: strings.TrimSpace(r.URL.Query().Get("project")),
	}
	if st := strings.TrimSpace(r.URL.Query().Get("status")); st != "" {
		f.Statuses = []string{st}
	}
	list, err := s.proposalStore.List(r.Context(), f)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "list failed: "+err.Error())
		return
	}
	out := make([]proposalJSON, 0, len(list))
	for _, p := range list {
		out = append(out, toProposalJSON(p))
	}
	respondJSON(w, http.StatusOK, map[string]any{"proposals": out, "count": len(out)})
}

func (s *Server) operatorDecide(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use POST")
		return
	}
	var req operatorDecideRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "INVALID_BODY", "invalid JSON: "+err.Error())
		return
	}
	var target string
	switch strings.ToLower(strings.TrimSpace(req.Decision)) {
	case "approve":
		target = persistence.ProposalStatusApproved
	case "reject":
		target = persistence.ProposalStatusRejected
	default:
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "decision must be 'approve' or 'reject'")
		return
	}
	actor := strings.TrimSpace(req.Actor)
	if actor == "" {
		actor = "operator"
	}
	err := s.proposalStore.SetStatus(r.Context(), id, target, actor)
	switch {
	case err == nil:
		p, _ := s.proposalStore.GetByID(r.Context(), id)
		respondJSON(w, http.StatusOK, toProposalJSON(p))
	case errors.Is(err, persistence.ErrProposalSelfApprove):
		respondError(w, http.StatusConflict, "SELF_APPROVAL_FORBIDDEN", "the proposer cannot approve their own proposal")
	case errors.Is(err, persistence.ErrProposalNotDraft):
		respondError(w, http.StatusConflict, "NOT_DRAFT", "proposal is not in DRAFT (already decided)")
	case errors.Is(err, persistence.ErrNotFound):
		respondError(w, http.StatusNotFound, "NOT_FOUND", "proposal not found")
	default:
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "decide failed: "+err.Error())
	}
}

func validProposalKind(k string) bool {
	switch k {
	case persistence.ProposalKindConfig, persistence.ProposalKindModel, persistence.ProposalKindScaffold:
		return true
	}
	return false
}

func validProposalScope(s string) bool {
	switch s {
	case persistence.ProposalScopeModel, persistence.ProposalScopeProject,
		persistence.ProposalScopeSwarm, persistence.ProposalScopeDaemon:
		return true
	}
	return false
}
