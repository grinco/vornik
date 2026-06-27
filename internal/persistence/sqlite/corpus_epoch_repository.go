package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// CorpusEpochRepository owns the corpus_epochs +
// corpus_epochs_active + corpus_rollbacks triad. RollbackTo wraps
// the multi-step mutation in a transaction so the active set never
// flips inconsistently.
type CorpusEpochRepository struct {
	db DBTX
}

func NewCorpusEpochRepository(db DBTX) *CorpusEpochRepository {
	return &CorpusEpochRepository{db: db}
}

func (r *CorpusEpochRepository) CreateEpoch(ctx context.Context, e *persistence.CorpusEpoch) error {
	if e == nil {
		return fmt.Errorf("CreateEpoch: nil epoch")
	}
	if e.ProjectID == "" {
		return fmt.Errorf("CreateEpoch: project_id required")
	}
	if e.ID == "" {
		e.ID = persistence.GenerateID("epoch")
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO corpus_epochs (
			id, project_id, ingest_execution_id, created_at,
			chunks_admitted, chunks_quarantined, chunks_verified,
			chunks_refuted, chunks_superseded, notes
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.ProjectID, e.IngestExecutionID, sqliteTime(e.CreatedAt),
		e.ChunksAdmitted, e.ChunksQuarantined, e.ChunksVerified,
		e.ChunksRefuted, e.ChunksSuperseded, e.Notes,
	)
	return err
}

func (r *CorpusEpochRepository) CloseEpoch(ctx context.Context, epochID string, counts persistence.CorpusEpochCounts) error {
	if epochID == "" {
		return fmt.Errorf("CloseEpoch: epochID required")
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE corpus_epochs
		SET closed_at = ?,
		    chunks_admitted    = ?,
		    chunks_quarantined = ?,
		    chunks_verified    = ?,
		    chunks_refuted     = ?,
		    chunks_superseded  = ?
		WHERE id = ? AND closed_at IS NULL`,
		sqliteTime(time.Now().UTC()),
		counts.Admitted, counts.Quarantined, counts.Verified,
		counts.Refuted, counts.Superseded, epochID)
	return err
}

func (r *CorpusEpochRepository) Activate(ctx context.Context, projectID, epochID, by, reason string) error {
	if projectID == "" || epochID == "" {
		return fmt.Errorf("Activate: project_id and epoch_id required")
	}
	if by == "" {
		by = "system"
	}
	if _, err := r.db.ExecContext(ctx, `
		INSERT INTO corpus_epochs_active (project_id, epoch_id, activated_at, activated_by, reason)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (project_id, epoch_id) DO NOTHING`,
		projectID, epochID, sqliteTime(time.Now().UTC()), by, reason); err != nil {
		return err
	}
	// Clear the explicit-deactivation tombstone (migration 89) so a
	// deliberately re-activated epoch is rollback-eligible again.
	_, err := r.db.ExecContext(ctx, `
		UPDATE corpus_epochs
		SET deactivated_at = NULL, deactivated_by = NULL
		WHERE id = ? AND project_id = ? AND deactivated_at IS NOT NULL`,
		epochID, projectID)
	return err
}

// Deactivate removes an epoch from the active set and stamps the
// explicit-deactivation tombstone (migration 89) so RollbackTo's
// re-activation pass cannot resurrect it.
func (r *CorpusEpochRepository) Deactivate(ctx context.Context, projectID, epochID, by string) error {
	if projectID == "" || epochID == "" {
		return fmt.Errorf("Deactivate: project_id and epoch_id required")
	}
	if by == "" {
		by = "system"
	}
	if _, err := r.db.ExecContext(ctx, `
		DELETE FROM corpus_epochs_active
		WHERE project_id = ? AND epoch_id = ?`,
		projectID, epochID); err != nil {
		return err
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE corpus_epochs
		SET deactivated_at = ?, deactivated_by = ?
		WHERE id = ? AND project_id = ? AND deactivated_at IS NULL`,
		sqliteTime(time.Now().UTC()), by, epochID, projectID)
	return err
}

func (r *CorpusEpochRepository) ListActive(ctx context.Context, projectID string) ([]string, error) {
	if projectID == "" {
		return nil, fmt.Errorf("ListActive: project_id required")
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT epoch_id FROM corpus_epochs_active
		WHERE project_id = ?
		ORDER BY activated_at DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (r *CorpusEpochRepository) ListEpochs(ctx context.Context, projectID string, limit int) ([]*persistence.CorpusEpoch, error) {
	if projectID == "" {
		return nil, fmt.Errorf("ListEpochs: project_id required")
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT e.id, e.project_id, e.ingest_execution_id, e.created_at,
		       e.closed_at, e.chunks_admitted, e.chunks_quarantined,
		       e.chunks_verified, e.chunks_refuted, e.chunks_superseded,
		       e.notes,
		       (a.epoch_id IS NOT NULL)
		FROM corpus_epochs e
		LEFT JOIN corpus_epochs_active a
		  ON a.project_id = e.project_id AND a.epoch_id = e.id
		WHERE e.project_id = ?
		ORDER BY e.created_at DESC
		LIMIT ?`, projectID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.CorpusEpoch
	for rows.Next() {
		ep, err := scanCorpusEpoch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ep)
	}
	return out, rows.Err()
}

func (r *CorpusEpochRepository) GetEpoch(ctx context.Context, epochID string) (*persistence.CorpusEpoch, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT e.id, e.project_id, e.ingest_execution_id, e.created_at,
		       e.closed_at, e.chunks_admitted, e.chunks_quarantined,
		       e.chunks_verified, e.chunks_refuted, e.chunks_superseded,
		       e.notes,
		       (a.epoch_id IS NOT NULL)
		FROM corpus_epochs e
		LEFT JOIN corpus_epochs_active a
		  ON a.project_id = e.project_id AND a.epoch_id = e.id
		WHERE e.id = ?`, epochID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return nil, sql.ErrNoRows
	}
	return scanCorpusEpoch(rows)
}

func scanCorpusEpoch(rows *sql.Rows) (*persistence.CorpusEpoch, error) {
	ep := &persistence.CorpusEpoch{}
	var (
		ingestExecID sql.NullString
		notes        sql.NullString
		createdAt    sqlTime
		closedAt     sqlNullTime
		isActiveBig  int64
	)
	if err := rows.Scan(
		&ep.ID, &ep.ProjectID, &ingestExecID, &createdAt,
		&closedAt, &ep.ChunksAdmitted, &ep.ChunksQuarantined,
		&ep.ChunksVerified, &ep.ChunksRefuted, &ep.ChunksSuperseded,
		&notes, &isActiveBig,
	); err != nil {
		return nil, err
	}
	if ingestExecID.Valid {
		ep.IngestExecutionID = &ingestExecID.String
	}
	ep.CreatedAt = createdAt.Time
	if closedAt.Valid {
		t := closedAt.Time
		ep.ClosedAt = &t
	}
	if notes.Valid {
		ep.Notes = &notes.String
	}
	ep.IsActive = isActiveBig != 0
	return ep, nil
}

// RollbackTo deactivates every epoch newer than the target and
// re-activates everything up to and including the target. Wrapped
// in a transaction (BEGIN IMMEDIATE) so the active set never lands
// in an inconsistent state.
// RollbackTo deactivates epochs newer than the target, re-activates
// non-tombstoned epochs at or before it, and restores chunks whose
// supersession was caused by a now-deactivated epoch — mirroring the
// postgres backend (migration 89; see
// https://docs.vornik.io).
func (r *CorpusEpochRepository) RollbackTo(ctx context.Context, projectID, targetEpochID, triggeredBy, reason string) (int, int, int, error) {
	if projectID == "" || targetEpochID == "" {
		return 0, 0, 0, fmt.Errorf("RollbackTo: project_id and target_epoch_id required")
	}
	db, ok := r.db.(*sql.DB)
	if !ok {
		return 0, 0, 0, fmt.Errorf("RollbackTo: requires *sql.DB")
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return 0, 0, 0, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Find the target epoch's created_at.
	var cutCreatedAt string
	if err := tx.QueryRowContext(ctx,
		`SELECT created_at FROM corpus_epochs WHERE id = ? AND project_id = ?`,
		targetEpochID, projectID,
	).Scan(&cutCreatedAt); err != nil {
		return 0, 0, 0, fmt.Errorf("target epoch not found in project: %w", err)
	}

	// Deactivate everything newer than the cut.
	deactivated := 0
	res, err := tx.ExecContext(ctx, `
		DELETE FROM corpus_epochs_active
		WHERE project_id = ?
		  AND epoch_id IN (
		    SELECT id FROM corpus_epochs
		    WHERE project_id = ? AND created_at > ?
		  )`, projectID, projectID, cutCreatedAt)
	if err != nil {
		return 0, 0, 0, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		deactivated = int(n)
	}

	// Re-activate everything up to and including the cut.
	activated := 0
	res, err = tx.ExecContext(ctx, `
		INSERT INTO corpus_epochs_active (project_id, epoch_id, activated_at, activated_by, reason)
		SELECT project_id, id, ?, ?, ?
		FROM corpus_epochs
		WHERE project_id = ? AND created_at <= ? AND closed_at IS NOT NULL
		  AND deactivated_at IS NULL
		ON CONFLICT (project_id, epoch_id) DO NOTHING`,
		sqliteTime(time.Now().UTC()), triggeredBy, reason, projectID, cutCreatedAt)
	if err != nil {
		return 0, 0, 0, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		activated = int(n)
	}

	// Restore pass (migration 89): un-supersede chunks whose
	// supersession was caused by an epoch this rollback just
	// deactivated. Mirrors the postgres backend; see the design doc.
	chunksRestored := 0
	res, err = tx.ExecContext(ctx, `
		UPDATE project_memory_chunks
		SET validation_status    = COALESCE(NULLIF(pre_supersede_status, ''), 'unverified'),
		    superseded_in_epoch  = NULL,
		    pre_supersede_status = NULL
		WHERE project_id = ?
		  AND validation_status = 'superseded'
		  AND superseded_in_epoch IN (
		    SELECT id FROM corpus_epochs
		    WHERE project_id = ? AND created_at > ?
		  )`, projectID, projectID, cutCreatedAt)
	if err != nil {
		return 0, 0, 0, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		chunksRestored = int(n)
	}

	// Find the previous most-recent epoch newer than the cut for
	// the from_epoch_id audit field.
	var fromEpoch sql.NullString
	_ = tx.QueryRowContext(ctx, `
		SELECT id FROM corpus_epochs
		WHERE project_id = ? AND created_at > ?
		ORDER BY created_at DESC LIMIT 1`,
		projectID, cutCreatedAt).Scan(&fromEpoch)
	var fromPtr any
	if fromEpoch.Valid {
		fromPtr = fromEpoch.String
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO corpus_rollbacks (
			id, project_id, from_epoch_id, to_epoch_id, triggered_by, reason, applied_at, chunks_restored
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		persistence.GenerateID("rb"), projectID, fromPtr, targetEpochID,
		triggeredBy, reason, sqliteTime(time.Now().UTC()), chunksRestored)
	if err != nil {
		return 0, 0, 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, 0, err
	}
	committed = true
	return deactivated, activated, chunksRestored, nil
}

// CountRollbackRestorable previews the restore pass for a rollback to
// targetEpochID — mirrors the postgres backend (SUM(CASE) instead of
// FILTER for SQLite-version safety). restorable = superseded chunks
// whose causing epoch would be deactivated; nonRestorable = superseded
// chunks with no recorded provenance.
func (r *CorpusEpochRepository) CountRollbackRestorable(ctx context.Context, projectID, targetEpochID string) (int, int, error) {
	if projectID == "" || targetEpochID == "" {
		return 0, 0, fmt.Errorf("CountRollbackRestorable: project_id and target_epoch_id required")
	}
	var restorable, nonRestorable int
	err := r.db.QueryRowContext(ctx, `
		SELECT
		  COALESCE(SUM(CASE WHEN c.superseded_in_epoch IN (
		      SELECT e.id FROM corpus_epochs e
		      WHERE e.project_id = ?
		        AND e.created_at > (SELECT t.created_at FROM corpus_epochs t
		                            WHERE t.id = ? AND t.project_id = ?)
		  ) THEN 1 ELSE 0 END), 0),
		  COALESCE(SUM(CASE WHEN c.superseded_in_epoch IS NULL THEN 1 ELSE 0 END), 0)
		FROM project_memory_chunks c
		WHERE c.project_id = ?
		  AND c.validation_status = 'superseded'`,
		projectID, targetEpochID, projectID, projectID).Scan(&restorable, &nonRestorable)
	if err != nil {
		return 0, 0, err
	}
	return restorable, nonRestorable, nil
}

func (r *CorpusEpochRepository) ListRollbacks(ctx context.Context, projectID string, limit int) ([]*persistence.CorpusRollback, error) {
	if projectID == "" {
		return nil, fmt.Errorf("ListRollbacks: project_id required")
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, project_id, from_epoch_id, to_epoch_id, triggered_by, reason, applied_at, chunks_restored
		FROM corpus_rollbacks
		WHERE project_id = ?
		ORDER BY applied_at DESC
		LIMIT ?`, projectID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.CorpusRollback
	for rows.Next() {
		rb := &persistence.CorpusRollback{}
		var (
			fromEpoch, toEpoch sql.NullString
			reasonStr          sql.NullString
			appliedAt          sqlTime
		)
		if err := rows.Scan(
			&rb.ID, &rb.ProjectID, &fromEpoch, &toEpoch,
			&rb.TriggeredBy, &reasonStr, &appliedAt, &rb.ChunksRestored,
		); err != nil {
			return nil, err
		}
		if fromEpoch.Valid {
			rb.FromEpochID = &fromEpoch.String
		}
		if toEpoch.Valid {
			rb.ToEpochID = &toEpoch.String
		}
		if reasonStr.Valid {
			rb.Reason = &reasonStr.String
		}
		rb.AppliedAt = appliedAt.Time
		out = append(out, rb)
	}
	return out, rows.Err()
}

// ErrEpochNotFound — sentinel for missing-epoch lookups.
var ErrEpochNotFound = errors.New("corpus epoch not found")

var _ = strings.Builder{}
