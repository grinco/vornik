package api

// Operator-profile REST surface for the `vornikctl operator`
// CLI + future external integrations. Mirrors the dispatcher
// tool's allow-list and audit semantics — the same key
// validation runs server-side regardless of caller.
//
//   GET    /api/v1/operators            — list
//   GET    /api/v1/operators/{id}       — show one
//   POST   /api/v1/operators/{id}       — set / unset a key
//                                          ({key, value, rationale})
//   DELETE /api/v1/operators/{id}       — forget (privacy revocation)
//
// Operator IDs carry a colon ("telegram:42", "webchat:abc") —
// safe in URL paths. The router uses everything after
// "/api/v1/operators/" as the operator id.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// operatorAllowedKeys mirrors the dispatcher's allow-list.
// Duplicated rather than imported to avoid the api package
// reaching into internal/dispatcher.
var operatorAllowedKeys = map[string]bool{
	"tone":                true,
	"verbosity":           true,
	"time_zone":           true,
	"communication_style": true,
	"preferred_channel":   true,
	"notes":               true,
}

// OperatorEntryJSON is the wire shape for one operator row.
type OperatorEntryJSON struct {
	OperatorID string `json:"operator_id"`
	Channel    string `json:"channel,omitempty"`
	Structured string `json:"structured"` // raw JSON object string
	Notes      string `json:"notes,omitempty"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

// OperatorListResponse wraps the list result.
type OperatorListResponse struct {
	Entries []OperatorEntryJSON `json:"entries"`
}

// OperatorShowResponse is the show endpoint's single-row body.
type OperatorShowResponse struct {
	Entry OperatorEntryJSON `json:"entry"`
}

// OperatorSetRequest is the body shape for POST /operators/{id}.
type OperatorSetRequest struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	Rationale string `json:"rationale"`
}

// OperatorForgetRequest is the body shape for DELETE
// /operators/{id}.
type OperatorForgetRequest struct {
	Rationale string `json:"rationale"`
}

// ListOperators handles GET /api/v1/operators.
func (s *Server) ListOperators(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	if !s.requireAdminGate(w, r) {
		return
	}
	if s.operatorProfileRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "OPERATORS_DISABLED",
			"operator profile repository not wired on this deployment")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := parseLimit(v, 1, 500); err == nil {
			limit = n
		}
	}
	rows, err := s.operatorProfileRepo.List(ctx, limit)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL", "list failed: "+err.Error())
		return
	}
	out := OperatorListResponse{Entries: make([]OperatorEntryJSON, 0, len(rows))}
	for _, row := range rows {
		if row == nil {
			continue
		}
		out.Entries = append(out.Entries, operatorRowToJSON(row))
	}
	respondJSON(w, http.StatusOK, out)
}

// ShowOperator handles GET /api/v1/operators/{id}.
func (s *Server) ShowOperator(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	if !s.requireAdminGate(w, r) {
		return
	}
	if s.operatorProfileRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "OPERATORS_DISABLED",
			"operator profile repository not wired on this deployment")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	row, err := s.operatorProfileRepo.Get(ctx, id)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "operator not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "INTERNAL", "lookup failed: "+err.Error())
		return
	}
	respondJSON(w, http.StatusOK, OperatorShowResponse{Entry: operatorRowToJSON(row)})
}

// SetOperatorKey handles POST /api/v1/operators/{id}. Same key
// allow-list + rationale requirement the dispatcher tool
// enforces — the security contract holds regardless of caller.
func (s *Server) SetOperatorKey(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST only")
		return
	}
	if !s.requireAdminGate(w, r) {
		return
	}
	if s.operatorProfileRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "OPERATORS_DISABLED",
			"operator profile repository not wired on this deployment")
		return
	}
	if id == "" {
		respondError(w, http.StatusBadRequest, "BAD_REQUEST", "operator id required")
		return
	}
	var req OperatorSetRequest
	body, err := readLimitedBody(w, r, 64*1024)
	if err != nil {
		respondError(w, http.StatusBadRequest, "READ_FAILED", err.Error())
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		respondError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid JSON body: "+err.Error())
		return
	}
	key := strings.TrimSpace(req.Key)
	rationale := strings.TrimSpace(req.Rationale)
	if key == "" {
		respondError(w, http.StatusBadRequest, "BAD_REQUEST", "'key' is required")
		return
	}
	if !operatorAllowedKeys[key] {
		respondError(w, http.StatusBadRequest, "BAD_REQUEST",
			fmt.Sprintf("key %q is not allow-listed", key))
		return
	}
	if rationale == "" {
		respondError(w, http.StatusBadRequest, "BAD_REQUEST",
			"'rationale' is required — every change carries an explanation for the audit log")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	current, err := s.operatorProfileRepo.Get(ctx, id)
	if err != nil && !errors.Is(err, persistence.ErrNotFound) {
		respondError(w, http.StatusInternalServerError, "INTERNAL", "lookup failed: "+err.Error())
		return
	}
	if current == nil {
		current = &persistence.OperatorProfile{OperatorID: id}
	}
	value := strings.TrimSpace(req.Value)
	if key == "notes" {
		current.Notes = value
	} else {
		structured := map[string]any{}
		if len(current.Structured) > 0 {
			_ = json.Unmarshal(current.Structured, &structured)
		}
		if value == "" {
			delete(structured, key)
		} else {
			structured[key] = value
		}
		raw, mErr := json.Marshal(structured)
		if mErr != nil {
			respondError(w, http.StatusInternalServerError, "INTERNAL", "marshal failed: "+mErr.Error())
			return
		}
		current.Structured = raw
	}
	if err := s.operatorProfileRepo.Upsert(ctx, current); err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL", "save failed: "+err.Error())
		return
	}
	// Audit row — source="api" so the audit panel + future
	// CLI-driven `vornikctl operator audit` can distinguish
	// CLI/api edits from dispatcher-tool writes (source=
	// "dispatcher") and UI form edits (source="ui").
	if s.adminAuditRepo != nil {
		afterJSON, _ := json.Marshal(map[string]string{
			"key": key, "value": value, "rationale": rationale,
		})
		_ = s.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
			Principal: id,
			Source:    "api",
			Action:    "operator_profile.updated",
			Target:    id,
			After:     string(afterJSON),
		})
	}
	respondJSON(w, http.StatusOK, OperatorShowResponse{Entry: operatorRowToJSON(current)})
}

// ForgetOperator handles DELETE /api/v1/operators/{id}.
// Privacy revocation: removes the row + writes an audit entry
// so the deletion remains traceable. Body is optional but
// supplying a rationale is encouraged.
func (s *Server) ForgetOperator(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodDelete {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "DELETE only")
		return
	}
	if !s.requireAdminGate(w, r) {
		return
	}
	if s.operatorProfileRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "OPERATORS_DISABLED",
			"operator profile repository not wired on this deployment")
		return
	}
	if id == "" {
		respondError(w, http.StatusBadRequest, "BAD_REQUEST", "operator id required")
		return
	}
	var req OperatorForgetRequest
	// Body is optional; ignore decode errors so a curl -X DELETE
	// with no payload still works. Still size-capped.
	limitJSONBody(w, r)
	_ = json.NewDecoder(r.Body).Decode(&req)
	rationale := strings.TrimSpace(req.Rationale)
	if rationale == "" {
		rationale = "operator removal via api"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.operatorProfileRepo.Delete(ctx, id); err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL", "delete failed: "+err.Error())
		return
	}
	// Also drop every identity-link row that resolves to this
	// canonical id so the speaker → operator mapping doesn't
	// outlive the profile it pointed at. Best-effort: failure
	// here is logged but doesn't fail the forget call (the
	// privacy revocation has already removed the data the
	// operator cares about).
	if s.operatorIdentityLinkRepo != nil {
		if err := s.operatorIdentityLinkRepo.DeleteAllForOperator(ctx, id); err != nil {
			s.logger.Warn().Err(err).Str("operator_id", id).
				Msg("operator forget: identity-link cleanup failed; rows may persist")
		}
	}
	if s.adminAuditRepo != nil {
		afterJSON, _ := json.Marshal(map[string]string{"rationale": rationale})
		_ = s.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
			Principal: id,
			Source:    "api",
			Action:    "operator_profile.forgotten",
			Target:    id,
			After:     string(afterJSON),
		})
	}
	respondJSON(w, http.StatusOK, map[string]string{"operator_id": id, "status": "forgotten"})
}

// operatorsRouter dispatches /api/v1/operators/{id} and the
// /links sub-path on the method dimension. Identity-link
// endpoints are handled by operator_links_handlers.go.
func (s *Server) operatorsRouter(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/operators/")
	if rest == "" {
		s.ListOperators(w, r)
		return
	}
	// Split into (id, suffix) on the first slash. Operator ids
	// themselves carry colons (`telegram:42`) so they're safe
	// path segments, but never contain "/" in the canonical
	// shape — anything after the first slash is the sub-path.
	id, suffix := rest, ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		id = rest[:i]
		suffix = rest[i+1:]
	}
	switch {
	case suffix == "":
		switch r.Method {
		case http.MethodGet:
			s.ShowOperator(w, r, id)
		case http.MethodPost:
			s.SetOperatorKey(w, r, id)
		case http.MethodDelete:
			s.ForgetOperator(w, r, id)
		default:
			respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED",
				"operator endpoint accepts GET / POST / DELETE")
		}
	case suffix == "links":
		switch r.Method {
		case http.MethodGet:
			s.ListOperatorLinks(w, r, id)
		case http.MethodPost:
			s.CreateOperatorLink(w, r, id)
		default:
			respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED",
				"links endpoint accepts GET / POST")
		}
	case suffix == "audit":
		if r.Method != http.MethodGet {
			respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED",
				"audit endpoint accepts GET only")
			return
		}
		s.ListOperatorAudit(w, r, id)
	case strings.HasPrefix(suffix, "links/"):
		channelSpeakerID := strings.TrimPrefix(suffix, "links/")
		if r.Method != http.MethodDelete {
			respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED",
				"links/{speaker} endpoint accepts DELETE only")
			return
		}
		s.DeleteOperatorLink(w, r, id, channelSpeakerID)
	default:
		respondError(w, http.StatusNotFound, "NOT_FOUND", "unknown operator path")
	}
}

// operatorRowToJSON converts a persistence row to the wire shape.
func operatorRowToJSON(row *persistence.OperatorProfile) OperatorEntryJSON {
	out := OperatorEntryJSON{
		OperatorID: row.OperatorID,
		Notes:      row.Notes,
		CreatedAt:  row.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:  row.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if i := strings.IndexByte(row.OperatorID, ':'); i > 0 {
		out.Channel = row.OperatorID[:i]
	}
	if len(row.Structured) > 0 {
		out.Structured = string(row.Structured)
	} else {
		out.Structured = "{}"
	}
	return out
}
