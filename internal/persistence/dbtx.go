package persistence

import (
	"context"
	"database/sql"
)

// DBTX is the common interface satisfied by *sql.DB, *sql.Tx, and the
// metrics-wrapped DB types. Repositories depend on this interface so
// callers can transparently inject an instrumented or non-Postgres
// backend without changing query code.
type DBTX interface {
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
}

// Tx is a transaction handle: a DBTX plus commit/rollback. Both
// *sql.Tx and *TxWithMetrics satisfy it.
type Tx interface {
	DBTX
	Commit() error
	Rollback() error
}

// BeginTx starts a transaction on db when db is a real connection
// pool, returning ok=true with a Tx the caller must Commit/Rollback.
//
// It recognises *both* the raw *sql.DB pool (BeginTx → *sql.Tx) and
// the production *DBWithMetrics wrapper (BeginTx → *TxWithMetrics).
// Before this helper existed, repositories asserted db.(*sql.DB) or an
// interface returning *sql.Tx directly; neither matched the
// metrics-wrapped handle the daemon actually injects, so corpus
// rollback panicked and transactional inserts silently degraded to
// non-atomic statements (bug sweep 2026-06-04).
//
// When db is already a transaction handle (an outer caller owns the
// transaction), ok=false and tx is nil: the caller should run its
// statements directly on db.
func BeginTx(ctx context.Context, db DBTX, opts *sql.TxOptions) (tx Tx, ok bool, err error) {
	switch d := db.(type) {
	case *DBWithMetrics:
		t, e := d.BeginTx(ctx, opts)
		if e != nil {
			return nil, true, e
		}
		return t, true, nil
	case interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	}:
		t, e := d.BeginTx(ctx, opts)
		if e != nil {
			return nil, true, e
		}
		return t, true, nil
	default:
		// Already inside a transaction (e.g. *sql.Tx); the outer
		// caller owns commit/rollback.
		return nil, false, nil
	}
}
