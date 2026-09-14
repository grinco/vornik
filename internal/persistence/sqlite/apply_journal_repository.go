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

// ApplyJournalRepository is the SQLite persistence.ApplyJournalRepository —
// the durable multi-file config apply record (LLD 2026-09-13 config-apply-
// journal §2; Postgres parity: migration 184). JSONB columns are TEXT here;
// the typed fields are encoded once by persistence.MarshalJournalColumns so
// the two stores cannot disagree about the document shape.
//
// Durability: the DSN opens with synchronous(FULL) (sqlite.go), the SQLite
// half of §8's contract — Prepare's commit is on disk before the engine
// writes its first temp file.
type ApplyJournalRepository struct {
	db DBTX
}

// NewApplyJournalRepository constructs the repository over db.
func NewApplyJournalRepository(db DBTX) *ApplyJournalRepository {
	return &ApplyJournalRepository{db: db}
}

// Compile-time interface pin.
var _ persistence.ApplyJournalRepository = (*ApplyJournalRepository)(nil)

const applyJournalColumns = `id, proposal_id, request_id, state, op_digest, schema_version,
	deployment_root, affected_projects, writer_epoch, ops, pre_images, read_set, progress,
	generation_before, generation_after, failure, created_at, updated_at, terminal_at`

// Prepare inserts row in PREPARED. A second open row for the same proposal
// collides on uq_config_apply_journal_open_proposal → ErrDuplicateKey.
func (r *ApplyJournalRepository) Prepare(ctx context.Context, row *persistence.ConfigApplyJournal) error {
	if err := persistence.ValidateConfigApplyJournal(row); err != nil {
		return err
	}
	cols, err := persistence.MarshalJournalColumns(row)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if row.CreatedAt.IsZero() {
		row.CreatedAt = now
	}
	if row.UpdatedAt.IsZero() {
		row.UpdatedAt = row.CreatedAt
	}
	if row.SchemaVersion == 0 {
		row.SchemaVersion = persistence.JournalSchemaVersion
	}
	row.State = persistence.JournalStatePrepared
	row.TerminalAt = nil
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO config_apply_journal (`+applyJournalColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		row.ID, row.ProposalID, nullStr(row.RequestID), row.State, row.OpDigest, row.SchemaVersion,
		row.DeploymentRoot, cols.AffectedProjects, row.WriterEpoch, cols.Ops, cols.PreImages, cols.ReadSet, cols.Progress,
		nullStr(row.GenerationBefore), nullStr(row.GenerationAfter), nullStr(row.Failure),
		sqliteTime(row.CreatedAt), sqliteTime(row.UpdatedAt), nil,
	)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return persistence.ErrDuplicateKey
	}
	return err
}

// Get fetches a row by id; ErrNotFound when absent.
func (r *ApplyJournalRepository) Get(ctx context.Context, id string) (*persistence.ConfigApplyJournal, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+applyJournalColumns+` FROM config_apply_journal WHERE id = ?`, id)
	return scanApplyJournalRow(row)
}

// OpenForProposal returns the open row for proposalID; ErrNotFound when none.
func (r *ApplyJournalRepository) OpenForProposal(ctx context.Context, proposalID string) (*persistence.ConfigApplyJournal, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+applyJournalColumns+` FROM config_apply_journal
		WHERE proposal_id = ? AND terminal_at IS NULL`, proposalID)
	return scanApplyJournalRow(row)
}

// ListOpen returns every open row, oldest first.
func (r *ApplyJournalRepository) ListOpen(ctx context.Context) ([]*persistence.ConfigApplyJournal, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+applyJournalColumns+` FROM config_apply_journal
		WHERE terminal_at IS NULL ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.ConfigApplyJournal
	for rows.Next() {
		j, serr := scanApplyJournal(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// Transition is the compare-and-set on state (§3). A row not in `from` is
// ErrInvalidTransition; an unknown id is ErrNotFound.
func (r *ApplyJournalRepository) Transition(ctx context.Context, id, from, to string, patch persistence.JournalPatch) error {
	now := sqliteTime(time.Now().UTC())
	set := []string{"state = ?", "updated_at = ?"}
	args := []any{to, now}
	if patch.GenerationAfter != "" {
		set = append(set, "generation_after = ?")
		args = append(args, patch.GenerationAfter)
	}
	if patch.Failure != "" {
		set = append(set, "failure = ?")
		args = append(args, patch.Failure)
	}
	if patch.Terminal {
		set = append(set, "terminal_at = ?")
		args = append(args, now)
	}
	args = append(args, id, from)
	res, err := r.db.ExecContext(ctx, `UPDATE config_apply_journal SET `+strings.Join(set, ", ")+
		` WHERE id = ? AND state = ?`, args...)
	if err != nil {
		return err
	}
	return r.casOutcome(ctx, res, id)
}

// casOutcome maps a zero-row compare-and-set to ErrNotFound (no such row) or
// ErrInvalidTransition (row exists in another state).
func (r *ApplyJournalRepository) casOutcome(ctx context.Context, res sql.Result, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	if _, gerr := r.Get(ctx, id); gerr != nil {
		return gerr
	}
	return persistence.ErrInvalidTransition
}

// AppendProgress appends path to the progress list (json_insert with the
// '$[#]' append path). ErrNotFound for an unknown id.
func (r *ApplyJournalRepository) AppendProgress(ctx context.Context, id, path string) error {
	res, err := r.db.ExecContext(ctx, `UPDATE config_apply_journal
		SET progress = json_insert(progress, '$[#]', ?), updated_at = ?
		WHERE id = ?`, path, sqliteTime(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

// MarkAppliedWithLedger commits VERIFYING → c.To on the journal and
// APPROVED → APPLIED on the proposal in one transaction (§3). Either
// refusal rolls back both writes.
func (r *ApplyJournalRepository) MarkAppliedWithLedger(ctx context.Context, c persistence.JournalLedgerCommit) error {
	if c.To != persistence.JournalStateApplied && c.To != persistence.JournalStatePendingRestart {
		return fmt.Errorf("config apply journal: %w: ledger commit must end in APPLIED or PENDING_RESTART, got %q",
			persistence.ErrInvalidTransition, c.To)
	}
	tx, ok, err := persistence.BeginTx(ctx, r.db, nil)
	if err != nil {
		return err
	}
	exec := r.db
	if ok {
		exec = tx
		defer func() { _ = tx.Rollback() }()
	}
	now := time.Now().UTC()
	res, err := exec.ExecContext(ctx, `UPDATE config_apply_journal
		SET state = ?, generation_after = COALESCE(?, generation_after), updated_at = ?, terminal_at = ?
		WHERE id = ? AND state = ?`,
		c.To, nullStr(c.GenerationAfter), sqliteTime(now), sqliteTime(now), c.JournalID, persistence.JournalStateVerifying)
	if err != nil {
		return err
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
		return persistence.ErrInvalidTransition
	}
	res, err = exec.ExecContext(ctx, `UPDATE control_plane_proposals
		SET status = ?, applied_by = ?, pre_apply_snapshot = ?, applied_at = ?
		WHERE id = ? AND status = 'APPROVED'`,
		persistence.ProposalStatusApplied, c.AppliedBy, c.Snapshot, sqliteTime(now), c.ProposalID)
	if err != nil {
		return err
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
		return persistence.ErrProposalNotApproved
	}
	if ok {
		return tx.Commit()
	}
	return nil
}

func scanApplyJournalRow(row *sql.Row) (*persistence.ConfigApplyJournal, error) {
	j, err := scanApplyJournal(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	return j, err
}

func scanApplyJournal(sc skillScanner) (*persistence.ConfigApplyJournal, error) {
	var (
		j          persistence.ConfigApplyJournal
		cols       persistence.ConfigApplyJournalJSON
		requestID  sql.NullString
		genBefore  sql.NullString
		genAfter   sql.NullString
		failure    sql.NullString
		createdAt  sqlTime
		updatedAt  sqlTime
		terminalAt sqlNullTime
	)
	if err := sc.Scan(
		&j.ID, &j.ProposalID, &requestID, &j.State, &j.OpDigest, &j.SchemaVersion,
		&j.DeploymentRoot, &cols.AffectedProjects, &j.WriterEpoch, &cols.Ops, &cols.PreImages, &cols.ReadSet, &cols.Progress,
		&genBefore, &genAfter, &failure, &createdAt, &updatedAt, &terminalAt,
	); err != nil {
		return nil, err
	}
	j.RequestID, j.GenerationBefore, j.GenerationAfter, j.Failure = requestID.String, genBefore.String, genAfter.String, failure.String
	j.CreatedAt, j.UpdatedAt = createdAt.Time, updatedAt.Time
	if terminalAt.Valid {
		t := terminalAt.Time
		j.TerminalAt = &t
	}
	if err := cols.Apply(&j); err != nil {
		return nil, err
	}
	return &j, nil
}
