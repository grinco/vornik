package postgres

import (
	"context"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// BudgetReservationRepository implements
// persistence.BudgetReservationRepository against PostgreSQL. The
// admission path (Reserve) serializes per-project with a transaction-scoped
// advisory lock so two concurrent task admissions can't both read the same
// headroom and overshoot the hard cap — the read-then-spend TOCTOU the
// reservation ledger exists to close (trading-hardening §1).
type BudgetReservationRepository struct {
	db DBTX
}

// NewBudgetReservationRepository constructs a repo over db. Pass a *sql.DB
// (or the daemon's metrics-wrapped handle) so Reserve can begin its own
// transaction via persistence.BeginTx.
func NewBudgetReservationRepository(db DBTX) *BudgetReservationRepository {
	return &BudgetReservationRepository{db: db}
}

// terminalTaskStatuses is the set whose reservations the sweep settles.
const terminalTaskStatusesSQL = `'COMPLETED','FAILED','CANCELLED'`

func (r *BudgetReservationRepository) Reserve(ctx context.Context, req persistence.ReserveRequest) (persistence.ReserveResult, error) {
	var res persistence.ReserveResult
	if req.ProjectID == "" || req.TaskID == "" {
		return res, fmt.Errorf("budget reservation: project_id and task_id required")
	}

	tx, ok, err := persistence.BeginTx(ctx, r.db, nil)
	if err != nil {
		return res, mapDBError(err)
	}
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

	// Per-project lock: held until the transaction ends, so the
	// sum-then-insert below is atomic against concurrent admissions for the
	// SAME project. hashtext() maps the project key to the int the advisory
	// lock space wants; collisions across projects only cost a little
	// needless serialization, never correctness.
	if _, err := exec.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "budget_reservation:"+req.ProjectID); err != nil {
		return res, mapDBError(err)
	}

	var reserved float64
	if err := exec.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(estimated_usd), 0) FROM budget_reservations WHERE project_id = $1 AND settled_at IS NULL`,
		req.ProjectID,
	).Scan(&reserved); err != nil {
		return res, mapDBError(err)
	}
	res.ReservedUSD = reserved

	// The unsettled reserved total is applied to BOTH the daily and monthly
	// caps. A reservation that outlives a UTC midnight/month boundary is
	// thus counted in the current window too — an over-count that only ever
	// refuses sooner (safe-side), which v1 accepts for simplicity.
	if req.DailyHardUSD > 0 && req.DailyCommittedUSD+reserved+req.EstimateUSD > req.DailyHardUSD {
		res.Blocked, res.Period = true, "daily"
		committed = true // nothing written; allow the deferred rollback to no-op cleanly
		if ok {
			_ = tx.Commit()
		}
		return res, nil
	}
	if req.MonthlyHardUSD > 0 && req.MonthlyCommittedUSD+reserved+req.EstimateUSD > req.MonthlyHardUSD {
		res.Blocked, res.Period = true, "monthly"
		committed = true
		if ok {
			_ = tx.Commit()
		}
		return res, nil
	}

	now := req.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if _, err := exec.ExecContext(ctx,
		`INSERT INTO budget_reservations (id, project_id, task_id, estimated_usd, reserved_at) VALUES ($1, $2, $3, $4, $5)`,
		persistence.GenerateID("bres"), req.ProjectID, req.TaskID, req.EstimateUSD, now,
	); err != nil {
		return res, mapDBError(err)
	}
	res.Reserved = true

	if ok {
		if err := tx.Commit(); err != nil {
			return persistence.ReserveResult{}, mapDBError(err)
		}
	}
	committed = true
	return res, nil
}

func (r *BudgetReservationRepository) SettleByTask(ctx context.Context, taskID string, now time.Time) (int64, error) {
	if taskID == "" {
		return 0, fmt.Errorf("budget reservation: task_id required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE budget_reservations SET settled_at = $1 WHERE task_id = $2 AND settled_at IS NULL`,
		now, taskID,
	)
	if err != nil {
		return 0, mapDBError(err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (r *BudgetReservationRepository) SweepTerminalAndStale(ctx context.Context, staleCutoff, now time.Time) (int64, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE budget_reservations
		 SET settled_at = $1
		 WHERE settled_at IS NULL
		   AND (reserved_at < $2
		        OR task_id IN (SELECT id FROM tasks WHERE status IN (`+terminalTaskStatusesSQL+`)))`,
		now, staleCutoff,
	)
	if err != nil {
		return 0, mapDBError(err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (r *BudgetReservationRepository) UnsettledSumByProject(ctx context.Context, projectID string) (float64, error) {
	var sum float64
	err := r.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(estimated_usd), 0) FROM budget_reservations WHERE project_id = $1 AND settled_at IS NULL`,
		projectID,
	).Scan(&sum)
	if err != nil {
		return 0, mapDBError(err)
	}
	return sum, nil
}
