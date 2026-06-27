package postgres

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"vornik.io/vornik/internal/persistence"
)

func knowledgeEntityRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "project_id", "type", "canonical_name",
		"aliases", "description", "properties", "embedding",
		"extracted_by", "resolved_by", "confidence",
		"lifecycle_state", "validation_status", "epoch_id", "expires_at", "supersedes_id",
		"created_at", "updated_at",
	})
}

func knowledgeEdgeRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "project_id", "from_entity", "to_entity", "predicate",
		"properties", "source_chunks", "extracted_by", "confidence", "faithfulness",
		"lifecycle_state", "epoch_id", "created_at",
	})
}

func TestKnowledgeEntityRepositoryCorePaths(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewKnowledgeEntityRepository(db)

	if err := repo.Insert(context.Background(), nil); err == nil {
		t.Fatal("expected nil entity error")
	}
	if _, err := repo.Get(context.Background(), ""); err == nil {
		t.Fatal("expected empty id error")
	}
	if _, err := repo.List(context.Background(), persistence.KnowledgeEntityFilter{}); err == nil {
		t.Fatal("expected missing project id error")
	}

	entity := &persistence.KnowledgeEntity{
		ProjectID: "proj-a", Type: "company", CanonicalName: "Acme",
		Aliases: []byte(`["ACME"]`), Properties: []byte(`{"ticker":"ACME"}`), Embedding: []float32{0.1, 0.2},
		ExtractedBy: "extractor",
	}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO knowledge_entities")).
		WithArgs(sqlmock.AnyArg(), entity.ProjectID, entity.Type, entity.CanonicalName, sqlmock.AnyArg(), entity.Description, sqlmock.AnyArg(), "[0.1,0.2]", entity.ExtractedBy, sqlmock.AnyArg(), float32(1), "published", "unverified", entity.EpochID, entity.ExpiresAt, entity.SupersedesID, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.Insert(context.Background(), entity); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}
	if entity.ID == "" || entity.CreatedAt.IsZero() || entity.UpdatedAt.IsZero() {
		t.Fatalf("Insert() did not set defaults: %#v", entity)
	}

	created := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, project_id, type, canonical_name")).
		WithArgs("kent-1").
		WillReturnRows(knowledgeEntityRows().AddRow(
			"kent-1", "proj-a", "company", "Acme",
			`["ACME"]`, "desc", `{"ticker":"ACME"}`, "[0.1,0.2]",
			"extractor", "resolver", float32(0.9),
			"published", "verified", "epoch-1", created.Add(time.Hour), nil,
			created, created,
		))
	got, err := repo.Get(context.Background(), "kent-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got == nil || got.ID != "kent-1" || len(got.Embedding) != 2 || got.EpochID == nil {
		t.Fatalf("Get() = %#v", got)
	}

	projectID := "proj-a"
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, project_id, type, canonical_name")).
		WithArgs(projectID, sqlmock.AnyArg(), sqlmock.AnyArg(), "%Ac%", 10, 3).
		WillReturnRows(knowledgeEntityRows().AddRow(
			"kent-2", projectID, "person", "Ada",
			nil, "", nil, nil,
			nil, nil, float32(1),
			"published", "unverified", nil, nil, nil,
			created, created,
		))
	list, err := repo.List(context.Background(), persistence.KnowledgeEntityFilter{
		ProjectID: projectID, Types: []string{"person"}, NameLike: "Ac", Limit: 10, Offset: 3,
	})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 || list[0].ID != "kent-2" {
		t.Fatalf("List() = %#v", list)
	}

	empty, err := repo.SimilarByEmbedding(context.Background(), "proj-a", "company", nil, 10)
	if err != nil || empty != nil {
		t.Fatalf("SimilarByEmbedding(empty) = %#v, %v", empty, err)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, project_id, type, canonical_name")).
		WithArgs("proj-a", "company", "[0.1]", 50).
		WillReturnError(errors.New("operator does not exist: vector <=> vector"))
	empty, err = repo.SimilarByEmbedding(context.Background(), "proj-a", "company", []float32{0.1}, 0)
	if err != nil || empty != nil {
		t.Fatalf("SimilarByEmbedding(pgvector missing) = %#v, %v", empty, err)
	}

	mock.ExpectExec(regexp.QuoteMeta("UPDATE knowledge_entities")).
		WithArgs("kent-1", "archived").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.UpdateLifecycle(context.Background(), "kent-1", "archived"); err != nil {
		t.Fatalf("UpdateLifecycle() error = %v", err)
	}
	mock.ExpectExec(regexp.QuoteMeta("UPDATE knowledge_entities")).
		WithArgs("kent-1", "Acme Corp").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.AddAlias(context.Background(), "kent-1", "Acme Corp"); err != nil {
		t.Fatalf("AddAlias() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestKnowledgeEdgeRepositoryCorePaths(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewKnowledgeEdgeRepository(db)

	if err := repo.UpsertEdge(context.Background(), nil); err == nil {
		t.Fatal("expected nil edge error")
	}
	if _, err := repo.Get(context.Background(), ""); err == nil {
		t.Fatal("expected empty id error")
	}
	if _, err := repo.List(context.Background(), persistence.KnowledgeEdgeFilter{}); err == nil {
		t.Fatal("expected missing project id error")
	}

	faithfulness := float32(0.8)
	edge := &persistence.KnowledgeEdge{
		ProjectID: "proj-a", FromEntity: "kent-1", ToEntity: "kent-2", Predicate: "works_at",
		Properties: []byte(`{"role":"CEO"}`), SourceChunks: []string{"chunk-1"}, ExtractedBy: "extractor",
		Faithfulness: &faithfulness,
	}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO knowledge_edges")).
		WithArgs(sqlmock.AnyArg(), edge.ProjectID, edge.FromEntity, edge.ToEntity, edge.Predicate, sqlmock.AnyArg(), sqlmock.AnyArg(), edge.ExtractedBy, float32(1), edge.Faithfulness, "published", edge.EpochID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.UpsertEdge(context.Background(), edge); err != nil {
		t.Fatalf("UpsertEdge() error = %v", err)
	}
	if edge.ID == "" || edge.CreatedAt.IsZero() || edge.LifecycleState != "published" {
		t.Fatalf("UpsertEdge() did not set defaults: %#v", edge)
	}

	created := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, project_id, from_entity, to_entity, predicate")).
		WithArgs("kedge-1").
		WillReturnRows(knowledgeEdgeRows().AddRow(
			"kedge-1", "proj-a", "kent-1", "kent-2", "works_at",
			`{"role":"CEO"}`, "{chunk-1,chunk-2}", "extractor", float32(0.9), float64(0.8),
			"published", "epoch-1", created,
		))
	got, err := repo.Get(context.Background(), "kedge-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got == nil || got.ID != "kedge-1" || len(got.SourceChunks) != 2 || got.Faithfulness == nil {
		t.Fatalf("Get() = %#v", got)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, project_id, from_entity, to_entity, predicate")).
		WithArgs("proj-a", sqlmock.AnyArg(), "kent-1", "kent-2", "works_at", 20).
		WillReturnRows(knowledgeEdgeRows().AddRow(
			"kedge-2", "proj-a", "kent-1", "kent-2", "works_at",
			nil, "{chunk-1}", nil, float32(1), nil,
			"published", nil, created,
		))
	list, err := repo.List(context.Background(), persistence.KnowledgeEdgeFilter{
		ProjectID: "proj-a", FromEntity: "kent-1", ToEntity: "kent-2", Predicate: "works_at", Limit: 20,
	})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 || list[0].ID != "kedge-2" {
		t.Fatalf("List() = %#v", list)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, project_id, from_entity, to_entity, predicate")).
		WithArgs("kent-1", 100).
		WillReturnRows(knowledgeEdgeRows())
	edges, err := repo.EdgesForEntity(context.Background(), "kent-1", 0)
	if err != nil || len(edges) != 0 {
		t.Fatalf("EdgesForEntity() = %#v, %v", edges, err)
	}

	mock.ExpectExec(regexp.QuoteMeta("UPDATE knowledge_edges SET lifecycle_state = $2 WHERE id = $1")).
		WithArgs("kedge-1", "archived").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.UpdateLifecycle(context.Background(), "kedge-1", "archived"); err != nil {
		t.Fatalf("UpdateLifecycle() error = %v", err)
	}
	mock.ExpectExec(regexp.QuoteMeta("WITH updated AS")).
		WithArgs("chunk-1").
		WillReturnResult(sqlmock.NewResult(0, 2))
	n, err := repo.DropChunkFromSources(context.Background(), "chunk-1")
	if err != nil || n != 2 {
		t.Fatalf("DropChunkFromSources() = %d, %v", n, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestVectorEncodingHelpers(t *testing.T) {
	if got := encodeVector(nil); got != nil {
		t.Fatalf("encodeVector(nil) = %#v, want nil", got)
	}
	if got := encodeVector([]float32{1, 2.5}); got != "[1,2.5]" {
		t.Fatalf("encodeVector() = %#v", got)
	}
	if got := decodeVector("[1, 2.5]"); len(got) != 2 || got[0] != 1 || got[1] != 2.5 {
		t.Fatalf("decodeVector() = %#v", got)
	}
	if got := decodeVector("bad"); got != nil {
		t.Fatalf("decodeVector(bad) = %#v, want nil", got)
	}
}
