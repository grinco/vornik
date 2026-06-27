package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// CorpusEpochRepository owns the snapshot tables (corpus_epochs,
// corpus_epochs_active, corpus_rollbacks). One row per ingest run
// in corpus_epochs; per-project visibility pointer in
// corpus_epochs_active; rollback events recorded in
// corpus_rollbacks for audit.
//
// Iceberg-style: epochs are manifests, the active set is a pointer
// table, rollback is an atomic UPDATE. See
// https://docs.vornik.io §8.
type CorpusEpochRepository struct {
	db DBTX
}

// NewCorpusEpochRepository constructs the repo.
func NewCorpusEpochRepository(db DBTX) *CorpusEpochRepository {
	return &CorpusEpochRepository{db: db}
}

// CreateEpoch inserts a new epoch row in 'open' state (closed_at
// NULL). Pipeline calls this at publish-step start. Caller fills
// counts + closes via CloseEpoch.
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
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`,
		e.ID, e.ProjectID, e.IngestExecutionID, e.CreatedAt,
		e.ChunksAdmitted, e.ChunksQuarantined, e.ChunksVerified,
		e.ChunksRefuted, e.ChunksSuperseded, e.Notes,
	)
	return mapDBError(err)
}

// CloseEpoch sets closed_at and updates the per-class counts.
// Insert into corpus_epochs_active happens in a follow-up call so
// the caller can decide whether to activate (e.g. an empty epoch
// with chunks_admitted=0 is closed but not activated).
func (r *CorpusEpochRepository) CloseEpoch(ctx context.Context, epochID string, counts persistence.CorpusEpochCounts) error {
	if epochID == "" {
		return fmt.Errorf("CloseEpoch: epochID required")
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE corpus_epochs
		SET closed_at = NOW(),
		    chunks_admitted    = $2,
		    chunks_quarantined = $3,
		    chunks_verified    = $4,
		    chunks_refuted     = $5,
		    chunks_superseded  = $6
		WHERE id = $1 AND closed_at IS NULL
	`, epochID, counts.Admitted, counts.Quarantined, counts.Verified,
		counts.Refuted, counts.Superseded)
	return mapDBError(err)
}

// Activate makes an epoch visible to default search. Idempotent
// via PRIMARY KEY (project_id, epoch_id) — a re-activate is a
// no-op. Also clears the explicit-deactivation tombstone so a
// deliberately re-activated epoch is once again eligible for
// rollback re-activation (migration 89).
func (r *CorpusEpochRepository) Activate(ctx context.Context, projectID, epochID, by, reason string) error {
	if projectID == "" || epochID == "" {
		return fmt.Errorf("Activate: project_id and epoch_id required")
	}
	if by == "" {
		by = "system"
	}
	if _, err := r.db.ExecContext(ctx, `
		INSERT INTO corpus_epochs_active (project_id, epoch_id, activated_by, reason)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (project_id, epoch_id) DO NOTHING
	`, projectID, epochID, by, reason); err != nil {
		return mapDBError(err)
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE corpus_epochs
		SET deactivated_at = NULL, deactivated_by = NULL
		WHERE id = $1 AND project_id = $2 AND deactivated_at IS NOT NULL
	`, epochID, projectID)
	return mapDBError(err)
}

// Deactivate removes an epoch from the active set and stamps the
// explicit-deactivation tombstone on corpus_epochs, so RollbackTo's
// re-activation pass cannot resurrect a deliberately-hidden epoch
// (migration 89; latent finding #4 of the 2026-06-04 bug sweep).
// Idempotent — already-inactive is a no-op for the active set; the
// tombstone keeps its FIRST stamp.
func (r *CorpusEpochRepository) Deactivate(ctx context.Context, projectID, epochID, by string) error {
	if projectID == "" || epochID == "" {
		return fmt.Errorf("Deactivate: project_id and epoch_id required")
	}
	if by == "" {
		by = "system"
	}
	if _, err := r.db.ExecContext(ctx, `
		DELETE FROM corpus_epochs_active
		WHERE project_id = $1 AND epoch_id = $2
	`, projectID, epochID); err != nil {
		return mapDBError(err)
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE corpus_epochs
		SET deactivated_at = NOW(), deactivated_by = $3
		WHERE id = $1 AND project_id = $2 AND deactivated_at IS NULL
	`, epochID, projectID, by)
	return mapDBError(err)
}

// ListActive returns the active epoch IDs for one project. Used by
// the search query to filter chunks; also surfaced in the dashboard.
func (r *CorpusEpochRepository) ListActive(ctx context.Context, projectID string) ([]string, error) {
	if projectID == "" {
		return nil, fmt.Errorf("ListActive: project_id required")
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT epoch_id
		FROM corpus_epochs_active
		WHERE project_id = $1
		ORDER BY activated_at DESC
	`, projectID)
	if err != nil {
		return nil, mapDBError(err)
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

// ListEpochs returns the most recent N epochs for one project,
// regardless of active status. Powers the dashboard's "recent
// snapshots" view + the rollback-target picker.
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
		       (a.epoch_id IS NOT NULL) AS is_active
		FROM corpus_epochs e
		LEFT JOIN corpus_epochs_active a
		  ON a.project_id = e.project_id AND a.epoch_id = e.id
		WHERE e.project_id = $1
		ORDER BY e.created_at DESC
		LIMIT $2
	`, projectID, limit)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.CorpusEpoch
	for rows.Next() {
		ep := &persistence.CorpusEpoch{}
		if err := rows.Scan(
			&ep.ID, &ep.ProjectID, &ep.IngestExecutionID, &ep.CreatedAt,
			&ep.ClosedAt, &ep.ChunksAdmitted, &ep.ChunksQuarantined,
			&ep.ChunksVerified, &ep.ChunksRefuted, &ep.ChunksSuperseded,
			&ep.Notes, &ep.IsActive,
		); err != nil {
			return nil, err
		}
		out = append(out, ep)
	}
	return out, rows.Err()
}

// GetEpoch returns one epoch by ID.
func (r *CorpusEpochRepository) GetEpoch(ctx context.Context, epochID string) (*persistence.CorpusEpoch, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT e.id, e.project_id, e.ingest_execution_id, e.created_at,
		       e.closed_at, e.chunks_admitted, e.chunks_quarantined,
		       e.chunks_verified, e.chunks_refuted, e.chunks_superseded,
		       e.notes,
		       (a.epoch_id IS NOT NULL) AS is_active
		FROM corpus_epochs e
		LEFT JOIN corpus_epochs_active a
		  ON a.project_id = e.project_id AND a.epoch_id = e.id
		WHERE e.id = $1
	`, epochID)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return nil, sql.ErrNoRows
	}
	ep := &persistence.CorpusEpoch{}
	if err := rows.Scan(
		&ep.ID, &ep.ProjectID, &ep.IngestExecutionID, &ep.CreatedAt,
		&ep.ClosedAt, &ep.ChunksAdmitted, &ep.ChunksQuarantined,
		&ep.ChunksVerified, &ep.ChunksRefuted, &ep.ChunksSuperseded,
		&ep.Notes, &ep.IsActive,
	); err != nil {
		return nil, err
	}
	return ep, nil
}

// RollbackTo deactivates every epoch newer than the target while
// re-activating every non-tombstoned epoch up to and including the
// target, and restores chunks whose supersession was caused by a
// now-deactivated epoch (validation_status back to
// pre_supersede_status). Atomic — one transaction, so search never
// sees a half-rolled-back state (epoch hidden but the prior version
// still superseded). Records the action + restore count in
// corpus_rollbacks.
//
// The restore pass is the fix for the 2026-06-04 bug-sweep critical
// finding: pre-fix, rolling back a bad re-ingest hid the new chunks
// but left the prior versions 'superseded' — BOTH unretrievable. See
// https://docs.vornik.io
//
// Returns the number of epochs deactivated, the number activated, and
// the number of chunks restored.
func (r *CorpusEpochRepository) RollbackTo(ctx context.Context, projectID, targetEpochID, triggeredBy, reason string) (deactivated, activated, chunksRestored int, err error) {
	if projectID == "" || targetEpochID == "" {
		return 0, 0, 0, fmt.Errorf("RollbackTo: project_id and target_epoch_id required")
	}

	// Use the shared beginner so this works under both the raw *sql.DB
	// pool and the daemon's *DBWithMetrics wrapper. The old
	// r.db.(*sql.DB) assertion panicked on the metrics-wrapped handle
	// the daemon injects, making operator-triggered corpus rollback
	// unusable in production (bug sweep 2026-06-04).
	tx, ok, err := persistence.BeginTx(ctx, r.db, nil)
	if err != nil {
		return 0, 0, 0, mapDBError(err)
	}
	// exec runs the statements: the new transaction, or r.db directly
	// when an outer caller already owns a transaction.
	exec := r.db
	committed := false
	if ok {
		exec = tx
		defer func() {
			if !committed {
				_ = tx.Rollback()
			}
		}()
	}

	// Find the target epoch's created_at — the cut line.
	var cutCreatedAt time.Time
	if err := exec.QueryRowContext(ctx,
		`SELECT created_at FROM corpus_epochs WHERE id = $1 AND project_id = $2`,
		targetEpochID, projectID,
	).Scan(&cutCreatedAt); err != nil {
		return 0, 0, 0, fmt.Errorf("target epoch not found in project: %w", err)
	}

	// Deactivate everything newer than the cut.
	res, err := exec.ExecContext(ctx, `
		DELETE FROM corpus_epochs_active
		WHERE project_id = $1
		  AND epoch_id IN (
		    SELECT id FROM corpus_epochs
		    WHERE project_id = $1 AND created_at > $2
		  )
	`, projectID, cutCreatedAt)
	if err != nil {
		return 0, 0, 0, mapDBError(err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		deactivated = int(n)
	}

	// Re-activate everything up to and including the cut — EXCEPT
	// epochs an operator explicitly deactivated (tombstoned via
	// Deactivate). Pre-migration-89 this clause was absent, so a
	// rollback resurrected deliberately-hidden epochs.
	res, err = exec.ExecContext(ctx, `
		INSERT INTO corpus_epochs_active (project_id, epoch_id, activated_by, reason)
		SELECT project_id, id, $3, $4
		FROM corpus_epochs
		WHERE project_id = $1 AND created_at <= $2 AND closed_at IS NOT NULL
		  AND deactivated_at IS NULL
		ON CONFLICT (project_id, epoch_id) DO NOTHING
	`, projectID, cutCreatedAt, triggeredBy, reason)
	if err != nil {
		return 0, 0, 0, mapDBError(err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		activated = int(n)
	}

	// Restore pass (migration 89): un-supersede chunks whose
	// supersession was CAUSED by an epoch this rollback just
	// deactivated. Keyed on the cause, not the victim — a chunk
	// superseded by a still-active epoch stays superseded because its
	// replacement remains visible. Restores the exact prior status
	// (verified chunks come back verified — the verification was
	// about THIS content, which is what returns); NULL provenance
	// falls back to 'unverified'. Idempotent: the columns clear on
	// restore, so a repeated rollback matches zero rows.
	res, err = exec.ExecContext(ctx, `
		UPDATE project_memory_chunks
		SET validation_status    = COALESCE(NULLIF(pre_supersede_status, ''), 'unverified'),
		    superseded_in_epoch  = NULL,
		    pre_supersede_status = NULL
		WHERE project_id = $1
		  AND validation_status = 'superseded'
		  AND superseded_in_epoch IN (
		    SELECT id FROM corpus_epochs
		    WHERE project_id = $1 AND created_at > $2
		  )
	`, projectID, cutCreatedAt)
	if err != nil {
		return 0, 0, 0, mapDBError(err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		chunksRestored = int(n)
	}

	// Find the previous most-recent epoch newer than the cut (the
	// epoch that was active before this rollback). Used for the
	// audit row's from_epoch_id. Single SELECT against the
	// epochs table (post-DELETE — corpus_epochs_active no longer
	// has the row), so we look at the epochs themselves.
	var fromEpoch *string
	{
		var id sql.NullString
		_ = exec.QueryRowContext(ctx, `
			SELECT id FROM corpus_epochs
			WHERE project_id = $1 AND created_at > $2
			ORDER BY created_at DESC LIMIT 1
		`, projectID, cutCreatedAt).Scan(&id)
		if id.Valid {
			s := id.String
			fromEpoch = &s
		}
	}

	// Audit row.
	_, err = exec.ExecContext(ctx, `
		INSERT INTO corpus_rollbacks (id, project_id, from_epoch_id, to_epoch_id, triggered_by, reason, chunks_restored)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, persistence.GenerateID("rb"), projectID, fromEpoch, targetEpochID, triggeredBy, reason, chunksRestored)
	if err != nil {
		return 0, 0, 0, mapDBError(err)
	}

	if ok {
		if err := tx.Commit(); err != nil {
			return 0, 0, 0, mapDBError(err)
		}
	}
	committed = true
	return deactivated, activated, chunksRestored, nil
}

// ListRollbacks returns recent rollback events for one project.
// Powers the dashboard's audit view.
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
		WHERE project_id = $1
		ORDER BY applied_at DESC
		LIMIT $2
	`, projectID, limit)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.CorpusRollback
	for rows.Next() {
		rb := &persistence.CorpusRollback{}
		if err := rows.Scan(
			&rb.ID, &rb.ProjectID, &rb.FromEpochID, &rb.ToEpochID,
			&rb.TriggeredBy, &rb.Reason, &rb.AppliedAt, &rb.ChunksRestored,
		); err != nil {
			return nil, err
		}
		out = append(out, rb)
	}
	return out, rows.Err()
}

// CountRollbackRestorable previews the restore pass for a rollback to
// targetEpochID without mutating anything: restorable = superseded
// chunks whose causing epoch would be deactivated; nonRestorable =
// superseded chunks in the project with no recorded provenance
// (pre-migration-89 history) — surfaced so the operator sees what
// CANNOT come back instead of a silent gap. See
// https://docs.vornik.io §3.4.
func (r *CorpusEpochRepository) CountRollbackRestorable(ctx context.Context, projectID, targetEpochID string) (restorable, nonRestorable int, err error) {
	if projectID == "" || targetEpochID == "" {
		return 0, 0, fmt.Errorf("CountRollbackRestorable: project_id and target_epoch_id required")
	}
	err = r.db.QueryRowContext(ctx, `
		SELECT
		  COUNT(*) FILTER (WHERE c.superseded_in_epoch IN (
		      SELECT e.id FROM corpus_epochs e
		      WHERE e.project_id = $1
		        AND e.created_at > (SELECT t.created_at FROM corpus_epochs t
		                            WHERE t.id = $2 AND t.project_id = $1)
		  )),
		  COUNT(*) FILTER (WHERE c.superseded_in_epoch IS NULL)
		FROM project_memory_chunks c
		WHERE c.project_id = $1
		  AND c.validation_status = 'superseded'
	`, projectID, targetEpochID).Scan(&restorable, &nonRestorable)
	if err != nil {
		return 0, 0, mapDBError(err)
	}
	return restorable, nonRestorable, nil
}

// ErrEpochNotFound — sentinel for missing-epoch lookups.
var ErrEpochNotFound = errors.New("corpus epoch not found")
