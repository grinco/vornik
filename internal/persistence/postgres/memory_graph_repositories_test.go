package postgres

import (
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"vornik.io/vornik/internal/persistence"
)

func TestEntityMentionRepositoryCRUD(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewEntityMentionRepository(db)

	if err := repo.Insert(context.Background(), nil); err == nil {
		t.Fatal("expected nil mention error")
	}
	if _, err := repo.ListByEntity(context.Background(), "", 10); err == nil {
		t.Fatal("expected empty entity id error")
	}

	charEnd := 12
	mention := &persistence.EntityMention{
		ChunkID: "chunk-1", EntityID: "entity-1", CharStart: 4, CharEnd: &charEnd, Surface: "Acme",
	}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO entity_mentions")).
		WithArgs(mention.ChunkID, mention.EntityID, mention.CharStart, mention.CharEnd, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.Insert(context.Background(), mention); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT m.chunk_id, m.entity_id, m.char_start, m.char_end, m.surface")).
		WithArgs("entity-1", 100).
		WillReturnRows(sqlmock.NewRows([]string{"chunk_id", "entity_id", "char_start", "char_end", "surface"}).
			AddRow("chunk-1", "entity-1", 4, 12, "Acme"))
	got, err := repo.ListByEntity(context.Background(), "entity-1", 0)
	if err != nil {
		t.Fatalf("ListByEntity() error = %v", err)
	}
	if len(got) != 1 || got[0].CharEnd == nil || *got[0].CharEnd != 12 || got[0].Surface != "Acme" {
		t.Fatalf("ListByEntity() = %#v", got)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT chunk_id, entity_id, char_start, char_end, surface")).
		WithArgs("chunk-1").
		WillReturnRows(sqlmock.NewRows([]string{"chunk_id", "entity_id", "char_start", "char_end", "surface"}).
			AddRow("chunk-1", "entity-2", 0, nil, nil))
	got, err = repo.ListByChunk(context.Background(), "chunk-1")
	if err != nil {
		t.Fatalf("ListByChunk() error = %v", err)
	}
	if len(got) != 1 || got[0].CharEnd != nil || got[0].Surface != "" {
		t.Fatalf("ListByChunk() = %#v", got)
	}

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM entity_mentions WHERE chunk_id = $1")).
		WithArgs("chunk-1").
		WillReturnResult(sqlmock.NewResult(0, 2))
	if err := repo.DeleteForChunk(context.Background(), "chunk-1"); err != nil {
		t.Fatalf("DeleteForChunk() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestMemoryQuarantineRepositoryLifecycle(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewMemoryQuarantineRepository(db)

	if err := repo.Insert(context.Background(), nil); err == nil {
		t.Fatal("expected nil item error")
	}
	if _, err := repo.ListPending(context.Background(), "", 10); err == nil {
		t.Fatal("expected empty project id error")
	}
	if _, err := repo.Get(context.Background(), ""); err == nil {
		t.Fatal("expected empty id error")
	}

	role := "coder"
	execID := "exec-1"
	class := "decision"
	detail := "missing audit overlap"
	item := &persistence.MemoryQuarantineItem{
		ProjectID: "proj-a", SourceArtifactID: "art-1", ProducerRole: &role,
		IngestExecutionID: &execID, Content: "content", ContentHash: "hash",
		ProposedClass: &class, FailedGate: "claim_audit_overlap", FailureDetail: &detail,
	}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO project_memory_quarantine")).
		WithArgs(sqlmock.AnyArg(), item.ProjectID, item.SourceArtifactID, item.ProducerRole, item.IngestExecutionID, item.Content, item.ContentHash, item.ProposedClass, item.FailedGate, item.FailureDetail, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.Insert(context.Background(), item); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}
	if item.ID == "" || item.QuarantinedAt.IsZero() {
		t.Fatalf("Insert() did not set defaults: %#v", item)
	}

	quarantinedAt := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows([]string{
		"id", "project_id", "source_artifact_id", "producer_role",
		"ingest_execution_id", "content", "content_hash",
		"proposed_class", "failed_gate", "failure_detail",
		"quarantined_at", "released_at", "released_chunk_id", "dropped_at",
	}).AddRow("qrtn-1", "proj-a", "art-1", role, execID, "content", "hash", class, "claim_audit_overlap", detail, quarantinedAt, nil, nil, nil)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, project_id, source_artifact_id, producer_role")).
		WithArgs("proj-a", 50).
		WillReturnRows(rows)
	pending, err := repo.ListPending(context.Background(), "proj-a", 0)
	if err != nil {
		t.Fatalf("ListPending() error = %v", err)
	}
	if len(pending) != 1 || pending[0].ID != "qrtn-1" || pending[0].ProducerRole == nil || *pending[0].ProducerRole != role {
		t.Fatalf("ListPending() = %#v", pending)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, project_id, source_artifact_id, producer_role")).
		WithArgs("missing").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "source_artifact_id", "producer_role",
			"ingest_execution_id", "content", "content_hash",
			"proposed_class", "failed_gate", "failure_detail",
			"quarantined_at", "released_at", "released_chunk_id", "dropped_at",
		}))
	if _, err := repo.Get(context.Background(), "missing"); err != sql.ErrNoRows {
		t.Fatalf("Get(missing) error = %v, want sql.ErrNoRows", err)
	}

	mock.ExpectExec(regexp.QuoteMeta("UPDATE project_memory_quarantine")).
		WithArgs("qrtn-1", "chunk-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.MarkReleased(context.Background(), "qrtn-1", "chunk-1"); err != nil {
		t.Fatalf("MarkReleased() error = %v", err)
	}

	mock.ExpectExec(regexp.QuoteMeta("UPDATE project_memory_quarantine")).
		WithArgs("qrtn-2").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.MarkDropped(context.Background(), "qrtn-2"); err != nil {
		t.Fatalf("MarkDropped() error = %v", err)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT failed_gate, COUNT(*)")).
		WithArgs("proj-a").
		WillReturnRows(sqlmock.NewRows([]string{"failed_gate", "count"}).AddRow("claim_audit_overlap", 3))
	counts, err := repo.CountByGate(context.Background(), "proj-a")
	if err != nil {
		t.Fatalf("CountByGate() error = %v", err)
	}
	if counts["claim_audit_overlap"] != 3 {
		t.Fatalf("CountByGate() = %#v", counts)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}
