package api

// Project archive / unarchive / delete-now REST surface. Mirrors
// the UI handlers in internal/ui/project_archive.go but with a
// JSON in/out contract for GitOps tooling and the vornikctl CLI.
//
//   POST /api/v1/projects/{id}/archive
//     body: {"grace":"7d","reason":"end of contract"}
//     resp: {"status":"archived","scheduled_delete_at":"...",
//            "archived_at":"...","reason":"...","archived_by":"..."}
//
//   POST /api/v1/projects/{id}/unarchive
//     body: {}
//     resp: {"status":"active"}
//
//   POST /api/v1/projects/{id}/delete-now
//     body: {}
//     resp: {"status":"archived","scheduled_delete_at":"<~now>"}
//
// All three share the projectarchive.LifecycleService — same
// YAML mutation, reload, and audit shape the UI emits.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/projectarchive"
	"vornik.io/vornik/internal/registry"
)

// ArchiveRequest is the wire shape for POST /archive.
type ArchiveRequest struct {
	Grace  string `json:"grace,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// ArchiveResponse wraps the resulting lifecycle snapshot.
type ArchiveResponse struct {
	ProjectID         string `json:"project_id"`
	Status            string `json:"status"`
	ArchivedAt        string `json:"archived_at,omitempty"`
	ScheduledDeleteAt string `json:"scheduled_delete_at,omitempty"`
	Reason            string `json:"reason,omitempty"`
	ArchivedBy        string `json:"archived_by,omitempty"`
}

// ProjectArchive handles POST /api/v1/projects/{id}/archive.
func (s *Server) ProjectArchive(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST only")
		return
	}
	if s.archiveService == nil {
		respondError(w, http.StatusServiceUnavailable, "ARCHIVE_DISABLED",
			"archive service not wired on this deployment")
		return
	}

	var req ArchiveRequest
	if r.Body != nil {
		// Cap the body to 4 KiB so a 1 GB JSON payload can't
		// OOM the handler. Empty body is fine — the service
		// defaults to a 7-day grace.
		if err := decodeJSONBody(w, r, 4*1024, &req); err != nil && !errors.Is(err, io.EOF) {
			respondError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid body: "+err.Error())
			return
		}
	}
	grace, err := projectarchive.ParseGraceDuration(req.Grace)
	if err != nil {
		respondError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid grace: "+err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	snap, err := s.archiveService.Archive(ctx, projectID, projectarchive.ArchiveInput{
		Grace:     grace,
		Reason:    strings.TrimSpace(req.Reason),
		Principal: apiArchivePrincipal(r),
	})
	if err != nil {
		s.handleArchiveError(w, err)
		return
	}
	s.writeArchiveAudit(ctx, r, "project.archive", projectID, map[string]any{
		"grace":               grace.String(),
		"scheduled_delete_at": snap.ScheduledDeleteAt.Format(time.RFC3339),
		"reason":              snap.Reason,
	})
	respondJSON(w, http.StatusOK, archiveSnapshotToJSON(projectID, snap))
}

// ProjectUnarchive handles POST /api/v1/projects/{id}/unarchive.
func (s *Server) ProjectUnarchive(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST only")
		return
	}
	if s.archiveService == nil {
		respondError(w, http.StatusServiceUnavailable, "ARCHIVE_DISABLED",
			"archive service not wired on this deployment")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.archiveService.Unarchive(ctx, projectID); err != nil {
		s.handleArchiveError(w, err)
		return
	}
	s.writeArchiveAudit(ctx, r, "project.unarchive", projectID, nil)
	respondJSON(w, http.StatusOK, ArchiveResponse{ProjectID: projectID, Status: "active"})
}

// ProjectDeleteNow handles POST /api/v1/projects/{id}/delete-now.
// Requires the project to be archived first — same gate the UI
// enforces.
func (s *Server) ProjectDeleteNow(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST only")
		return
	}
	if s.archiveService == nil {
		respondError(w, http.StatusServiceUnavailable, "ARCHIVE_DISABLED",
			"archive service not wired on this deployment")
		return
	}
	if s.projectRegistry == nil {
		respondError(w, http.StatusServiceUnavailable, "REGISTRY_UNAVAILABLE",
			"project registry not wired")
		return
	}
	p := s.projectRegistry.GetProject(projectID)
	if p == nil {
		respondError(w, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("project %q not found", projectID))
		return
	}
	if !p.IsArchived() {
		respondError(w, http.StatusConflict, "NOT_ARCHIVED",
			"project must be archived before delete-now (call /archive first)")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.archiveService.ScheduleDeleteNow(ctx, projectID, true); err != nil {
		s.handleArchiveError(w, err)
		return
	}
	s.writeArchiveAudit(ctx, r, "project.delete-now", projectID, map[string]any{
		"scheduled_delete_at": time.Now().UTC().Format(time.RFC3339),
	})
	now := time.Now().UTC()
	resp := ArchiveResponse{
		ProjectID:         projectID,
		Status:            "archived",
		ScheduledDeleteAt: now.Format(time.RFC3339),
	}
	respondJSON(w, http.StatusOK, resp)
}

// Note: per-project /archive, /unarchive, /delete-now subpaths
// are dispatched from routes.go's project sub-router; the
// dedicated routing function that lived here was removed
// when the parent router absorbed those branches.

// handleArchiveError translates the projectarchive package's
// error strings into the right HTTP status + code. The service
// already returns operator-friendly messages; this just picks
// the right status.
func (s *Server) handleArchiveError(w http.ResponseWriter, err error) {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "not found"):
		respondError(w, http.StatusNotFound, "NOT_FOUND", msg)
	case strings.Contains(msg, "grace must be") || strings.Contains(msg, "grace exceeds") ||
		strings.Contains(msg, "invalid project id") || strings.Contains(msg, "delete-now requires"):
		respondError(w, http.StatusBadRequest, "BAD_REQUEST", msg)
	case strings.Contains(msg, "not wired") || strings.Contains(msg, "not configured"):
		respondError(w, http.StatusServiceUnavailable, "ARCHIVE_DISABLED", msg)
	default:
		respondError(w, http.StatusInternalServerError, "INTERNAL", msg)
	}
}

// writeArchiveAudit best-effort writes one row per archive
// action. Same shape the UI emits so /ui/admin/audit shows both
// surfaces in one timeline.
func (s *Server) writeArchiveAudit(ctx context.Context, r *http.Request, action, projectID string, extras map[string]any) {
	if s.adminAuditRepo == nil {
		return
	}
	principal := apiArchivePrincipal(r)
	if principal == "" {
		principal = "api-anonymous"
	}
	after := map[string]any{"project_id": projectID}
	for k, v := range extras {
		after[k] = v
	}
	afterJSON, _ := json.Marshal(after)
	_ = s.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
		Principal: principal,
		Source:    "api",
		Action:    action,
		Target:    projectID,
		After:     string(afterJSON),
		IP:        clientIPFromRequest(r),
		UserAgent: r.UserAgent(),
	})
}

// apiArchivePrincipal picks the operator identity for the
// audit row. Matched-API-key principal wins; falls through to
// the X-Operator-Id header for tooling that authenticates via
// a shared key but identifies its operator via header.
func apiArchivePrincipal(r *http.Request) string {
	if p := apiKeyPrincipalFromContext(r.Context()); p != "" {
		return p
	}
	if op := r.Header.Get("X-Operator-Id"); op != "" {
		return op
	}
	return ""
}

// archiveSnapshotToJSON converts the service's snapshot to the
// wire shape. Empty optional fields stay omitempty so callers
// don't have to handle "" specifically.
func archiveSnapshotToJSON(projectID string, snap projectarchive.LifecycleSnapshot) ArchiveResponse {
	out := ArchiveResponse{
		ProjectID:  projectID,
		Status:     snap.Status,
		Reason:     snap.Reason,
		ArchivedBy: snap.ArchivedBy,
	}
	if !snap.ArchivedAt.IsZero() {
		out.ArchivedAt = snap.ArchivedAt.Format(time.RFC3339)
	}
	if !snap.ScheduledDeleteAt.IsZero() {
		out.ScheduledDeleteAt = snap.ScheduledDeleteAt.Format(time.RFC3339)
	}
	return out
}

// Ensure registry import stays referenced even if a future
// trimming pass removes the last direct mention.
var _ = registry.DefaultMaxCallDepth
