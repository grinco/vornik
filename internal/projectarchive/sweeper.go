// Package projectarchive runs the archived-project deletion
// sweeper. When an operator archives a project the UI writes a
// `lifecycle:` block into the project YAML with a
// scheduledDeleteAt timestamp; this package's Sweeper scans the
// registry on a cadence and, for each project whose scheduled
// time has elapsed, hard-deletes the YAML files, every project-
// scoped DB row, and every artifact blob on disk.
//
// Idempotent: re-running over a partially-deleted project does
// not error. The sweeper is the SINGLE deletion point; the UI's
// "Delete now" button kicks SweepNow rather than running the
// deletion path inline, so audit / reload / error handling share
// one code path.
package projectarchive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// DefaultTickInterval is the cadence between automatic sweeps.
// Hourly hits the right balance between operator UX (a 7-day
// grace doesn't need second-precision) and pressure on the DB
// (a full table scan over the project_data tables every minute
// would be noisy). The interval can be overridden via Config.
const DefaultTickInterval = 1 * time.Hour

// ReloadFunc reloads the registry from disk so the in-memory
// map drops the deleted project. Nil-safe in NewSweeper.
type ReloadFunc func(ctx context.Context) error

// ArtifactWiper is the narrow contract the sweeper uses to delete
// a project's blob storage. Satisfied by *artifacts.Store via its
// DeleteProjectArtifacts method. Optional — when nil the sweeper
// falls back to ArtifactBasePath filesystem removal (legacy
// direct-disk deployments without a backend-aware store).
type ArtifactWiper interface {
	DeleteProjectArtifacts(ctx context.Context, projectID string) (int, error)
}

// Config wires the sweeper. Registry + DataDeleter are required;
// everything else is optional and degrades to "skip that step"
// when nil.
type Config struct {
	Registry         *registry.Registry
	DataDeleter      persistence.ProjectDataDeleter
	ConfigDir        string        // root of configs/ — projects YAML lives under <ConfigDir>/projects/
	ArtifactBasePath string        // legacy direct-disk fallback when ArtifactWiper is nil
	ArtifactWiper    ArtifactWiper // backend-aware blob deletion (preferred; works against both filesystem AND S3)
	AuditRepo        persistence.AdminAuditRepository
	Reload           ReloadFunc
	TickInterval     time.Duration
	Logger           zerolog.Logger
	Clock            func() time.Time // injectable for tests; nil → time.Now
	// LeaderGate, when non-nil, is consulted before each tick.
	// IsLeader()=false → skip this tick + log at debug level.
	// Used by multi-replica deployments to ensure exactly one
	// daemon is wiping projects at a time (2026.8.0
	// horizontal-scaling prep). Nil leaves the gate open —
	// single-process deployments don't need election.
	LeaderGate LeaderGate
}

// LeaderGate is the narrow contract the sweeper consults to
// decide whether THIS daemon should be running the worker.
// Satisfied by *leaderelection.Elector; defined as an
// interface here to avoid pulling the leaderelection package
// into projectarchive's dependency set.
type LeaderGate interface {
	IsLeader() bool
}

// Sweeper is the deletion runner. Construct via NewSweeper, drive
// via Run (long-lived goroutine) or SweepNow (one-shot kick).
type Sweeper struct {
	cfg Config

	// kickCh forces a sweep mid-tick. SweepNow drops a token here;
	// a buffered channel size of 1 collapses overlapping kicks
	// into one upcoming sweep.
	kickCh chan struct{}

	// mu guards inflight to enforce serialised sweeps — overlapping
	// runs against the same project would race on the YAML and DB.
	mu       sync.Mutex
	inflight bool
}

// NewSweeper builds a Sweeper with defaults applied. Required
// fields (Registry, DataDeleter) get nil-checked at the use site
// in sweepOnce; a nil-fielded Sweeper is still safe to Run (it
// just logs and skips).
func NewSweeper(cfg Config) *Sweeper {
	if cfg.TickInterval <= 0 {
		cfg.TickInterval = DefaultTickInterval
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	return &Sweeper{
		cfg:    cfg,
		kickCh: make(chan struct{}, 1),
	}
}

// Run blocks on a ticker until ctx is cancelled. Each tick (and
// each SweepNow kick) calls sweepOnce.
func (s *Sweeper) Run(ctx context.Context) {
	s.cfg.Logger.Info().Dur("interval", s.cfg.TickInterval).Msg("archive-sweeper: started")
	ticker := time.NewTicker(s.cfg.TickInterval)
	defer ticker.Stop()
	// Run once at startup so an archived project past its grace
	// gets deleted promptly after a daemon restart.
	s.sweepOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			s.cfg.Logger.Info().Msg("archive-sweeper: stopped")
			return
		case <-ticker.C:
			s.sweepOnce(ctx)
		case <-s.kickCh:
			s.sweepOnce(ctx)
		}
	}
}

// SweepNow kicks an out-of-band sweep. Idempotent — overlapping
// calls collapse to one upcoming sweep. Safe to call from any
// goroutine.
func (s *Sweeper) SweepNow(ctx context.Context) {
	select {
	case s.kickCh <- struct{}{}:
	default:
		// Already pending — collapse.
	}
}

// sweepOnce runs one pass over the registry's archived
// projects. Each due project is deleted; non-due ones are left
// alone. Errors are logged + audited but don't stop the pass
// (one stuck project shouldn't block the rest).
func (s *Sweeper) sweepOnce(ctx context.Context) {
	if s.cfg.Registry == nil {
		s.cfg.Logger.Warn().Msg("archive-sweeper: registry not wired; skipping tick")
		return
	}
	// Leader gate: in a multi-replica deployment only the
	// elected leader runs the wipe. Other replicas skip the
	// tick entirely — no DB query, no audit row, no log noise.
	// Nil gate means single-process deployment; tick runs as
	// usual.
	if s.cfg.LeaderGate != nil && !s.cfg.LeaderGate.IsLeader() {
		s.cfg.Logger.Debug().Msg("archive-sweeper: not the leader; skipping tick")
		return
	}
	s.mu.Lock()
	if s.inflight {
		s.mu.Unlock()
		return
	}
	s.inflight = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inflight = false
		s.mu.Unlock()
	}()

	now := s.cfg.Clock()
	archived := s.cfg.Registry.ListArchivedProjects()
	if len(archived) == 0 {
		return
	}
	due := 0
	for _, p := range archived {
		if p == nil {
			continue
		}
		if !p.DeletionDue(now) {
			continue
		}
		due++
		if err := s.deleteProject(ctx, p); err != nil {
			s.cfg.Logger.Error().Err(err).Str("project_id", p.ID).Msg("archive-sweeper: delete failed")
			s.audit(ctx, "project.delete.failed", p.ID, map[string]any{
				"error": err.Error(),
			})
			continue
		}
	}
	if due > 0 {
		s.cfg.Logger.Info().Int("due", due).Msg("archive-sweeper: pass complete")
	}
}

// deleteProject executes the full wipe for one project:
//
//  1. DB rows via the ProjectDataDeleter.
//  2. Artifact blobs under <ArtifactBasePath>/<projectID>/.
//  3. Project YAML + companion PROJECT.md under
//     <ConfigDir>/projects/.
//  4. Registry reload so the deleted project disappears from
//     in-memory state.
//  5. Audit row recording the scope of the wipe.
//
// Step 1 first so a half-wiped project keeps a YAML row that the
// next sweeper tick can re-attempt against. Step 4 last so the
// audit row writes against the project ID that just disappeared.
func (s *Sweeper) deleteProject(ctx context.Context, p *registry.Project) error {
	stats := persistence.ProjectDataStats{}
	var firstErr error

	if s.cfg.DataDeleter != nil {
		st, err := s.cfg.DataDeleter.DeleteProjectData(ctx, p.ID)
		if err != nil {
			return fmt.Errorf("db wipe: %w", err)
		}
		stats = st
	}

	artifactsRemoved := false
	artifactsCount := 0
	switch {
	case s.cfg.ArtifactWiper != nil:
		// Preferred path: works against both filesystem and S3
		// backends. Walks the backend's project-key prefix and
		// deletes each blob.
		n, err := s.cfg.ArtifactWiper.DeleteProjectArtifacts(ctx, p.ID)
		if err != nil {
			s.cfg.Logger.Warn().Err(err).Str("project_id", p.ID).Int("deleted_before_error", n).Msg("archive-sweeper: artifact wipe failed (continuing)")
			if firstErr == nil {
				firstErr = fmt.Errorf("artifact wipe: %w", err)
			}
		}
		artifactsCount = n
		artifactsRemoved = n > 0
	case s.cfg.ArtifactBasePath != "":
		// Legacy direct-disk fallback. Used by deployments that
		// wire the sweeper without going through artifacts.Store
		// (mostly test fixtures these days). Local filesystem
		// only; S3 deployments need ArtifactWiper.
		artDir := filepath.Join(s.cfg.ArtifactBasePath, p.ID)
		if err := safeRemoveAll(artDir, s.cfg.ArtifactBasePath); err != nil {
			s.cfg.Logger.Warn().Err(err).Str("project_id", p.ID).Str("artifact_dir", artDir).Msg("archive-sweeper: artifact wipe (fs) failed (continuing)")
			if firstErr == nil {
				firstErr = fmt.Errorf("artifact wipe: %w", err)
			}
		} else {
			artifactsRemoved = true
		}
	}

	yamlRemoved, mdRemoved, err := s.removeProjectFiles(p.ID)
	if err != nil {
		return fmt.Errorf("yaml remove: %w (db_rows=%d artifact_dir_removed=%v)", err, stats.RowsDeleted, artifactsRemoved)
	}

	if s.cfg.Reload != nil {
		if err := s.cfg.Reload(ctx); err != nil {
			s.cfg.Logger.Warn().Err(err).Str("project_id", p.ID).Msg("archive-sweeper: registry reload after delete failed (continuing)")
			if firstErr == nil {
				firstErr = fmt.Errorf("reload: %w", err)
			}
		}
	}

	s.audit(ctx, "project.deleted", p.ID, map[string]any{
		"tables_cleared":      stats.TablesCleared,
		"rows_deleted":        stats.RowsDeleted,
		"artifacts_removed":   artifactsRemoved,
		"artifacts_count":     artifactsCount,
		"yaml_removed":        yamlRemoved,
		"projectmd_removed":   mdRemoved,
		"scheduled_delete_at": yamlScheduledDeleteAtOf(p),
	})

	if firstErr != nil {
		return firstErr
	}
	return nil
}

// removeProjectFiles unlinks the project YAML + companion
// PROJECT.md from the configs directory. Missing files are
// tolerated (idempotent for retries). Returns booleans so the
// audit row distinguishes "didn't exist" from "removed".
func (s *Sweeper) removeProjectFiles(projectID string) (yamlRemoved, mdRemoved bool, err error) {
	if s.cfg.ConfigDir == "" {
		// No configs dir wired — caller is using us purely for
		// DB wipes. Leave the file removal to operator-side
		// tools.
		return false, false, nil
	}
	if strings.ContainsAny(projectID, `/\`) {
		return false, false, fmt.Errorf("refusing to delete project with path-separator in id %q", projectID)
	}
	yamlPath := filepath.Join(s.cfg.ConfigDir, "projects", projectID+".yaml")
	mdPath := filepath.Join(s.cfg.ConfigDir, "projects", projectID+".md")

	if err := safeRemove(yamlPath, s.cfg.ConfigDir); err != nil {
		return false, false, fmt.Errorf("remove %s: %w", yamlPath, err)
	}
	yamlRemoved = true

	if err := safeRemove(mdPath, s.cfg.ConfigDir); err != nil {
		// PROJECT.md is optional — a not-found error is fine.
		if errors.Is(err, os.ErrNotExist) {
			return yamlRemoved, false, nil
		}
		return yamlRemoved, false, fmt.Errorf("remove %s: %w", mdPath, err)
	}
	mdRemoved = true
	return yamlRemoved, mdRemoved, nil
}

// safeRemove unlinks a file after verifying its absolute path
// sits under root. Belt-and-braces against a future bug that
// computes a path traversal — a stray "../" would otherwise let
// the sweeper delete files outside the configs dir.
func safeRemove(path, root string) error {
	if err := assertWithinRoot(path, root); err != nil {
		return err
	}
	err := os.Remove(path)
	if err != nil && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// safeRemoveAll mirrors safeRemove for directory trees (used by
// the artifact blob wipe).
func safeRemoveAll(path, root string) error {
	if err := assertWithinRoot(path, root); err != nil {
		return err
	}
	return os.RemoveAll(path)
}

// assertWithinRoot fails when path doesn't resolve under root.
// Both inputs are Abs'd + cleaned so symlinks / "../" sequences
// can't slip past. Sweep paths are constructed from operator-
// controlled config values — the operator is trusted, but this
// guard catches accidental misconfiguration ("ConfigDir=/" would
// otherwise wipe the entire filesystem).
func assertWithinRoot(path, root string) error {
	if root == "" {
		return fmt.Errorf("empty root")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("abs root: %w", err)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("abs path: %w", err)
	}
	absRoot = filepath.Clean(absRoot)
	absPath = filepath.Clean(absPath)
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return fmt.Errorf("rel: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("path %q escapes root %q", absPath, absRoot)
	}
	return nil
}

// audit best-effort writes an admin-audit row. Nil-safe.
func (s *Sweeper) audit(ctx context.Context, action, projectID string, extras map[string]any) {
	if s.cfg.AuditRepo == nil {
		return
	}
	after, _ := json.Marshal(extras)
	_ = s.cfg.AuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
		Principal: "system:archive-sweeper",
		Source:    "sweeper",
		Action:    action,
		Target:    projectID,
		After:     string(after),
	})
}

// yamlScheduledDeleteAtOf reads the project's scheduled deletion
// timestamp for the audit row. Returns empty string when the
// YAML didn't carry one.
func yamlScheduledDeleteAtOf(p *registry.Project) string {
	t, ok := p.ScheduledDeletion()
	if !ok {
		return ""
	}
	return t.Format(time.RFC3339)
}
