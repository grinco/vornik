package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// BudgetReservationRepository implements
// persistence.BudgetReservationRepository against SQLite (embedded / test
// deployments). SQLite has no advisory locks, but it serializes writers
// globally, so the sum-then-insert in Reserve runs inside one transaction
// and the write lock makes the insert atomic. A narrow read-race remains if
// two admissions SUM before either INSERTs — acceptable because SQLite
// deployments are single-process and low-concurrency; the Postgres backend
// (production) closes the race with a per-project advisory lock.
type BudgetReservationRepository struct {
	db *sql.DB
}

// NewBudgetReservationRepository constructs a repo over db.
func NewBudgetReservationRepository(db *sql.DB) *BudgetReservationRepository {
	return &BudgetReservationRepository{db: db}
}

const terminalTaskStatusesSQL = `'COMPLETED','FAILED','CANCELLED'`

func (r *BudgetReservationRepository) Reserve(ctx context.Context, req persistence.ReserveRequest) (persistence.ReserveResult, error) {
	var res persistence.ReserveResult
	if req.ProjectID == "" || req.TaskID == "" {
		return res, fmt.Errorf("budget reservation: project_id and task_id required")
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	var reserved float64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(estimated_usd), 0) FROM budget_reservations WHERE project_id = ? AND settled_at IS NULL`,
		req.ProjectID,
	).Scan(&reserved); err != nil {
		return res, err
	}
	res.ReservedUSD = reserved

	// Unsettled reserved total applies to both caps (safe-side over-count
	// across a day/month boundary). See the Postgres repo for the rationale.
	if req.DailyHardUSD > 0 && req.DailyCommittedUSD+reserved+req.EstimateUSD > req.DailyHardUSD {
		res.Blocked, res.Period = true, "daily"
		committed = true
		return res, tx.Commit()
	}
	if req.MonthlyHardUSD > 0 && req.MonthlyCommittedUSD+reserved+req.EstimateUSD > req.MonthlyHardUSD {
		res.Blocked, res.Period = true, "monthly"
		committed = true
		return res, tx.Commit()
	}

	now := req.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO budget_reservations (id, project_id, task_id, estimated_usd, reserved_at) VALUES (?, ?, ?, ?, ?)`,
		persistence.GenerateID("bres"), req.ProjectID, req.TaskID, req.EstimateUSD, sqliteTime(now),
	); err != nil {
		return res, err
	}
	if err := tx.Commit(); err != nil {
		return persistence.ReserveResult{}, err
	}
	committed = true
	res.Reserved = true
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
		`UPDATE budget_reservations SET settled_at = ? WHERE task_id = ? AND settled_at IS NULL`,
		sqliteTime(now), taskID,
	)
	if err != nil {
		return 0, err
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
		 SET settled_at = ?
		 WHERE settled_at IS NULL
		   AND (reserved_at < ?
		        OR task_id IN (SELECT id FROM tasks WHERE status IN (`+terminalTaskStatusesSQL+`)))`,
		sqliteTime(now), sqliteTime(staleCutoff),
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (r *BudgetReservationRepository) UnsettledSumByProject(ctx context.Context, projectID string) (float64, error) {
	var sum float64
	err := r.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(estimated_usd), 0) FROM budget_reservations WHERE project_id = ? AND settled_at IS NULL`,
		projectID,
	).Scan(&sum)
	if err != nil {
		return 0, err
	}
	return sum, nil
}
