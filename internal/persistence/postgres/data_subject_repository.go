package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/datasubject"
)

// DataSubjectRepository persists the GDPR data-subject axis and the rights
// ledger, and collects the content an Art 15 export discloses.
//
// see LLD § https://docs.vornik.io
type DataSubjectRepository struct {
	db *sql.DB
}

// NewDataSubjectRepository constructs the repository.
func NewDataSubjectRepository(db *sql.DB) *DataSubjectRepository {
	return &DataSubjectRepository{db: db}
}

// CreateSubject inserts a subject.
func (r *DataSubjectRepository) CreateSubject(ctx context.Context, s datasubject.Subject) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO data_subjects (id, display_name) VALUES ($1, $2)`, s.ID, s.DisplayName)
	return mapDBError(err)
}

// GetSubject fetches one subject.
func (r *DataSubjectRepository) GetSubject(ctx context.Context, id string) (datasubject.Subject, error) {
	var s datasubject.Subject
	var state sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT id, display_name, request_state FROM data_subjects WHERE id = $1`, id).
		Scan(&s.ID, &s.DisplayName, &state)
	if err != nil {
		return datasubject.Subject{}, mapDBError(err)
	}
	s.RequestState = state.String
	return s, nil
}

// ListSubjects returns subjects, newest first.
func (r *DataSubjectRepository) ListSubjects(ctx context.Context, limit int) ([]datasubject.Subject, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, display_name, COALESCE(request_state, '')
		   FROM data_subjects ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []datasubject.Subject
	for rows.Next() {
		var s datasubject.Subject
		if err := rows.Scan(&s.ID, &s.DisplayName, &s.RequestState); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// AddIdentifier records an identifier. Idempotent: re-binding the same
// identifier is normal (the same person mails in twice), so a repeat is not an
// error.
func (r *DataSubjectRepository) AddIdentifier(ctx context.Context, subjectID string, id datasubject.Identifier) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO data_subject_identifiers (subject_id, kind, value, source, confidence)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (subject_id, kind, value) DO NOTHING`,
		subjectID, id.Kind, id.Value, string(id.Source), string(id.Confidence))
	return mapDBError(err)
}

// ListIdentifiers returns a subject's identifiers.
func (r *DataSubjectRepository) ListIdentifiers(ctx context.Context, subjectID string) ([]datasubject.Identifier, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT kind, value, source, confidence FROM data_subject_identifiers
		  WHERE subject_id = $1 ORDER BY kind, value`, subjectID)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []datasubject.Identifier
	for rows.Next() {
		var id datasubject.Identifier
		var src, conf string
		if err := rows.Scan(&id.Kind, &id.Value, &src, &conf); err != nil {
			return nil, err
		}
		id.Source, id.Confidence = datasubject.Source(src), datasubject.Confidence(conf)
		out = append(out, id)
	}
	return out, rows.Err()
}

// FindSubjectByIdentifier resolves an identifier to a subject id. Returns
// ("", nil) when unknown — an unrecognised handle is an ordinary outcome, not an
// error.
func (r *DataSubjectRepository) FindSubjectByIdentifier(ctx context.Context, kind, value string) (string, error) {
	var id string
	err := r.db.QueryRowContext(ctx,
		`SELECT subject_id FROM data_subject_identifiers WHERE kind = $1 AND value = $2 LIMIT 1`,
		kind, value).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", mapDBError(err)
	}
	return id, nil
}

// AddLink records that a subject appears in a row.
//
// Validates through datasubject.Link so a link can never name a table outside
// the closed set — the same discipline the retention sweeper applies, and the
// reason an erasure cannot silently skip a table it does not know how to act on.
func (r *DataSubjectRepository) AddLink(ctx context.Context, subjectID string, l datasubject.Link) error {
	if err := l.Validate(); err != nil {
		return err
	}
	var project any
	if l.ProjectID != "" {
		project = l.ProjectID
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO data_subject_links
		   (subject_id, table_name, row_id, project_id, source, confidence, exclusivity)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (subject_id, table_name, row_id) DO UPDATE
		   SET confidence  = EXCLUDED.confidence,
		       exclusivity = EXCLUDED.exclusivity`,
		subjectID, string(l.Table), l.RowID, project,
		string(l.Source), string(l.Confidence), string(l.Exclusivity))
	return mapDBError(err)
}

// ListLinks returns a subject's links.
func (r *DataSubjectRepository) ListLinks(ctx context.Context, subjectID string) ([]datasubject.Link, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT table_name, row_id, COALESCE(project_id, ''), source, confidence, exclusivity
		   FROM data_subject_links WHERE subject_id = $1 ORDER BY table_name, row_id`, subjectID)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []datasubject.Link
	for rows.Next() {
		var l datasubject.Link
		var table, src, conf, excl string
		if err := rows.Scan(&table, &l.RowID, &l.ProjectID, &src, &conf, &excl); err != nil {
			return nil, err
		}
		l.Table = datasubject.LinkableTable(table)
		l.Source, l.Confidence, l.Exclusivity =
			datasubject.Source(src), datasubject.Confidence(conf), datasubject.Exclusivity(excl)
		out = append(out, l)
	}
	return out, rows.Err()
}

// CountOtherSubjectsOnRow reports how many OTHER subjects are linked to the
// same row.
//
// This is what turns exclusivity from a claim into an observation: a row with
// another subject on it is shared regardless of what a binder guessed, and
// Art 15(4) turns on the answer.
func (r *DataSubjectRepository) CountOtherSubjectsOnRow(ctx context.Context, subjectID, table, rowID string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM data_subject_links
		  WHERE table_name = $1 AND row_id = $2 AND subject_id <> $3`,
		table, rowID, subjectID).Scan(&n)
	return n, mapDBError(err)
}

// --- rights ledger ---

// CreateRequest opens a request.
func (r *DataSubjectRepository) CreateRequest(ctx context.Context, req datasubject.Request) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO data_subject_requests (id, subject_id, kind, state, opened_at, erasure_ground)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		req.ID, req.SubjectID, string(req.Kind), string(req.State), req.OpenedAt,
		nullIfEmpty(string(req.ErasureGround)))
	return mapDBError(err)
}

// GetRequest fetches one request.
func (r *DataSubjectRepository) GetRequest(ctx context.Context, id string) (datasubject.Request, error) {
	var req datasubject.Request
	var kind, state string
	var by, how, extReason, hash, refused, ground sql.NullString
	var verifiedAt sql.NullTime
	err := r.db.QueryRowContext(ctx,
		`SELECT id, subject_id, kind, state, opened_at, verified_by, verified_how, verified_at,
		        extended, extended_reason, report_hash, refused_reason, erasure_ground
		   FROM data_subject_requests WHERE id = $1`, id).
		Scan(&req.ID, &req.SubjectID, &kind, &state, &req.OpenedAt, &by, &how, &verifiedAt,
			&req.Extended, &extReason, &hash, &refused, &ground)
	if err != nil {
		return datasubject.Request{}, mapDBError(err)
	}
	req.Kind, req.State = datasubject.RequestKind(kind), datasubject.RequestState(state)
	req.VerifiedBy, req.VerifiedHow = by.String, how.String
	req.VerifiedAt = verifiedAt.Time
	req.ExtendedReason, req.ReportHash, req.RefusedReason = extReason.String, hash.String, refused.String
	req.ErasureGround = datasubject.ErasureGround(ground.String)
	return req, nil
}

// SaveRequest persists a state transition.
func (r *DataSubjectRepository) SaveRequest(ctx context.Context, req datasubject.Request) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE data_subject_requests
		    SET state = $2, verified_by = $3, verified_how = $4, verified_at = $5,
		        extended = $6, extended_reason = $7, report_hash = $8, refused_reason = $9,
		        updated_at = NOW()
		  WHERE id = $1`,
		req.ID, string(req.State), nullIfEmpty(req.VerifiedBy), nullIfEmpty(req.VerifiedHow),
		nullTimeIfZero(req.VerifiedAt), req.Extended, nullIfEmpty(req.ExtendedReason),
		nullIfEmpty(req.ReportHash), nullIfEmpty(req.RefusedReason))
	return mapDBError(err)
}

// ListLiveRequests returns requests still awaiting a response, oldest first —
// the query the Art 12(3) deadline check runs.
func (r *DataSubjectRepository) ListLiveRequests(ctx context.Context) ([]datasubject.Request, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, subject_id, kind, state, opened_at, extended
		   FROM data_subject_requests
		  WHERE state IN ('open', 'verified')
		  ORDER BY opened_at`)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []datasubject.Request
	for rows.Next() {
		var req datasubject.Request
		var kind, state string
		if err := rows.Scan(&req.ID, &req.SubjectID, &kind, &state, &req.OpenedAt, &req.Extended); err != nil {
			return nil, err
		}
		req.Kind, req.State = datasubject.RequestKind(kind), datasubject.RequestState(state)
		out = append(out, req)
	}
	return out, rows.Err()
}

// --- content collection ---

// collectorSpec says how to read a linkable table's personal data.
//
// Table-driven rather than per-table code so a table cannot be silently
// forgotten: the specs are asserted against datasubject's closed set by
// TestEveryLinkableTableHasACollector. Column and table names come only from
// this map — never from a link row — so nothing user-influenced reaches SQL.
type collectorSpec struct {
	idCol       string
	contentCols []string
	projectCol  string // empty when the table is global
	// originNote describes where such a row comes from, answering Art 14(2)(f).
	originNote string
}

var collectorSpecs = map[datasubject.LinkableTable]collectorSpec{
	datasubject.TableChatAuditLog: {
		idCol: "id", contentCols: []string{"user_message", "response"}, projectCol: "project_id",
		originNote: "a chat turn you sent to this deployment",
	},
	datasubject.TableTaskMessages: {
		idCol: "id", contentCols: []string{"content"},
		originNote: "a message in a task conversation",
	},
	datasubject.TableProjectMemoryChunks: {
		idCol: "id", contentCols: []string{"content_title", "content"}, projectCol: "project_id",
		originNote: "long-term memory derived from correspondence, documents, or chat",
	},
	datasubject.TableArtifacts: {
		idCol: "id", contentCols: []string{"name"},
		originNote: "a file uploaded to this deployment",
	},
	datasubject.TableExtractedDocuments: {
		idCol: "id", contentCols: []string{"mime_type"}, projectCol: "project_id",
		originNote: "text extracted from an uploaded file",
	},
	datasubject.TableChannelSessions: {
		idCol: "session_id", contentCols: []string{"active_project"},
		originNote: "per-channel session state",
	},
	datasubject.TableKnowledgeEntities: {
		idCol: "id", contentCols: []string{"canonical_name", "description"}, projectCol: "project_id",
		originNote: "an entity extracted from ingested content",
	},
	datasubject.TableOperatorProfile: {
		idCol: "operator_id", contentCols: []string{"notes"},
		originNote: "the profile this assistant keeps about you",
	},
	datasubject.TableUserIdentities: {
		idCol: "id", contentCols: []string{"channel", "external_id", "display"},
		originNote: "your identity mapping for a channel",
	},
}

// CollectItems reads the content behind a subject's links.
//
// Exclusivity is RE-DERIVED here rather than trusted from the link row: if any
// other subject is linked to the same row it is shared, whatever a binder
// guessed. Art 15(4) turns on that answer, so it is observed at export time
// rather than inherited from an earlier assumption.
//
// A row that has since disappeared is skipped rather than reported — telling a
// subject about data that no longer exists would be its own inaccuracy.
func (r *DataSubjectRepository) CollectItems(ctx context.Context, subjectID string, links []datasubject.Link) ([]datasubject.Item, error) {
	out := make([]datasubject.Item, 0, len(links))
	for _, l := range links {
		spec, ok := collectorSpecs[l.Table]
		if !ok {
			// Unreachable while the collector test passes; refuse rather than
			// silently omit, so a new table cannot slip out of exports.
			return nil, fmt.Errorf("postgres: no collector for linkable table %q", l.Table)
		}
		cols := append([]string{}, spec.contentCols...)
		q := fmt.Sprintf(`SELECT %s FROM %s WHERE %s = $1`,
			strings.Join(coalesceAll(cols), ", "), l.Table, spec.idCol)
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := r.db.QueryRowContext(ctx, q, l.RowID).Scan(ptrs...); err != nil {
			if err == sql.ErrNoRows {
				continue // row is gone; reporting it would be inaccurate
			}
			return nil, fmt.Errorf("postgres: collect %s/%s: %w", l.Table, l.RowID, err)
		}
		parts := make([]string, 0, len(vals))
		for _, v := range vals {
			if s, _ := v.(string); strings.TrimSpace(s) != "" {
				parts = append(parts, s)
			}
		}

		others, err := r.CountOtherSubjectsOnRow(ctx, subjectID, string(l.Table), l.RowID)
		if err != nil {
			return nil, err
		}
		excl := l.Exclusivity
		if others > 0 {
			excl = datasubject.SharedRow
		}

		out = append(out, datasubject.Item{
			Table: l.Table, RowID: l.RowID, ProjectID: l.ProjectID,
			Source: l.Source, Confidence: l.Confidence, Exclusivity: excl,
			Content: strings.Join(parts, "\n"),
			Context: fmt.Sprintf("%s record %s", l.Table, l.RowID),
			Origin:  spec.originNote,
			// Only data the subject themselves supplied is portable under
			// Art 20; a derived summary is not, however much it is about them.
			ProvidedBySubject: l.Table == datasubject.TableChatAuditLog ||
				l.Table == datasubject.TableTaskMessages,
		})
	}
	return out, nil
}

// coalesceAll wraps each column so a NULL scans as an empty string rather than
// failing the whole collection.
func coalesceAll(cols []string) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = fmt.Sprintf("COALESCE(%s::text, '')", c)
	}
	return out
}

func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func nullTimeIfZero(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// DeleteRow removes one row of a linkable table, together with every
// data_subject_link pointing at it, as part of an Art 17 erasure.
//
// The table and its id column come ONLY from collectorSpecs — the same closed map
// the export collector uses, never from a link row — so nothing user-influenced
// reaches SQL. Reusing that map rather than adding a parallel eraser map means
// TestEveryLinkableTableHasACollector guards erasure coverage too: a new linkable
// table cannot become erasable-but-uncollectable or the reverse.
//
// ARTIFACTS ARE REFUSED. `extracted_documents` has no foreign key on
// `source_artifact_id`, and `project_memory_chunks.artifact_id` is ON DELETE SET
// NULL, so deleting an artifact row orphans its extraction and leaves the derived
// embedding in the vector store while destroying the provenance that would let
// anyone find it. internal/erasure is the only correct path; the datasubject
// Executor routes artifacts there, and this refusal is the defence in depth for
// when something routes them wrongly.
//
// The links go with the row because a link asserts "this person appears in this
// row". Once the row is gone that assertion is itself stale personal data, and
// keeping it would retain a record of the person's presence after erasing the
// thing it referred to. The durable record of what was erased lives in
// data_subject_requests, which is declared retained under Art 17(3)(b).
func (r *DataSubjectRepository) DeleteRow(ctx context.Context, table datasubject.LinkableTable, rowID string) error {
	if table == datasubject.TableArtifacts {
		return fmt.Errorf("postgres: refusing to delete %s/%s as a plain row — "+
			"artifacts must go through the erasure cascade (internal/erasure), which also removes the "+
			"extraction rows, the derived memory chunks, and the on-disk storage directory",
			table, rowID)
	}
	spec, ok := collectorSpecs[table]
	if !ok {
		return fmt.Errorf("postgres: no spec for linkable table %q — refusing to build a DELETE for it", table)
	}
	if strings.TrimSpace(rowID) == "" {
		return fmt.Errorf("postgres: row id is required to delete from %s", table)
	}
	if r == nil || r.db == nil {
		return fmt.Errorf("postgres: DeleteRow requires a database handle")
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgres: begin erasure tx for %s/%s: %w", table, rowID, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM %s WHERE %s = $1`, table, spec.idCol), rowID); err != nil {
		return fmt.Errorf("postgres: delete %s/%s: %w", table, rowID, err)
	}
	// Every subject's link to this row, not just the requester's: the row is gone,
	// so any remaining link would point at nothing.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM data_subject_links WHERE table_name = $1 AND row_id = $2`,
		string(table), rowID); err != nil {
		return fmt.Errorf("postgres: delete links for %s/%s: %w", table, rowID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("postgres: commit erasure of %s/%s: %w", table, rowID, err)
	}
	return nil
}

// SaveRequestReport stores the rights-request report and its hash together.
//
// Together deliberately: report_hash is the Art 5(2) evidence that a right was
// honoured, and a hash without the document it fingerprints attests to nothing.
// Before this existed the hash was written to the ledger whenever a request was
// actioned while the report itself was saved only if the operator passed --out.
//
// This is also what makes deleting data_subject_links on erasure defensible. A
// link asserts "this person appears in this row" and becomes stale personal data
// once the row is gone, so it is removed — but the record of WHAT was erased has
// to survive somewhere, and it survives here: the report enumerates every table,
// row, disposition and reason, retained under Art 17(3)(b).
func (r *DataSubjectRepository) SaveRequestReport(ctx context.Context, requestID, reportJSON, hash string) error {
	if strings.TrimSpace(requestID) == "" {
		return fmt.Errorf("postgres: request id is required to store a report")
	}
	if strings.TrimSpace(reportJSON) == "" {
		return fmt.Errorf("postgres: refusing to record a report hash with no report body — " +
			"a fingerprint of an unsaved document is not accountability evidence")
	}
	if strings.TrimSpace(hash) == "" {
		return fmt.Errorf("postgres: refusing to store a report with no hash — nothing would pin it")
	}
	if r == nil || r.db == nil {
		return fmt.Errorf("postgres: SaveRequestReport requires a database handle")
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE data_subject_requests
		    SET report_json = $2, report_hash = $3, updated_at = NOW()
		  WHERE id = $1`, requestID, reportJSON, hash)
	return mapDBError(err)
}
