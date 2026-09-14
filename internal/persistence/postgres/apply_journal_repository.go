package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ApplyJournalRepository is the PostgreSQL persistence.ApplyJournalRepository
// — the durable multi-file config apply record (LLD 2026-09-13 config-apply-
// journal §2, migration 184). The typed JSON fields are encoded once by
// persistence.MarshalJournalColumns so the two stores cannot disagree about
// the document shape.
//
// Durability: synchronous_commit=on is the Postgres half of §8's contract
// (storage.ProbeDurability reads it back); Prepare's commit is on disk
// before the engine writes its first temp file.
type ApplyJournalRepository struct {
	db DBTX
}

// NewApplyJournalRepository constructs the repository over db.
func NewApplyJournalRepository(db DBTX) *ApplyJournalRepository {
	return &ApplyJournalRepository{db: db}
}

// Compile-time interface pin.
var _ persistence.ApplyJournalRepository = (*ApplyJournalRepository)(nil)

const pgApplyJournalColumns = `id, proposal_id, request_id, state, op_digest, schema_version,
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
		INSERT INTO config_apply_journal (`+pgApplyJournalColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9,$10::jsonb,$11::jsonb,$12::jsonb,$13::jsonb,
		        $14,$15,$16,$17,$18,NULL)`,
		row.ID, row.ProposalID, pgNullStr(row.RequestID), row.State, row.OpDigest, row.SchemaVersion,
		row.DeploymentRoot, cols.AffectedProjects, row.WriterEpoch, cols.Ops, cols.PreImages, cols.ReadSet, cols.Progress,
		pgNullStr(row.GenerationBefore), pgNullStr(row.GenerationAfter), pgNullStr(row.Failure),
		row.CreatedAt, row.UpdatedAt,
	)
	return mapDBError(err)
}

// Get fetches a row by id; ErrNotFound when absent.
func (r *ApplyJournalRepository) Get(ctx context.Context, id string) (*persistence.ConfigApplyJournal, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+pgApplyJournalColumns+` FROM config_apply_journal WHERE id = $1`, id)
	return scanPGApplyJournalRow(row)
}

// OpenForProposal returns the open row for proposalID; ErrNotFound when none.
func (r *ApplyJournalRepository) OpenForProposal(ctx context.Context, proposalID string) (*persistence.ConfigApplyJournal, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+pgApplyJournalColumns+` FROM config_apply_journal
		WHERE proposal_id = $1 AND terminal_at IS NULL`, proposalID)
	return scanPGApplyJournalRow(row)
}

// ListOpen returns every open row, oldest first.
func (r *ApplyJournalRepository) ListOpen(ctx context.Context) ([]*persistence.ConfigApplyJournal, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+pgApplyJournalColumns+` FROM config_apply_journal
		WHERE terminal_at IS NULL ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.ConfigApplyJournal
	for rows.Next() {
		j, serr := scanPGApplyJournal(rows)
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
	now := time.Now().UTC()
	set := []string{"state = $1", "updated_at = $2"}
	args := []any{to, now}
	next := func(clause string, v any) {
		args = append(args, v)
		set = append(set, fmt.Sprintf(clause, len(args)))
	}
	if patch.GenerationAfter != "" {
		next("generation_after = $%d", patch.GenerationAfter)
	}
	if patch.Failure != "" {
		next("failure = $%d", patch.Failure)
	}
	if patch.Terminal {
		next("terminal_at = $%d", now)
	}
	args = append(args, id, from)
	res, err := r.db.ExecContext(ctx, fmt.Sprintf(`UPDATE config_apply_journal SET %s WHERE id = $%d AND state = $%d`,
		strings.Join(set, ", "), len(args)-1, len(args)), args...)
	if err != nil {
		return mapDBError(err)
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

// AppendProgress appends path to the progress array. ErrNotFound for an
// unknown id.
func (r *ApplyJournalRepository) AppendProgress(ctx context.Context, id, path string) error {
	res, err := r.db.ExecContext(ctx, `UPDATE config_apply_journal
		SET progress = progress || to_jsonb($1::text), updated_at = $2
		WHERE id = $3`, path, time.Now().UTC(), id)
	if err != nil {
		return mapDBError(err)
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
		SET state = $1, generation_after = COALESCE($2, generation_after), updated_at = $3, terminal_at = $3
		WHERE id = $4 AND state = $5`,
		c.To, pgNullStr(c.GenerationAfter), now, c.JournalID, persistence.JournalStateVerifying)
	if err != nil {
		return mapDBError(err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
		return persistence.ErrInvalidTransition
	}
	res, err = exec.ExecContext(ctx, `UPDATE control_plane_proposals
		SET status = $1, applied_by = $2, pre_apply_snapshot = $3, applied_at = $4
		WHERE id = $5 AND status = 'APPROVED'`,
		persistence.ProposalStatusApplied, c.AppliedBy, c.Snapshot, now, c.ProposalID)
	if err != nil {
		return mapDBError(err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
		return persistence.ErrProposalNotApproved
	}
	if ok {
		return tx.Commit()
	}
	return nil
}

func scanPGApplyJournalRow(row *sql.Row) (*persistence.ConfigApplyJournal, error) {
	j, err := scanPGApplyJournal(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	return j, err
}

func scanPGApplyJournal(sc pgSkillScanner) (*persistence.ConfigApplyJournal, error) {
	var (
		j          persistence.ConfigApplyJournal
		cols       persistence.ConfigApplyJournalJSON
		requestID  sql.NullString
		genBefore  sql.NullString
		genAfter   sql.NullString
		failure    sql.NullString
		terminalAt sql.NullTime
	)
	if err := sc.Scan(
		&j.ID, &j.ProposalID, &requestID, &j.State, &j.OpDigest, &j.SchemaVersion,
		&j.DeploymentRoot, &cols.AffectedProjects, &j.WriterEpoch, &cols.Ops, &cols.PreImages, &cols.ReadSet, &cols.Progress,
		&genBefore, &genAfter, &failure, &j.CreatedAt, &j.UpdatedAt, &terminalAt,
	); err != nil {
		return nil, err
	}
	j.RequestID, j.GenerationBefore, j.GenerationAfter, j.Failure = requestID.String, genBefore.String, genAfter.String, failure.String
	if terminalAt.Valid {
		t := terminalAt.Time
		j.TerminalAt = &t
	}
	if err := cols.Apply(&j); err != nil {
		return nil, err
	}
	return &j, nil
}
