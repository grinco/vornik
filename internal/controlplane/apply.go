package controlplane

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// Gated apply/rollback of control-plane proposals (LLD 2026-07-08-control-
// plane-phase2-apply-rollback-design v2, review green). The daemon writes an
// APPROVED proposal's new file content, hot-reloads, and auto-rolls-back if
// the reload rejects it — so the daemon never runs a bad config. Whole-file
// replace only; no diff-patching. All the risky logic lives here behind
// injected deps so it's unit-testable without the full daemon.

// applyMaxContentBytes caps the applied file (and thus the snapshot) — config
// files are KB; refuse a larger apply (design §Apply step 1).
const applyMaxContentBytes = persistence.ProposalMaxFieldBytes // 64 KiB

var (
	// ErrReviewOnly means the proposal has no apply_target (it describes a
	// problem, not a specific file rewrite) — approve/action it by hand.
	ErrReviewOnly = errors.New("control-plane: proposal is review-only (no apply target)")
	// ErrBusy means a task in the affected scope is RUNNING/LEASED; retry idle.
	ErrBusy = errors.New("control-plane: tasks running in scope; retry in an idle window")
	// ErrDaemonAckRequired means a daemon-scope apply needs the second ack.
	ErrDaemonAckRequired = errors.New("control-plane: daemon-scope apply requires acknowledgement")
	// ErrPathTraversal means apply_target escapes the config dir.
	ErrPathTraversal = errors.New("control-plane: apply target escapes the config directory")
	// ErrContentTooLarge means the apply content exceeds the size cap.
	ErrContentTooLarge = errors.New("control-plane: apply content too large")
	// ErrApplyInProgress means another apply/rollback holds the global lock.
	ErrApplyInProgress = errors.New("control-plane: another apply is in progress")
	// ErrSnapshotMissing means rollback found an empty/corrupt snapshot.
	ErrSnapshotMissing = errors.New("control-plane: pre-apply snapshot missing; restore by hand")
)

// ApplyEngine applies + rolls back proposals against the deployed config tree.
// Deps are funcs/interfaces so tests can drive every path.
type ApplyEngine struct {
	Proposals persistence.ProposalRepository
	// ConfigDir is the deployed config tree root; apply_target is resolved
	// relative to it (path-traversal-guarded).
	ConfigDir string
	// Reload performs a synchronous config reload; a non-nil error means the
	// new config was rejected (→ auto-rollback).
	Reload func() error
	// Validate parses + lints the proposed content for its target BEFORE any
	// write; a non-nil error aborts the apply with no file touched.
	Validate func(relTarget, content string) error
	// HasActiveTasks reports whether the given project (""=any project, for
	// daemon-scope) has a RUNNING/LEASED task.
	HasActiveTasks func(ctx context.Context, projectID string) (bool, error)
	Logger         zerolog.Logger

	mu sync.Mutex // global apply lock (serialises all applies + rollbacks)
}

// resolveTarget joins + cleans apply_target under ConfigDir and rejects any
// path that escapes it.
func (e *ApplyEngine) resolveTarget(rel string) (string, error) {
	base := filepath.Clean(e.ConfigDir)
	full := filepath.Clean(filepath.Join(base, rel))
	if full != base && !strings.HasPrefix(full, base+string(os.PathSeparator)) {
		return "", ErrPathTraversal
	}
	return full, nil
}

// Apply applies an APPROVED proposal. Returns nil only after the new config
// reloaded cleanly and the ledger recorded APPLIED.
func (e *ApplyEngine) Apply(ctx context.Context, id, actor string, ackDaemon bool) error {
	if !e.mu.TryLock() {
		return ErrApplyInProgress
	}
	defer e.mu.Unlock()

	p, err := e.Proposals.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if p.Status != persistence.ProposalStatusApproved {
		return persistence.ErrProposalNotApproved
	}
	if strings.TrimSpace(p.ApplyTarget) == "" {
		return ErrReviewOnly
	}
	if p.BlastRadius == persistence.ProposalScopeDaemon && !ackDaemon {
		return ErrDaemonAckRequired
	}
	if len(p.ApplyContent) > applyMaxContentBytes {
		return ErrContentTooLarge
	}
	target, err := e.resolveTarget(p.ApplyTarget)
	if err != nil {
		return err
	}
	// Busy check: daemon-scope looks across all projects ("" ), else the
	// proposal's project.
	scope := p.ProjectID
	if p.BlastRadius == persistence.ProposalScopeDaemon {
		scope = ""
	}
	if e.HasActiveTasks != nil {
		busy, berr := e.HasActiveTasks(ctx, scope)
		if berr != nil {
			return fmt.Errorf("busy check failed: %w", berr)
		}
		if busy {
			return ErrBusy
		}
	}
	// Validate BEFORE any write.
	if e.Validate != nil {
		if verr := e.Validate(p.ApplyTarget, p.ApplyContent); verr != nil {
			return fmt.Errorf("validation failed: %w", verr)
		}
	}
	// Snapshot the current bytes (in memory) for auto-rollback.
	snapshot, err := os.ReadFile(target) //nolint:gosec // path is guarded above
	if err != nil {
		return fmt.Errorf("read current file for snapshot: %w", err)
	}
	// Atomic write of the new content.
	if err := atomicWrite(target, []byte(p.ApplyContent)); err != nil {
		return fmt.Errorf("apply write failed (original untouched): %w", err)
	}
	// Reload; on failure, auto-rollback to the snapshot.
	if rerr := e.reload(); rerr != nil {
		if rbErr := atomicWrite(target, snapshot); rbErr != nil {
			e.Logger.Error().Err(rbErr).Str("target", target).
				Msg("control-plane: CRITICAL auto-rollback write failed; config may be inconsistent")
			return fmt.Errorf("apply reload failed AND auto-rollback failed: %w (rollback: %v)", rerr, rbErr)
		}
		_ = e.reload() // best-effort restore of the known-good config
		e.Logger.Warn().Err(rerr).Str("proposal_id", id).
			Msg("control-plane: apply reload rejected; auto-rolled-back, proposal stays APPROVED")
		return fmt.Errorf("apply reload rejected (auto-rolled-back): %w", rerr)
	}
	// Reload OK — persist APPLIED + the snapshot. If this DB write fails, the
	// file+ledger would diverge, so roll the file back and stay APPROVED.
	if merr := e.Proposals.MarkApplied(ctx, id, actor, string(snapshot)); merr != nil {
		if rbErr := atomicWrite(target, snapshot); rbErr == nil {
			_ = e.reload()
		}
		return fmt.Errorf("apply recorded failed, rolled back file: %w", merr)
	}
	e.Logger.Info().Str("proposal_id", id).Str("target", target).Str("applied_by", actor).
		Msg("control-plane: proposal applied")
	return nil
}

// Rollback restores an APPLIED proposal's pre-apply snapshot.
func (e *ApplyEngine) Rollback(ctx context.Context, id string) error {
	if !e.mu.TryLock() {
		return ErrApplyInProgress
	}
	defer e.mu.Unlock()

	p, err := e.Proposals.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if p.Status != persistence.ProposalStatusApplied {
		return persistence.ErrProposalNotApplied
	}
	if p.PreApplySnapshot == "" {
		return ErrSnapshotMissing
	}
	target, err := e.resolveTarget(p.ApplyTarget)
	if err != nil {
		return err
	}
	if err := atomicWrite(target, []byte(p.PreApplySnapshot)); err != nil {
		return fmt.Errorf("rollback write failed: %w", err)
	}
	if rerr := e.reload(); rerr != nil {
		// The restored snapshot itself failed to reload — leave it on disk
		// (last-known-good) and alert; don't half-apply.
		e.Logger.Error().Err(rerr).Str("proposal_id", id).
			Msg("control-plane: CRITICAL rollback reload failed; last-known-good left on disk")
		return fmt.Errorf("rollback reload failed: %w", rerr)
	}
	if err := e.Proposals.MarkRolledBack(ctx, id); err != nil {
		return fmt.Errorf("rollback recorded failed: %w", err)
	}
	e.Logger.Info().Str("proposal_id", id).Msg("control-plane: proposal rolled back")
	return nil
}

func (e *ApplyEngine) reload() error {
	if e.Reload == nil {
		return nil
	}
	return e.Reload()
}

// atomicWrite writes data to a temp file in the target's dir (0600), fsyncs,
// and renames it over the target (atomic on POSIX). The temp file is removed
// on every error path — no .bak, no stray copy left behind.
func atomicWrite(target string, data []byte) error {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".cp-apply-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}
