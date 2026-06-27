package projectarchive

// Lifecycle-mutation service shared by the UI handlers and the
// REST API. Owns the "write a `lifecycle:` block into the
// project YAML + reload the registry" flow so both surfaces
// (UI form POST + JSON API POST) go through one implementation
// — same audit shape, same reload semantics, same atomic write.
//
// The YAML patch step is injected via the YAMLPatcher callback
// so the projectarchive package doesn't depend on the UI
// package's surgical patcher (and vice versa — the UI keeps
// its existing patcher unchanged).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultGraceDuration is the archive-flow grace window when the
// caller doesn't pass a duration. Matches the operator-requested
// default from the original archive feature spec.
const DefaultGraceDuration = 7 * 24 * time.Hour

// MinGraceDuration prevents an accidental "instant delete" via
// the archive flow. delete-now is the separate path that
// shortens the window to ~now.
const MinGraceDuration = 1 * time.Minute

// MaxGraceDuration prevents an LLM-driven archive call from
// scheduling a "delete in 100 years" — defensive only; operator
// intent rarely exceeds a quarter.
const MaxGraceDuration = 365 * 24 * time.Hour

// PatchOp is one mutation against a project YAML's mapping.
// Path walks into nested mappings; Value is the new leaf scalar
// (string/bool/int); RemoveIfEmpty deletes the key when Value is
// the zero value of its type. Mirrors the UI package's yamlPatch
// shape; defined here so the API caller doesn't have to import
// the UI package.
type PatchOp struct {
	Path          []string
	Value         any
	RemoveIfEmpty bool
}

// YAMLPatcher applies a list of patches to a YAML document and
// returns the rewritten bytes. UI's applyYAMLPatches satisfies
// this contract; the service depends on the interface rather
// than the concrete UI helper to keep the package boundary
// clean.
type YAMLPatcher func(content []byte, patches []PatchOp) ([]byte, error)

// Reloader is called after a successful YAML mutation so the
// daemon's in-memory registry picks up the change. nil-tolerant
// at the call site — when no reloader is wired, the service
// returns nil and the operator restarts the daemon for the
// change to take effect.
type Reloader func(ctx context.Context) error

// LifecycleService owns the archive / unarchive / delete-now
// flow. Construct via NewLifecycleService once per daemon; the
// UI Server + API Server share the same instance.
type LifecycleService struct {
	ConfigDir string
	Patcher   YAMLPatcher
	Reload    Reloader
	// Sweeper is the heartbeat the service kicks after a
	// delete-now action so the project gets wiped within a
	// second instead of waiting for the next regular tick.
	// Nil-safe — when not wired the project is wiped on the
	// next scheduled sweep (default 1h).
	Sweeper interface{ SweepNow(ctx context.Context) }
}

// ArchiveInput carries the operator-supplied params for an
// archive call. Grace defaults to DefaultGraceDuration when
// zero. Reason / Principal are optional, recorded in the YAML
// for the audit trail.
type ArchiveInput struct {
	Grace     time.Duration
	Reason    string
	Principal string
}

// LifecycleSnapshot summarises the post-mutation state for the
// caller's response. The UI uses ScheduledDeleteAt for the
// redirect banner; the API echoes it back in the JSON response.
type LifecycleSnapshot struct {
	Status            string
	ArchivedAt        time.Time
	ScheduledDeleteAt time.Time
	Reason            string
	ArchivedBy        string
}

// Archive flips the project YAML's lifecycle.status to archived
// + schedules its deletion. Returns the resolved snapshot so the
// caller can render the operator-visible response.
func (s *LifecycleService) Archive(ctx context.Context, projectID string, in ArchiveInput) (LifecycleSnapshot, error) {
	if err := s.checkPrereqs(projectID); err != nil {
		return LifecycleSnapshot{}, err
	}
	grace := in.Grace
	if grace == 0 {
		grace = DefaultGraceDuration
	}
	if grace < MinGraceDuration {
		return LifecycleSnapshot{}, fmt.Errorf("grace must be at least %s (use delete-now for instant deletion)", MinGraceDuration)
	}
	if grace > MaxGraceDuration {
		return LifecycleSnapshot{}, fmt.Errorf("grace exceeds maximum %s", MaxGraceDuration)
	}

	now := time.Now().UTC()
	deleteAt := now.Add(grace)
	reason := strings.TrimSpace(in.Reason)
	principal := strings.TrimSpace(in.Principal)

	if err := s.applyPatches(projectID, []PatchOp{
		{Path: []string{"lifecycle", "status"}, Value: "archived"},
		{Path: []string{"lifecycle", "archivedAt"}, Value: now.Format(time.RFC3339)},
		{Path: []string{"lifecycle", "scheduledDeleteAt"}, Value: deleteAt.Format(time.RFC3339)},
		{Path: []string{"lifecycle", "reason"}, Value: reason, RemoveIfEmpty: true},
		{Path: []string{"lifecycle", "archivedBy"}, Value: principal, RemoveIfEmpty: true},
	}); err != nil {
		return LifecycleSnapshot{}, fmt.Errorf("archive: %w", err)
	}
	if err := s.reloadIfWired(ctx); err != nil {
		return LifecycleSnapshot{}, fmt.Errorf("archive saved but reload failed: %w", err)
	}
	return LifecycleSnapshot{
		Status:            "archived",
		ArchivedAt:        now,
		ScheduledDeleteAt: deleteAt,
		Reason:            reason,
		ArchivedBy:        principal,
	}, nil
}

// Unarchive clears the lifecycle block, restoring the project
// to active. Idempotent on already-active projects.
func (s *LifecycleService) Unarchive(ctx context.Context, projectID string) error {
	if err := s.checkPrereqs(projectID); err != nil {
		return err
	}
	patches := []PatchOp{
		{Path: []string{"lifecycle", "status"}, Value: "", RemoveIfEmpty: true},
		{Path: []string{"lifecycle", "archivedAt"}, Value: "", RemoveIfEmpty: true},
		{Path: []string{"lifecycle", "scheduledDeleteAt"}, Value: "", RemoveIfEmpty: true},
		{Path: []string{"lifecycle", "reason"}, Value: "", RemoveIfEmpty: true},
		{Path: []string{"lifecycle", "archivedBy"}, Value: "", RemoveIfEmpty: true},
	}
	if err := s.applyPatches(projectID, patches); err != nil {
		return fmt.Errorf("unarchive: %w", err)
	}
	if err := s.reloadIfWired(ctx); err != nil {
		return fmt.Errorf("unarchive saved but reload failed: %w", err)
	}
	return nil
}

// ScheduleDeleteNow rewinds scheduledDeleteAt to ~now so the
// next sweeper tick (or the synchronous Kick) wipes the project.
// Refuses when the project isn't already archived — operators
// must go through the archive flow first.
func (s *LifecycleService) ScheduleDeleteNow(ctx context.Context, projectID string, isArchived bool) error {
	if err := s.checkPrereqs(projectID); err != nil {
		return err
	}
	if !isArchived {
		return fmt.Errorf("delete-now requires project to be archived first")
	}
	now := time.Now().UTC()
	if err := s.applyPatches(projectID, []PatchOp{
		{Path: []string{"lifecycle", "scheduledDeleteAt"}, Value: now.Format(time.RFC3339)},
	}); err != nil {
		return fmt.Errorf("delete-now: %w", err)
	}
	if err := s.reloadIfWired(ctx); err != nil {
		return fmt.Errorf("delete-now saved but reload failed: %w", err)
	}
	if s.Sweeper != nil {
		// Asynchronous so the caller doesn't block on the
		// full DB+blob+YAML wipe.
		go s.Sweeper.SweepNow(context.Background())
	}
	return nil
}

// ParseGraceDuration accepts "<N>d" (days), any
// time.ParseDuration shape, "" / "default" → DefaultGraceDuration.
// Centralised here so the UI form parser and the JSON API parser
// share the same rules.
func ParseGraceDuration(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "default" {
		return DefaultGraceDuration, nil
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

// checkPrereqs validates the bits the LifecycleService needs to
// do its job. Surfaces missing wiring as an explicit error so
// callers know to wire ConfigDir / Patcher.
func (s *LifecycleService) checkPrereqs(projectID string) error {
	if s == nil {
		return fmt.Errorf("lifecycle service not wired")
	}
	if s.ConfigDir == "" {
		return fmt.Errorf("config directory not configured")
	}
	if s.Patcher == nil {
		return fmt.Errorf("yaml patcher not wired")
	}
	if projectID == "" || strings.ContainsAny(projectID, `/\`) {
		return fmt.Errorf("invalid project id %q", projectID)
	}
	path := filepath.Join(s.ConfigDir, "projects", projectID+".yaml")
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("project %q not found", projectID)
		}
		return fmt.Errorf("stat project yaml: %w", err)
	}
	return nil
}

// applyPatches reads + patches + writes the project YAML
// atomically. Uses os.Rename for the swap so a crash mid-write
// leaves either the old or the new content — never a partial.
func (s *LifecycleService) applyPatches(projectID string, patches []PatchOp) error {
	path := filepath.Join(s.ConfigDir, "projects", projectID+".yaml")
	existing, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read project yaml: %w", err)
	}
	patched, err := s.Patcher(existing, patches)
	if err != nil {
		return fmt.Errorf("apply patches: %w", err)
	}
	return atomicWrite(path, patched)
}

// reloadIfWired calls the configured reloader; nil is a no-op.
func (s *LifecycleService) reloadIfWired(ctx context.Context) error {
	if s.Reload == nil {
		return nil
	}
	return s.Reload(ctx)
}

// atomicWrite writes content via a temp file in the same
// directory + os.Rename for an atomic swap. Falls back to the
// existing file's mode (or 0o600 if it didn't exist) so the
// daemon doesn't widen permissions on a previously-restricted
// file.
func atomicWrite(path string, content []byte) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}

	tmp, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	cleanup = false
	return nil
}
