package memory

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rs/zerolog"
)

func TestNew_NilDBError(t *testing.T) {
	if _, err := New(Config{}, nil, zerolog.Nop()); err == nil {
		t.Fatal("want err")
	}
}

func TestNew_AppliesDefaults(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	m, err := New(Config{}, db, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if m.Indexer == nil || m.Searcher == nil || m.Embedder == nil || m.worker == nil {
		t.Fatalf("manager not fully wired: %+v", m)
	}
	if m.Repository() == nil {
		t.Fatal("repo")
	}

	// Nil-safe Repository / Stats.
	var nilM *Manager
	if nilM.Repository() != nil {
		t.Fatal("nil repo")
	}
	if got, err := nilM.Stats(context.Background()); got != nil || err != nil {
		t.Fatal("nil stats")
	}
}

func TestSetters_NilSafe(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	m, _ := New(Config{}, db, zerolog.Nop())
	m.SetMetrics(freshMetrics())
	m.SetSecrets(nil, nil)
	m.SetTitler(nil)

	// Nil-safe even on bare Manager.
	bare := &Manager{}
	bare.SetSecrets(nil, nil)
	bare.SetTitler(nil)
}

func TestStartStop_LifecycleAndStats(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	m, _ := New(Config{WorkerConcurrency: 1}, db, zerolog.Nop())
	m.SetMetrics(freshMetrics())

	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery("FROM project_memory_chunks").
		WillReturnRows(sqlmock.NewRows([]string{"project_id", "chunks_total", "chunks_embedded", "queue_depth"}).
			AddRow("p", 1, 1, 0))

	got, err := m.Stats(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("stats: %v %v", got, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	cancel()
	m.Stop()
}

func TestCollectStateGauges_RespondsToCancel(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	m, _ := New(Config{}, db, zerolog.Nop())
	metrics := freshMetrics()
	m.SetMetrics(metrics)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.collectStateGauges(ctx, metrics)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("collectStateGauges did not return on cancel")
	}
}
