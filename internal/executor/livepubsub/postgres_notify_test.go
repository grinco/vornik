package livepubsub

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rs/zerolog"
)

// TestNewPostgresNotifier constructor returns a non-nil
// notifier when handed a non-nil *sql.DB. Most of the value
// is the compile-time check; this also pins the field is set.
func TestNewPostgresNotifier(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()
	n := NewPostgresNotifier(db)
	if n == nil {
		t.Errorf("NewPostgresNotifier(non-nil) returned nil")
	}
}

// TestPostgresNotifier_Notify_FiresPgNotify pins the SQL
// shape: `SELECT pg_notify($1, $2)` with the channel + payload
// as bind params. lib/pq's bare `NOTIFY <chan>, '<payload>'`
// form doesn't accept placeholders so the function-form is
// load-bearing.
func TestPostgresNotifier_Notify_FiresPgNotify(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_notify($1, $2)")).
		WithArgs("vornik_live", "exec-1|0|node-a").
		WillReturnResult(sqlmock.NewResult(0, 0))

	notifier := NewPostgresNotifier(db)
	if err := notifier.Notify(context.Background(), "vornik_live", "exec-1|0|node-a"); err != nil {
		t.Errorf("Notify: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestPostgresNotifier_Notify_NilDBErrors defends the
// constructor's nil-db guard. Without this, a misconfigured
// daemon would panic on the first NOTIFY attempt instead of
// surfacing a clean error.
func TestPostgresNotifier_Notify_NilDBErrors(t *testing.T) {
	var n *PostgresNotifier
	if err := n.Notify(context.Background(), "ch", "p"); err == nil {
		t.Errorf("nil notifier should error")
	}
	empty := &PostgresNotifier{}
	if err := empty.Notify(context.Background(), "ch", "p"); err == nil {
		t.Errorf("empty notifier should error")
	}
}

// TestPostgresNotifier_Notify_DriverErrorPropagates: a Postgres
// ExecContext failure (e.g. connection blip) MUST propagate so
// the dbBackedPublisher's Publish logs the failure instead of
// silently dropping the cross-replica fanout.
func TestPostgresNotifier_Notify_DriverErrorPropagates(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_notify")).
		WillReturnError(sql.ErrConnDone)

	notifier := NewPostgresNotifier(db)
	if err := notifier.Notify(context.Background(), "vornik_live", "x"); err == nil {
		t.Errorf("driver error should propagate")
	} else if !errors.Is(err, sql.ErrConnDone) {
		t.Errorf("wrong error type: %v", err)
	}
}

// TestNewPostgresListener constructor pins the basic shape.
// The Start path opens a real pq.Listener which can't be unit-
// tested without a live Postgres; the constructor + DSN field
// assignment is what we can pin here.
func TestNewPostgresListener(t *testing.T) {
	l := NewPostgresListener("host=localhost", zerolog.Nop())
	if l == nil {
		t.Errorf("NewPostgresListener returned nil")
	}
}

// TestNewPostgresListener_NilDSNStartErrors defends the empty-
// DSN branch of Start. Without this guard, lib/pq.Listener
// would panic on the first connect attempt.
func TestNewPostgresListener_NilDSNStartErrors(t *testing.T) {
	var l *PostgresListener
	if _, err := l.Start(context.Background(), "ch"); err == nil {
		t.Errorf("nil listener Start should error")
	}
	empty := &PostgresListener{}
	if _, err := empty.Start(context.Background(), "ch"); err == nil {
		t.Errorf("empty-DSN listener Start should error")
	}
}
