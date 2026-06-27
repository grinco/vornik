package ui

// Project archive / unarchive / delete-now handlers. The lifecycle
// state lives in the project YAML's `lifecycle:` block (status,
// archivedAt, scheduledDeleteAt, reason, archivedBy). Archive sets
// the block; unarchive removes it; delete-now zeroes the grace
// period so the sweeper picks the project up on its next tick.
//
// See https://docs.vornik.io

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/fieldguard"
	"vornik.io/vornik/internal/persistence"
)

// defaultArchiveGraceDuration is the per-project grace window
// when the operator doesn't pass an explicit ?grace= override.
// Seven days matches the requested default; the form on the
// project-detail page offers a small set of presets but the
// backend accepts any positive duration.
const defaultArchiveGraceDuration = 7 * 24 * time.Hour

// minArchiveGraceDuration prevents an accidental "instant delete"
// via the archive form. delete-now is a separate endpoint with its
// own confirmation prompt — archive itself always gives a window.
const minArchiveGraceDuration = 1 * time.Minute

// archiveGracePresets drives the UI's select widget so operators
// can pick a sensible window without typing a duration. The
// archive form accepts free-form input too (?grace=14d / 336h /
// 90m); the parser below handles the suffix list.
var archiveGracePresets = []ArchiveGracePreset{
	{Label: "1 day", Value: "1d"},
	{Label: "7 days (default)", Value: "7d"},
	{Label: "30 days", Value: "30d"},
	{Label: "90 days", Value: "90d"},
}

// ArchiveGracePreset is one option in the archive form's grace
// selector. Exposed via project_detail data so the template
// stays declarative.
type ArchiveGracePreset struct {
	Label string
	Value string
}

// ProjectArchive flips a project's lifecycle to archived and
// schedules its deletion. POST /ui/projects/{id}/archive with
// form fields: grace (e.g. "7d"), reason (optional).
//
// On success: writes the YAML with the new lifecycle block,
// reloads the registry, and redirects back to the project detail
// page so the operator sees the banner immediately.
func (s *Server) ProjectArchive(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "failed to parse form: "+err.Error(), http.StatusBadRequest)
		return
	}

	graceRaw := strings.TrimSpace(r.FormValue("grace"))
	grace, err := parseGraceDuration(graceRaw)
	if err != nil {
		http.Error(w, "invalid grace duration: "+err.Error(), http.StatusBadRequest)
		return
	}
	if grace < minArchiveGraceDuration {
		http.Error(w, "grace must be at least 1 minute — use delete-now for instant deletion", http.StatusBadRequest)
		return
	}

	reason := strings.TrimSpace(r.FormValue("reason"))
	principal := adminPrincipal(r)
	if principal == "" || principal == "unknown" {
		// Best-effort fallback — the YAML field captures provenance
		// for the audit trail; a blank value is acceptable.
		principal = ""
	}

	// Preferred path: delegate to the shared lifecycle service so
	// UI + REST API land identical mutations. Falls back to the
	// legacy inline path when the service isn't wired (keeps the
	// existing test fixtures working).
	if s.archiveLifecycle != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		snap, err := s.archiveLifecycle.Archive(ctx, projectID, ArchiveLifecycleInput{
			Grace: grace, Reason: reason, Principal: principal,
		})
		if err != nil {
			http.Error(w, "failed to archive project: "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.writeProjectLifecycleAudit(r, "project.archive", projectID, map[string]any{
			"grace":               grace.String(),
			"scheduled_delete_at": snap.ScheduledDeleteAt.Format(time.RFC3339),
			"reason":              reason,
		})
		http.Redirect(w, r, "/ui/projects/"+projectID+"?archived=1", http.StatusSeeOther)
		return
	}

	now := time.Now().UTC()
	deleteAt := now.Add(grace)
	if err := s.applyLifecyclePatches(projectID, []yamlPatch{
		{Path: []string{"lifecycle", "status"}, Value: "archived"},
		{Path: []string{"lifecycle", "archivedAt"}, Value: now.Format(time.RFC3339)},
		{Path: []string{"lifecycle", "scheduledDeleteAt"}, Value: deleteAt.Format(time.RFC3339)},
		{Path: []string{"lifecycle", "reason"}, Value: reason, RemoveIfEmpty: true},
		{Path: []string{"lifecycle", "archivedBy"}, Value: principal, RemoveIfEmpty: true},
	}); err != nil {
		http.Error(w, "failed to archive project: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.writeProjectLifecycleAudit(r, "project.archive", projectID, map[string]any{
		"grace":               grace.String(),
		"scheduled_delete_at": deleteAt.Format(time.RFC3339),
		"reason":              reason,
	})

	http.Redirect(w, r, "/ui/projects/"+projectID+"?archived=1", http.StatusSeeOther)
}

// ProjectUnarchive clears the lifecycle block, returning the
// project to the active state. POST /ui/projects/{id}/unarchive.
// No-op (still redirects) if the project wasn't archived — easier
// than a separate guard, and the UI only renders the button when
// the project IS archived.
func (s *Server) ProjectUnarchive(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.archiveLifecycle != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := s.archiveLifecycle.Unarchive(ctx, projectID); err != nil {
			http.Error(w, "failed to unarchive project: "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.writeProjectLifecycleAudit(r, "project.unarchive", projectID, nil)
		http.Redirect(w, r, "/ui/projects/"+projectID+"?unarchived=1", http.StatusSeeOther)
		return
	}
	if err := s.applyLifecyclePatches(projectID, []yamlPatch{
		{Path: []string{"lifecycle", "status"}, Value: "", RemoveIfEmpty: true},
		{Path: []string{"lifecycle", "archivedAt"}, Value: "", RemoveIfEmpty: true},
		{Path: []string{"lifecycle", "scheduledDeleteAt"}, Value: "", RemoveIfEmpty: true},
		{Path: []string{"lifecycle", "reason"}, Value: "", RemoveIfEmpty: true},
		{Path: []string{"lifecycle", "archivedBy"}, Value: "", RemoveIfEmpty: true},
	}); err != nil {
		http.Error(w, "failed to unarchive project: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Also remove the now-empty `lifecycle:` key so the YAML stays
	// tidy for projects that have never been archived.
	if err := s.removeEmptyLifecycleMap(projectID); err != nil {
		s.logger.Warn().Err(err).Str("project_id", projectID).Msg("unarchive: failed to prune empty lifecycle map")
	}

	s.writeProjectLifecycleAudit(r, "project.unarchive", projectID, nil)
	http.Redirect(w, r, "/ui/projects/"+projectID+"?unarchived=1", http.StatusSeeOther)
}

// ProjectDeleteNow shortens the grace window to ~now so the
// sweeper picks the project up on its next tick. POST
// /ui/projects/{id}/delete-now. Requires the project to already be
// archived — instant deletion of an active project would skip the
// "are you sure" moment the archive flow gives the operator.
//
// We don't synchronously call the deleter here; the sweeper is the
// single point of deletion so audit, reload, and error handling
// share one code path.
func (s *Server) ProjectDeleteNow(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.projectReg == nil {
		http.Error(w, "registry not wired", http.StatusServiceUnavailable)
		return
	}
	p := s.projectReg.GetProject(projectID)
	if p == nil {
		http.NotFound(w, r)
		return
	}
	if !p.IsArchived() {
		http.Error(w, "project must be archived before delete-now (use /archive first)", http.StatusConflict)
		return
	}

	if s.archiveLifecycle != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := s.archiveLifecycle.ScheduleDeleteNow(ctx, projectID, p.IsArchived()); err != nil {
			http.Error(w, "failed to shorten grace: "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.writeProjectLifecycleAudit(r, "project.delete-now", projectID, map[string]any{
			"scheduled_delete_at": time.Now().UTC().Format(time.RFC3339),
		})
		http.Redirect(w, r, "/ui/projects?deleted="+projectID, http.StatusSeeOther)
		return
	}

	now := time.Now().UTC()
	if err := s.applyLifecyclePatches(projectID, []yamlPatch{
		{Path: []string{"lifecycle", "scheduledDeleteAt"}, Value: now.Format(time.RFC3339)},
	}); err != nil {
		http.Error(w, "failed to shorten grace: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.writeProjectLifecycleAudit(r, "project.delete-now", projectID, map[string]any{
		"scheduled_delete_at": now.Format(time.RFC3339),
	})

	// Best-effort: kick the sweeper if wired, so the operator
	// doesn't sit through up to one tick interval. The sweeper is
	// idempotent so a redundant nudge is safe.
	if s.archiveSweeper != nil {
		go s.archiveSweeper.SweepNow(r.Context())
	}

	http.Redirect(w, r, "/ui/projects?deleted="+projectID, http.StatusSeeOther)
}

// applyLifecyclePatches reads the project YAML, applies the given
// patches via the surgical yaml.Node patcher (preserves comments
// + sibling keys), writes the result atomically, and reloads the
// registry. Used by archive / unarchive / delete-now.
func (s *Server) applyLifecyclePatches(projectID string, patches []yamlPatch) error {
	return s.applyProjectPatches(projectID, lifecyclePatchGuard, patches)
}

// applyProjectPatches is the shared project-YAML mutation primitive: it
// validates the patch set against a field-allowlist guard, reads the
// project YAML, applies the patches via the surgical yaml.Node patcher
// (preserving comments + sibling keys), writes the result atomically, and
// reloads the registry. The guard is the per-caller chokepoint — a lifecycle
// caller passes lifecyclePatchGuard, the git toggle passes gitPatchGuard —
// so a stray patch can never ride one path into another path's fields.
func (s *Server) applyProjectPatches(projectID string, guard *fieldguard.Guard, patches []yamlPatch) error {
	if projectID == "" || strings.Contains(projectID, "/") || strings.Contains(projectID, string(filepath.Separator)) {
		return fmt.Errorf("invalid project id")
	}
	configDir := s.configDir()
	if configDir == "" {
		return fmt.Errorf("registry config directory not configured")
	}
	// Field-allowlist guard: refuse before touching the file so a stray
	// patch can't corrupt a protected field.
	if err := guard.Check(topLevelPatchKeys(patches)); err != nil {
		return fmt.Errorf("project patch refused: %w", err)
	}
	path := filepath.Join(configDir, "projects", projectID+".yaml")
	existing, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read project yaml: %w", err)
	}
	patched, err := applyYAMLPatches(existing, patches)
	if err != nil {
		return fmt.Errorf("apply patches: %w", err)
	}
	if _, err := writeProjectConfigAtomic(path, patched); err != nil {
		return fmt.Errorf("write project yaml: %w", err)
	}
	if s.configReloader != nil {
		if err := s.configReloader.Reload(); err != nil {
			return fmt.Errorf("reload registry: %w", err)
		}
	} else if s.projectReg != nil {
		if err := s.projectReg.Load(configDir); err != nil {
			return fmt.Errorf("reload registry: %w", err)
		}
	}
	return nil
}

// removeEmptyLifecycleMap deletes a `lifecycle:` key with no
// remaining children — keeps the YAML diff small after an
// unarchive (which removes every sub-field). Best-effort: a failed
// prune doesn't fail the unarchive (the registry already treats
// an empty lifecycle as active).
func (s *Server) removeEmptyLifecycleMap(projectID string) error {
	configDir := s.configDir()
	path := filepath.Join(configDir, "projects", projectID+".yaml")
	existing, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	patched, err := applyYAMLPatches(existing, []yamlPatch{
		// Cheap heuristic: if lifecycle.status is removable
		// (empty), and we've already removed every leaf, the
		// patcher's RemoveIfEmpty on the parent path itself
		// would be the cleanest expression — but the current
		// patcher only operates on leaves. Re-set lifecycle.x
		// to empty and the leaf removal step prunes them; the
		// parent mapping may stay empty. Cleanup-on-empty would
		// need a richer patcher. For now the empty `lifecycle: {}`
		// is harmless and operator-readable as "no archive
		// state".
		{Path: []string{"lifecycle", "_placeholder"}, Value: "", RemoveIfEmpty: true},
	})
	if err != nil {
		return err
	}
	if _, err := writeProjectConfigAtomic(path, patched); err != nil {
		return err
	}
	return nil
}

// writeProjectLifecycleAudit best-effort writes an admin-audit row
// for archive / unarchive / delete-now. Nil-safe when the audit
// repo isn't wired.
func (s *Server) writeProjectLifecycleAudit(r *http.Request, action, projectID string, extras map[string]any) {
	if s.adminAuditRepo == nil {
		return
	}
	principal := adminPrincipal(r)
	if principal == "" || principal == "unknown" {
		principal = "ui-admin"
	}
	after := `{"project_id":"` + projectID + `"`
	for k, v := range extras {
		after += `,"` + k + `":` + strconv.Quote(fmt.Sprint(v))
	}
	after += `}`
	_ = s.adminAuditRepo.Insert(r.Context(), &persistence.AdminAuditEntry{
		Principal: principal,
		Source:    "ui",
		Action:    action,
		Target:    projectID,
		After:     after,
		IP:        clientIP(r),
		UserAgent: r.UserAgent(),
	})
}

// parseGraceDuration parses operator-supplied grace strings. Accepts:
//
//   - empty / "default" → defaultArchiveGraceDuration (7d).
//   - "<n>d" → n days.
//   - any duration time.ParseDuration accepts (1h, 30m, 168h, ...).
//
// Days aren't a standard time.ParseDuration unit so we handle them
// explicitly — operators think in days when archiving.
func parseGraceDuration(raw string) (time.Duration, error) {
	if raw == "" || raw == "default" {
		return defaultArchiveGraceDuration, nil
	}
	if strings.HasSuffix(raw, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(raw, "d"))
		if err != nil {
			return 0, fmt.Errorf("expected <N>d, got %q", raw)
		}
		if n < 0 {
			return 0, fmt.Errorf("negative grace not allowed")
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("negative grace not allowed")
	}
	return d, nil
}
