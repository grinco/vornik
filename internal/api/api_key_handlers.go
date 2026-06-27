package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/persistence"
)

// Endpoints:
//
//   POST   /api/v1/projects/{id}/keys              CreateAPIKey
//   GET    /api/v1/projects/{id}/keys              ListAPIKeys
//   POST   /api/v1/projects/{id}/keys/{kid}/rotate RotateAPIKey
//   DELETE /api/v1/projects/{id}/keys/{kid}        RevokeAPIKey
//
// All four sit behind AuthMiddleware + ProjectAuthMiddleware so
// the caller must already be authorised for the named project.
// During the static-keys deprecation window an operator with an
// "all projects" static key can manage any project's DB-backed
// keys; once static keys go away in 2026.8.0 only a DB-backed key
// already bound to that project will let the caller in.

// createAPIKeyRequest is the body shape for POST /keys.
//
// Name is operator-friendly and shown in the list view. ExpiresAt
// is optional; nil/zero means "no expiry". A future iteration will
// add rate-limit + scope fields here.
type createAPIKeyRequest struct {
	Name      string     `json:"name"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// createAPIKeyResponse carries the one-time-visible secret back to
// the caller. The secret is NEVER persisted in raw form (only the
// sha256 hex hash is) and NEVER logged. Subsequent List calls
// return KeyPrefix only.
type createAPIKeyResponse struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	ProjectID string     `json:"project_id"`
	Secret    string     `json:"secret"`
	KeyPrefix string     `json:"key_prefix"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// CreateAPIKey handles POST /api/v1/projects/{id}/keys.
// Returns the raw secret EXACTLY ONCE; the caller must capture it
// or lose access. Status: 201 Created.
func (s *Server) CreateAPIKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}
	if s.apiKeyRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "API_KEYS_DISABLED",
			"api-key surface not configured")
		return
	}
	projectID := extractProjectID(r)
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId is required")
		return
	}
	body, err := readLimitedBody(w, r, 4096)
	if err != nil {
		respondError(w, http.StatusBadRequest, "READ_FAILED", err.Error())
		return
	}
	var req createAPIKeyRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			respondError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
			return
		}
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "name is required")
		return
	}
	if len(req.Name) > 128 {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "name must be ≤ 128 characters")
		return
	}
	// Reserve the task-scoped key-name prefix for the container
	// scheduler's minter. CallMCPTool trusts any authenticated key
	// whose name carries this prefix to bind the caller to that task
	// ID; allowing an operator to mint a key named "agent:task_<X>"
	// would let it impersonate task X's agent (confused deputy).
	// Reject it at grant time (FIX 3).
	if _, reserved := persistence.TaskIDFromKeyName(req.Name); reserved {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"name must not use the reserved \""+persistence.TaskKeyNamePrefix+"\" prefix")
		return
	}

	secret, err := apikey.Generate(projectID)
	if err != nil {
		// Hyphenated project IDs and empty IDs are the two failure
		// modes; the project registry already rejects both, so this
		// is defense-in-depth.
		respondError(w, http.StatusBadRequest, "INVALID_PROJECT",
			"project id is not key-compatible: "+err.Error())
		return
	}
	row := &persistence.APIKey{
		ID:        persistence.GenerateID("akey"),
		ProjectID: projectID,
		Name:      req.Name,
		KeyHash:   apikey.Hash(secret),
		KeyPrefix: apikey.DisplayPrefix(secret),
		CreatedAt: time.Now().UTC(),
		ExpiresAt: req.ExpiresAt,
		CreatedBy: callerForAudit(r),
	}
	if err := s.apiKeyRepo.Create(r.Context(), row); err != nil {
		s.logger.Warn().Err(err).Str("project", projectID).
			Msg("api-key: create failed")
		respondError(w, http.StatusInternalServerError, "DB_ERROR",
			"failed to create api key")
		return
	}
	respondJSON(w, http.StatusCreated, createAPIKeyResponse{
		ID:        row.ID,
		Name:      row.Name,
		ProjectID: row.ProjectID,
		Secret:    secret,
		KeyPrefix: row.KeyPrefix,
		CreatedAt: row.CreatedAt,
		ExpiresAt: row.ExpiresAt,
	})
}

// listAPIKeysResponse is the GET response body shape. Each row
// omits the secret (not stored) and the hash (defense-in-depth:
// surfacing the hash would let an attacker who steals a backup
// brute-force candidate keys against it offline).
type listAPIKeysResponse struct {
	Keys []apiKeyListEntry `json:"keys"`
}

type apiKeyListEntry struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	KeyPrefix  string     `json:"key_prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	CreatedBy  string     `json:"created_by,omitempty"`
	// AllowedWorkflows surfaces the companion-scoped allowlist so
	// the `vornikctl key update --add-workflow / --remove-workflow`
	// flows can read the current state in one round-trip before
	// PUTting the mutated list. omitempty so non-companion keys
	// (no client_kind, no allowlist) render as before.
	AllowedWorkflows []string `json:"allowed_workflows,omitempty"`
	// AllowPush indicates whether this key may push via git-over-HTTPS.
	// Default false; enabled per-key by `vornikctl key update --allow-push`.
	// omitempty so keys that never had push enabled render cleanly.
	AllowPush bool `json:"allow_push,omitempty"`
}

// ListAPIKeys handles GET /api/v1/projects/{id}/keys.
// Returns every key for the project (including revoked rows;
// the UI renders them dimmed). Never returns the raw secret.
func (s *Server) ListAPIKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}
	if s.apiKeyRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "API_KEYS_DISABLED",
			"api-key surface not configured")
		return
	}
	projectID := extractProjectID(r)
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId is required")
		return
	}
	rows, err := s.apiKeyRepo.ListByProject(r.Context(), projectID)
	if err != nil {
		s.logger.Warn().Err(err).Str("project", projectID).
			Msg("api-key: list failed")
		respondError(w, http.StatusInternalServerError, "DB_ERROR",
			"failed to list api keys")
		return
	}
	out := listAPIKeysResponse{Keys: make([]apiKeyListEntry, 0, len(rows))}
	for _, k := range rows {
		out.Keys = append(out.Keys, apiKeyListEntry{
			ID:               k.ID,
			Name:             k.Name,
			KeyPrefix:        k.KeyPrefix,
			CreatedAt:        k.CreatedAt,
			LastUsedAt:       k.LastUsedAt,
			ExpiresAt:        k.ExpiresAt,
			RevokedAt:        k.RevokedAt,
			CreatedBy:        k.CreatedBy,
			AllowedWorkflows: k.AllowedWorkflows,
			AllowPush:        k.AllowPush,
		})
	}
	respondJSON(w, http.StatusOK, out)
}

// RotateAPIKey handles POST /api/v1/projects/{id}/keys/{kid}/rotate.
// Creates a new key with the same name + expiry, then revokes the
// old one. Returns the new secret EXACTLY ONCE. Atomic from the
// caller's perspective: the new key is usable before the old one
// is revoked, so a polling client doesn't see a window where
// neither key works.
//
// Open question per design doc: a grace period where both keys
// authenticate is deferred — for now rotation is "issue new,
// revoke old" with the gap being whatever the caller takes to
// swap configs.
func (s *Server) RotateAPIKey(w http.ResponseWriter, r *http.Request, keyID string) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}
	if s.apiKeyRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "API_KEYS_DISABLED",
			"api-key surface not configured")
		return
	}
	projectID := extractProjectID(r)
	if projectID == "" || keyID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId and keyId required")
		return
	}
	// Find the prior row by listing — small per-project N, no
	// LookupByID needed on the repo for the v1 surface. The
	// scoping to project_id provides the IDOR guard: an attacker
	// with a key for project A cannot rotate a key from project B
	// even with the keyID guessed.
	rows, err := s.apiKeyRepo.ListByProject(r.Context(), projectID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "DB_ERROR", "failed to list api keys")
		return
	}
	var prior *persistence.APIKey
	for _, k := range rows {
		if k.ID == keyID {
			prior = k
			break
		}
	}
	if prior == nil {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "api key not found in this project")
		return
	}
	if prior.RevokedAt != nil {
		respondError(w, http.StatusConflict, "ALREADY_REVOKED",
			"cannot rotate a revoked key")
		return
	}
	secret, err := apikey.Generate(projectID)
	if err != nil {
		respondError(w, http.StatusBadRequest, "INVALID_PROJECT",
			"project id is not key-compatible: "+err.Error())
		return
	}
	// Shared carry-over helper — keeps this path and the UI rotate path
	// from diverging on which columns survive a rotation (2026-06-27).
	fresh := prior.RotatedCopy(
		persistence.GenerateID("akey"),
		apikey.Hash(secret),
		apikey.DisplayPrefix(secret),
		callerForAudit(r),
		time.Now().UTC(),
	)
	if err := s.apiKeyRepo.Create(r.Context(), fresh); err != nil {
		s.logger.Warn().Err(err).Str("project", projectID).
			Msg("api-key: rotate-create failed")
		respondError(w, http.StatusInternalServerError, "DB_ERROR",
			"failed to mint rotated key")
		return
	}
	// New row is live; now revoke the old. If this step errors the
	// old key is still around (acceptable: caller has the new one
	// and can retry the revoke). Idempotent on the repo side.
	if err := s.apiKeyRepo.Revoke(r.Context(), prior.ID); err != nil {
		s.logger.Warn().Err(err).Str("project", projectID).Str("key", prior.ID).
			Msg("api-key: rotate-revoke failed (new key already issued)")
	}
	respondJSON(w, http.StatusCreated, createAPIKeyResponse{
		ID:        fresh.ID,
		Name:      fresh.Name,
		ProjectID: fresh.ProjectID,
		Secret:    secret,
		KeyPrefix: fresh.KeyPrefix,
		CreatedAt: fresh.CreatedAt,
		ExpiresAt: fresh.ExpiresAt,
	})
}

// RevokeAPIKey handles DELETE /api/v1/projects/{id}/keys/{kid}.
// Idempotent — revoking an already-revoked key returns 204 too.
func (s *Server) RevokeAPIKey(w http.ResponseWriter, r *http.Request, keyID string) {
	if r.Method != http.MethodDelete {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}
	if s.apiKeyRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "API_KEYS_DISABLED",
			"api-key surface not configured")
		return
	}
	projectID := extractProjectID(r)
	if projectID == "" || keyID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId and keyId required")
		return
	}
	// IDOR guard: verify the key belongs to this project before
	// revoking. The repo's Revoke is keyed only on `id` (UNIQUE)
	// so without this check an authenticated caller could revoke
	// any key in any project given the keyID. List-scoped check
	// keeps the surface IDOR-safe.
	rows, err := s.apiKeyRepo.ListByProject(r.Context(), projectID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "DB_ERROR", "failed to list api keys")
		return
	}
	found := false
	for _, k := range rows {
		if k.ID == keyID {
			found = true
			break
		}
	}
	if !found {
		// Don't disambiguate "wrong project" from "doesn't exist" —
		// both return 404.
		respondError(w, http.StatusNotFound, "NOT_FOUND", "api key not found in this project")
		return
	}
	if err := s.apiKeyRepo.Revoke(r.Context(), keyID); err != nil {
		s.logger.Warn().Err(err).Str("project", projectID).Str("key", keyID).
			Msg("api-key: revoke failed")
		respondError(w, http.StatusInternalServerError, "DB_ERROR",
			"failed to revoke api key")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// updateAllowedWorkflowsRequest is the body shape for
// PUT /api/v1/projects/{id}/keys/{kid}/workflows. Empty list means
// "every workflow the project permits" (matches the nullable column
// convention) — passing nil and passing [] produce the same row.
type updateAllowedWorkflowsRequest struct {
	AllowedWorkflows []string `json:"allowed_workflows"`
}

const (
	maxAPIKeyAllowedWorkflows    = 64
	maxAPIKeyAllowedWorkflowName = 128
)

func normalizeAPIKeyAllowedWorkflows(in []string) ([]string, string) {
	if len(in) == 0 {
		return []string{}, ""
	}
	if len(in) > maxAPIKeyAllowedWorkflows {
		return nil, "allowed_workflows must contain at most 64 entries"
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		name := strings.TrimSpace(raw)
		if name == "" {
			return nil, "allowed_workflows entries must be non-empty"
		}
		if len(name) > maxAPIKeyAllowedWorkflowName {
			return nil, "allowed_workflows entries must be ≤ 128 characters"
		}
		if _, dup := seen[name]; dup {
			return nil, "allowed_workflows entries must be unique"
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out, ""
}

// UpdateAPIKeyAllowedWorkflows handles PUT /api/v1/projects/{id}/keys/{kid}/workflows.
// Replaces the key's allowed_workflows list wholesale. Add / remove
// semantics live in the CLI (`vornikctl key update --add-workflow`
// fetches → mutates → PUTs) so the HTTP surface stays simple and
// race-free at the handler boundary — concurrent PUTs serialise at
// the DB UPDATE, last write wins.
func (s *Server) UpdateAPIKeyAllowedWorkflows(w http.ResponseWriter, r *http.Request, keyID string) {
	if r.Method != http.MethodPut {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}
	if s.apiKeyRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "API_KEYS_DISABLED",
			"api-key surface not configured")
		return
	}
	projectID := extractProjectID(r)
	if projectID == "" || keyID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId and keyId required")
		return
	}
	var req updateAllowedWorkflowsRequest
	limitJSONBody(w, r)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"body must be {\"allowed_workflows\": [...]}: "+err.Error())
		return
	}
	// IDOR guard mirrors RevokeAPIKey: verify the key belongs to
	// this project before touching it. UpdateAllowedWorkflows is
	// keyed only on id (UNIQUE) so without this check an
	// authenticated caller could rewrite any project's key's
	// allowlist given just the keyID.
	rows, err := s.apiKeyRepo.ListByProject(r.Context(), projectID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "DB_ERROR", "failed to list api keys")
		return
	}
	var current *persistence.APIKey
	for _, k := range rows {
		if k.ID == keyID {
			current = k
			break
		}
	}
	if current == nil {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "api key not found in this project")
		return
	}
	if current.RevokedAt != nil {
		respondError(w, http.StatusConflict, "ALREADY_REVOKED",
			"cannot update workflows for a revoked key")
		return
	}
	allowed, validationErr := normalizeAPIKeyAllowedWorkflows(req.AllowedWorkflows)
	if validationErr != "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", validationErr)
		return
	}
	if err := s.apiKeyRepo.UpdateAllowedWorkflows(r.Context(), keyID, allowed); err != nil {
		s.logger.Warn().Err(err).Str("project", projectID).Str("key", keyID).
			Msg("api-key: update allowed_workflows failed")
		respondError(w, http.StatusInternalServerError, "DB_ERROR",
			"failed to update allowed workflows")
		return
	}
	// Return the updated view: the existing apiKeyListEntry plus the
	// new allowed_workflows slice so the CLI can confirm the final
	// state in one round-trip. No secret — that's only ever returned
	// by create / rotate.
	respondJSON(w, http.StatusOK, struct {
		apiKeyListEntry
		AllowedWorkflows []string `json:"allowed_workflows"`
	}{
		apiKeyListEntry: apiKeyListEntry{
			ID:         current.ID,
			Name:       current.Name,
			KeyPrefix:  current.KeyPrefix,
			CreatedAt:  current.CreatedAt,
			LastUsedAt: current.LastUsedAt,
			ExpiresAt:  current.ExpiresAt,
			RevokedAt:  current.RevokedAt,
			CreatedBy:  current.CreatedBy,
		},
		AllowedWorkflows: allowed,
	})
}

// updateAllowPushRequest is the body shape for
// PUT /api/v1/projects/{id}/keys/{kid}/allow-push.
type updateAllowPushRequest struct {
	AllowPush bool `json:"allow_push"`
}

// UpdateAllowPushHandler handles PUT /api/v1/projects/{id}/keys/{kid}/allow-push.
// Flips the allow_push capability on a single key. The key must belong
// to the named project (IDOR guard identical to RevokeAPIKey / UpdateAPIKeyAllowedWorkflows —
// 404 on cross-project or missing key). Maps ErrAPIKeyNotFound→404.
// Returns the updated key record (no secret).
func (s *Server) UpdateAllowPushHandler(w http.ResponseWriter, r *http.Request, keyID string) {
	if r.Method != http.MethodPut {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}
	if s.apiKeyRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "API_KEYS_DISABLED",
			"api-key surface not configured")
		return
	}
	projectID := extractProjectID(r)
	if projectID == "" || keyID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId and keyId required")
		return
	}
	var req updateAllowPushRequest
	limitJSONBody(w, r)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"body must be {\"allow_push\": true|false}: "+err.Error())
		return
	}
	// IDOR guard mirrors RevokeAPIKey + UpdateAPIKeyAllowedWorkflows:
	// verify the key belongs to this project before touching it.
	// UpdateAllowPush is keyed only on id (UNIQUE) so without this
	// check an authenticated caller could flip any project's key given
	// just the keyID.
	rows, err := s.apiKeyRepo.ListByProject(r.Context(), projectID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "DB_ERROR", "failed to list api keys")
		return
	}
	var current *persistence.APIKey
	for _, k := range rows {
		if k.ID == keyID {
			current = k
			break
		}
	}
	if current == nil {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "api key not found in this project")
		return
	}
	if current.RevokedAt != nil {
		respondError(w, http.StatusConflict, "ALREADY_REVOKED",
			"cannot update allow_push for a revoked key")
		return
	}
	if err := s.apiKeyRepo.UpdateAllowPush(r.Context(), keyID, req.AllowPush); err != nil {
		s.logger.Warn().Err(err).Str("project", projectID).Str("key", keyID).
			Msg("api-key: update allow_push failed")
		respondError(w, http.StatusInternalServerError, "DB_ERROR",
			"failed to update allow_push")
		return
	}
	// Return the updated view: same shape as apiKeyListEntry but with
	// AllowPush reflected. No secret — only create/rotate return that.
	respondJSON(w, http.StatusOK, apiKeyListEntry{
		ID:               current.ID,
		Name:             current.Name,
		KeyPrefix:        current.KeyPrefix,
		CreatedAt:        current.CreatedAt,
		LastUsedAt:       current.LastUsedAt,
		ExpiresAt:        current.ExpiresAt,
		RevokedAt:        current.RevokedAt,
		CreatedBy:        current.CreatedBy,
		AllowedWorkflows: current.AllowedWorkflows,
		AllowPush:        req.AllowPush,
	})
}

// callerForAudit returns a short string identifying who issued
// the request, for the api_keys.created_by audit column. The
// auth middleware stashes the API key ID under apiKeyIDKey when
// a DB-backed key was used; we surface a short fingerprint of
// that here. Static-key callers don't have an ID — they get the
// literal "static" so the audit trail still distinguishes the
// two paths.
func callerForAudit(r *http.Request) string {
	if id, ok := r.Context().Value(apiKeyIDKey).(string); ok && id != "" {
		return id
	}
	return "static"
}

// SplitTrailingActionPath helps the dispatcher in
// apiV1ProjectsHandler parse `/keys/{kid}/<action>` URLs. Returns
// (keyID, action, ok). action is empty when the URL is just
// /keys/{kid}.
func splitKeyActionPath(remaining string) (keyID, action string, ok bool) {
	// remaining is the part AFTER /api/v1/projects/{p} —
	// typically "/keys/<kid>" or "/keys/<kid>/<action>".
	if !strings.HasPrefix(remaining, "/keys/") {
		return "", "", false
	}
	rest := strings.TrimPrefix(remaining, "/keys/")
	rest = strings.TrimSuffix(rest, "/")
	if rest == "" {
		return "", "", false
	}
	// Reject any "/keys/" lookalike with more than one further
	// segment after the action — keeps the surface from
	// silently accepting `/keys/<kid>/rotate/extra`.
	parts := strings.Split(rest, "/")
	switch len(parts) {
	case 1:
		return parts[0], "", true
	case 2:
		return parts[0], parts[1], true
	}
	return "", "", false
}
