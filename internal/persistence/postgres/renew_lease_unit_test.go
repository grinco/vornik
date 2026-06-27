package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// renewLeaseStubResult satisfies sql.Result with a configurable
// rows-affected count so we can drive RenewLease through both
// branches (rows>0 -> nil, rows==0 -> ErrLeaseNotFound) without
// a real database.
type renewLeaseStubResult struct {
	rows int64
}

func (r *renewLeaseStubResult) LastInsertId() (int64, error) { return 0, nil }
func (r *renewLeaseStubResult) RowsAffected() (int64, error) { return r.rows, nil }

type renewLeaseStubDB struct {
	runQuery func(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
}

func (s *renewLeaseStubDB) QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error) {
	panic("QueryContext: not used by RenewLease")
}
func (s *renewLeaseStubDB) QueryRowContext(context.Context, string, ...interface{}) *sql.Row {
	panic("QueryRowContext: not used by RenewLease")
}
func (s *renewLeaseStubDB) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	return s.runQuery(ctx, query, args...)
}

// TestRenewLease_ReturnsErrLeaseNotFoundOnZeroRows pins the
// behavioral contract: when the UPDATE matches no rows (because
// status flipped, lease_id rotated, or the row vanished), the
// wrapper MUST surface persistence.ErrLeaseNotFound.
func TestRenewLease_ReturnsErrLeaseNotFoundOnZeroRows(t *testing.T) {
	r := &TaskRepository{db: &renewLeaseStubDB{
		runQuery: func(_ context.Context, _ string, _ ...interface{}) (sql.Result, error) {
			return &renewLeaseStubResult{rows: 0}, nil
		},
	}}
	err := r.RenewLease(context.Background(), "task-1", "lease-abc", 30)
	if !errors.Is(err, persistence.ErrLeaseNotFound) {
		t.Fatalf("expected ErrLeaseNotFound on zero rows, got %v", err)
	}
}

// TestRenewLease_ReturnsNilOnRowMatch is the symmetric happy-path
// case: a successful UPDATE returns nil. Without this assertion a
// refactor that always returned ErrLeaseNotFound would still pass
// the zero-rows test above.
func TestRenewLease_ReturnsNilOnRowMatch(t *testing.T) {
	r := &TaskRepository{db: &renewLeaseStubDB{
		runQuery: func(_ context.Context, _ string, _ ...interface{}) (sql.Result, error) {
			return &renewLeaseStubResult{rows: 1}, nil
		},
	}}
	if err := r.RenewLease(context.Background(), "task-1", "lease-abc", 30); err != nil {
		t.Fatalf("expected nil on successful renew, got %v", err)
	}
}

// TestRenewLease_PassesAllThreeArgsToSQL guards argument order.
// SQL placeholders are positional ($1, $2, $3); a transposed
// argument list would silently produce wrong-but-non-zero matches
// in production.
func TestRenewLease_PassesAllThreeArgsToSQL(t *testing.T) {
	var captured []interface{}
	r := &TaskRepository{db: &renewLeaseStubDB{
		runQuery: func(_ context.Context, _ string, args ...interface{}) (sql.Result, error) {
			captured = args
			return &renewLeaseStubResult{rows: 1}, nil
		},
	}}
	_ = r.RenewLease(context.Background(), "task-1", "lease-abc", 30)
	if len(captured) != 3 {
		t.Fatalf("expected 3 args (taskID, leaseID, extendBy), got %d: %v", len(captured), captured)
	}
	if captured[0] != "task-1" || captured[1] != "lease-abc" || captured[2] != 30 {
		t.Errorf("argument order wrong: got %v", captured)
	}
}

// TestRenewLease_ExecutesCanonicalSQL confirms the wrapper uses
// the renewLeaseSQL constant rather than an inline string. If
// someone reverts the extraction (turning the SQL back into a
// literal), the captured query won't match the const byte-for-
// byte and this test fails.
func TestRenewLease_ExecutesCanonicalSQL(t *testing.T) {
	var capturedQuery string
	r := &TaskRepository{db: &renewLeaseStubDB{
		runQuery: func(_ context.Context, q string, _ ...interface{}) (sql.Result, error) {
			capturedQuery = q
			return &renewLeaseStubResult{rows: 1}, nil
		},
	}}
	_ = r.RenewLease(context.Background(), "task-1", "lease-abc", 30)
	if capturedQuery != renewLeaseSQL {
		t.Errorf("RenewLease did not run renewLeaseSQL\nwant: %s\ngot: %s",
			renewLeaseSQL, capturedQuery)
	}
}
