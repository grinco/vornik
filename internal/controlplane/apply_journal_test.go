package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// LLD 2026-09-13-config-apply-journal-design §7, items 1–14, each driven
// through the injectable crash point (ApplyEngine.crashAfter) against the
// real SQLite journal repository — the store, not a double, is what proves
// the row survives the "crash".

const (
	jaContentA = "id: a\n"
	jaContentB = "id: b\n"
)

type journalEnv struct {
	e       *ApplyEngine
	repo    persistence.ProposalRepository
	journal persistence.ApplyJournalRepository
	db      *sqlite.DB
	dir     string
}

func newJournalEnv(t *testing.T) *journalEnv {
	t.Helper()
	db, err := sqlite.Connect(context.Background(), sqlite.DefaultConfig())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	dir := t.TempDir()
	for _, d := range []string{"projects", "swarms"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(oldContent), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	env := &journalEnv{
		repo:    sqlite.NewProposalRepository(db.DB),
		journal: sqlite.NewApplyJournalRepository(db.DB),
		db:      db,
		dir:     dir,
	}
	env.e = &ApplyEngine{
		Proposals: env.repo, Journal: env.journal, ConfigDir: dir,
		Reload: func() error { return nil }, Logger: zerolog.Nop(),
	}
	return env
}

// bundleOps is the three-file bundle every case applies: two creates and
// the config.yaml replace, which orderOps places last.
func bundleOps() []applyFileOp {
	return []applyFileOp{
		{Op: applyOpReplace, Path: "config.yaml", Content: newContent},
		{Op: applyOpCreate, Path: "projects/a.yaml", Content: jaContentA},
		{Op: applyOpCreate, Path: "projects/b.yaml", Content: jaContentB},
	}
}

// seed creates + approves a scaffold proposal for project "digest" carrying
// evidence (a read_set envelope, or "").
func (env *journalEnv) seed(t *testing.T, evidence string) string {
	t.Helper()
	raw, _ := json.Marshal(bundleOps())
	p := &persistence.ControlPlaneProposal{
		ID: persistence.GenerateID("cpp"), ProjectID: "digest",
		Kind: persistence.ProposalKindScaffold, BlastRadius: persistence.ProposalScopeProject,
		Title: "scaffold digest", ApplyOps: string(raw), Evidence: evidence,
		Status: persistence.ProposalStatusDraft, ProposedBy: "assistant",
	}
	if err := env.repo.Create(context.Background(), p); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := env.repo.SetStatus(context.Background(), p.ID, persistence.ProposalStatusApproved, "vadim"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	return p.ID
}

func (env *journalEnv) path(rel string) string { return filepath.Join(env.dir, rel) }

func (env *journalEnv) write(t *testing.T, rel, content string) {
	t.Helper()
	if err := os.WriteFile(env.path(rel), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func (env *journalEnv) exists(rel string) bool {
	_, err := os.Stat(env.path(rel))
	return err == nil
}

// latestJournal returns the newest journal row for a proposal, terminal or
// not (OpenForProposal misses once a row closes).
func (env *journalEnv) latestJournal(t *testing.T, proposalID string) *persistence.ConfigApplyJournal {
	t.Helper()
	var id string
	err := env.db.DB.QueryRowContext(context.Background(),
		`SELECT id FROM config_apply_journal WHERE proposal_id = ? ORDER BY created_at DESC, id DESC LIMIT 1`, proposalID).Scan(&id)
	if err != nil {
		t.Fatalf("no journal row for %s: %v", proposalID, err)
	}
	row, err := env.journal.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("journal get: %v", err)
	}
	return row
}

func (env *journalEnv) proposalStatus(t *testing.T, id string) string {
	t.Helper()
	p, err := env.repo.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("proposal get: %v", err)
	}
	return p.Status
}

// assertPreImages asserts every byte of the bundle is back where it started.
func (env *journalEnv) assertPreImages(t *testing.T) {
	t.Helper()
	if got := readFile(t, env.path("config.yaml")); got != oldContent {
		t.Errorf("config.yaml not restored: %q", got)
	}
	for _, rel := range []string{"projects/a.yaml", "projects/b.yaml"} {
		if env.exists(rel) {
			t.Errorf("created file %s must be gone after revert", rel)
		}
	}
}

// assertApplied asserts the bundle is on disk and both records say APPLIED.
func (env *journalEnv) assertApplied(t *testing.T, id string) {
	t.Helper()
	if got := readFile(t, env.path("config.yaml")); got != newContent {
		t.Errorf("config.yaml not applied: %q", got)
	}
	if !env.exists("projects/a.yaml") || !env.exists("projects/b.yaml") {
		t.Errorf("created files missing after apply")
	}
	if st := env.proposalStatus(t, id); st != persistence.ProposalStatusApplied {
		t.Errorf("ledger = %s, want APPLIED", st)
	}
	j := env.latestJournal(t, id)
	if j.State != persistence.JournalStateApplied || j.TerminalAt == nil {
		t.Errorf("journal = %s (terminal %v), want APPLIED", j.State, j.TerminalAt)
	}
}

// crashAt installs a crash point for one stage; onStage runs a side effect
// at that stage first (a hand edit in the window) and crash says whether to
// stop there.
func (env *journalEnv) crashAt(stage string, crash bool, onStage func()) {
	env.e.crashAfter = func(s string) bool {
		if s != stage {
			return false
		}
		if onStage != nil {
			onStage()
		}
		return crash
	}
}

// fakeEpochGate is a leaderelection.EpochVerifier whose epoch is superseded
// from the failAt-th verification on.
type fakeEpochGate struct {
	calls  int
	failAt int
}

func (g *fakeEpochGate) VerifyEpoch(context.Context) (bool, int64, error) {
	g.calls++
	if g.failAt > 0 && g.calls >= g.failAt {
		return false, 2, nil
	}
	return true, 1, nil
}

// §7.1 — crash after PREPARED commit, before write #1 → reconcile: FAILED,
// files untouched.
func TestJournal_CrashAfterPrepared_ReconcileFails(t *testing.T) {
	env := newJournalEnv(t)
	id := env.seed(t, "")
	env.crashAt("prepared", true, nil)
	if err := env.e.Apply(context.Background(), id, "vadim", false); !errors.Is(err, errCrashInjected) {
		t.Fatalf("expected the injected crash, got %v", err)
	}
	if j := env.latestJournal(t, id); j.State != persistence.JournalStatePrepared || j.TerminalAt != nil {
		t.Fatalf("PREPARED must be on disk before write #1: %+v", j)
	}
	env.assertPreImages(t)
	env.e.crashAfter = nil
	if err := env.e.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	j := env.latestJournal(t, id)
	if j.State != persistence.JournalStateFailed || j.TerminalAt == nil || !strings.Contains(j.Failure, "nothing mutated") {
		t.Fatalf("reconcile of an unmutated PREPARED row must be FAILED: %+v", j)
	}
	env.assertPreImages(t)
	if st := env.proposalStatus(t, id); st != persistence.ProposalStatusApproved {
		t.Errorf("proposal must stay APPROVED (retryable), got %s", st)
	}
}

// §7.2 — crash after rename #1, before its progress append → reconcile
// redoes none of #1, finishes #2..N, APPLIED.
func TestJournal_CrashAfterFirstRename_ReconcileFinishes(t *testing.T) {
	env := newJournalEnv(t)
	id := env.seed(t, "")
	env.crashAt("write:projects/a.yaml", true, nil)
	if err := env.e.Apply(context.Background(), id, "vadim", false); !errors.Is(err, errCrashInjected) {
		t.Fatalf("expected the injected crash, got %v", err)
	}
	j := env.latestJournal(t, id)
	if j.State != persistence.JournalStateApplying || len(j.Progress) != 0 {
		t.Fatalf("crash between rename and progress append leaves APPLYING with empty progress: %+v", j)
	}
	if !env.exists("projects/a.yaml") || env.exists("projects/b.yaml") || readFile(t, env.path("config.yaml")) != oldContent {
		t.Fatal("exactly the first rename must have landed")
	}
	env.e.crashAfter = nil
	if err := env.e.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	env.assertApplied(t, id)
	j = env.latestJournal(t, id)
	// a.yaml was already at intended content: not redone, so not re-appended.
	if strings.Join(j.Progress, ",") != "projects/b.yaml,config.yaml" {
		t.Errorf("reconcile must redo only the missing writes, progress = %v", j.Progress)
	}
	p, _ := env.repo.GetByID(context.Background(), id)
	if p.AppliedBy != JournalReconcileActor {
		t.Errorf("applied_by = %q, want the reconcile actor", p.AppliedBy)
	}
}

// §7.3 — crash after all renames, before VERIFYING → APPLIED after reload.
func TestJournal_CrashBeforeVerifying_ReconcileApplies(t *testing.T) {
	env := newJournalEnv(t)
	id := env.seed(t, "")
	reloads := 0
	env.e.Reload = func() error { reloads++; return nil }
	env.crashAt("progress:config.yaml", true, nil)
	if err := env.e.Apply(context.Background(), id, "vadim", false); !errors.Is(err, errCrashInjected) {
		t.Fatalf("expected the injected crash, got %v", err)
	}
	if j := env.latestJournal(t, id); j.State != persistence.JournalStateApplying || len(j.Progress) != 3 {
		t.Fatalf("all renames committed, state must be APPLYING with full progress: %+v", j)
	}
	if reloads != 0 {
		t.Fatal("no reload before the crash")
	}
	env.e.crashAfter = nil
	if err := env.e.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if reloads != 1 {
		t.Errorf("reconcile must reload exactly once, got %d", reloads)
	}
	env.assertApplied(t, id)
}

// §7.4 — reload failure → REVERTED, every byte identical to the pre-image,
// created files gone, proposal still APPROVED.
func TestJournal_ReloadFailure_Reverted(t *testing.T) {
	env := newJournalEnv(t)
	id := env.seed(t, "")
	calls := 0
	env.e.Reload = func() error {
		calls++
		if calls == 1 {
			return errors.New("bad yaml")
		}
		return nil
	}
	err := env.e.Apply(context.Background(), id, "vadim", false)
	if err == nil || !strings.Contains(err.Error(), "reload rejected") {
		t.Fatalf("expected the reload rejection, got %v", err)
	}
	env.assertPreImages(t)
	j := env.latestJournal(t, id)
	if j.State != persistence.JournalStateReverted || j.TerminalAt == nil || !strings.Contains(j.Failure, "reload rejected") {
		t.Fatalf("journal must be terminal REVERTED naming the reload: %+v", j)
	}
	if st := env.proposalStatus(t, id); st != persistence.ProposalStatusApproved {
		t.Errorf("proposal must stay APPROVED after a revert, got %s", st)
	}
	if calls != 2 {
		t.Errorf("expected the reload of restored pre-images, reloads = %d", calls)
	}
}

// §7.5 — hand edit of a target in the window between PREPARED and write #1
// → FAILED naming the path, proposal auto-retired, nothing written.
func TestJournal_TargetEditedAfterPrepared_FailedNothingWritten(t *testing.T) {
	env := newJournalEnv(t)
	id := env.seed(t, "")
	env.crashAt("prepared", false, func() { env.write(t, "config.yaml", "hand: edit\n") })
	err := env.e.Apply(context.Background(), id, "vadim", false)
	if !errors.Is(err, ErrStaleBase) {
		t.Fatalf("expected ErrStaleBase, got %v", err)
	}
	j := env.latestJournal(t, id)
	if j.State != persistence.JournalStateFailed || !strings.Contains(j.Failure, "config.yaml") {
		t.Fatalf("journal must be FAILED naming config.yaml: %+v", j)
	}
	if env.exists("projects/a.yaml") || env.exists("projects/b.yaml") || readFile(t, env.path("config.yaml")) != "hand: edit\n" {
		t.Error("nothing may be written after a pre-write drift")
	}
	if st := env.proposalStatus(t, id); st != persistence.ProposalStatusRejected {
		t.Errorf("drifted proposal must be auto-retired to REJECTED, got %s", st)
	}
}

// readSetEvidence builds the assistant's read_set envelope for one dependency.
func readSetEvidence(path, hash string) string {
	b, _ := json.Marshal(journalEvidence{ReadSet: map[string]string{path: hash}})
	return string(b)
}

// §7.6 — hand edit of a read-only dependency (before the apply is called)
// → ErrStaleBase, auto-retired, nothing written, no journal row prepared.
func TestJournal_ReadDependencyEdited_StaleBase(t *testing.T) {
	env := newJournalEnv(t)
	env.write(t, "swarms/shared.yaml", "swarm: v1\n")
	id := env.seed(t, readSetEvidence("swarms/shared.yaml", hashBytes([]byte("swarm: v1\n"))))
	env.write(t, "swarms/shared.yaml", "swarm: v2\n")
	err := env.e.Apply(context.Background(), id, "vadim", false)
	if !errors.Is(err, ErrStaleBase) || !strings.Contains(err.Error(), "swarms/shared.yaml") {
		t.Fatalf("expected ErrStaleBase naming the dependency, got %v", err)
	}
	if env.exists("projects/a.yaml") || readFile(t, env.path("config.yaml")) != oldContent {
		t.Error("nothing may be written")
	}
	if st := env.proposalStatus(t, id); st != persistence.ProposalStatusRejected {
		t.Errorf("proposal must be auto-retired, got %s", st)
	}
	if _, err := env.journal.OpenForProposal(context.Background(), id); !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("pass-1 drift happens before PREPARED; no row expected, got %v", err)
	}
}

// §7.7 — create collision: the target appeared after PREPARED → FAILED,
// the hand-written file untouched.
func TestJournal_CreateCollisionAfterPrepared_Failed(t *testing.T) {
	env := newJournalEnv(t)
	id := env.seed(t, "")
	env.crashAt("prepared", false, func() { env.write(t, "projects/a.yaml", "mine\n") })
	if err := env.e.Apply(context.Background(), id, "vadim", false); !errors.Is(err, ErrStaleBase) {
		t.Fatalf("expected ErrStaleBase, got %v", err)
	}
	j := env.latestJournal(t, id)
	if j.State != persistence.JournalStateFailed || !strings.Contains(j.Failure, "projects/a.yaml") {
		t.Fatalf("journal must be FAILED naming projects/a.yaml: %+v", j)
	}
	if readFile(t, env.path("projects/a.yaml")) != "mine\n" || env.exists("projects/b.yaml") || readFile(t, env.path("config.yaml")) != oldContent {
		t.Error("the collision must leave every file exactly as found")
	}
}

// §7.8 — a concurrent second proposal touching an overlapping path gets
// ErrApplyInProgress; writes never interleave.
func TestJournal_ConcurrentOverlappingApply_Refused(t *testing.T) {
	env := newJournalEnv(t)
	first := env.seed(t, "")
	raw, _ := json.Marshal([]applyFileOp{{Op: applyOpReplace, Path: "config.yaml", Content: "server:\n  address: :7070\n"}})
	second := &persistence.ControlPlaneProposal{
		ID: persistence.GenerateID("cpp"), ProjectID: "digest", Kind: persistence.ProposalKindConfig,
		BlastRadius: persistence.ProposalScopeProject, Title: "overlap", ApplyOps: string(raw),
		Status: persistence.ProposalStatusDraft, ProposedBy: "assistant",
	}
	_ = env.repo.Create(context.Background(), second)
	_ = env.repo.SetStatus(context.Background(), second.ID, persistence.ProposalStatusApproved, "vadim")

	holding := make(chan struct{})
	release := make(chan struct{})
	env.crashAt("prepared", false, func() { close(holding); <-release })
	done := make(chan error, 1)
	go func() { done <- env.e.Apply(context.Background(), first, "vadim", false) }()
	<-holding
	if err := env.e.Apply(context.Background(), second.ID, "vadim", false); !errors.Is(err, ErrApplyInProgress) {
		t.Fatalf("overlapping apply during an open journal must be ErrApplyInProgress, got %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first apply: %v", err)
	}
	env.assertApplied(t, first)
	if st := env.proposalStatus(t, second.ID); st != persistence.ProposalStatusApproved {
		t.Errorf("refused proposal must be untouched, got %s", st)
	}
}

// §7.9 — stale writer epoch discovered before write #3 → REVERTING →
// REVERTED, every pre-image back, proposal APPROVED.
func TestJournal_StaleEpochBeforeThirdWrite_Reverted(t *testing.T) {
	env := newJournalEnv(t)
	id := env.seed(t, "")
	gate := &fakeEpochGate{failAt: 3} // checks: write #1, #2, #3
	env.e.LeaderGate = gate
	env.e.WriterEpoch = func() int64 { return 1 }
	err := env.e.Apply(context.Background(), id, "vadim", false)
	if !errors.Is(err, ErrWriterFenced) {
		t.Fatalf("expected ErrWriterFenced, got %v", err)
	}
	if gate.calls != 3 {
		t.Errorf("fence must be checked before each write, calls = %d", gate.calls)
	}
	env.assertPreImages(t)
	j := env.latestJournal(t, id)
	if j.State != persistence.JournalStateReverted || j.WriterEpoch != 1 || !strings.Contains(j.Failure, "fence before write #3") {
		t.Fatalf("journal must be REVERTED carrying the PREPARE epoch and the fence reason: %+v", j)
	}
	if st := env.proposalStatus(t, id); st != persistence.ProposalStatusApproved {
		t.Errorf("proposal must stay APPROVED, got %s", st)
	}
}

// §7.10 — drift during REVERTING → DRIFT, the drifted file untouched, the
// affected project blocked.
func TestJournal_DriftDuringRevert_BlocksProject(t *testing.T) {
	env := newJournalEnv(t)
	id := env.seed(t, "")
	env.e.Reload = func() error { return errors.New("bad yaml") }
	// Between REVERTING and the first restore, a hand edit lands on the file
	// this row created.
	env.crashAt("reverting", false, func() { env.write(t, "projects/a.yaml", "operator: edited\n") })
	err := env.e.Apply(context.Background(), id, "vadim", false)
	if !errors.Is(err, ErrJournalDrift) {
		t.Fatalf("expected ErrJournalDrift, got %v", err)
	}
	j := env.latestJournal(t, id)
	if j.State != persistence.JournalStateDrift || j.TerminalAt != nil || !strings.Contains(j.Failure, "projects/a.yaml") {
		t.Fatalf("journal must park OPEN in DRIFT naming the path: %+v", j)
	}
	if readFile(t, env.path("projects/a.yaml")) != "operator: edited\n" {
		t.Error("a drifted target must never be written over")
	}
	// Reverse order: config.yaml and b.yaml were restored before a.yaml drifted.
	if readFile(t, env.path("config.yaml")) != oldContent || env.exists("projects/b.yaml") {
		t.Error("targets before the drift point must be restored")
	}
	blocked, berr := env.e.BlockedProjects(context.Background())
	if berr != nil || len(blocked) != 1 || blocked[0] != "digest" {
		t.Fatalf("BlockedProjects = %v, %v; want [digest]", blocked, berr)
	}
	// Reconcile leaves a DRIFT row to the operator.
	if rerr := env.e.Reconcile(context.Background()); rerr != nil {
		t.Fatalf("Reconcile must skip DRIFT rows: %v", rerr)
	}
	if j = env.latestJournal(t, id); j.State != persistence.JournalStateDrift {
		t.Errorf("reconcile must not touch a DRIFT row, got %s", j.State)
	}
}

// §7.11 — restart-only keys → PENDING_RESTART, ledger APPLIED, the engine
// reports the pending restart.
func TestJournal_RestartOnlyKeys_PendingRestart(t *testing.T) {
	env := newJournalEnv(t)
	id := env.seed(t, "")
	env.e.RestartOnly = func(_ context.Context, ops []JournaledOp) (bool, error) { return len(ops) == 3, nil }
	if err := env.e.Apply(context.Background(), id, "vadim", false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	j := env.latestJournal(t, id)
	if j.State != persistence.JournalStatePendingRestart || j.TerminalAt == nil {
		t.Fatalf("journal = %s, want terminal PENDING_RESTART", j.State)
	}
	if st := env.proposalStatus(t, id); st != persistence.ProposalStatusApplied {
		t.Errorf("ledger = %s, want APPLIED", st)
	}
	if !env.e.RestartPending() {
		t.Error("RestartPending must report the pending restart")
	}
}

// §7.12 — APPLIED asserts the resolved generation, not the reload return:
// a reload that returns nil but leaves the old generation active is
// reverted.
func TestJournal_GenerationNotConfirmed_Reverted(t *testing.T) {
	env := newJournalEnv(t)
	id := env.seed(t, "")
	gen := "gen-1"
	env.e.Generation = func() string { return gen }
	env.e.VerifyGeneration = func(context.Context, []JournaledOp) error {
		return errors.New("registry still at gen-1 for projects/a.yaml")
	}
	err := env.e.Apply(context.Background(), id, "vadim", false)
	if err == nil || !strings.Contains(err.Error(), "generation not confirmed") {
		t.Fatalf("expected the generation failure, got %v", err)
	}
	env.assertPreImages(t)
	j := env.latestJournal(t, id)
	if j.State != persistence.JournalStateReverted || j.GenerationBefore != "gen-1" || !strings.Contains(j.Failure, "generation") {
		t.Fatalf("journal must be REVERTED with the generation failure: %+v", j)
	}
	if st := env.proposalStatus(t, id); st != persistence.ProposalStatusApplied {
		if st != persistence.ProposalStatusApproved {
			t.Errorf("proposal = %s, want APPROVED", st)
		}
	} else {
		t.Error("a reload that returned nil must NOT mark the ledger APPLIED without the generation")
	}
}

// §7.13 — the trusted delete touches only engine-created paths whose bytes
// are exactly what this row wrote; a replaced path is restored, and a
// bystander file in the same directory is never touched.
func TestJournal_TrustedDeleteBoundary(t *testing.T) {
	env := newJournalEnv(t)
	env.write(t, "projects/bystander.yaml", "keep: me\n")
	id := env.seed(t, "")
	env.e.Reload = func() error { return errors.New("bad yaml") }
	if err := env.e.Apply(context.Background(), id, "vadim", false); err == nil {
		t.Fatal("expected the reload rejection")
	}
	env.assertPreImages(t)
	if readFile(t, env.path("projects/bystander.yaml")) != "keep: me\n" {
		t.Error("a file this row did not create must never be deleted")
	}
	if !env.exists("config.yaml") {
		t.Error("a replaced path is restored, never deleted")
	}
}

// §7.14 — a read dependency edited in the window BETWEEN the PREPARED commit
// and the pre-write-#1 recheck → FAILED naming the path, no file mutated.
func TestJournal_ReadDependencyEditedAfterPrepared_FailedNamesPath(t *testing.T) {
	env := newJournalEnv(t)
	env.write(t, "swarms/shared.yaml", "swarm: v1\n")
	id := env.seed(t, readSetEvidence("swarms/shared.yaml", hashBytes([]byte("swarm: v1\n"))))
	env.crashAt("prepared", false, func() { env.write(t, "swarms/shared.yaml", "swarm: v2\n") })
	err := env.e.Apply(context.Background(), id, "vadim", false)
	if !errors.Is(err, ErrStaleBase) || !strings.Contains(err.Error(), "swarms/shared.yaml") {
		t.Fatalf("expected ErrStaleBase naming the dependency, got %v", err)
	}
	j := env.latestJournal(t, id)
	if j.State != persistence.JournalStateFailed || j.TerminalAt == nil || !strings.Contains(j.Failure, "swarms/shared.yaml") {
		t.Fatalf("journal must be FAILED naming swarms/shared.yaml: %+v", j)
	}
	if j.ReadSet["swarms/shared.yaml"] != hashBytes([]byte("swarm: v1\n")) {
		t.Errorf("the PREPARED row must carry the grounded read-set hash: %v", j.ReadSet)
	}
	if env.exists("projects/a.yaml") || env.exists("projects/b.yaml") || readFile(t, env.path("config.yaml")) != oldContent {
		t.Error("no file may be mutated")
	}
	if st := env.proposalStatus(t, id); st != persistence.ProposalStatusRejected {
		t.Errorf("proposal must be auto-retired, got %s", st)
	}
}

// R8 — a retry that finds an open journal resumes reconciliation instead of
// preparing a second row, and reports success when the resumed apply lands.
func TestJournal_RetryResumesOpenJournal(t *testing.T) {
	env := newJournalEnv(t)
	id := env.seed(t, "")
	env.crashAt("progress:projects/a.yaml", true, nil)
	if err := env.e.Apply(context.Background(), id, "vadim", false); !errors.Is(err, errCrashInjected) {
		t.Fatalf("expected the injected crash, got %v", err)
	}
	open := env.latestJournal(t, id)
	env.e.crashAfter = nil
	if err := env.e.Apply(context.Background(), id, "vadim", false); err != nil {
		t.Fatalf("retry must resume the open journal and succeed, got %v", err)
	}
	env.assertApplied(t, id)
	if again := env.latestJournal(t, id); again.ID != open.ID {
		t.Errorf("retry must not prepare a second row: %s vs %s", again.ID, open.ID)
	}
}

// An absent read_set key (legacy evidence, detector metrics) is an empty
// read set — the journaled path applies such proposals unchanged.
func TestJournal_EvidenceWithoutReadSetApplies(t *testing.T) {
	env := newJournalEnv(t)
	id := env.seed(t, `{"base_hash":"`+hashBytes([]byte(oldContent))+`","metrics":{"p95":1}}`)
	if err := env.e.Apply(context.Background(), id, "vadim", false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	env.assertApplied(t, id)
	j := env.latestJournal(t, id)
	if len(j.ReadSet) != 0 || len(j.PreImages) != 3 || j.AffectedProjects[0] != "digest" {
		t.Errorf("journal row shape: %+v", j)
	}
	if p, _ := env.repo.GetByID(context.Background(), id); !strings.Contains(p.PreApplySnapshot, `"existed":false`) {
		t.Errorf("ledger snapshot must be derived from the journal pre-images: %q", p.PreApplySnapshot)
	}
}
