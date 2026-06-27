package memory

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rs/zerolog"
)

// fenceGate is a LeaderGate that ALSO implements the
// leaderelection.EpochVerifier shape (VerifyEpoch). DangerousWriteAllowed
// type-asserts the gate to a verifier; ok=false means a newer leader has
// superseded this (stale) one and the dangerous write must be skipped.
// A plain IsLeader-only gate (the existing LeaderGate) keeps pre-fence
// behaviour — that path is covered by the workers' existing tick gating.
type fenceGate struct {
	leader   atomic.Bool
	epochOK  atomic.Bool
	verified atomic.Int32
}

func (g *fenceGate) IsLeader() bool { return g.leader.Load() }

func (g *fenceGate) VerifyEpoch(_ context.Context) (ok bool, current int64, err error) {
	g.verified.Add(1)
	return g.epochOK.Load(), 1, nil
}

// TestConsolidateOne_StaleLeader_NoWrite pins review finding B1 for the
// gist consolidation write: a stale leader (IsLeader true, VerifyEpoch
// ok=false) must NOT run ConsolidateProject's chunk scan or UpsertGist.
// We set NO sqlmock expectations — any query is a fence failure.
func TestConsolidateOne_StaleLeader_NoWrite(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)
	gate := &fenceGate{}
	gate.leader.Store(true)
	gate.epochOK.Store(false)
	w := &ConsolidateWorker{
		Consolid:   NewConsolidator(repo),
		Repo:       repo,
		Projects:   &stubProjectLister{ids: []string{"proj-a"}},
		Interval:   time.Hour,
		Logger:     zerolog.Nop(),
		LeaderGate: gate,
	}

	if err := w.consolidateOne(context.Background(), "proj-a"); err != nil {
		t.Fatalf("consolidateOne returned error: %v", err)
	}

	if got := gate.verified.Load(); got == 0 {
		t.Errorf("fence did not consult VerifyEpoch")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("stale leader issued SQL (fence failed): %v", err)
	}
}

// TestConsolidateOne_CurrentLeader_Writes confirms the fence opens
// cleanly: a verifier reporting ok=true runs the scan + UpsertGist.
func TestConsolidateOne_CurrentLeader_Writes(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)
	gate := &fenceGate{}
	gate.leader.Store(true)
	gate.epochOK.Store(true)
	w := &ConsolidateWorker{
		Consolid:   NewConsolidator(repo),
		Repo:       repo,
		Projects:   &stubProjectLister{ids: []string{"proj-a"}},
		Interval:   time.Hour,
		Logger:     zerolog.Nop(),
		LeaderGate: gate,
	}

	mock.ExpectQuery(`SELECT content`).
		WithArgs("proj-a", 1000).
		WillReturnRows(sqlmock.NewRows([]string{"content"}).AddRow("alpha beta beta"))
	mock.ExpectExec(`INSERT INTO project_gists`).
		WithArgs("proj-a", sqlmock.AnyArg(), 1, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := w.consolidateOne(context.Background(), "proj-a"); err != nil {
		t.Fatalf("consolidateOne returned error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("current leader did not write the gist: %v", err)
	}
}

// TestProcessOne_StaleLeader_NoLLMCall pins B1 for the LLM-tier write:
// a stale leader must NOT read the gist, build a sample, spend the LLM
// call, or UpsertNarrative. No sqlmock expectations + zero writer calls.
func TestProcessOne_StaleLeader_NoLLMCall(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)
	fp := &titlerFakeProvider{replies: []titlerReply{{content: "should not fire"}}}
	gate := &fenceGate{}
	gate.leader.Store(true)
	gate.epochOK.Store(false)
	w := &LLMConsolidateWorker{
		Writer:     NewNarrativeWriter(fp, ""),
		Repo:       repo,
		Projects:   &stubProjectLister{ids: []string{"proj-a"}},
		Interval:   time.Hour,
		Logger:     zerolog.Nop(),
		LeaderGate: gate,
	}

	if err := w.processOne(context.Background(), "proj-a"); err != nil {
		t.Fatalf("processOne returned error: %v", err)
	}

	if got := gate.verified.Load(); got == 0 {
		t.Errorf("fence did not consult VerifyEpoch")
	}
	if fp.calls.Load() != 0 {
		t.Errorf("stale leader spent %d LLM calls; want 0", fp.calls.Load())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("stale leader issued SQL (fence failed): %v", err)
	}
}

// TestProcessOne_CurrentLeader_Writes confirms the LLM-tier fence opens
// cleanly for a current leader: gist read, LLM call, narrative UPDATE.
func TestProcessOne_CurrentLeader_Writes(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)
	fp := &titlerFakeProvider{replies: []titlerReply{{content: "narrative for proj-a"}}}
	gate := &fenceGate{}
	gate.leader.Store(true)
	gate.epochOK.Store(true)
	w := &LLMConsolidateWorker{
		Writer:     NewNarrativeWriter(fp, ""),
		Repo:       repo,
		Projects:   &stubProjectLister{ids: []string{"proj-a"}},
		Interval:   time.Hour,
		Logger:     zerolog.Nop(),
		LeaderGate: gate,
	}

	mock.ExpectQuery(`SELECT project_id, terms_json, chunks_scanned`).
		WithArgs("proj-a").
		WillReturnRows(sqlmock.NewRows([]string{
			"project_id", "terms_json", "chunks_scanned", "generated_at",
			"duration_ms", "narrative", "narrative_model", "narrative_generated_at",
		}).AddRow("proj-a", `[{"Term":"x","Count":1}]`, 1, time.Now(), 1, nil, nil, nil))
	mock.ExpectQuery(`SELECT content`).
		WithArgs("proj-a", 8).
		WillReturnRows(sqlmock.NewRows([]string{"content"}).AddRow("chunk one"))
	mock.ExpectExec(`UPDATE project_gists`).
		WithArgs("proj-a", "narrative for proj-a", "", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := w.processOne(context.Background(), "proj-a"); err != nil {
		t.Fatalf("processOne returned error: %v", err)
	}
	if fp.calls.Load() != 1 {
		t.Errorf("current leader made %d LLM calls; want 1", fp.calls.Load())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("current leader did not write the narrative: %v", err)
	}
}
