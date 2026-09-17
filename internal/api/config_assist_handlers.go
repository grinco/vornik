package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	a2aclient "vornik.io/vornik/internal/a2a/client"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/configassist"
	"vornik.io/vornik/internal/persistence"
)

// The configuration assistant's operator REST entrypoint (2026-09-13 design
// §6.3, entrypoint 1; plan §7f). COMMUNITY: /api/v1/operator/assist, gated by
// requireOperatorCapability (operator scope + explicit capability, review
// R2), never under /api/v1/admin/. One request = one intent = one
// proposal or one named refusal (design §3).

// ConfigAssistDeps is what the container supplies beyond what the server
// already holds (registry, proposals, applier, chat provider, config).
type ConfigAssistDeps struct {
	// TreeRoot is the deployed configs directory (the assistant's root,
	// never its parent — design §10a).
	TreeRoot string
	// ApplyPrefix is TreeRoot relative to the apply engine's ConfigDir.
	ApplyPrefix string
	// ConfigPath is config.yaml, for the fresh hygiene evaluation.
	ConfigPath string
	// WorkspaceRoot is runtime.project_workspace_path for the virtual
	// PROJECT_CONTEXT.md bridge.
	WorkspaceRoot string
	// SecretSnapshot is the doctor's boot-time secret-field snapshot.
	SecretSnapshot func() map[string]string
	// Usage records model spend (assistant + judge) to the ledger.
	Usage configassist.UsageRecorder
}

// EnableConfigAssistant constructs the engine over the server's own
// wiring. Called by the container once the API server exists; a missing
// registry, ledger or chat provider leaves the entrypoint at 503.
func (s *Server) EnableConfigAssistant(deps ConfigAssistDeps) {
	if s.projectRegistry == nil || s.proposalStore == nil || s.chatProvider == nil {
		s.logger.Warn().Bool("registry", s.projectRegistry != nil).Bool("ledger", s.proposalStore != nil).Bool("chat", s.chatProvider != nil).
			Msg("config-assistant: not wired (registry, proposal ledger and chat provider are all required)")
		return
	}
	store, ok := s.proposalStore.(configassist.ProposalStore)
	if !ok {
		s.logger.Warn().Msg("config-assistant: proposal repository lacks the identifier columns (GetByIdempotencyKey); entrypoint stays closed")
		return
	}
	e := &configassist.Engine{
		TreeRoot: deps.TreeRoot, ApplyPrefix: deps.ApplyPrefix, ConfigPath: deps.ConfigPath, WorkspaceRoot: deps.WorkspaceRoot, SecretSnapshot: deps.SecretSnapshot,
		Config: func() config.AssistantConfig {
			if s.config == nil {
				return config.AssistantConfig{}
			}
			return s.config.ConfigAssistant
		},
		Registry: s.projectRegistry, Evidence: assistEvidence{s: s}, Assistant: s.chatProvider, Judge: s.chatProvider,
		Proposals: store, Usage: deps.Usage, Logger: s.logger,
	}
	if s.proposalApplier != nil {
		e.Applier = s.proposalApplier
	}
	// Architect consultation (WP7): the peer resolves LIVE from
	// a2a.peers at call time so a re-pointed destination is detected by
	// the consultant's pre-dispatch check; the audit sink is the CE
	// admin-audit repository (nil ⇒ every consult refused, test 38).
	e.Peer = &a2aPeer{s: s}
	e.Audit = s.adminAuditRepo
	s.configAssist = e
}

// a2aPeer adapts the configured architect peer (a2a.peers[<consult.peer>])
// to configassist.Peer over the shared outbound A2A client.
type a2aPeer struct {
	s      *Server
	client *a2aclient.Client
	once   sync.Once
}

func (p *a2aPeer) Name() string {
	if p.s.config == nil {
		return ""
	}
	return p.s.config.ConfigAssistant.Consult.Peer
}

func (p *a2aPeer) Ask(ctx context.Context, question string) (string, error) {
	if p.s.config == nil {
		return "", errors.New("no config")
	}
	name := p.s.config.ConfigAssistant.Consult.Peer
	peer, ok := p.s.config.A2A.Peers[name]
	if !ok {
		return "", fmt.Errorf("a2a.peers has no entry %q", name)
	}
	p.once.Do(func() { p.client = a2aclient.New() })
	res, err := p.client.Call(ctx, a2aclient.CallRequest{
		AgentURL: peer.URL, APIKey: peer.APIKey, Text: question,
		Metadata: map[string]any{config.ConsultHopHeader: 1},
		Timeout:  p.s.config.ConfigAssistant.Consult.EffectiveTimeout(),
	})
	if err != nil {
		return "", err
	}
	return res.Answer, nil
}

// ConfigAssistant returns the wired engine (nil when the entrypoint is closed)
// so the UI console entrypoint can share it.
func (s *Server) ConfigAssistant() *configassist.Engine { return s.configAssist }

// assistEvidence adapts the server's existing measurement surfaces to the
// assistant's Evidence contract (design §4.1: read-only, through the same
// implementations the operator sees).
type assistEvidence struct{ s *Server }

func (a assistEvidence) AutonomyHealth(ctx context.Context, projectID string) (any, bool, error) {
	if a.s.autonomyEvalRepo == nil || a.s.projectRegistry == nil {
		return nil, false, nil
	}
	p := a.s.projectRegistry.GetProject(projectID)
	if p == nil {
		return nil, false, nil
	}
	payload, err := a.s.autonomyHealthPayload(ctx, p, projectID, 24*7)
	if err != nil {
		return nil, false, err
	}
	return payload, true, nil
}

func (a assistEvidence) RatingsRollup(_ context.Context, _ string) (any, bool, error) {
	// The ratings rollup ships per SKILL (skill_rating_rollup.go) with its
	// counterfactual arms; a per-project rollup with arms does not exist
	// yet. Absent is reported as NOT MEASURED — never as a raw ratio
	// (design §4.1.1 rule 3).
	return nil, false, nil
}

func (a assistEvidence) QualityPercentiles(_ context.Context, _ string) (any, bool, error) {
	// No per-project percentile surface is exposed by the server today;
	// the honest answer is NOT MEASURED until one exists.
	return nil, false, nil
}

func (a assistEvidence) JudgeVerdicts(ctx context.Context, projectID string) (any, bool, error) {
	if a.s.verdictRepo == nil {
		return nil, false, nil
	}
	rows, err := a.s.verdictRepo.ListRecent(ctx, projectID, 50)
	if err != nil {
		return nil, false, err
	}
	if len(rows) == 0 {
		return nil, false, nil
	}
	counts := map[string]int{}
	byRole := map[string]map[string]int{}
	for _, v := range rows {
		counts[v.Verdict]++
		if byRole[v.Role] == nil {
			byRole[v.Role] = map[string]int{}
		}
		byRole[v.Role][v.Verdict]++
	}
	return map[string]any{"window": "last 50 verdicts", "counts": counts, "by_role": byRole, "note": "abstain is not a pass"}, true, nil
}

// assistRequest is the entrypoint's body.
type assistRequest struct {
	ProjectID      string `json:"projectId"`
	Intent         string `json:"intent"`
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
}

// OperatorAssist handles POST /api/v1/operator/assist.
func (s *Server) OperatorAssist(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use POST")
		return
	}
	if !s.requireOperatorCapability(w, r) {
		return
	}
	if s.configAssist == nil {
		respondError(w, http.StatusServiceUnavailable, "ASSISTANT_UNAVAILABLE", "configuration assistant not wired on this daemon (enable the config-assistant feature)")
		return
	}
	var body assistRequest
	limitJSONBody(w, r)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "INVALID_BODY", "expected {\"projectId\":...,\"intent\":...}: "+err.Error())
		return
	}
	body.ProjectID, body.Intent = strings.TrimSpace(body.ProjectID), strings.TrimSpace(body.Intent)
	if body.ProjectID == "" || body.Intent == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId and intent are required")
		return
	}
	entrypoint := configassist.EntrypointREST
	if r.Header.Get("X-Vornik-Client") == "vornikctl" {
		entrypoint = configassist.EntrypointCLI
	}
	req := configassist.Request{
		ProjectID: body.ProjectID, Intent: body.Intent, Entrypoint: entrypoint, IdempotencyKey: body.IdempotencyKey,
		RequestID: persistence.GenerateID("careq"), Actor: assistActor(r),
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	res, err := s.configAssist.Propose(ctx, req)
	if err != nil {
		var ref *configassist.Refusal
		if errors.As(err, &ref) {
			respondJSON(w, http.StatusOK, map[string]any{"refusal": ref, "request_id": req.RequestID})
			return
		}
		s.logger.Error().Err(err).Str("project_id", body.ProjectID).Msg("config-assistant: request failed")
		respondError(w, http.StatusInternalServerError, "ASSISTANT_FAILED", err.Error())
		return
	}
	status := http.StatusOK
	if res.Proposal != nil && !res.Duplicate {
		status = http.StatusCreated
	}
	respondJSON(w, status, res)
}

// assistActor derives the ledger actor from the request (review R2/R5):
// stable ids only, never a bearer secret.
func assistActor(r *http.Request) configassist.Actor {
	ctx := r.Context()
	if id := IdentityFromContext(ctx); id != nil && id.Extra != nil {
		if uid, _ := id.Extra[auth.ExtraSessionUserID].(string); uid != "" {
			return configassist.Actor{Kind: "human", AccountID: uid, CredentialID: "session:" + SessionIDFromContext(ctx), SourceID: "session:" + uid, Principal: "session:" + uid}
		}
	}
	if p := apiKeyPrincipalFromContext(ctx); p != "" {
		a := configassist.Actor{Kind: "credential", CredentialID: p, SourceID: p, Principal: p}
		if row, ok := extraDBKeyRow(IdentityFromContext(ctx)); ok && row.OwnerUserID != "" {
			a.Kind, a.AccountID = "human", row.OwnerUserID
		}
		return a
	}
	if !IsAuthEnabledFromContext(ctx) {
		return configassist.Actor{Kind: "credential", CredentialID: "auth-disabled", SourceID: "auth-disabled", Principal: "auth-disabled"}
	}
	return configassist.Actor{Kind: "credential", Principal: "unknown"}
}

// extraDBKeyRow is a nil-safe accessor for the matched DB key row.
func extraDBKeyRow(id *auth.Identity) (*persistence.APIKey, bool) {
	if id == nil || id.Extra == nil {
		return nil, false
	}
	row, ok := id.Extra[auth.ExtraDBKeyRow].(*persistence.APIKey)
	return row, ok && row != nil
}
