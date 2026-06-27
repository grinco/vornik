package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rs/zerolog"
)

func TestNewUtilityScorer_DefaultsAndBuilders(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	s := NewUtilityScorer(db, zerolog.Nop())
	if s.window != 30*24*time.Hour || s.interval != time.Hour || s.maxBoost != 1.0 {
		t.Fatalf("defaults: %+v", s)
	}
	s.WithWindow(time.Hour).WithInterval(time.Minute).WithMaxBoost(0.5)
	if s.window != time.Hour || s.interval != time.Minute || s.maxBoost != 0.5 {
		t.Fatalf("builders: %+v", s)
	}
	// Zero values ignored.
	s.WithWindow(0).WithInterval(-1).WithMaxBoost(0)
	if s.window != time.Hour || s.interval != time.Minute || s.maxBoost != 0.5 {
		t.Fatalf("zero builders should not override: %+v", s)
	}
}

func TestRecomputeAll_NilGuards(t *testing.T) {
	var nilS *UtilityScorer
	if _, err := nilS.RecomputeAll(context.Background()); err == nil {
		t.Fatal("nil receiver: want err")
	}
	s := &UtilityScorer{}
	if _, err := s.RecomputeAll(context.Background()); err == nil {
		t.Fatal("nil db: want err")
	}
}

func TestRecomputeAll_Happy(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	s := NewUtilityScorer(db, zerolog.Nop())
	mock.ExpectExec("UPDATE project_memory_chunks").
		WithArgs("2592000 seconds", 1.0).
		WillReturnResult(sqlmock.NewResult(0, 17))
	n, err := s.RecomputeAll(context.Background())
	if err != nil || n != 17 {
		t.Fatalf("got %d %v", n, err)
	}
}

func TestRecomputeAll_QueryError(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	s := NewUtilityScorer(db, zerolog.Nop())
	mock.ExpectExec("UPDATE project_memory_chunks").WillReturnError(errors.New("db down"))
	if _, err := s.RecomputeAll(context.Background()); err == nil {
		t.Fatal("want err")
	}
}

func TestRun_NilDBExitsImmediately(t *testing.T) {
	s := &UtilityScorer{done: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		s.Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not exit on nil db")
	}
}

func TestRun_FiresThenStops(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	// First tick on Run; further ticks tolerated if interval is short.
	mock.MatchExpectationsInOrder(false)
	mock.ExpectExec("UPDATE project_memory_chunks").
		WillReturnResult(sqlmock.NewResult(0, 0))

	s := NewUtilityScorer(db, zerolog.Nop()).WithInterval(50 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	time.Sleep(20 * time.Millisecond) // let the immediate tick run
	cancel()
	s.Stop()
}

func TestStop_NilSafe(t *testing.T) {
	var nilS *UtilityScorer
	nilS.Stop()           // must not panic
	s := &UtilityScorer{} // cancel nil
	s.Stop()
}

func TestTick_ErrorLogged(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	s := NewUtilityScorer(db, zerolog.Nop())
	mock.ExpectExec("UPDATE project_memory_chunks").WillReturnError(errors.New("boom"))
	// Direct tick call exercises the error log path.
	s.tick(context.Background())
}
