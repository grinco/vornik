package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"vornik.io/vornik/internal/leaderelection"
	"vornik.io/vornik/internal/persistence"
)

// Journaled apply — the durable multi-file path of ApplyEngine.Apply
// (https://docs.vornik.io).
//
// apply.go's legacy path writes N files and reverses in memory; a crash
// between the second rename and MarkApplied leaves the tree half-new and the
// ledger APPROVED. When Journal is set, Apply instead commits a PREPARED row
// carrying every pre-image and read-set hash BEFORE the first temp file
// (§1.1), records each committed rename (§3), and lands APPLIED together
// with the ledger in one transaction — so Reconcile can finish a
// proven-complete generation or restore every pre-image after a restart,
// never guess (§5). The legacy path is untouched when Journal is nil.

var (
	// ErrJournalDrift means a target matched neither its pre-image nor the
	// intended content during recovery (§1.4). The file is left alone, the
	// row parks in DRIFT, and its projects stay blocked until an operator
	// resolves it with `vornikctl control-plane journal resolve`.
	ErrJournalDrift = errors.New("control-plane: config target drifted under recovery; operator resolution required")
	// ErrWriterFenced means the config-writer lease epoch was superseded
	// between PREPARE and a write (§1.2); the apply aborted into recovery.
	ErrWriterFenced = errors.New("control-plane: config-writer lease superseded; apply aborted")
	// ErrJournalReconciled means Apply found an open journal for this
	// proposal (a crashed earlier attempt) and reconciled it instead of
	// preparing a second one (R8). The wrapped detail names the outcome; if
	// the proposal is still APPROVED it may be re-applied.
	ErrJournalReconciled = errors.New("control-plane: an open apply journal for this proposal was reconciled instead of re-applied")
	// ErrJournalRootMismatch means an open row was prepared against a
	// different deployment root than this engine's ConfigDir; it is left
	// alone rather than reconciled against the wrong tree.
	ErrJournalRootMismatch = errors.New("control-plane: apply journal deployment root does not match this config dir")

	// errCrashInjected is what the test crash point returns: the engine
	// stops dead, exactly where a process crash would, with no further
	// journal write.
	errCrashInjected = errors.New("control-plane: injected crash")
)

// JournalReconcileActor is the applied_by stamped on a proposal whose apply
// was completed by Reconcile after a crash rather than by the operator's
// original call (§5). The approver remains the human who approved it.
const JournalReconcileActor = "system:journal-reconcile"

// JournaledOp is one op as the journal and the engine hooks see it.
type JournaledOp struct {
	Op            string
	Path          string
	Content       string
	ContentSHA256 string
	Existed       bool
}

// journalEvidence is the Evidence envelope the journaled path reads:
// read_set maps a read dependency's rel path to its sha256 or "ABSENT".
type journalEvidence struct {
	ReadSet map[string]string `json:"read_set"`
}

// affectedProjectsAll is the affected_projects marker for a daemon-scope
// apply: every project is affected.
const affectedProjectsAll = "*"

// crash consults the injected crash point. Production never sets it.
func (e *ApplyEngine) crash(stage string) bool {
	return e.crashAfter != nil && e.crashAfter(stage)
}

// fenced reports the writer-lease verdict (§1.2). A nil gate proceeds, as
// leaderelection.DangerousWriteAllowed already does.
func (e *ApplyEngine) fenced(ctx context.Context) (bool, string) {
	ok, reason := leaderelection.DangerousWriteAllowed(ctx, e.LeaderGate)
	if !ok {
		leaderelection.LeaderFenceRejected("config_writer")
	}
	return !ok, reason
}

func (e *ApplyEngine) writerEpoch() int64 {
	if e.WriterEpoch == nil {
		return 0
	}
	return e.WriterEpoch()
}

func (e *ApplyEngine) generation() string {
	if e.Generation == nil {
		return ""
	}
	return e.Generation()
}

// RestartPending reports whether the last journaled apply in this process
// ended PENDING_RESTART. Process-local by design: a restart is what clears
// it (§3 "the operator restarts via vornikctl").
func (e *ApplyEngine) RestartPending() bool { return e.pendingRestart.Load() }

// journaledOps derives the hook-facing view of resolved ops.
func journaledOps(resolved []resolvedOp) []JournaledOp {
	out := make([]JournaledOp, 0, len(resolved))
	for _, ro := range resolved {
		out = append(out, JournaledOp{Op: ro.op.Op, Path: ro.op.Path, Content: ro.op.Content,
			ContentSHA256: hashBytes([]byte(ro.op.Content)), Existed: ro.existed})
	}
	return out
}

// parseReadSet extracts the optional {"read_set":{...}} from Evidence. An
// absent key — or evidence that is not a JSON object at all (detector
// metrics, a bare base_hash envelope) — is an empty read set, the same
// tolerance parseBaseHash extends.
func parseReadSet(evidence string) map[string]string {
	var ev journalEvidence
	if strings.TrimSpace(evidence) == "" || json.Unmarshal([]byte(evidence), &ev) != nil || ev.ReadSet == nil {
		return map[string]string{}
	}
	return ev.ReadSet
}

// checkReadSet returns the first read dependency whose current bytes differ
// from what the proposal grounded on (§4), or "" when every one holds.
func (e *ApplyEngine) checkReadSet(readSet map[string]string) (string, error) {
	for rel, want := range readSet {
		full, err := e.resolveTarget(rel)
		if err != nil {
			return "", err
		}
		cur, existed, rerr := readIfExists(full)
		if rerr != nil {
			return "", fmt.Errorf("read dependency %s: %w", rel, rerr)
		}
		if want == persistence.JournalReadSetAbsent {
			if existed {
				return rel, nil
			}
			continue
		}
		if !existed || hashBytes(cur) != want {
			return rel, nil
		}
	}
	return "", nil
}

// checkTargets returns the first op target whose current bytes differ from
// its captured pre-image (or that appeared where absence was expected), or
// "" when every target still matches (§4 first bullet).
func checkTargets(resolved []resolvedOp) (string, error) {
	for _, ro := range resolved {
		cur, existed, err := readIfExists(ro.target)
		if err != nil {
			return "", fmt.Errorf("re-read %s: %w", ro.op.Path, err)
		}
		if existed != ro.existed || (existed && hashBytes(cur) != hashBytes(ro.preImage)) {
			return ro.op.Path, nil
		}
	}
	return "", nil
}

// buildJournalRow assembles the PREPARED row (§2) from the resolved ops.
func (e *ApplyEngine) buildJournalRow(p *persistence.ControlPlaneProposal, resolved []resolvedOp, readSet map[string]string) *persistence.ConfigApplyJournal {
	ops := make([]persistence.JournalOp, 0, len(resolved))
	pre := make(map[string]persistence.JournalPreImage, len(resolved))
	for _, ro := range resolved {
		ops = append(ops, persistence.JournalOp{Op: ro.op.Op, Path: ro.op.Path, ContentSHA256: hashBytes([]byte(ro.op.Content))})
		img := persistence.JournalPreImage{Existed: ro.existed}
		if ro.existed {
			img.SHA256 = hashBytes(ro.preImage)
			img.Content = string(ro.preImage)
		}
		pre[ro.op.Path] = img
	}
	root, err := filepath.EvalSymlinks(e.ConfigDir)
	if err != nil {
		root = filepath.Clean(e.ConfigDir)
	}
	affected := []string{affectedProjectsAll}
	if p.ProjectID != "" && p.BlastRadius != persistence.ProposalScopeDaemon {
		affected = []string{p.ProjectID}
	}
	var reqID string
	var ev struct {
		RequestID string `json:"request_id"`
	}
	if json.Unmarshal([]byte(p.Evidence), &ev) == nil {
		reqID = ev.RequestID
	}
	if reqID == "" {
		reqID = p.RequestID
	}
	return &persistence.ConfigApplyJournal{
		ID:               persistence.GenerateID("caj"),
		ProposalID:       p.ID,
		RequestID:        reqID,
		OpDigest:         opDigest(ops),
		SchemaVersion:    persistence.JournalSchemaVersion,
		DeploymentRoot:   root,
		AffectedProjects: affected,
		WriterEpoch:      e.writerEpoch(),
		Ops:              ops,
		PreImages:        pre,
		ReadSet:          readSet,
		GenerationBefore: e.generation(),
	}
}

// opDigest is sha256 over the canonical (ordered) op list.
func opDigest(ops []persistence.JournalOp) string {
	b, _ := json.Marshal(ops)
	return hashBytes(b)
}

// failPrepared closes a PREPARED row as FAILED (§3: only when no file was
// mutated) and, when retireProposalID is set, auto-retires that proposal
// exactly as the legacy ErrStaleBase path does.
func (e *ApplyEngine) failPrepared(ctx context.Context, row *persistence.ConfigApplyJournal, failure, retireProposalID string) {
	if err := e.Journal.Transition(ctx, row.ID, persistence.JournalStatePrepared, persistence.JournalStateFailed,
		persistence.JournalPatch{Failure: failure, Terminal: true}); err != nil {
		e.Logger.Error().Err(err).Str("journal_id", row.ID).Msg("control-plane: journal PREPARED→FAILED write failed")
	}
	if retireProposalID != "" {
		e.autoRetireStale(ctx, retireProposalID)
	}
}

// autoRetireStale is the ErrStaleBase auto-retire (design 2026-07-23 §B),
// shared with the legacy path's semantics.
func (e *ApplyEngine) autoRetireStale(ctx context.Context, id string) {
	if serr := e.Proposals.SetStatus(ctx, id, persistence.ProposalStatusRejected, AutoRetireStaleActor); serr != nil {
		e.Logger.Warn().Err(serr).Str("proposal_id", id).Msg("control-plane: stale proposal auto-retire failed")
		return
	}
	e.Logger.Info().Str("proposal_id", id).Msg("control-plane: stale proposal auto-retired (config drifted since drafted)")
}

// applyJournaled is the journaled continuation of Apply, entered after every
// pre-write check of the legacy path has passed and still under e.mu.
func (e *ApplyEngine) applyJournaled(ctx context.Context, p *persistence.ControlPlaneProposal, actor string, resolved []resolvedOp) error {
	readSet := parseReadSet(p.Evidence)
	// §4 pass 1: before PREPARED is committed. No row exists yet, so drift
	// here is the plain ErrStaleBase auto-retire.
	if drifted, cerr := e.checkReadSet(readSet); cerr != nil {
		return cerr
	} else if drifted != "" {
		e.autoRetireStale(ctx, p.ID)
		return fmt.Errorf("%w: read dependency %s changed", ErrStaleBase, drifted)
	}
	row := e.buildJournalRow(p, resolved, readSet)
	if perr := e.Journal.Prepare(ctx, row); perr != nil {
		if errors.Is(perr, persistence.ErrDuplicateKey) {
			open, oerr := e.Journal.OpenForProposal(ctx, p.ID)
			if oerr != nil {
				return fmt.Errorf("journal prepare collided but no open row found: %w", oerr)
			}
			return e.resumeOpenJournal(ctx, open)
		}
		return fmt.Errorf("journal prepare: %w", perr)
	}
	if e.crash("prepared") {
		return errCrashInjected
	}
	// §4 pass 2: immediately before write #1, same lock. A hand edit in the
	// window since PREPARED is caught here with nothing mutated (§7.14).
	if drifted, cerr := e.recheckBeforeFirstWrite(readSet, resolved); cerr != nil {
		e.failPrepared(ctx, row, "recheck: "+cerr.Error(), "")
		return cerr
	} else if drifted != "" {
		e.failPrepared(ctx, row, "drift before write #1: "+drifted, p.ID)
		return fmt.Errorf("%w: %s changed since this proposal was drafted", ErrStaleBase, drifted)
	}
	if isFenced, reason := e.fenced(ctx); isFenced {
		e.failPrepared(ctx, row, "fence before write #1: "+reason, "")
		return fmt.Errorf("%w: %s", ErrWriterFenced, reason)
	}
	if werr := e.writeJournaled(ctx, row, resolved); werr != nil {
		return werr
	}
	return e.verifyAndCommit(ctx, row, p, actor, resolved)
}

// recheckBeforeFirstWrite is §4 pass 2: every op target and every read
// dependency, immediately before write #1.
func (e *ApplyEngine) recheckBeforeFirstWrite(readSet map[string]string, resolved []resolvedOp) (string, error) {
	if drifted, err := checkTargets(resolved); err != nil || drifted != "" {
		return drifted, err
	}
	return e.checkReadSet(readSet)
}

// writeJournaled performs the write loop of §3: fence, per-target
// pre-image recheck, atomic write, PREPARED→APPLYING after the first
// rename, progress append after each. Any failure after write #1 goes
// through revert; a failure with nothing mutated closes the row FAILED.
func (e *ApplyEngine) writeJournaled(ctx context.Context, row *persistence.ConfigApplyJournal, resolved []resolvedOp) error {
	for i, ro := range resolved {
		if i > 0 {
			if isFenced, reason := e.fenced(ctx); isFenced {
				return e.abortWrites(ctx, row, resolved, i, "fence before write #"+fmt.Sprint(i+1)+": "+reason, fmt.Errorf("%w: %s", ErrWriterFenced, reason))
			}
			if drifted, err := checkTargets(resolved[i:]); err != nil || drifted != "" {
				if err == nil {
					err = fmt.Errorf("%w: %s changed during apply", ErrStaleBase, drifted)
				}
				return e.abortWrites(ctx, row, resolved, i, "drift before write #"+fmt.Sprint(i+1)+": "+drifted, err)
			}
		}
		if werr := atomicWrite(ro.target, []byte(ro.op.Content)); werr != nil {
			return e.abortWrites(ctx, row, resolved, i, "write failed for "+ro.op.Path+": "+werr.Error(),
				fmt.Errorf("apply write failed for %s: %w", ro.op.Path, werr))
		}
		if i == 0 {
			if terr := e.Journal.Transition(ctx, row.ID, persistence.JournalStatePrepared, persistence.JournalStateApplying, persistence.JournalPatch{}); terr != nil {
				return e.abortWrites(ctx, row, resolved, i+1, "journal PREPARED→APPLYING failed: "+terr.Error(), fmt.Errorf("journal: %w", terr))
			}
		}
		if e.crash("write:" + ro.op.Path) {
			return errCrashInjected
		}
		if perr := e.Journal.AppendProgress(ctx, row.ID, ro.op.Path); perr != nil {
			return e.abortWrites(ctx, row, resolved, i+1, "journal progress append failed: "+perr.Error(), fmt.Errorf("journal: %w", perr))
		}
		if e.crash("progress:" + ro.op.Path) {
			return errCrashInjected
		}
	}
	if terr := e.Journal.Transition(ctx, row.ID, persistence.JournalStateApplying, persistence.JournalStateVerifying, persistence.JournalPatch{}); terr != nil {
		return e.abortWrites(ctx, row, resolved, len(resolved), "journal APPLYING→VERIFYING failed: "+terr.Error(), fmt.Errorf("journal: %w", terr))
	}
	if e.crash("verifying") {
		return errCrashInjected
	}
	return nil
}

// abortWrites routes a write-loop failure: nothing mutated (written == 0 and
// every target still at its pre-image) → PREPARED→FAILED; otherwise the
// revert path. cause is returned to the caller unless revert drifts.
func (e *ApplyEngine) abortWrites(ctx context.Context, row *persistence.ConfigApplyJournal, resolved []resolvedOp, written int, failure string, cause error) error {
	if written == 0 {
		if drifted, err := checkTargets(resolved); err == nil && drifted == "" {
			e.failPrepared(ctx, row, failure, "")
			return cause
		}
	}
	if rerr := e.revertJournaled(ctx, row, resolved, failure); rerr != nil {
		return rerr
	}
	return cause
}

// verifyAndCommit is VERIFYING → APPLIED | PENDING_RESTART (§3): reload,
// confirm the resolved generation (§1.3), then land the journal's terminal
// state with the ledger in one transaction. Any failure reverts.
func (e *ApplyEngine) verifyAndCommit(ctx context.Context, row *persistence.ConfigApplyJournal, p *persistence.ControlPlaneProposal, actor string, resolved []resolvedOp) error {
	if rerr := e.reload(); rerr != nil {
		if verr := e.revertJournaled(ctx, row, resolved, "reload rejected: "+rerr.Error()); verr != nil {
			return verr
		}
		return fmt.Errorf("apply reload rejected (reverted): %w", rerr)
	}
	if e.VerifyGeneration != nil {
		if gerr := e.VerifyGeneration(ctx, journaledOps(resolved)); gerr != nil {
			if verr := e.revertJournaled(ctx, row, resolved, "generation not confirmed: "+gerr.Error()); verr != nil {
				return verr
			}
			return fmt.Errorf("apply generation not confirmed (reverted): %w", gerr)
		}
	}
	to := persistence.JournalStateApplied
	if e.RestartOnly != nil {
		restart, rerr := e.RestartOnly(ctx, journaledOps(resolved))
		if rerr != nil {
			if verr := e.revertJournaled(ctx, row, resolved, "restart classification failed: "+rerr.Error()); verr != nil {
				return verr
			}
			return fmt.Errorf("restart classification failed (reverted): %w", rerr)
		}
		if restart {
			to = persistence.JournalStatePendingRestart
		}
	}
	commit := persistence.JournalLedgerCommit{
		JournalID: row.ID, To: to, GenerationAfter: e.generation(),
		ProposalID: p.ID, AppliedBy: actor, Snapshot: buildSnapshot(p, resolved),
	}
	if merr := e.Journal.MarkAppliedWithLedger(ctx, commit); merr != nil {
		if verr := e.revertJournaled(ctx, row, resolved, "ledger commit failed: "+merr.Error()); verr != nil {
			return verr
		}
		return fmt.Errorf("apply recorded failed, reverted writes: %w", merr)
	}
	e.pendingRestart.Store(to == persistence.JournalStatePendingRestart)
	e.Logger.Info().Str("proposal_id", p.ID).Str("journal_id", row.ID).Str("state", to).
		Int("ops", len(resolved)).Str("applied_by", actor).Msg("control-plane: proposal applied (journaled)")
	e.mirror(p.ID, mirrorSetFromWritten(resolved))
	return nil
}

// revertJournaled is any → REVERTING → REVERTED | DRIFT (§3, §5 REVERTING
// row, §6 trusted delete). It restores in reverse op order, comparing
// hashes: a target at its pre-image is done; at the intended content is
// restored (a created file is deleted only when its bytes are exactly what
// this row wrote); anything else is DRIFT and the file is left alone.
func (e *ApplyEngine) revertJournaled(ctx context.Context, row *persistence.ConfigApplyJournal, resolved []resolvedOp, failure string) error {
	cur, err := e.Journal.Get(ctx, row.ID)
	if err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	if cur.State != persistence.JournalStateReverting {
		if terr := e.Journal.Transition(ctx, row.ID, cur.State, persistence.JournalStateReverting,
			persistence.JournalPatch{Failure: failure}); terr != nil {
			return fmt.Errorf("journal →REVERTING: %w", terr)
		}
	}
	if e.crash("reverting") {
		return errCrashInjected
	}
	for i := len(resolved) - 1; i >= 0; i-- {
		if derr := e.restoreOne(ctx, row, resolved[i]); derr != nil {
			return derr
		}
	}
	if rerr := e.reload(); rerr != nil {
		e.Logger.Error().Err(rerr).Str("journal_id", row.ID).
			Msg("control-plane: CRITICAL reload of restored pre-images failed; last-known-good left on disk")
	}
	if terr := e.Journal.Transition(ctx, row.ID, persistence.JournalStateReverting, persistence.JournalStateReverted,
		persistence.JournalPatch{GenerationAfter: e.generation(), Terminal: true}); terr != nil {
		return fmt.Errorf("journal →REVERTED: %w", terr)
	}
	e.Logger.Warn().Str("journal_id", row.ID).Str("proposal_id", row.ProposalID).Str("failure", failure).
		Msg("control-plane: apply reverted; every pre-image restored, proposal stays APPROVED")
	return nil
}

// restoreOne puts one target back to its pre-image by hash comparison. The
// only delete the engine ever performs is here: a path this row created
// (existed=false) whose current sha256 equals the content this row wrote
// (§6).
func (e *ApplyEngine) restoreOne(ctx context.Context, row *persistence.ConfigApplyJournal, ro resolvedOp) error {
	cur, existed, err := readIfExists(ro.target)
	if err != nil {
		return fmt.Errorf("revert: read %s: %w", ro.op.Path, err)
	}
	switch {
	case existed == ro.existed && (!existed || hashBytes(cur) == hashBytes(ro.preImage)):
		return nil // already at the pre-image
	case existed && hashBytes(cur) == hashBytes([]byte(ro.op.Content)):
		if ro.existed {
			if werr := atomicWrite(ro.target, ro.preImage); werr != nil {
				return fmt.Errorf("revert: restore %s: %w", ro.op.Path, werr)
			}
			return nil
		}
		if rerr := os.Remove(ro.target); rerr != nil && !os.IsNotExist(rerr) {
			return fmt.Errorf("revert: delete created %s: %w", ro.op.Path, rerr)
		}
		return nil
	default:
		return e.markDrift(ctx, row, ro.op.Path, cur, existed)
	}
}

// markDrift parks the row in DRIFT naming the path and hash (§3) and returns
// ErrJournalDrift. The file is not touched.
func (e *ApplyEngine) markDrift(ctx context.Context, row *persistence.ConfigApplyJournal, rel string, cur []byte, existed bool) error {
	have := "absent"
	if existed {
		have = hashBytes(cur)
	}
	failure := fmt.Sprintf("drift: %s is %s; matches neither pre-image nor intended content", rel, have)
	if terr := e.Journal.Transition(ctx, row.ID, persistence.JournalStateReverting, persistence.JournalStateDrift,
		persistence.JournalPatch{Failure: failure}); terr != nil {
		return fmt.Errorf("journal →DRIFT: %w", terr)
	}
	e.Logger.Error().Str("journal_id", row.ID).Str("proposal_id", row.ProposalID).Str("path", rel).
		Strs("blocked_projects", row.AffectedProjects).
		Msg("control-plane: EXTERNAL DRIFT under recovery; file left alone, projects blocked until an operator resolves the journal")
	return fmt.Errorf("%w: %s", ErrJournalDrift, failure)
}

// resumeOpenJournal is the R8 retry: Apply found an open row for this
// proposal, so reconcile it rather than prepare a second one. Nil when the
// resumed apply landed (APPLIED / PENDING_RESTART); otherwise
// ErrJournalReconciled naming the state it ended in — a DRIFT row stays an
// operator's, so the retry is refused until it is resolved.
func (e *ApplyEngine) resumeOpenJournal(ctx context.Context, open *persistence.ConfigApplyJournal) error {
	if rerr := e.reconcileRow(ctx, open); rerr != nil {
		return rerr
	}
	after, gerr := e.Journal.Get(ctx, open.ID)
	if gerr != nil {
		return gerr
	}
	if after.State == persistence.JournalStateApplied || after.State == persistence.JournalStatePendingRestart {
		return nil
	}
	return fmt.Errorf("%w: journal %s ended %s", ErrJournalReconciled, open.ID, after.State)
}

// Reconcile runs the §5 startup recovery over every open journal row. It
// takes the engine lock (ErrApplyInProgress if an apply is running) and
// continues past a failed row so one drift cannot hide another; the joined
// error names each.
func (e *ApplyEngine) Reconcile(ctx context.Context) error {
	if e.Journal == nil {
		return nil
	}
	if !e.mu.TryLock() {
		return ErrApplyInProgress
	}
	defer e.mu.Unlock()
	rows, err := e.Journal.ListOpen(ctx)
	if err != nil {
		return fmt.Errorf("journal list open: %w", err)
	}
	var errs []error
	for _, row := range rows {
		if rerr := e.reconcileRow(ctx, row); rerr != nil {
			errs = append(errs, fmt.Errorf("journal %s (proposal %s): %w", row.ID, row.ProposalID, rerr))
		}
	}
	return errors.Join(errs...)
}

// BlockedProjects returns the affected_projects of every DRIFT row — the
// set the scheduler's admission keeps out of autonomy until an operator
// resolves the journal (§5). affectedProjectsAll ("*") means every project.
func (e *ApplyEngine) BlockedProjects(ctx context.Context) ([]string, error) {
	if e.Journal == nil {
		return nil, nil
	}
	rows, err := e.Journal.ListOpen(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, row := range rows {
		if row.State != persistence.JournalStateDrift {
			continue
		}
		for _, p := range row.AffectedProjects {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out, nil
}

// reconcileRow dispatches one open row by state (§5 table). DRIFT rows are
// an operator's, not the reconciler's.
func (e *ApplyEngine) reconcileRow(ctx context.Context, row *persistence.ConfigApplyJournal) error {
	if row.State == persistence.JournalStateDrift {
		return nil
	}
	resolved, p, err := e.resolvedFromJournal(ctx, row)
	if err != nil {
		return err
	}
	switch row.State {
	case persistence.JournalStatePrepared:
		return e.reconcilePrepared(ctx, row, p, resolved)
	case persistence.JournalStateApplying:
		return e.reconcileApplying(ctx, row, p, resolved)
	case persistence.JournalStateVerifying:
		return e.verifyAndCommit(ctx, row, p, JournalReconcileActor, resolved)
	case persistence.JournalStateReverting:
		return e.revertJournaled(ctx, row, resolved, row.Failure)
	default:
		return fmt.Errorf("journal %s in unexpected open state %q", row.ID, row.State)
	}
}

// resolvedFromJournal rebuilds the resolved ops from the row's pre-images
// and the proposal's intended content, refusing a root or digest mismatch.
func (e *ApplyEngine) resolvedFromJournal(ctx context.Context, row *persistence.ConfigApplyJournal) ([]resolvedOp, *persistence.ControlPlaneProposal, error) {
	root, err := filepath.EvalSymlinks(e.ConfigDir)
	if err != nil {
		root = filepath.Clean(e.ConfigDir)
	}
	if row.DeploymentRoot != root {
		return nil, nil, fmt.Errorf("%w: row %s, engine %s", ErrJournalRootMismatch, row.DeploymentRoot, root)
	}
	p, err := e.Proposals.GetByID(ctx, row.ProposalID)
	if err != nil {
		return nil, nil, fmt.Errorf("proposal: %w", err)
	}
	ops, err := e.buildOps(p)
	if err != nil {
		return nil, nil, err
	}
	orderOps(ops)
	byPath := make(map[string]applyFileOp, len(ops))
	for _, op := range ops {
		byPath[op.Path] = op
	}
	resolved := make([]resolvedOp, 0, len(row.Ops))
	for _, jop := range row.Ops {
		op, ok := byPath[jop.Path]
		if !ok || hashBytes([]byte(op.Content)) != jop.ContentSHA256 {
			return nil, nil, fmt.Errorf("journal %s: proposal content for %s no longer matches the op digest", row.ID, jop.Path)
		}
		target, terr := e.resolveTarget(jop.Path)
		if terr != nil {
			return nil, nil, terr
		}
		img := row.PreImages[jop.Path]
		resolved = append(resolved, resolvedOp{op: op, target: target, existed: img.Existed, preImage: []byte(img.Content)})
	}
	return resolved, p, nil
}

// targetStatus classifies one target: "pre" (at its pre-image), "intended"
// (at the content this row writes), or "drift".
func targetStatus(ro resolvedOp) (string, error) {
	cur, existed, err := readIfExists(ro.target)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", ro.op.Path, err)
	}
	switch {
	case existed == ro.existed && (!existed || hashBytes(cur) == hashBytes(ro.preImage)):
		return "pre", nil
	case existed && hashBytes(cur) == hashBytes([]byte(ro.op.Content)):
		return "intended", nil
	default:
		return "drift", nil
	}
}

// reconcilePrepared: every target at its pre-image → FAILED (no-op); any
// target at intended content → treat as APPLYING (§5).
func (e *ApplyEngine) reconcilePrepared(ctx context.Context, row *persistence.ConfigApplyJournal, p *persistence.ControlPlaneProposal, resolved []resolvedOp) error {
	allPre := true
	for _, ro := range resolved {
		st, err := targetStatus(ro)
		if err != nil {
			return err
		}
		if st != "pre" {
			allPre = false
		}
	}
	if allPre {
		e.failPrepared(ctx, row, "reconcile: crashed before write #1; nothing mutated", "")
		return nil
	}
	if terr := e.Journal.Transition(ctx, row.ID, persistence.JournalStatePrepared, persistence.JournalStateApplying, persistence.JournalPatch{}); terr != nil {
		return fmt.Errorf("journal PREPARED→APPLYING: %w", terr)
	}
	return e.reconcileApplying(ctx, row, p, resolved)
}

// reconcileApplying: per op, at intended → done; at pre-image → redo the
// write; neither → DRIFT. Then VERIFYING and the commit step (§5).
func (e *ApplyEngine) reconcileApplying(ctx context.Context, row *persistence.ConfigApplyJournal, p *persistence.ControlPlaneProposal, resolved []resolvedOp) error {
	for _, ro := range resolved {
		st, err := targetStatus(ro)
		if err != nil {
			return err
		}
		switch st {
		case "intended":
			continue
		case "pre":
			if isFenced, reason := e.fenced(ctx); isFenced {
				return e.revertOrErr(ctx, row, resolved, "fence during reconcile: "+reason, fmt.Errorf("%w: %s", ErrWriterFenced, reason))
			}
			if werr := atomicWrite(ro.target, []byte(ro.op.Content)); werr != nil {
				return e.revertOrErr(ctx, row, resolved, "reconcile write failed for "+ro.op.Path+": "+werr.Error(), werr)
			}
			if perr := e.Journal.AppendProgress(ctx, row.ID, ro.op.Path); perr != nil {
				return fmt.Errorf("journal: %w", perr)
			}
		default:
			cur, existed, _ := readIfExists(ro.target)
			if terr := e.Journal.Transition(ctx, row.ID, persistence.JournalStateApplying, persistence.JournalStateReverting,
				persistence.JournalPatch{Failure: "reconcile: " + ro.op.Path + " matches neither image"}); terr != nil {
				return fmt.Errorf("journal →REVERTING: %w", terr)
			}
			return e.markDrift(ctx, row, ro.op.Path, cur, existed)
		}
	}
	if terr := e.Journal.Transition(ctx, row.ID, persistence.JournalStateApplying, persistence.JournalStateVerifying, persistence.JournalPatch{}); terr != nil {
		return fmt.Errorf("journal APPLYING→VERIFYING: %w", terr)
	}
	return e.verifyAndCommit(ctx, row, p, JournalReconcileActor, resolved)
}

// revertOrErr reverts and returns cause unless the revert itself drifted.
func (e *ApplyEngine) revertOrErr(ctx context.Context, row *persistence.ConfigApplyJournal, resolved []resolvedOp, failure string, cause error) error {
	if rerr := e.revertJournaled(ctx, row, resolved, failure); rerr != nil {
		return rerr
	}
	return cause
}
