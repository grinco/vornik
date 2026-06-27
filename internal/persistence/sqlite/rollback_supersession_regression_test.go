package sqlite_test

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// Behavioural regressions for the rollback × supersession fix
// (migration 89; https://docs.vornik.io
// design.md), run against a real database so the SQL semantics —
// not just the statement shapes — are pinned. The postgres backend
// shares the same statement logic (pinned by sqlmock tests +
// the repotest integration suite).
//
// The headline scenario (2026-06-04 bug-sweep critical finding):
// re-ingest supersedes v1; operator rolls the bad ingest back; v2 is
// hidden — and pre-fix v1 STAYED superseded, so both versions were
// unretrievable. Post-fix the restore pass un-supersedes v1.

type rollbackRig struct {
	t    *testing.T
	db   *sqlite.DB
	repo *sqlite.CorpusEpochRepository
	ctx  context.Context
}

func newRollbackRig(t *testing.T) *rollbackRig {
	t.Helper()
	db := newTestDB(t)
	return &rollbackRig{
		t:    t,
		db:   db,
		repo: sqlite.NewCorpusEpochRepository(db.DB),
		ctx:  context.Background(),
	}
}

// epoch creates + closes + activates an epoch with a deterministic
// created_at so the rollback cut lines are unambiguous.
func (r *rollbackRig) epoch(id string, createdAt time.Time) {
	r.t.Helper()
	e := &persistence.CorpusEpoch{ID: id, ProjectID: "p1", CreatedAt: createdAt}
	if err := r.repo.CreateEpoch(r.ctx, e); err != nil {
		r.t.Fatalf("CreateEpoch(%s): %v", id, err)
	}
	if err := r.repo.CloseEpoch(r.ctx, id, persistence.CorpusEpochCounts{Admitted: 1}); err != nil {
		r.t.Fatalf("CloseEpoch(%s): %v", id, err)
	}
	if err := r.repo.Activate(r.ctx, "p1", id, "test", "setup"); err != nil {
		r.t.Fatalf("Activate(%s): %v", id, err)
	}
}

// chunk inserts a chunk row directly (the memory.Repository runs
// postgres-flavoured SQL, so the slim sqlite schema is populated by
// hand — same columns the restore pass reads).
func (r *rollbackRig) chunk(id, status string, supersededInEpoch, preStatus *string) {
	r.t.Helper()
	if _, err := r.db.ExecContext(r.ctx, `
		INSERT INTO project_memory_chunks
			(id, project_id, content, created_at, validation_status, superseded_in_epoch, pre_supersede_status)
		VALUES (?, 'p1', 'content '||?, ?, ?, ?, ?)`,
		id, id, time.Now().UTC().Format(time.RFC3339), status, supersededInEpoch, preStatus,
	); err != nil {
		r.t.Fatalf("insert chunk %s: %v", id, err)
	}
}

func (r *rollbackRig) chunkStatus(id string) (status string, prov, pre *string) {
	r.t.Helper()
	if err := r.db.QueryRowContext(r.ctx, `
		SELECT validation_status, superseded_in_epoch, pre_supersede_status
		FROM project_memory_chunks WHERE id = ?`, id,
	).Scan(&status, &prov, &pre); err != nil {
		r.t.Fatalf("read chunk %s: %v", id, err)
	}
	return status, prov, pre
}

func rbStrPtr(s string) *string { return &s }

var rigBase = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

// TestRollback_RestoresSupersededChunks — the headline regression.
func TestRollback_RestoresSupersededChunks(t *testing.T) {
	r := newRollbackRig(t)
	r.epoch("E1", rigBase)
	r.epoch("E2", rigBase.Add(time.Hour))

	// v1 (from E1's ingest) was superseded by E2's re-ingest while it
	// was 'verified'; v2 is E2's replacement content.
	r.chunk("v1", "superseded", rbStrPtr("E2"), rbStrPtr("verified"))
	r.chunk("v2", "unverified", nil, nil)

	// Preview must announce the restore.
	restorable, nonRestorable, err := r.repo.CountRollbackRestorable(r.ctx, "p1", "E1")
	if err != nil {
		t.Fatalf("CountRollbackRestorable: %v", err)
	}
	if restorable != 1 || nonRestorable != 0 {
		t.Fatalf("preview = (%d,%d), want (1,0)", restorable, nonRestorable)
	}

	deact, _, restored, err := r.repo.RollbackTo(r.ctx, "p1", "E1", "operator", "bad re-ingest")
	if err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}
	if deact != 1 {
		t.Errorf("deactivated = %d, want 1 (E2)", deact)
	}
	if restored != 1 {
		t.Errorf("chunksRestored = %d, want 1 — pre-fix this was 0 and v1 stayed dead", restored)
	}

	status, prov, pre := r.chunkStatus("v1")
	if status != "verified" {
		t.Errorf("v1 status = %q, want verified (exact prior status restored)", status)
	}
	if prov != nil || pre != nil {
		t.Errorf("v1 provenance not cleared: prov=%v pre=%v", prov, pre)
	}

	// Audit row carries the count.
	rbs, err := r.repo.ListRollbacks(r.ctx, "p1", 10)
	if err != nil || len(rbs) != 1 {
		t.Fatalf("ListRollbacks: %v (%d rows)", err, len(rbs))
	}
	if rbs[0].ChunksRestored != 1 {
		t.Errorf("audit ChunksRestored = %d, want 1", rbs[0].ChunksRestored)
	}
}

// TestRollback_ChainComposition — A ←(E2)— B ←(E3)— C. Rolling back
// to E2 restores B only; rolling back further to E1 restores A too.
func TestRollback_ChainComposition(t *testing.T) {
	r := newRollbackRig(t)
	r.epoch("E1", rigBase)
	r.epoch("E2", rigBase.Add(time.Hour))
	r.epoch("E3", rigBase.Add(2*time.Hour))

	r.chunk("A", "superseded", rbStrPtr("E2"), rbStrPtr("unverified"))
	r.chunk("B", "superseded", rbStrPtr("E3"), rbStrPtr("verified"))
	r.chunk("C", "unverified", nil, nil)

	// Rollback to E2: deactivates E3 → B restored, A untouched.
	_, _, restored, err := r.repo.RollbackTo(r.ctx, "p1", "E2", "operator", "step1")
	if err != nil {
		t.Fatalf("RollbackTo(E2): %v", err)
	}
	if restored != 1 {
		t.Fatalf("restored = %d, want 1 (B only)", restored)
	}
	if s, _, _ := r.chunkStatus("B"); s != "verified" {
		t.Errorf("B = %q, want verified", s)
	}
	if s, _, _ := r.chunkStatus("A"); s != "superseded" {
		t.Errorf("A = %q, want still superseded (its cause E2 is still active)", s)
	}

	// Rollback to E1: deactivates E2 → A restored.
	_, _, restored, err = r.repo.RollbackTo(r.ctx, "p1", "E1", "operator", "step2")
	if err != nil {
		t.Fatalf("RollbackTo(E1): %v", err)
	}
	if restored != 1 {
		t.Fatalf("restored = %d, want 1 (A)", restored)
	}
	if s, _, _ := r.chunkStatus("A"); s != "unverified" {
		t.Errorf("A = %q, want unverified", s)
	}
}

// TestRollback_RepeatIsIdempotent_AndNullPreStatusDefaults — a second
// identical rollback restores nothing (provenance cleared on the
// first), and a chunk with provenance but NULL prior status falls
// back to 'unverified'.
func TestRollback_RepeatIsIdempotent_AndNullPreStatusDefaults(t *testing.T) {
	r := newRollbackRig(t)
	r.epoch("E1", rigBase)
	r.epoch("E2", rigBase.Add(time.Hour))

	r.chunk("v1", "superseded", rbStrPtr("E2"), nil) // NULL prior status

	_, _, restored, err := r.repo.RollbackTo(r.ctx, "p1", "E1", "operator", "first")
	if err != nil || restored != 1 {
		t.Fatalf("first rollback: restored=%d err=%v", restored, err)
	}
	if s, _, _ := r.chunkStatus("v1"); s != "unverified" {
		t.Errorf("v1 = %q, want unverified (NULL prior status fallback)", s)
	}

	_, _, restored, err = r.repo.RollbackTo(r.ctx, "p1", "E1", "operator", "second")
	if err != nil || restored != 0 {
		t.Fatalf("repeat rollback: restored=%d err=%v, want 0", restored, err)
	}
}

// TestRollback_DoesNotResurrectTombstonedEpochs — finding #4 of the
// sweep: an epoch an operator explicitly deactivated must stay
// inactive through a rollback's re-activation pass; a later explicit
// Activate clears the tombstone and re-admits it.
func TestRollback_DoesNotResurrectTombstonedEpochs(t *testing.T) {
	r := newRollbackRig(t)
	r.epoch("E1", rigBase)
	r.epoch("E2", rigBase.Add(time.Hour))
	r.epoch("E3", rigBase.Add(2*time.Hour))

	// Operator hides E1 deliberately (bad data).
	if err := r.repo.Deactivate(r.ctx, "p1", "E1", "operator"); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}

	// Roll back to E2: E3 deactivated, epochs <= E2 re-activated —
	// but NOT the tombstoned E1.
	if _, _, _, err := r.repo.RollbackTo(r.ctx, "p1", "E2", "operator", "rollback"); err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}
	active, _ := r.repo.ListActive(r.ctx, "p1")
	for _, id := range active {
		if id == "E1" {
			t.Fatal("tombstoned E1 was resurrected by rollback — pre-fix behaviour")
		}
	}

	// Explicit re-activation clears the tombstone; the next rollback
	// re-admits E1.
	if err := r.repo.Activate(r.ctx, "p1", "E1", "operator", "restored"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if err := r.repo.Deactivate(r.ctx, "p1", "E3", "rollback-test"); err != nil {
		t.Fatalf("Deactivate(E3): %v", err)
	}
	if _, _, _, err := r.repo.RollbackTo(r.ctx, "p1", "E2", "operator", "again"); err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}
	found := false
	active, _ = r.repo.ListActive(r.ctx, "p1")
	for _, id := range active {
		if id == "E1" {
			found = true
		}
	}
	if !found {
		t.Error("E1 should be rollback-eligible again after explicit Activate cleared the tombstone")
	}
}

// TestCountRollbackRestorable_ReportsNonRestorable — pre-migration
// supersessions (NULL provenance) are surfaced, not silently skipped.
func TestCountRollbackRestorable_ReportsNonRestorable(t *testing.T) {
	r := newRollbackRig(t)
	r.epoch("E1", rigBase)
	r.epoch("E2", rigBase.Add(time.Hour))

	r.chunk("old-history", "superseded", nil, nil)           // pre-migration: no provenance
	r.chunk("restorable", "superseded", rbStrPtr("E2"), nil) // restorable
	r.chunk("kept", "superseded", rbStrPtr("E1"), nil)       // cause survives the rollback

	restorable, nonRestorable, err := r.repo.CountRollbackRestorable(r.ctx, "p1", "E1")
	if err != nil {
		t.Fatalf("CountRollbackRestorable: %v", err)
	}
	if restorable != 1 || nonRestorable != 1 {
		t.Fatalf("preview = (%d,%d), want (1,1)", restorable, nonRestorable)
	}

	// Apply matches the preview; the surviving-cause chunk stays.
	_, _, restored, err := r.repo.RollbackTo(r.ctx, "p1", "E1", "operator", "check")
	if err != nil || restored != 1 {
		t.Fatalf("RollbackTo: restored=%d err=%v, want 1", restored, err)
	}
	if s, _, _ := r.chunkStatus("kept"); s != "superseded" {
		t.Errorf("kept = %q, want still superseded (cause E1 remains active)", s)
	}
	if s, _, _ := r.chunkStatus("old-history"); s != "superseded" {
		t.Errorf("old-history = %q, want still superseded (no provenance)", s)
	}
}

// TestRollback_TargetNotFound — guard coverage: rolling back to an
// unknown epoch fails cleanly without mutating anything.
func TestRollback_TargetNotFound(t *testing.T) {
	r := newRollbackRig(t)
	r.epoch("E1", rigBase)
	if _, _, _, err := r.repo.RollbackTo(r.ctx, "p1", "missing", "operator", ""); err == nil {
		t.Fatal("rollback to a missing epoch must error")
	}
	active, _ := r.repo.ListActive(r.ctx, "p1")
	if len(active) != 1 || active[0] != "E1" {
		t.Fatalf("active set mutated on failed rollback: %v", active)
	}
}

// TestRollback_GuardBranches — input guards + the *sql.DB requirement
// (RollbackTo manages its own transaction, so a DBTX that is already
// a transaction is refused).
func TestRollback_GuardBranches(t *testing.T) {
	r := newRollbackRig(t)
	if _, _, err := r.repo.CountRollbackRestorable(r.ctx, "", ""); err == nil {
		t.Error("CountRollbackRestorable empty args must error")
	}

	tx, err := r.db.BeginTx(r.ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	txRepo := sqlite.NewCorpusEpochRepository(tx)
	if _, _, _, err := txRepo.RollbackTo(r.ctx, "p1", "e1", "op", ""); err == nil {
		t.Error("RollbackTo over a *sql.Tx must refuse (manages its own transaction)")
	}
}
