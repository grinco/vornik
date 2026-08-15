//go:build integration

package postgres

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/datasubject"
)

// Increment 4 (KG binder, resolve on demand) against a real database, because
// every property here is one a fake cannot demonstrate: an ILIKE over a JSONB
// alias array, a primary-key collision on link reassignment, and a CASCADE.
//
// Design: https://docs.vornik.io
// §4.2 binder 3.

func seedEntity(t *testing.T, db *DB, id, projectID, name, aliasesJSON string) {
	t.Helper()
	_, err := db.DB.Exec(`
		INSERT INTO knowledge_entities (id, project_id, type, canonical_name, aliases, lifecycle_state)
		VALUES ($1, $2, 'PERSON', $3, $4::jsonb, 'published')`,
		id, projectID, name, aliasesJSON)
	if err != nil {
		t.Fatalf("seed entity %s: %v", id, err)
	}
}

// seedChunk inserts a memory chunk. entity_mentions references it, and
// content_hash is UNIQUE per project, so each chunk needs distinct content.
func seedChunkInProject(t *testing.T, db *DB, id, projectID, content string) {
	t.Helper()
	_, err := db.DB.Exec(`
		INSERT INTO project_memory_chunks (id, project_id, source_name, content, content_hash)
		VALUES ($1, $2, 'integration-test', $3, md5($3))`,
		id, projectID, content)
	if err != nil {
		t.Fatalf("seed chunk %s: %v", id, err)
	}
}

// The coverage hole this index exists to close: KnowledgeEntityRepository.List
// matches canonical_name only, so a person stored as "J. Doe" with the alias
// "Jane Doe" would never be proposed for a request naming Jane Doe — an
// erasure that reports success while her data remains.
func TestIntegrationKGIndex_FindsPersonByAliasNotOnlyCanonicalName(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	idx := NewDataSubjectKGIndex(db.DB)

	const project = "kg-resolve-alias-test"
	resetKGFixture(t, db, project, "ds_real", "ds_ph", "ds_1", "ds_2")
	seedEntity(t, db, "ent_alias", project, "J. Doe", `["Jane Doe","Janey"]`)
	seedEntity(t, db, "ent_canon", project, "Jane Doe", `[]`)
	seedEntity(t, db, "ent_other", project, "Someone Else", `[]`)

	got, err := idx.FindPersonEntities(ctx, project, "Jane Doe", 50)
	if err != nil {
		t.Fatalf("FindPersonEntities: %v", err)
	}
	found := map[string]bool{}
	for _, e := range got {
		found[e.ID] = true
	}
	if !found["ent_alias"] {
		t.Error("an entity matching only on an alias was not found — this is the erasure gap")
	}
	if !found["ent_canon"] {
		t.Error("an entity matching on canonical name was not found")
	}
	if found["ent_other"] {
		t.Error("an unrelated entity was proposed")
	}
	for _, e := range got {
		if e.ID == "ent_alias" && len(e.Aliases) != 2 {
			t.Errorf("aliases not decoded for display: %+v", e.Aliases)
		}
	}
}

// A draft or superseded entity is not what the graph currently believes;
// binding a subject to one records a link the graph itself no longer stands
// behind.
func TestIntegrationKGIndex_SkipsUnpublishedEntities(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	idx := NewDataSubjectKGIndex(db.DB)

	const project = "kg-resolve-lifecycle-test"
	resetKGFixture(t, db, project, "ds_real", "ds_ph", "ds_1", "ds_2")
	seedEntity(t, db, "ent_live", project, "Jane Doe", `[]`)
	// A DIFFERENT canonical name, because (project_id, type, canonical_name) is
	// UNIQUE — a superseded entity and its replacement cannot share one. Both
	// still match the search below, which is what the assertion turns on.
	if _, err := db.DB.Exec(`
		INSERT INTO knowledge_entities (id, project_id, type, canonical_name, lifecycle_state)
		VALUES ('ent_dead', $1, 'PERSON', 'Jane Doe (former)', 'superseded')`, project); err != nil {
		t.Fatalf("seed superseded: %v", err)
	}
	got, err := idx.FindPersonEntities(ctx, project, "Jane", 50)
	if err != nil {
		t.Fatalf("FindPersonEntities: %v", err)
	}
	if len(got) != 1 || got[0].ID != "ent_live" {
		t.Errorf("want only the published entity, got %+v", got)
	}
}

// entity_mentions carries one row per OCCURRENCE. Several mentions in one
// chunk are still one row of personal data, and a duplicate would be a
// duplicated link attempt on every resolve.
func TestIntegrationKGIndex_MentionChunksAreDistinct(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	idx := NewDataSubjectKGIndex(db.DB)

	const project = "kg-resolve-mentions-test"
	resetKGFixture(t, db, project, "ds_real", "ds_ph", "ds_1", "ds_2")
	seedEntity(t, db, "ent_m", project, "Jane Doe", `[]`)
	// entity_mentions.chunk_id is a FOREIGN KEY into project_memory_chunks, so
	// the chunks have to exist first — the same constraint that makes the
	// mention rows cascade when a chunk or an entity is erased.
	seedChunkInProject(t, db, "chunk_1", project, "Jane Doe joined the call.")
	seedChunkInProject(t, db, "chunk_2", project, "Jane Doe sent the file.")
	for _, m := range []struct {
		chunk      string
		start, end int
	}{
		{"chunk_1", 0, 8}, {"chunk_1", 40, 48}, {"chunk_2", 5, 13},
	} {
		if _, err := db.DB.Exec(`
			INSERT INTO entity_mentions (chunk_id, entity_id, char_start, char_end, surface)
			VALUES ($1, 'ent_m', $2, $3, 'Jane Doe')`, m.chunk, m.start, m.end); err != nil {
			t.Fatalf("seed mention: %v", err)
		}
	}
	got, err := idx.MentionChunks(ctx, "ent_m", 100)
	if err != nil {
		t.Fatalf("MentionChunks: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("want 2 distinct chunks, got %d (%v)", len(got), got)
	}
}

// Adoption's core move. The collision is the COMMON case — the operator
// resolving a person often binds an entity whose chunks the placeholder
// already linked — so an UPDATE would fail on the primary key exactly when
// this is most needed.
func TestIntegrationReassignLinks_SurvivesRowsTheTargetAlreadyHas(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	repo := &DataSubjectRepository{db: db.DB}

	const project = "kg-reassign-test"
	resetKGFixture(t, db, project, "ds_real", "ds_ph", "ds_1", "ds_2")
	for id, name := range map[string]string{"ds_real": "Jane Doe", "ds_ph": "kg:ent_x"} {
		if err := repo.CreateSubject(ctx, datasubject.Subject{ID: id, DisplayName: name}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	link := func(subject, row string) {
		t.Helper()
		if err := repo.AddLink(ctx, subject, datasubject.Link{
			Table: datasubject.TableProjectMemoryChunks, RowID: row, ProjectID: project,
			Source: datasubject.SourceKGExtraction, Confidence: datasubject.ConfidencePossible,
			Exclusivity: datasubject.SharedRow,
		}); err != nil {
			t.Fatalf("link %s/%s: %v", subject, row, err)
		}
	}
	link("ds_ph", "chunk_shared")
	link("ds_ph", "chunk_only_placeholder")
	link("ds_real", "chunk_shared") // the collision

	moved, err := repo.ReassignLinks(ctx, "ds_ph", "ds_real")
	if err != nil {
		t.Fatalf("ReassignLinks: %v", err)
	}
	if moved != 2 {
		t.Errorf("moved = %d, want 2 (both source rows are now covered by the target)", moved)
	}
	after, err := repo.ListLinks(ctx, "ds_real")
	if err != nil {
		t.Fatalf("ListLinks: %v", err)
	}
	rows := map[string]bool{}
	for _, l := range after {
		rows[l.RowID] = true
	}
	if !rows["chunk_shared"] || !rows["chunk_only_placeholder"] {
		t.Errorf("target is missing rows after adoption: %+v", after)
	}
	left, err := repo.ListLinks(ctx, "ds_ph")
	if err != nil {
		t.Fatalf("ListLinks(placeholder): %v", err)
	}
	if len(left) != 0 {
		t.Errorf("placeholder still holds %d links; the index would double-count the person", len(left))
	}
}

func TestIntegrationReassignLinks_RefusesADegenerateMove(t *testing.T) {
	db := newIntegrationDB(t)
	repo := &DataSubjectRepository{db: db.DB}
	if _, err := repo.ReassignLinks(context.Background(), "ds_a", "ds_a"); err == nil {
		t.Error("reassigning a subject onto itself must be refused, not silently delete its links")
	}
}

// The index is itself personal data and must never outlive the subject it
// describes (design §8) — asserted against the real CASCADE, not the schema
// text.
func TestIntegrationDeleteSubject_TakesIdentifiersAndLinksWithIt(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	repo := &DataSubjectRepository{db: db.DB}

	if err := repo.CreateSubject(ctx, datasubject.Subject{ID: "ds_gone", DisplayName: "kg:ent_y"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.AddIdentifier(ctx, "ds_gone", datasubject.Identifier{
		Kind: datasubject.KindKGEntity, Value: "ent_y",
		Source: datasubject.SourceKGExtraction, Confidence: datasubject.ConfidencePossible,
	}); err != nil {
		t.Fatalf("add identifier: %v", err)
	}
	if err := repo.AddLink(ctx, "ds_gone", datasubject.Link{
		Table: datasubject.TableProjectMemoryChunks, RowID: "chunk_z", ProjectID: "p",
		Source: datasubject.SourceKGExtraction, Confidence: datasubject.ConfidencePossible,
		Exclusivity: datasubject.SharedRow,
	}); err != nil {
		t.Fatalf("add link: %v", err)
	}
	if err := repo.DeleteSubject(ctx, "ds_gone"); err != nil {
		t.Fatalf("DeleteSubject: %v", err)
	}
	owner, err := repo.FindSubjectByIdentifier(ctx, datasubject.KindKGEntity, "ent_y")
	if err != nil {
		t.Fatalf("FindSubjectByIdentifier: %v", err)
	}
	if owner != "" {
		t.Errorf("identifier outlived its subject (owner=%q) — the entity would read as still bound", owner)
	}
	links, err := repo.ListLinks(ctx, "ds_gone")
	if err != nil {
		t.Fatalf("ListLinks: %v", err)
	}
	if len(links) != 0 {
		t.Errorf("links outlived their subject: %+v", links)
	}
}

// resetKGFixture removes this test's seed rows before it writes them.
//
// These tests share one integration database, and none of them cleaned up: they
// passed on a fresh DB and failed on every re-run with "duplicate key". That is
// not a harmless quirk — it makes the lane un-rerunnable, so a developer who
// runs it twice sees failures that have nothing to do with their change and
// learns to distrust it. Cleaning BEFORE the seed rather than after also
// survives a previous run that crashed midway.
func resetKGFixture(t *testing.T, db *DB, project string, subjectIDs ...string) {
	t.Helper()
	for _, stmt := range []string{
		`DELETE FROM data_subject_links WHERE project_id = $1`,
		`DELETE FROM knowledge_mentions WHERE project_id = $1`,
		`DELETE FROM knowledge_entities WHERE project_id = $1`,
		`DELETE FROM project_memory_chunks WHERE project_id = $1`,
	} {
		if _, err := db.DB.Exec(stmt, project); err != nil {
			t.Logf("reset %s: %v", stmt, err)
		}
	}
	for _, id := range subjectIDs {
		if _, err := db.DB.Exec(`DELETE FROM data_subjects WHERE id = $1`, id); err != nil {
			t.Logf("reset subject %s: %v", id, err)
		}
	}
}
