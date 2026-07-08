package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	// ErrScaffoldConflict means a create target already exists or a replace
	// target is missing (Phase 2b multi-op pre-flight; also the orphan/crash
	// re-apply signal). Maps to 409.
	ErrScaffoldConflict = errors.New("control-plane: scaffold conflict (create target exists or replace target missing)")
	// ErrTooManyOps means an apply carries more than scaffoldMaxOps file ops.
	ErrTooManyOps = errors.New("control-plane: too many apply ops")
	// ErrStaleBase means the on-disk config changed since the proposal was
	// drafted (its recorded base hash no longer matches) — re-draft against
	// current config. Optimistic concurrency for hub-authored config edits.
	ErrStaleBase = errors.New("control-plane: config changed since this proposal was drafted; re-draft it")
)

// scaffoldMaxOps caps the file ops in one apply (a project + swarm + a few
// workflows; refuse a runaway plan). Design §3.
const scaffoldMaxOps = 12

// applyFileOp is one file operation in a multi-op (scaffold) apply.
type applyFileOp struct {
	Op      string `json:"op"`   // applyOpCreate | applyOpReplace
	Path    string `json:"path"` // relative to ConfigDir (path-traversal-guarded)
	Content string `json:"content"`
}

const (
	applyOpCreate  = "create"
	applyOpReplace = "replace"
)

// snapshotEntry / snapshotEnvelope are the multi-op rollback record (design §3,
// F4/S4): existed=false ⇒ delete on rollback; existed=true ⇒ restore Content.
type snapshotEntry struct {
	Existed bool   `json:"existed"`
	Content string `json:"content,omitempty"`
}

type snapshotEnvelope struct {
	Version int                      `json:"version"`
	Entries map[string]snapshotEntry `json:"entries"`
}

const snapshotEnvelopeVersion = 1

// resolvedOp is an op after path-resolution + pre-image capture.
type resolvedOp struct {
	op       applyFileOp
	target   string // absolute, guarded
	existed  bool
	preImage []byte
}

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
	ops, err := e.buildOps(p)
	if err != nil {
		return err
	}
	if len(ops) == 0 {
		return ErrReviewOnly
	}
	if len(ops) > scaffoldMaxOps {
		return ErrTooManyOps
	}
	if p.BlastRadius == persistence.ProposalScopeDaemon && !ackDaemon {
		return ErrDaemonAckRequired
	}
	total := 0
	for _, op := range ops {
		total += len(op.Content)
	}
	if total > applyMaxContentBytes {
		return ErrContentTooLarge
	}
	// Deterministic ordering (design §4): creates first, then replaces, with
	// config.yaml replaced LAST so a crash leaves referenced files already on
	// disk (orphans inert). Enforced by the engine, not the generator.
	orderOps(ops)
	// Busy check: daemon-scope looks across all projects (""), else the
	// proposal's project. SKIPPED when the proposer declared the change
	// live-apply (non-disruptive to in-flight tasks) — the all-projects gate
	// never opens on a busy daemon, so a daemon-scope MCP add/remove would
	// otherwise be un-appliable in production. Everything else below (base-hash,
	// validate, atomic write, reload+rollback) still runs unchanged. See
	// https://docs.vornik.io
	if !p.LiveApply && e.HasActiveTasks != nil {
		scope := p.ProjectID
		if p.BlastRadius == persistence.ProposalScopeDaemon {
			scope = ""
		}
		busy, berr := e.HasActiveTasks(ctx, scope)
		if berr != nil {
			return fmt.Errorf("busy check failed: %w", berr)
		}
		if busy {
			return ErrBusy
		}
	}
	// Resolve + pre-flight + validate every op BEFORE any write.
	resolved := make([]resolvedOp, 0, len(ops))
	for _, op := range ops {
		target, terr := e.resolveTarget(op.Path)
		if terr != nil {
			return terr
		}
		pre, existed, rerr := readIfExists(target)
		if rerr != nil {
			return fmt.Errorf("read %s for snapshot: %w", op.Path, rerr)
		}
		switch op.Op {
		case applyOpCreate:
			if existed {
				return fmt.Errorf("%w: create target %s already exists", ErrScaffoldConflict, op.Path)
			}
		case applyOpReplace:
			if !existed {
				return fmt.Errorf("%w: replace target %s does not exist", ErrScaffoldConflict, op.Path)
			}
		default:
			return fmt.Errorf("unknown apply op %q for %s", op.Op, op.Path)
		}
		if e.Validate != nil {
			if verr := e.Validate(op.Path, op.Content); verr != nil {
				return fmt.Errorf("validation failed for %s: %w", op.Path, verr)
			}
		}
		resolved = append(resolved, resolvedOp{op: op, target: target, existed: existed, preImage: pre})
	}
	// Optimistic concurrency: if the proposal recorded a base hash (hub-authored
	// config edit), refuse when the on-disk file has changed since drafting.
	if berr := verifyBaseHash(p, resolved); berr != nil {
		return berr
	}
	// Snapshot: single legacy replace keeps the bare pre-image string (Phase-2a
	// back-compat); multi-op uses the versioned JSON envelope (§3).
	snapshot := buildSnapshot(p, resolved)
	// Write every op; on any write failure reverse what we wrote and abort.
	written := make([]resolvedOp, 0, len(resolved))
	for _, ro := range resolved {
		if werr := atomicWrite(ro.target, []byte(ro.op.Content)); werr != nil {
			e.reverseWrites(written)
			_ = e.reload()
			return fmt.Errorf("apply write failed for %s (reversed): %w", ro.op.Path, werr)
		}
		written = append(written, ro)
	}
	// Reload; on failure reverse every write, restore known-good, stay APPROVED.
	if rerr := e.reload(); rerr != nil {
		e.reverseWrites(written)
		_ = e.reload()
		e.Logger.Warn().Err(rerr).Str("proposal_id", id).
			Msg("control-plane: apply reload rejected; reversed all writes, proposal stays APPROVED")
		return fmt.Errorf("apply reload rejected (reversed): %w", rerr)
	}
	// Reload OK — persist APPLIED + snapshot. On DB failure reverse the writes
	// so file+ledger never diverge.
	if merr := e.Proposals.MarkApplied(ctx, id, actor, snapshot); merr != nil {
		e.reverseWrites(written)
		_ = e.reload()
		return fmt.Errorf("apply recorded failed, reversed writes: %w", merr)
	}
	e.Logger.Info().Str("proposal_id", id).Int("ops", len(written)).Str("applied_by", actor).
		Msg("control-plane: proposal applied")
	return nil
}

// buildOps derives the ordered op list from a proposal: the multi-op ApplyOps
// JSON when present, else the single (ApplyTarget, ApplyContent) as a one-op
// replace (Phase-2a back-compat). Empty ⇒ review-only.
func (e *ApplyEngine) buildOps(p *persistence.ControlPlaneProposal) ([]applyFileOp, error) {
	if strings.TrimSpace(p.ApplyOps) != "" {
		var ops []applyFileOp
		if err := json.Unmarshal([]byte(p.ApplyOps), &ops); err != nil {
			return nil, fmt.Errorf("apply_ops parse: %w", err)
		}
		return ops, nil
	}
	if strings.TrimSpace(p.ApplyTarget) == "" {
		return nil, nil
	}
	return []applyFileOp{{Op: applyOpReplace, Path: p.ApplyTarget, Content: p.ApplyContent}}, nil
}

// orderOps sorts creates before replaces, with a config.yaml replace last so a
// crash mid-write leaves referenced files present (orphans inert; §4).
func orderOps(ops []applyFileOp) {
	rank := func(o applyFileOp) int {
		if o.Op == applyOpCreate {
			return 0
		}
		if filepath.Base(o.Path) == "config.yaml" {
			return 2
		}
		return 1
	}
	sort.SliceStable(ops, func(i, j int) bool { return rank(ops[i]) < rank(ops[j]) })
}

// buildSnapshot returns the rollback snapshot string: a bare pre-image for a
// single legacy replace (Phase-2a), else the versioned JSON envelope.
func buildSnapshot(p *persistence.ControlPlaneProposal, resolved []resolvedOp) string {
	if strings.TrimSpace(p.ApplyOps) == "" && len(resolved) == 1 {
		return string(resolved[0].preImage)
	}
	env := snapshotEnvelope{Version: snapshotEnvelopeVersion, Entries: map[string]snapshotEntry{}}
	for _, ro := range resolved {
		e := snapshotEntry{Existed: ro.existed}
		if ro.existed {
			e.Content = string(ro.preImage)
		}
		env.Entries[ro.op.Path] = e
	}
	b, _ := json.Marshal(env)
	return string(b)
}

// reverseWrites undoes applied writes in reverse order: delete created files,
// restore replaced files from their pre-image. A reverse failure is CRITICAL
// (file+ledger may diverge) but never panics.
func (e *ApplyEngine) reverseWrites(written []resolvedOp) {
	for i := len(written) - 1; i >= 0; i-- {
		ro := written[i]
		var err error
		if ro.existed {
			err = atomicWrite(ro.target, ro.preImage)
		} else {
			err = os.Remove(ro.target)
			if os.IsNotExist(err) {
				err = nil
			}
		}
		if err != nil {
			e.Logger.Error().Err(err).Str("target", ro.target).
				Msg("control-plane: CRITICAL reverse-write failed; file+ledger may diverge — free the resource and re-apply")
		}
	}
}

// verifyBaseHash enforces optimistic concurrency for hub-authored config edits.
// If the proposal's Evidence carries {"base_hash":"<sha256>"}, the op editing
// that path must find the on-disk pre-image hashing to the same value — else the
// file changed since the proposal was drafted (ErrStaleBase). Proposals without
// a base hash (workers, scaffold, legacy) skip the check entirely.
func verifyBaseHash(p *persistence.ControlPlaneProposal, resolved []resolvedOp) error {
	if strings.TrimSpace(p.Evidence) == "" {
		return nil
	}
	var ev struct {
		BaseHash string `json:"base_hash"`
	}
	if err := json.Unmarshal([]byte(p.Evidence), &ev); err != nil || ev.BaseHash == "" {
		return nil // Evidence isn't a base-hash envelope — not a hub config edit
	}
	// The base hash covers the (single) config-target file this proposal edits.
	for _, ro := range resolved {
		if !ro.existed {
			continue
		}
		if hashBytes(ro.preImage) == ev.BaseHash {
			return nil
		}
	}
	return ErrStaleBase
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// readIfExists returns (bytes, true, nil) if the file exists, (nil, false, nil)
// if absent, or an error for any other read failure.
func readIfExists(path string) ([]byte, bool, error) {
	b, err := os.ReadFile(path) //nolint:gosec // path is guarded by resolveTarget
	if err == nil {
		return b, true, nil
	}
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	return nil, false, err
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
	if err := e.restoreSnapshot(p); err != nil {
		return err
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

// restoreSnapshot restores a proposal's pre-apply state. A single legacy
// replace (no ApplyOps) restores the bare pre-image to ApplyTarget; a multi-op
// proposal parses the JSON envelope and, per entry, deletes created files
// (existed=false) or restores replaced files (existed=true).
func (e *ApplyEngine) restoreSnapshot(p *persistence.ControlPlaneProposal) error {
	if strings.TrimSpace(p.ApplyOps) == "" {
		target, err := e.resolveTarget(p.ApplyTarget)
		if err != nil {
			return err
		}
		if err := atomicWrite(target, []byte(p.PreApplySnapshot)); err != nil {
			return fmt.Errorf("rollback write failed: %w", err)
		}
		return nil
	}
	var env snapshotEnvelope
	if err := json.Unmarshal([]byte(p.PreApplySnapshot), &env); err != nil || env.Version == 0 {
		return ErrSnapshotMissing
	}
	for relPath, entry := range env.Entries {
		target, err := e.resolveTarget(relPath)
		if err != nil {
			return err
		}
		if entry.Existed {
			if err := atomicWrite(target, []byte(entry.Content)); err != nil {
				return fmt.Errorf("rollback restore %s failed: %w", relPath, err)
			}
			continue
		}
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("rollback delete %s failed: %w", relPath, err)
		}
	}
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
