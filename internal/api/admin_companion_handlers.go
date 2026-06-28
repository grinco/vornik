package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/persistence"
)

// Companion-plugin admin surface (LLD 21).
//
//   POST /api/v1/admin/companion/grant   — mint a scoped bearer key
//   GET  /api/v1/admin/companion/keys    — list companion keys for a project
//
// Both gate through requireAdminGate (admin-key allowlist + admin.enabled
// check) — companion keys are an operator action, not a per-project
// self-service surface. The minted key never has admin scope itself;
// it's a regular DB-backed bearer with extra scope columns set.
//
// The grant handler returns the raw secret exactly once. Subsequent
// list calls (and the existing /api/v1/projects/{id}/keys list) return
// only the prefix. Operators capture the secret at grant time and feed
// it to the host LLM client's plugin store.

// knownCompanionClients enumerates the host-LLM clients we currently
// support. Kept as a closed set so a typo at grant time (e.g.
// "claude-cli" instead of "claude-code") fails loudly rather than
// silently producing a key that filters incorrectly in list views.
// Add new clients here when their plugin adapter ships.
var knownCompanionClients = map[string]bool{
	"claude-code": true,
	"codex":       true,
	"gemini-cli":  true,
	"opencode":    true,
}

// companionGrantRequest is the body shape for POST /admin/companion/grant.
//
// AllowedWorkflows is the most ergonomically dangerous field — an
// explicit empty array means "no workflows" (effectively a useless
// key); a missing field means "every workflow the project permits".
// We require non-empty IF the field is present, and reject duplicates
// at validation time so an operator review of the list isn't misled.
type companionGrantRequest struct {
	ProjectID        string     `json:"projectId"`
	SessionLabel     string     `json:"sessionLabel"`
	ClientKind       string     `json:"clientKind"`
	AllowedWorkflows []string   `json:"allowedWorkflows,omitempty"`
	BudgetCapUSD     *float64   `json:"budgetCapUsd,omitempty"`
	ExpiresAt        *time.Time `json:"expiresAt,omitempty"`
	// LLD 22 companion RAG capabilities. Default false; the CLI
	// surfaces --memory-read / --memory-write / --memory-all.
	MemoryRead  bool `json:"memoryRead,omitempty"`
	MemoryWrite bool `json:"memoryWrite,omitempty"`
	// DefaultRepoScope (migration 110) is the repo_scope the companion
	// MCP memory surface stamps on calls that omit it. Set it for
	// clients without a SessionStart scope injector (e.g. Codex) so
	// their deposits can't silently land NULL-scoped. Empty = no
	// default. CLI flag: --repo-scope.
	DefaultRepoScope string `json:"defaultRepoScope,omitempty"`
}

// companionGrantResponse carries the one-time-visible secret back.
// Mirrors the createAPIKeyResponse shape for visual consistency with
// the existing CLI, plus the companion-scope echo so operators can
// confirm the grant landed with the intended scope.
type companionGrantResponse struct {
	ID               string     `json:"id"`
	ProjectID        string     `json:"projectId"`
	SessionLabel     string     `json:"sessionLabel,omitempty"`
	ClientKind       string     `json:"clientKind"`
	Secret           string     `json:"secret"`
	KeyPrefix        string     `json:"keyPrefix"`
	AllowedWorkflows []string   `json:"allowedWorkflows,omitempty"`
	BudgetCapUSD     *float64   `json:"budgetCapUsd,omitempty"`
	CreatedAt        time.Time  `json:"createdAt"`
	ExpiresAt        *time.Time `json:"expiresAt,omitempty"`
	MemoryRead       bool       `json:"memoryRead,omitempty"`
	MemoryWrite      bool       `json:"memoryWrite,omitempty"`
	DefaultRepoScope string     `json:"defaultRepoScope,omitempty"`
}

// CompanionGrant handles POST /api/v1/admin/companion/grant. Mints a
// per-session bearer scoped to one project + an optional workflow
// allowlist + an optional USD budget cap. Returns the raw secret
// EXACTLY ONCE — the caller must capture it.
//
// Validation, in order (every failure surfaces a distinct error code
// so the CLI/UI can pinpoint the operator's mistake):
//   - clientKind is in the closed set of known clients
//   - projectId references a loaded project
//   - allowedWorkflows (if present) is non-empty + each ID resolves
//     in the project registry
//   - budgetCapUsd (if present) is > 0
//
// The handler is intentionally strict about workflow IDs even when
// the operator could omit the field. We'd rather fail at grant time
// than at the first delegate() call when the plugin user is mid-flow.
func (s *Server) CompanionGrant(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use POST")
		return
	}
	if !s.requireAdminGate(w, r) {
		return
	}
	if s.apiKeyRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "API_KEYS_DISABLED",
			"api-key surface not configured")
		return
	}

	body, err := readLimitedBody(w, r, 8192)
	if err != nil {
		respondError(w, http.StatusBadRequest, "READ_FAILED", err.Error())
		return
	}
	var req companionGrantRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			respondError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
			return
		}
	}
	req.ProjectID = strings.TrimSpace(req.ProjectID)
	req.ClientKind = strings.TrimSpace(req.ClientKind)
	req.SessionLabel = strings.TrimSpace(req.SessionLabel)
	req.DefaultRepoScope = strings.TrimSpace(req.DefaultRepoScope)
	// Bound the default scope to the same ceiling remember() enforces on
	// a caller-supplied repo_scope so a stored default can never exceed
	// what an explicit arg may carry.
	if len(req.DefaultRepoScope) > rememberMaxRepoScopeBytes {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			fmt.Sprintf("defaultRepoScope must be <= %d bytes", rememberMaxRepoScopeBytes))
		return
	}

	// Client kind first — cheap check, narrows further validation
	// to known shapes.
	if req.ClientKind == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "clientKind is required")
		return
	}
	if !knownCompanionClients[req.ClientKind] {
		respondError(w, http.StatusBadRequest, "UNKNOWN_CLIENT",
			"clientKind must be one of: claude-code, codex, gemini-cli, opencode")
		return
	}

	if req.ProjectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId is required")
		return
	}
	if s.projectRegistry == nil {
		respondError(w, http.StatusServiceUnavailable, "REGISTRY_UNAVAILABLE",
			"project registry not initialised")
		return
	}
	project := s.projectRegistry.GetProject(req.ProjectID)
	if project == nil {
		respondError(w, http.StatusNotFound, "PROJECT_NOT_FOUND",
			"project not found: "+req.ProjectID)
		return
	}

	// AllowedWorkflows — empty-but-present is rejected (footgun);
	// missing entirely means "no workflow filter, project default
	// applies". Each non-empty entry must resolve in the registry
	// so an operator typo doesn't produce a dead key.
	if req.AllowedWorkflows != nil {
		if len(req.AllowedWorkflows) == 0 {
			respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
				"allowedWorkflows is empty; omit the field entirely to allow all project workflows")
			return
		}
		seen := make(map[string]bool, len(req.AllowedWorkflows))
		for _, wfID := range req.AllowedWorkflows {
			wfID = strings.TrimSpace(wfID)
			if wfID == "" {
				respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
					"allowedWorkflows contains an empty entry")
				return
			}
			if seen[wfID] {
				respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
					"allowedWorkflows contains duplicate entry: "+wfID)
				return
			}
			seen[wfID] = true
			if s.projectRegistry.GetWorkflow(wfID) == nil {
				respondError(w, http.StatusBadRequest, "UNKNOWN_WORKFLOW",
					"workflow not found in registry: "+wfID)
				return
			}
		}
	}

	if req.BudgetCapUSD != nil && *req.BudgetCapUSD <= 0 {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"budgetCapUsd must be > 0 when set (omit the field for uncapped)")
		return
	}

	// Generate + persist. apikey.Generate rejects empty/hyphen-
	// problematic IDs, but the registry lookup above already
	// narrowed us to a real project, so this branch is purely
	// defensive.
	secret, err := apikey.Generate(req.ProjectID)
	if err != nil {
		respondError(w, http.StatusBadRequest, "INVALID_PROJECT",
			"project id is not key-compatible: "+err.Error())
		return
	}

	now := time.Now().UTC()
	name := req.SessionLabel
	if name == "" {
		// The DB column is NOT NULL on name; for a companion grant
		// where the operator didn't set a label we fall back to a
		// time-stamped one. Keeps existing list-view rendering
		// consistent without breaking the schema.
		name = "companion-" + req.ClientKind + "-" + now.Format("20060102-150405")
	}

	// LLD 22: memory_write implies memory_read so the caller can
	// always verify a deposit landed via recall(). Honour the
	// implication server-side too (the CLI also enforces it) so
	// future callers that POST directly can't end up in the
	// nonsensical write-without-read state.
	memoryRead := req.MemoryRead || req.MemoryWrite
	row := &persistence.APIKey{
		ID:               persistence.GenerateID("akey"),
		ProjectID:        req.ProjectID,
		Name:             name,
		KeyHash:          apikey.Hash(secret),
		KeyPrefix:        apikey.DisplayPrefix(secret),
		CreatedAt:        now,
		ExpiresAt:        req.ExpiresAt,
		CreatedBy:        callerForAudit(r),
		AllowedWorkflows: req.AllowedWorkflows,
		BudgetCapUSD:     req.BudgetCapUSD,
		ClientKind:       req.ClientKind,
		SessionLabel:     req.SessionLabel,
		DefaultRepoScope: req.DefaultRepoScope,
		MemoryRead:       memoryRead,
		MemoryWrite:      req.MemoryWrite,
	}
	if err := s.apiKeyRepo.Create(r.Context(), row); err != nil {
		s.logger.Warn().Err(err).
			Str("project", req.ProjectID).
			Str("client_kind", req.ClientKind).
			Msg("companion grant: db create failed")
		respondError(w, http.StatusInternalServerError, "DB_ERROR",
			"failed to create companion key")
		return
	}

	respondJSON(w, http.StatusCreated, companionGrantResponse{
		ID:               row.ID,
		ProjectID:        row.ProjectID,
		SessionLabel:     row.SessionLabel,
		ClientKind:       row.ClientKind,
		Secret:           secret,
		KeyPrefix:        row.KeyPrefix,
		AllowedWorkflows: row.AllowedWorkflows,
		BudgetCapUSD:     row.BudgetCapUSD,
		CreatedAt:        row.CreatedAt,
		ExpiresAt:        row.ExpiresAt,
		MemoryRead:       row.MemoryRead,
		MemoryWrite:      row.MemoryWrite,
		DefaultRepoScope: row.DefaultRepoScope,
	})
}

// companionKeyEntry is the per-row shape for the list endpoint.
// Mirrors apiKeyListEntry plus the companion-scope echo. Never
// includes the secret or the hash.
type companionKeyEntry struct {
	ID               string     `json:"id"`
	ProjectID        string     `json:"projectId"`
	SessionLabel     string     `json:"sessionLabel,omitempty"`
	ClientKind       string     `json:"clientKind"`
	KeyPrefix        string     `json:"keyPrefix"`
	AllowedWorkflows []string   `json:"allowedWorkflows,omitempty"`
	BudgetCapUSD     *float64   `json:"budgetCapUsd,omitempty"`
	DefaultRepoScope string     `json:"defaultRepoScope,omitempty"`
	CreatedAt        time.Time  `json:"createdAt"`
	LastUsedAt       *time.Time `json:"lastUsedAt,omitempty"`
	ExpiresAt        *time.Time `json:"expiresAt,omitempty"`
	RevokedAt        *time.Time `json:"revokedAt,omitempty"`
	CreatedBy        string     `json:"createdBy,omitempty"`
}

type companionKeyListResponse struct {
	Keys []companionKeyEntry `json:"keys"`
}

// CompanionKeysList handles GET /api/v1/admin/companion/keys.
// Required query param: projectId. Returns every companion-scoped
// key for the project (including revoked rows) newest-first.
//
// The existing `vornikctl key revoke` flow handles revocation
// uniformly across all api_keys rows — no companion-specific
// revoke endpoint needed.
func (s *Server) CompanionKeysList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
		return
	}
	if !s.requireAdminGate(w, r) {
		return
	}
	if s.apiKeyRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "API_KEYS_DISABLED",
			"api-key surface not configured")
		return
	}
	projectID := strings.TrimSpace(r.URL.Query().Get("projectId"))
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"projectId query parameter is required")
		return
	}
	keys, err := s.apiKeyRepo.ListCompanionByProject(r.Context(), projectID)
	if err != nil {
		s.logger.Warn().Err(err).Str("project", projectID).
			Msg("companion list: db query failed")
		respondError(w, http.StatusInternalServerError, "DB_ERROR",
			"failed to list companion keys")
		return
	}

	out := make([]companionKeyEntry, 0, len(keys))
	for _, k := range keys {
		if k == nil {
			continue
		}
		out = append(out, companionKeyEntry{
			ID:               k.ID,
			ProjectID:        k.ProjectID,
			SessionLabel:     k.SessionLabel,
			ClientKind:       k.ClientKind,
			KeyPrefix:        k.KeyPrefix,
			AllowedWorkflows: k.AllowedWorkflows,
			BudgetCapUSD:     k.BudgetCapUSD,
			DefaultRepoScope: k.DefaultRepoScope,
			CreatedAt:        k.CreatedAt,
			LastUsedAt:       k.LastUsedAt,
			ExpiresAt:        k.ExpiresAt,
			RevokedAt:        k.RevokedAt,
			CreatedBy:        k.CreatedBy,
		})
	}
	respondJSON(w, http.StatusOK, companionKeyListResponse{Keys: out})
}
