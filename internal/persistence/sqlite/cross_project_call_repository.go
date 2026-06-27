package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// CrossProjectCallRepository is the SQLite stub for the inter-
// project orchestration ledger. v1 is Postgres-only — the
// migration uses JSONB + TIMESTAMPTZ + partial indexes that
// don't translate cleanly to the consolidated SQLite schema.
// This stub satisfies the interface so the storage abstraction
// can construct a complete *Repos struct on the SQLite branch
// (local dev / tests), but every method returns
// ErrSQLiteNotSupported so an operator who tries to use a
// `call_project` step on SQLite gets a clear error rather
// than silent breakage.
type CrossProjectCallRepository struct {
	// db is unused today but held so a future SQLite-native
	// impl can drop in without changing the constructor
	// signature. Marked _ to keep go vet quiet.
	_ *sql.DB
}

// NewCrossProjectCallRepository returns the stub. Same
// constructor shape as the postgres impl so the storage
// abstraction can switch backends transparently.
func NewCrossProjectCallRepository(db *sql.DB) *CrossProjectCallRepository {
	return &CrossProjectCallRepository{}
}

// ErrSQLiteNotSupported is returned by every method. Callers
// in the executor handler already nil-check the repo for the
// "disabled" path, but if they don't, this error surfaces a
// clear reason rather than a misleading SQL error.
var ErrSQLiteNotSupported = errors.New("cross_project_calls: inter-project orchestration is Postgres-only in v1 (SQLite migration deferred)")

func (r *CrossProjectCallRepository) Create(context.Context, *persistence.CrossProjectCall) error {
	return ErrSQLiteNotSupported
}

func (r *CrossProjectCallRepository) Get(context.Context, string) (*persistence.CrossProjectCall, error) {
	return nil, ErrSQLiteNotSupported
}

func (r *CrossProjectCallRepository) GetByCalleeTaskID(context.Context, string) (*persistence.CrossProjectCall, error) {
	return nil, ErrSQLiteNotSupported
}

func (r *CrossProjectCallRepository) SetCalleeTaskID(context.Context, string, string) error {
	return ErrSQLiteNotSupported
}

func (r *CrossProjectCallRepository) MarkRunning(context.Context, string) error {
	return ErrSQLiteNotSupported
}

func (r *CrossProjectCallRepository) MarkCompleted(context.Context, string, []byte) error {
	return ErrSQLiteNotSupported
}

func (r *CrossProjectCallRepository) MarkFailed(context.Context, string, string) error {
	return ErrSQLiteNotSupported
}

func (r *CrossProjectCallRepository) MarkRejected(context.Context, string, string) error {
	return ErrSQLiteNotSupported
}

func (r *CrossProjectCallRepository) ClaimTimedOut(context.Context, time.Time, int) ([]*persistence.CrossProjectCall, error) {
	return nil, ErrSQLiteNotSupported
}

func (r *CrossProjectCallRepository) List(context.Context, persistence.CPCListFilter) ([]*persistence.CrossProjectCall, error) {
	return nil, ErrSQLiteNotSupported
}

func (r *CrossProjectCallRepository) AdminCancel(context.Context, string, string) error {
	return ErrSQLiteNotSupported
}
