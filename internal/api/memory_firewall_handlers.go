package api

// Policy-Aware Memory Firewall admin endpoints. Same admin
// gate matrix as /admin/audit (admin.enabled + admin-key
// allowlist).
//
//   GET  /api/v1/admin/memory/policy/evaluations
//   GET  /api/v1/admin/memory/policy/mode
//
// LLD: https://docs.vornik.io
// § "API".
//
// v1 scope: read-only operator surfaces (recent evaluations +
// current daemon enforcement mode). Per-chunk policy mutation
// (POST /policy/chunks/{id}) is deferred — it needs the
// "policy_digest recompute on edit" path which is bigger than
// the rest of Phase C combined.

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/memoryfirewall"
	"vornik.io/vornik/internal/persistence"
)

// WithMemoryPolicyEvaluations wires the audit repo behind the
// firewall admin endpoints. Nil keeps them at 503 with a
// "not configured" message.
func WithMemoryPolicyEvaluations(repo persistence.MemoryPolicyEvaluationRepository) ServerOption {
	return func(s *Server) {
		s.memoryPolicyEvaluations = repo
	}
}

// MemoryFirewallEditor is the narrow surface the
// POST /api/v1/admin/memory/policy/chunks/{id} handler needs
// from the memory subsystem. Lets the handler load + mutate
// per-chunk policy without dragging the full memory.Repository
// into the API package's import surface.
type MemoryFirewallEditor interface {
	LoadChunkPolicies(ctx context.Context, chunkIDs []string) (map[string]ChunkPolicyRow, error)
	UpdateChunkPolicy(ctx context.Context, row ChunkPolicyRow) (int64, error)
}

// ChunkPolicyRow mirrors memory.ChunkPolicyRow at the api
// package boundary so handlers + the editor interface don't
// depend on internal/memory. Field-for-field copy.
type ChunkPolicyRow struct {
	ChunkID            string
	TenantID           string
	SensitivityTier    string
	ProvenanceSource   string
	ProvenanceProducer string
	ProvenanceTrust    int
	ProvenanceURL      string
	FirewallExpiresAt  *time.Time
	PermittedRoles     []string
	AllowedPurposes    []string
	PolicyDigest       string
	ContentClass       string
	ValidationStatus   string
}

// WithMemoryFirewallEditor wires the per-chunk policy editor
// behind POST .../policy/chunks/{id}. Nil → endpoint returns
// 503 NOT_CONFIGURED so deployments that haven't migrated to
// the firewall don't expose a half-baked surface.
func WithMemoryFirewallEditor(ed MemoryFirewallEditor) ServerOption {
	return func(s *Server) {
		s.memoryFirewallEditor = ed
	}
}

// WithMemoryFirewallMode supplies the current enforcement mode
// so the GET /policy/mode endpoint can report it. Optional —
// when unset the endpoint returns "unknown".
func WithMemoryFirewallMode(mode memoryfirewall.EnforcementMode) ServerOption {
	return func(s *Server) {
		s.memoryFirewallMode = string(mode)
	}
}

// WithMemoryFirewallProjectModeFn supplies a resolver for per-
// project enforcement-mode overrides. Same shape as the
// Searcher's FirewallDeps.ModeForProject; production wires both
// to the same Container helper so the API + recall path stay
// consistent. Nil keeps the /policy/mode endpoint at
// daemon-level reporting (Phase C semantics).
func WithMemoryFirewallProjectModeFn(fn func(projectID string) (memoryfirewall.EnforcementMode, bool)) ServerOption {
	return func(s *Server) {
		s.memoryFirewallProjectModeFn = fn
	}
}

// AdminMemoryFirewallEvaluations handles
// GET /api/v1/admin/memory/policy/evaluations?project_id=X&decision=Y&since=YYYY-MM-DD&limit=N.
//
// 503 BLACKBOX_DISABLED  — repo not wired.
// 400 BAD_REQUEST       — required project_id missing.
// 200 + list + count    — paged evaluation rows, newest first.
func (s *Server) AdminMemoryFirewallEvaluations(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminGate(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	if s.memoryPolicyEvaluations == nil {
		respondError(w, http.StatusServiceUnavailable, "FIREWALL_DISABLED",
			"memory firewall audit repo not wired on this deployment")
		return
	}
	projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "BAD_REQUEST", "project_id query param required")
		return
	}
	decision := strings.TrimSpace(r.URL.Query().Get("decision"))
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	since := time.Now().Add(-7 * 24 * time.Hour) // default to last 7 days
	if s := r.URL.Query().Get("since"); s != "" {
		// Accept either RFC3339 or YYYY-MM-DD (the vornikctl
		// CLI convention).
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			since = t
		} else if t, err := time.Parse("2006-01-02", s); err == nil {
			since = t
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	rows, err := s.memoryPolicyEvaluations.ListRecent(ctx, projectID, decision, since, limit)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL", "list evaluations: "+err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"evaluations": rows,
		"count":       len(rows),
		"filters": map[string]any{
			"project_id": projectID,
			"decision":   decision,
			"since":      since.Format(time.RFC3339),
			"limit":      limit,
		},
	})
}

// AdminMemoryFirewallEvaluationsByDigest handles
// GET /api/v1/admin/memory/policy/evaluations/digest/{policy_digest}?limit=N.
//
// Returns every evaluation row recorded under a policy digest — the
// proof-verifier surface from the firewall LLD § REST endpoints
// ("show me everyone who saw chunk X under policy revision A"). This
// route 404'd before the 2026-05-29 drift fix (§8.3): documented but
// never registered.
//
// 503 FIREWALL_DISABLED — repo not wired.
// 400 BAD_REQUEST       — empty digest in path.
// 200 + list + count    — evaluation rows for the digest, newest first.
func (s *Server) AdminMemoryFirewallEvaluationsByDigest(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminGate(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	if s.memoryPolicyEvaluations == nil {
		respondError(w, http.StatusServiceUnavailable, "FIREWALL_DISABLED",
			"memory firewall audit repo not wired on this deployment")
		return
	}
	const prefix = "/api/v1/admin/memory/policy/evaluations/digest/"
	digest := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, prefix))
	if digest == "" || strings.Contains(digest, "/") {
		respondError(w, http.StatusBadRequest, "BAD_REQUEST",
			"policy_digest required: GET /api/v1/admin/memory/policy/evaluations/digest/{policy_digest}")
		return
	}
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	rows, err := s.memoryPolicyEvaluations.ListByDigest(ctx, digest, limit)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL", "list evaluations by digest: "+err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"evaluations":   rows,
		"count":         len(rows),
		"policy_digest": digest,
		"limit":         limit,
	})
}

// chunkPolicyUpdateRequest is the JSON body for
// POST /api/v1/admin/memory/policy/chunks/{id}. All fields are
// optional pointers — only non-nil fields are applied. Allows
// callers to update a single dimension without re-supplying
// the others.
type chunkPolicyUpdateRequest struct {
	TenantID           *string    `json:"tenant_id,omitempty"`
	SensitivityTier    *string    `json:"sensitivity_tier,omitempty"`
	ProvenanceSource   *string    `json:"provenance_source,omitempty"`
	ProvenanceProducer *string    `json:"provenance_producer,omitempty"`
	ProvenanceTrust    *int       `json:"provenance_trust,omitempty"`
	ProvenanceURL      *string    `json:"provenance_url,omitempty"`
	FirewallExpiresAt  *time.Time `json:"firewall_expires_at,omitempty"`
	// PermittedRoles + AllowedPurposes: empty slice = "clear
	// the list" (deny-all under strict-enforce); nil = "leave
	// untouched". Callers distinguish via JSON null vs [].
	PermittedRoles  *[]string `json:"permitted_roles,omitempty"`
	AllowedPurposes *[]string `json:"allowed_purposes,omitempty"`
}

// chunkPolicyUpdateResponse carries the freshly-stored policy
// + the new digest. Lets the caller verify the round-trip
// without a separate GET.
type chunkPolicyUpdateResponse struct {
	ChunkID      string         `json:"chunk_id"`
	PolicyDigest string         `json:"policy_digest"`
	Policy       ChunkPolicyRow `json:"policy"`
	AuditEntry   string         `json:"audit_entry_id,omitempty"`
}

// AdminMemoryFirewallChunkPolicy handles
// POST /api/v1/admin/memory/policy/chunks/{id}. Mutates the
// per-chunk policy columns + recomputes policy_digest +
// emits an admin_audit row tagged with the operator-principal.
//
// 503 FIREWALL_DISABLED — editor not wired.
// 400 BAD_REQUEST       — malformed JSON or empty path.
// 404 CHUNK_NOT_FOUND   — UpdateChunkPolicy affected 0 rows.
// 200 + new policy      — success.
func (s *Server) AdminMemoryFirewallChunkPolicy(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminGate(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST only")
		return
	}
	if s.memoryFirewallEditor == nil {
		respondError(w, http.StatusServiceUnavailable, "FIREWALL_DISABLED",
			"memory firewall editor not wired on this deployment")
		return
	}
	const prefix = "/api/v1/admin/memory/policy/chunks/"
	chunkID := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, prefix))
	if chunkID == "" || strings.Contains(chunkID, "/") {
		respondError(w, http.StatusBadRequest, "BAD_REQUEST",
			"chunk_id required: POST /api/v1/admin/memory/policy/chunks/{chunk_id}")
		return
	}
	body, err := readLimitedBody(w, r, 1<<15) // 32 KiB cap; policy bodies are tiny
	if err != nil {
		return
	}
	var req chunkPolicyUpdateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		respondError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid JSON: "+err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// Load existing — required so the merge starts from the
	// stored state, not an empty Policy. Missing chunk = 404.
	existing, err := s.memoryFirewallEditor.LoadChunkPolicies(ctx, []string{chunkID})
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL", "load chunk policy: "+err.Error())
		return
	}
	row, ok := existing[chunkID]
	if !ok {
		respondError(w, http.StatusNotFound, "CHUNK_NOT_FOUND", "chunk not found: "+chunkID)
		return
	}
	row.ChunkID = chunkID

	// Apply partial update. Nil pointer = leave untouched;
	// non-nil pointer = replace (including with empty value).
	if req.TenantID != nil {
		row.TenantID = *req.TenantID
	}
	if req.SensitivityTier != nil {
		row.SensitivityTier = *req.SensitivityTier
	}
	if req.ProvenanceSource != nil {
		row.ProvenanceSource = *req.ProvenanceSource
	}
	if req.ProvenanceProducer != nil {
		row.ProvenanceProducer = *req.ProvenanceProducer
	}
	if req.ProvenanceTrust != nil {
		row.ProvenanceTrust = *req.ProvenanceTrust
	}
	if req.ProvenanceURL != nil {
		row.ProvenanceURL = *req.ProvenanceURL
	}
	if req.FirewallExpiresAt != nil {
		t := *req.FirewallExpiresAt
		row.FirewallExpiresAt = &t
	}
	if req.PermittedRoles != nil {
		row.PermittedRoles = *req.PermittedRoles
	}
	if req.AllowedPurposes != nil {
		row.AllowedPurposes = *req.AllowedPurposes
	}

	// Recompute digest from the merged row. Build a
	// memoryfirewall.Policy from the row's fields so we get
	// the same canonicalisation the evaluator uses.
	policy := policyFromChunkRow(row)
	row.PolicyDigest = memoryfirewall.PolicyDigest(policy)

	rows, err := s.memoryFirewallEditor.UpdateChunkPolicy(ctx, row)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL", "update chunk policy: "+err.Error())
		return
	}
	if rows == 0 {
		respondError(w, http.StatusNotFound, "CHUNK_NOT_FOUND", "chunk not found after merge")
		return
	}

	// Best-effort admin-audit row. Failure logs but doesn't
	// surface (the policy edit succeeded — the audit is the
	// compliance trail, not the action).
	auditID := ""
	if s.adminAuditRepo != nil {
		principal := apiKeyPrincipalFromContext(r.Context())
		if principal == "" {
			principal = "anonymous-admin"
		}
		entry := &persistence.AdminAuditEntry{
			Principal: principal,
			Source:    "api",
			Action:    "memoryfirewall.policy.update",
			Target:    chunkID,
			After:     row.PolicyDigest,
			IP:        clientIPFromRequest(r),
			UserAgent: r.UserAgent(),
		}
		if err := s.adminAuditRepo.Insert(ctx, entry); err == nil {
			auditID = entry.ID
		}
	}

	respondJSON(w, http.StatusOK, chunkPolicyUpdateResponse{
		ChunkID:      chunkID,
		PolicyDigest: row.PolicyDigest,
		Policy:       row,
		AuditEntry:   auditID,
	})
}

// policyFromChunkRow converts the wire-side ChunkPolicyRow back
// into a memoryfirewall.Policy for digest computation. Mirrors
// chunkFromPolicyRow in internal/memory/firewall_recall.go but
// stays in the api package to avoid the memory import.
func policyFromChunkRow(row ChunkPolicyRow) memoryfirewall.Policy {
	purposes := make([]memoryfirewall.Purpose, 0, len(row.AllowedPurposes))
	for _, p := range row.AllowedPurposes {
		purposes = append(purposes, memoryfirewall.Purpose(p))
	}
	return memoryfirewall.Policy{
		Provenance: memoryfirewall.Provenance{
			Source:     memoryfirewall.ProvenanceSource(row.ProvenanceSource),
			ProducerID: row.ProvenanceProducer,
			TrustLevel: row.ProvenanceTrust,
			SourceURL:  row.ProvenanceURL,
		},
		Sensitivity:     memoryfirewall.SensitivityTier(row.SensitivityTier),
		ExpiresAt:       row.FirewallExpiresAt,
		TenantID:        row.TenantID,
		PermittedRoles:  row.PermittedRoles,
		AllowedPurposes: purposes,
	}
}

// AdminMemoryFirewallEvaluationsCSV handles
// GET /api/v1/admin/memory/policy/evaluations.csv.
// Same query params as the JSON variant; streams the rows as
// RFC 4180 CSV with a header line. Compliance reviewers
// running spreadsheet workflows want this shape; the JSON
// variant stays the canonical operator surface.
//
// Default window 30 days (vs 7 for JSON) since CSV exports
// usually drive monthly compliance reports.
func (s *Server) AdminMemoryFirewallEvaluationsCSV(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminGate(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	if s.memoryPolicyEvaluations == nil {
		respondError(w, http.StatusServiceUnavailable, "FIREWALL_DISABLED",
			"memory firewall audit repo not wired on this deployment")
		return
	}
	projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "BAD_REQUEST", "project_id query param required")
		return
	}
	decision := strings.TrimSpace(r.URL.Query().Get("decision"))
	// CSV export defaults to a larger window than the JSON
	// variant (operators pulling a compliance report want
	// "the last month", not "the last week").
	limit := 1000
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 10000 {
			limit = n
		}
	}
	since := time.Now().Add(-30 * 24 * time.Hour)
	if s := r.URL.Query().Get("since"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			since = t
		} else if t, err := time.Parse("2006-01-02", s); err == nil {
			since = t
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	rows, err := s.memoryPolicyEvaluations.ListRecent(ctx, projectID, decision, since, limit)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL", "list evaluations: "+err.Error())
		return
	}

	filename := fmt.Sprintf("memory_policy_evaluations_%s_%s.csv",
		projectID, time.Now().UTC().Format("20060102T150405Z"))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.WriteHeader(http.StatusOK)

	cw := csv.NewWriter(w)
	defer cw.Flush()

	// Header row — stable column order; downstream
	// spreadsheet templates rely on it.
	_ = cw.Write([]string{
		"evaluated_at",
		"project_id",
		"tenant_id",
		"chunk_id",
		"decision",
		"policy_digest",
		"request_role",
		"request_purpose",
		"request_operator",
		"trace_id",
		"reason_detail",
	})
	for _, row := range rows {
		_ = cw.Write([]string{
			row.EvaluatedAt.UTC().Format(time.RFC3339),
			row.ProjectID,
			row.TenantID,
			row.ChunkID,
			string(row.Decision),
			row.PolicyDigest,
			row.RequestRole,
			row.RequestPurpose,
			row.RequestOperator,
			row.TraceID,
			row.ReasonDetail,
		})
	}
}

// AdminMemoryFirewallMode handles GET /api/v1/admin/memory/policy/mode.
// Returns the daemon's current enforcement mode + a short
// explanation of what each mode means. When ?project_id= is
// supplied, also reports the per-project override (Phase D
// follow-on, 2026.5.9). The operator UI uses this to render a
// status chip on the firewall landing page.
func (s *Server) AdminMemoryFirewallMode(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminGate(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	daemon := s.memoryFirewallMode
	if daemon == "" {
		daemon = "unknown"
	}
	resp := map[string]any{
		"mode":        daemon,
		"daemon_mode": daemon,
		"description_by_mode": map[string]string{
			"off":      "Firewall evaluates + audits, but blocked chunks still surface in recall results.",
			"advisory": "Firewall evaluates + audits, blocked chunks surface with a PolicyWarning.",
			"enforce":  "Firewall evaluates + audits, blocked chunks do NOT surface in recall results.",
		},
		"note": "Per-project Firewall.Mode in the project YAML overrides the daemon default. Empty / unset = inherit.",
	}
	// Optional per-project lookup. Resolver lives behind a
	// narrow interface so the api package doesn't pull in
	// the memory or registry packages.
	if projectID := strings.TrimSpace(r.URL.Query().Get("project_id")); projectID != "" && s.memoryFirewallProjectModeFn != nil {
		if mode, ok := s.memoryFirewallProjectModeFn(projectID); ok {
			resp["project_id"] = projectID
			resp["project_mode"] = string(mode)
			resp["mode"] = string(mode) // effective mode for this project
		} else {
			resp["project_id"] = projectID
			resp["project_mode"] = "" // empty = inherit
		}
	}
	respondJSON(w, http.StatusOK, resp)
}
