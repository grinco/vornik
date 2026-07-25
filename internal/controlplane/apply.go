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
	"vornik.io/vornik/internal/safepath"
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
	// ErrRollbackTargetDrifted means the proposal's target changed since it was
	// applied — a later overlapping apply overwrote it, or it was hand-edited — so
	// restoring this proposal's snapshot would clobber that change. Roll back the
	// newer change first (design 2026-07-23 §D).
	ErrRollbackTargetDrifted = errors.New("control-plane: rollback target changed since it was applied (a later change overwrote it, or it was hand-edited); roll back the newer change first")
	// ErrTradingSwarmRefused means a cost/quality-detector proposal was refused at
	// apply time because its typed change targets a trading/broker swarm — the
	// applier-side mirror of the detector-side trading exclusion (defense in depth,
	// review-20260721-a7bf #6; design 2026-07-24-applier-trading-refusal). Produced
	// by (*Actionizer).RefuseTradingTarget via the ValidateChange hook.
	// Operator-authored proposals are NOT subject to it.
	ErrTradingSwarmRefused = errors.New("control-plane: refusing to apply a cost/quality-detector proposal to a trading swarm")
)

// AutoRetireStaleActor is the approver stamped on a proposal auto-retired
// (→ REJECTED) because its config target drifted since drafting (design
// 2026-07-23 §B). The authoring detector re-files a fresh proposal.
const AutoRetireStaleActor = "system:auto-retire-stale"

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
	// ValidateChange semantically re-validates a proposal's typed Evidence
	// "change" against CURRENT state before any write (actionable-proposals
	// §4.5: e.g. a model deprecated between draft and apply). Nil → skipped.
	// Distinct from Validate (per-file syntax): this one sees the proposal.
	ValidateChange func(ctx context.Context, p *persistence.ControlPlaneProposal) error
	// Mirror best-effort propagates the final on-disk state of every path
	// this proposal touched to the source config tree + git (two-trees
	// discipline, actionable-proposals §4.7). Called ONCE per successful
	// apply/rollback, after the ledger write; a nil byte slice means the
	// path was deleted (rollback of a create). Errors only WARN — the
	// deployed tree is the daemon's source of truth. Nil → no mirroring.
	Mirror func(proposalID string, files map[string][]byte) error
	Logger zerolog.Logger

	// KindAppliers routes proposal Kinds that mutate application state
	// directly (rather than a deployed config file) to a registered
	// KindApplier — see kind_applier.go. Nil/absent-kind falls through to
	// the file-based apply path unchanged.
	KindAppliers map[string]KindApplier

	mu sync.Mutex // global apply lock (serialises all applies + rollbacks)
}

// resolveTarget joins + cleans apply_target under ConfigDir and rejects any
// path that escapes it.
func (e *ApplyEngine) resolveTarget(rel string) (string, error) {
	// Route through the canonical symlink-resolving guard (audit 2026-07-09
	// LOW-4 / F-1): JoinUnder resolves symlinks in the deepest existing
	// prefix and re-checks containment, so a pre-existing symlink inside the
	// config tree pointing outside it can't be written through — the lexical
	// Clean+HasPrefix guard this replaces did not resolve symlinks.
	//
	// rel is proposal-authored, so absolute paths must be rejected rather than
	// silently re-targeted under ConfigDir. JoinUnderRel keeps that input-shape
	// contract while preserving JoinUnder's containment checks.
	full, err := safepath.JoinUnderRel(e.ConfigDir, rel)
	if err != nil {
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
	if ka := e.kindApplier(p.Kind); ka != nil {
		// Same daemon-ack gate as the file path below (apply.go's
		// ProposalScopeDaemon/ProposalScopeSwarm check) — a kind-applier
		// proposal is still subject to the blast-radius acknowledgement.
		if (p.BlastRadius == persistence.ProposalScopeDaemon || p.BlastRadius == persistence.ProposalScopeSwarm) && !ackDaemon {
			return ErrDaemonAckRequired
		}
		snap, aerr := ka.Apply(ctx, p)
		if aerr != nil {
			return aerr
		}
		if merr := e.Proposals.MarkApplied(ctx, id, actor, snap); merr != nil {
			return fmt.Errorf("apply recorded failed: %w", merr)
		}
		e.Logger.Info().Str("proposal_id", id).Str("kind", p.Kind).
			Msg("control-plane: kind-applier proposal applied")
		return nil
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
	// Daemon scope affects every project; swarm scope affects every project
	// referencing the swarm file (actionable-proposals §4.5) — both demand
	// the explicit second ack.
	if (p.BlastRadius == persistence.ProposalScopeDaemon || p.BlastRadius == persistence.ProposalScopeSwarm) && !ackDaemon {
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
		if errors.Is(berr, ErrStaleBase) {
			if serr := e.Proposals.SetStatus(ctx, id, persistence.ProposalStatusRejected, AutoRetireStaleActor); serr != nil {
				e.Logger.Warn().Err(serr).Str("proposal_id", id).
					Msg("control-plane: stale proposal auto-retire failed")
			} else {
				e.Logger.Info().Str("proposal_id", id).
					Msg("control-plane: stale proposal auto-retired (config drifted since drafted)")
			}
		}
		return berr
	}
	// Semantic re-validation of the typed change against current state
	// (actionable-proposals §4.5) — after all preconditions, before any write.
	if e.ValidateChange != nil {
		if verr := e.ValidateChange(ctx, p); verr != nil {
			return fmt.Errorf("change re-validation failed: %w", verr)
		}
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
	// Two-trees mirror (§4.7): once per proposal, final state only, after the
	// ledger write. Best-effort — a failure leaves the deployed apply intact.
	e.mirror(id, mirrorSetFromWritten(written))
	return nil
}

// mirror invokes the Mirror hook nil-safely, demoting errors to a WARN.
func (e *ApplyEngine) mirror(proposalID string, files map[string][]byte) {
	if e.Mirror == nil || len(files) == 0 {
		return
	}
	if err := e.Mirror(proposalID, files); err != nil {
		e.Logger.Warn().Err(err).Str("proposal_id", proposalID).
			Msg("control-plane: applied to deployed tree; source-tree mirror failed — sync by hand")
	}
}

// mirrorSetFromWritten maps each written op's rel path to its final content.
func mirrorSetFromWritten(written []resolvedOp) map[string][]byte {
	files := make(map[string][]byte, len(written))
	for _, ro := range written {
		files[ro.op.Path] = []byte(ro.op.Content)
	}
	return files
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
	base, ok := parseBaseHash(p.Evidence)
	if !ok {
		return nil // Evidence isn't a base-hash envelope — not a hub config edit
	}
	// The base hash covers the (single) config-target file this proposal edits.
	for _, ro := range resolved {
		if !ro.existed {
			continue
		}
		if hashBytes(ro.preImage) == base {
			return nil
		}
	}
	return ErrStaleBase
}

// parseBaseHash extracts a proposal Evidence's optional {"base_hash":"..."}.
// ok=false when Evidence is empty, not JSON, or carries no base_hash.
func parseBaseHash(evidence string) (string, bool) {
	if strings.TrimSpace(evidence) == "" {
		return "", false
	}
	var ev struct {
		BaseHash string `json:"base_hash"`
	}
	if err := json.Unmarshal([]byte(evidence), &ev); err != nil || ev.BaseHash == "" {
		return "", false
	}
	return ev.BaseHash, true
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

// proposalTargets returns the rel target paths a proposal writes (multi-op ops,
// else the single ApplyTarget; empty for review-only / kind-applier proposals).
func proposalTargets(p *persistence.ControlPlaneProposal) []string {
	if strings.TrimSpace(p.ApplyOps) != "" {
		var ops []applyFileOp
		if err := json.Unmarshal([]byte(p.ApplyOps), &ops); err == nil {
			out := make([]string, 0, len(ops))
			for _, o := range ops {
				out = append(out, o.Path)
			}
			return out
		}
	}
	if strings.TrimSpace(p.ApplyTarget) != "" {
		return []string{p.ApplyTarget}
	}
	return nil
}

// targetsOverlap reports whether two rel-path sets share any path.
func targetsOverlap(a, b []string) bool {
	set := make(map[string]struct{}, len(a))
	for _, x := range a {
		set[x] = struct{}{}
	}
	for _, y := range b {
		if _, ok := set[y]; ok {
			return true
		}
	}
	return false
}

// expectedContentByPath maps each rel path a proposal writes to the content it
// wrote (byte-exact; the apply engine writes op.Content/ApplyContent verbatim).
func expectedContentByPath(p *persistence.ControlPlaneProposal) map[string]string {
	m := map[string]string{}
	if strings.TrimSpace(p.ApplyOps) != "" {
		var ops []applyFileOp
		if err := json.Unmarshal([]byte(p.ApplyOps), &ops); err == nil {
			for _, o := range ops {
				m[o.Path] = o.Content
			}
		}
		return m
	}
	if strings.TrimSpace(p.ApplyTarget) != "" {
		m[p.ApplyTarget] = p.ApplyContent
	}
	return m
}

// rollbackUnsafe reports whether rolling P back would clobber a later change:
// ordering (a later-or-equal overlapping APPLIED proposal exists) or drift (disk
// no longer matches what P applied). Returns an error only for infra failures
// (list/read); the bool is the safety verdict.
func (e *ApplyEngine) rollbackUnsafe(ctx context.Context, p *persistence.ControlPlaneProposal) (bool, error) {
	// A non-empty ApplyOps that won't parse means we can't reason about this
	// proposal's targets — fail closed (refuse the rollback) rather than proceed
	// unguarded.
	if s := strings.TrimSpace(p.ApplyOps); s != "" {
		var ops []applyFileOp
		if err := json.Unmarshal([]byte(s), &ops); err != nil {
			return true, nil
		}
	}
	targets := proposalTargets(p)
	if len(targets) == 0 {
		return false, nil
	}
	// Ordering: is any OTHER applied proposal that overlaps P's targets applied
	// at-or-after P? If so, P is not the live top — refuse (content-independent,
	// closes the identical-content case).
	applied, err := e.Proposals.List(ctx, persistence.ProposalListFilter{Statuses: []string{persistence.ProposalStatusApplied}})
	if err != nil {
		return false, fmt.Errorf("rollback safety: list applied: %w", err)
	}
	for _, q := range applied {
		if q == nil || q.ID == p.ID {
			continue
		}
		if !targetsOverlap(targets, proposalTargets(q)) {
			continue
		}
		// q supersedes p if it applied strictly later, or at the same instant with a
		// higher ID (stable tie-break → exactly one of an equal-timestamp pair is the
		// live top). Unknown timestamps can't be ordered: fail closed (treat p as not
		// top when an overlapping APPLIED q exists) rather than risk clobbering q.
		if p.AppliedAt == nil || q.AppliedAt == nil {
			return true, nil
		}
		if q.AppliedAt.After(*p.AppliedAt) || (q.AppliedAt.Equal(*p.AppliedAt) && q.ID > p.ID) {
			return true, nil
		}
	}
	// Drift: disk must still equal exactly what P applied.
	for rel, want := range expectedContentByPath(p) {
		full, terr := e.resolveTarget(rel)
		if terr != nil {
			return false, terr
		}
		cur, existed, rerr := readIfExists(full)
		if rerr != nil {
			return false, fmt.Errorf("rollback safety: read %s: %w", rel, rerr)
		}
		if !existed || hashBytes(cur) != hashBytes([]byte(want)) {
			return true, nil
		}
	}
	return false, nil
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
	if ka := e.kindApplier(p.Kind); ka != nil {
		if err := ka.Rollback(ctx, p); err != nil {
			return err
		}
		if err := e.Proposals.MarkRolledBack(ctx, id); err != nil {
			return fmt.Errorf("rollback recorded failed: %w", err)
		}
		e.Logger.Info().Str("proposal_id", id).Str("kind", p.Kind).
			Msg("control-plane: kind-applier proposal rolled back")
		return nil
	}
	if unsafe, uerr := e.rollbackUnsafe(ctx, p); uerr != nil {
		return uerr
	} else if unsafe {
		e.Logger.Warn().Str("proposal_id", id).
			Msg("control-plane: rollback refused — target overwritten by a later change or hand-edited")
		return ErrRollbackTargetDrifted
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
	e.mirror(id, mirrorSetFromSnapshot(p))
	return nil
}

// mirrorSetFromSnapshot maps each restored path to its restored content
// (nil = the rollback deleted a created file).
func mirrorSetFromSnapshot(p *persistence.ControlPlaneProposal) map[string][]byte {
	if strings.TrimSpace(p.ApplyOps) == "" {
		return map[string][]byte{p.ApplyTarget: []byte(p.PreApplySnapshot)}
	}
	var env snapshotEnvelope
	if err := json.Unmarshal([]byte(p.PreApplySnapshot), &env); err != nil {
		return nil
	}
	files := make(map[string][]byte, len(env.Entries))
	for rel, entry := range env.Entries {
		if entry.Existed {
			files[rel] = []byte(entry.Content)
		} else {
			files[rel] = nil
		}
	}
	return files
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
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("sync parent dir: %w", err)
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}
