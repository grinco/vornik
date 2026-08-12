package memory

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rs/zerolog"
)

func TestNewWorker_AndSetters(t *testing.T) {
	r, _, cleanup := newRepo(t)
	defer cleanup()
	w := NewWorker(Config{}, r, NewEmbedder(Config{}), zerolog.Nop())
	if w.repo != r {
		t.Fatal("wiring")
	}
	w.setMetrics(freshMetrics())
	if w.metrics == nil {
		t.Fatal("metrics")
	}
	tt := &Titler{}
	w.SetTitler(tt)
	if w.titler != tt {
		t.Fatal("titler")
	}
}

func TestDLQBackoff(t *testing.T) {
	w := &Worker{}
	if got := w.dlqBackoff(-1); got != 24*time.Hour {
		t.Fatalf("neg: %v", got)
	}
	if got := w.dlqBackoff(0); got != 10*time.Minute {
		t.Fatalf("0: %v", got)
	}
	if got := w.dlqBackoff(1); got != 20*time.Minute {
		t.Fatalf("1: %v", got)
	}
	if got := w.dlqBackoff(100); got != 24*time.Hour {
		t.Fatalf("overflow: %v", got)
	}
}

// helper: build an embedder pointed at an httptest server that returns
// vectors of given dimension.
func newFakeEmbedder(t *testing.T, dim int) (*Embedder, func()) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embeddingRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		resp := embeddingResponse{}
		for i := range req.Input {
			vec := make([]float32, dim)
			for j := range vec {
				vec[j] = float32(j)
			}
			resp.Data = append(resp.Data, struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			}{Index: i, Embedding: vec})
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	cfg := Config{EmbeddingEndpoint: srv.URL, EmbeddingModel: "m", EmbeddingDimension: dim}
	return NewEmbedder(cfg), srv.Close
}

func TestProcessBatch_EmptyQueueNoop(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	w := NewWorker(Config{EmbeddingDimension: 3}, r, NewEmbedder(Config{}), zerolog.Nop())
	w.setMetrics(freshMetrics())

	mock.ExpectQuery("FROM memory_embed_dlq").
		WithArgs(workerBatchSize).
		WillReturnRows(sqlmock.NewRows([]string{
			"chunk_id", "project_id", "reason", "last_error",
			"retry_count", "retry_after", "first_failed_at", "last_failed_at",
		}))
	// Migration 158: the batch is CLAIMED, not deleted, so a crash before the
	// embedding is stored leaves it re-claimable. An empty claim acks nothing.
	mock.ExpectBegin()
	mock.ExpectQuery("UPDATE memory_embed_queue SET claimed_at").
		WithArgs(workerBatchSize, EmbedClaimReclaimAfter.String()).
		WillReturnRows(sqlmock.NewRows([]string{"chunk_id"}))
	mock.ExpectCommit()
	w.processBatch(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessBatch_EmbeddingFailureMovesToDLQ(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	emb, closeFn := newFakeEmbedder(t, 3)
	closeFn() // closed → network error → Embed returns nil
	w := NewWorker(Config{EmbeddingDimension: 3}, r, emb, zerolog.Nop())
	w.setMetrics(freshMetrics())

	mock.ExpectQuery("FROM memory_embed_dlq").
		WillReturnRows(sqlmock.NewRows([]string{
			"chunk_id", "project_id", "reason", "last_error",
			"retry_count", "retry_after", "first_failed_at", "last_failed_at",
		}))
	mock.ExpectQuery("UPDATE memory_embed_queue SET claimed_at").
		WillReturnRows(sqlmock.NewRows([]string{"chunk_id"}).AddRow("c1"))
	mock.ExpectQuery("SELECT id, project_id").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "task_id", "artifact_id", "source_name",
			"chunk_index", "content", "content_hash", "created_at",
		}).AddRow("c1", "p", "", "", "s", 0, "text", "h", time.Now()))

	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM memory_embed_queue").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO memory_embed_dlq").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	w.processBatch(context.Background())
}

func TestProcessBatch_EmptyEmbeddingParksInDLQ(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	// Server returns no Data → vecs slice has nil entries.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(embeddingResponse{})
	}))
	defer srv.Close()
	emb := NewEmbedder(Config{EmbeddingEndpoint: srv.URL, EmbeddingModel: "m"})

	w := NewWorker(Config{EmbeddingDimension: 3}, r, emb, zerolog.Nop())
	w.setMetrics(freshMetrics())

	mock.ExpectQuery("FROM memory_embed_dlq").
		WillReturnRows(sqlmock.NewRows([]string{
			"chunk_id", "project_id", "reason", "last_error",
			"retry_count", "retry_after", "first_failed_at", "last_failed_at",
		}))
	mock.ExpectQuery("UPDATE memory_embed_queue SET claimed_at").
		WillReturnRows(sqlmock.NewRows([]string{"chunk_id"}).AddRow("c1"))
	mock.ExpectQuery("SELECT id, project_id").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "task_id", "artifact_id", "source_name",
			"chunk_index", "content", "content_hash", "created_at",
		}).AddRow("c1", "p", "", "", "s", 0, "text", "h", time.Now()))

	// DLQ move + park.
	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM memory_embed_queue").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO memory_embed_dlq").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec("UPDATE memory_embed_dlq").WillReturnResult(sqlmock.NewResult(0, 1))
	w.processBatch(context.Background())
}

func TestProcessBatch_DimensionMismatchParks(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	emb, closeFn := newFakeEmbedder(t, 5) // returns dim=5
	defer closeFn()

	w := NewWorker(Config{EmbeddingDimension: 3}, r, emb, zerolog.Nop())
	w.setMetrics(freshMetrics())

	mock.ExpectQuery("FROM memory_embed_dlq").
		WillReturnRows(sqlmock.NewRows([]string{
			"chunk_id", "project_id", "reason", "last_error",
			"retry_count", "retry_after", "first_failed_at", "last_failed_at",
		}))
	mock.ExpectQuery("UPDATE memory_embed_queue SET claimed_at").
		WillReturnRows(sqlmock.NewRows([]string{"chunk_id"}).AddRow("c1"))
	mock.ExpectQuery("SELECT id, project_id").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "task_id", "artifact_id", "source_name",
			"chunk_index", "content", "content_hash", "created_at",
		}).AddRow("c1", "p", "", "", "s", 0, "text", "h", time.Now()))

	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM memory_embed_queue").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO memory_embed_dlq").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec("UPDATE memory_embed_dlq").WillReturnResult(sqlmock.NewResult(0, 1))
	w.processBatch(context.Background())
}

func TestProcessBatch_HappyPathStoresAndTitles(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	emb, closeFn := newFakeEmbedder(t, 3)
	defer closeFn()

	w := NewWorker(Config{EmbeddingDimension: 3}, r, emb, zerolog.Nop())
	w.setMetrics(freshMetrics())

	mock.ExpectQuery("FROM memory_embed_dlq").
		WillReturnRows(sqlmock.NewRows([]string{
			"chunk_id", "project_id", "reason", "last_error",
			"retry_count", "retry_after", "first_failed_at", "last_failed_at",
		}))
	mock.ExpectQuery("UPDATE memory_embed_queue SET claimed_at").
		WillReturnRows(sqlmock.NewRows([]string{"chunk_id"}).AddRow("c1"))
	mock.ExpectQuery("SELECT id, project_id").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "task_id", "artifact_id", "source_name",
			"chunk_index", "content", "content_hash", "created_at",
		}).AddRow("c1", "p", "", "", "s", 0, "text content", "h", time.Now()))

	mock.ExpectExec("UPDATE project_memory_chunks SET embedding").
		WillReturnResult(sqlmock.NewResult(0, 1))
	w.processBatch(context.Background())
}

func TestProcessBatch_StoreFailureMovesToDLQ(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	emb, closeFn := newFakeEmbedder(t, 3)
	defer closeFn()

	w := NewWorker(Config{EmbeddingDimension: 3}, r, emb, zerolog.Nop())

	mock.ExpectQuery("FROM memory_embed_dlq").
		WillReturnRows(sqlmock.NewRows([]string{
			"chunk_id", "project_id", "reason", "last_error",
			"retry_count", "retry_after", "first_failed_at", "last_failed_at",
		}))
	mock.ExpectQuery("UPDATE memory_embed_queue SET claimed_at").
		WillReturnRows(sqlmock.NewRows([]string{"chunk_id"}).AddRow("c1"))
	mock.ExpectQuery("SELECT id, project_id").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "task_id", "artifact_id", "source_name",
			"chunk_index", "content", "content_hash", "created_at",
		}).AddRow("c1", "p", "", "", "s", 0, "text", "h", time.Now()))

	mock.ExpectExec("UPDATE project_memory_chunks SET embedding").WillReturnError(errors.New("disk full"))
	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM memory_embed_queue").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO memory_embed_dlq").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	w.processBatch(context.Background())
}

func TestProcessBatch_BatchFailureFallsBackToPerChunk(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embeddingRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Input) > 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if len(req.Input) == 1 && req.Input[0] == "bad" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		resp := embeddingResponse{Data: []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}{{Index: 0, Embedding: []float32{1, 2, 3}}}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	w := NewWorker(Config{EmbeddingEndpoint: srv.URL, EmbeddingModel: "m", EmbeddingDimension: 3}, r, NewEmbedder(Config{EmbeddingEndpoint: srv.URL, EmbeddingModel: "m", EmbeddingDimension: 3}), zerolog.Nop())

	mock.ExpectQuery("FROM memory_embed_dlq").
		WillReturnRows(sqlmock.NewRows([]string{
			"chunk_id", "project_id", "reason", "last_error",
			"retry_count", "retry_after", "first_failed_at", "last_failed_at",
		}))
	mock.ExpectBegin()
	mock.ExpectQuery("UPDATE memory_embed_queue SET claimed_at").
		WithArgs(workerBatchSize, EmbedClaimReclaimAfter.String()).
		WillReturnRows(sqlmock.NewRows([]string{"chunk_id"}).AddRow("c1").AddRow("c2"))
	mock.ExpectQuery("SELECT id, project_id").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "task_id", "artifact_id", "source_name",
			"chunk_index", "content", "content_hash", "created_at",
		}).
			AddRow("c1", "p", "", "", "s", 0, "ok", "h1", time.Now()).
			AddRow("c2", "p", "", "", "s", 1, "bad", "h2", time.Now()))
	mock.ExpectCommit()
	mock.ExpectExec("UPDATE project_memory_chunks SET embedding").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM memory_embed_queue").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO memory_embed_dlq").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	// Both chunks are now terminal — one stored, one parked in the DLQ — so the
	// worker releases the leases. Without this ack the claims would age out and the
	// chunks would be embedded again on a later tick.
	mock.ExpectExec("DELETE FROM memory_embed_queue").WillReturnResult(sqlmock.NewResult(0, 2))

	w.processBatch(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessBatch_DequeueErrorReturns(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	w := NewWorker(Config{EmbeddingDimension: 3}, r, NewEmbedder(Config{}), zerolog.Nop())

	mock.ExpectQuery("FROM memory_embed_dlq").
		WillReturnRows(sqlmock.NewRows([]string{
			"chunk_id", "project_id", "reason", "last_error",
			"retry_count", "retry_after", "first_failed_at", "last_failed_at",
		}))
	mock.ExpectQuery("UPDATE memory_embed_queue SET claimed_at").WillReturnError(errors.New("boom"))
	w.processBatch(context.Background())
}

func TestReplayDueDLQ_EmptyAndError(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	w := NewWorker(Config{}, r, NewEmbedder(Config{}), zerolog.Nop())

	mock.ExpectQuery("FROM memory_embed_dlq").
		WillReturnRows(sqlmock.NewRows([]string{
			"chunk_id", "project_id", "reason", "last_error",
			"retry_count", "retry_after", "first_failed_at", "last_failed_at",
		}))
	if n, err := w.replayDueDLQ(context.Background()); n != 0 || err != nil {
		t.Fatalf("empty: %d %v", n, err)
	}

	mock.ExpectQuery("FROM memory_embed_dlq").WillReturnError(errors.New("boom"))
	if _, err := w.replayDueDLQ(context.Background()); err == nil {
		t.Fatal("want err")
	}
}

func TestReplayDueDLQ_MovesRows(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	w := NewWorker(Config{}, r, NewEmbedder(Config{}), zerolog.Nop())

	mock.ExpectQuery("FROM memory_embed_dlq").
		WillReturnRows(sqlmock.NewRows([]string{
			"chunk_id", "project_id", "reason", "last_error",
			"retry_count", "retry_after", "first_failed_at", "last_failed_at",
		}).AddRow("c1", "p", "r", "e", 1, time.Now(), time.Now(), time.Now()))
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT chunk_id, project_id FROM memory_embed_dlq").
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"chunk_id", "project_id"}).AddRow("c1", "p"))
	mock.ExpectExec("INSERT INTO memory_embed_queue").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM memory_embed_dlq").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if n, err := w.replayDueDLQ(context.Background()); err != nil || n != 1 {
		t.Fatalf("got %d %v", n, err)
	}
}

func TestStartStop_ContextCancel(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	w := NewWorker(Config{WorkerConcurrency: 1}, r, NewEmbedder(Config{}), zerolog.Nop())
	w.setMetrics(freshMetrics())

	// processBatch may or may not fire depending on timing; tolerate either by
	// matching whatever it ran. Keep poll-interval-related expectations loose:
	// we just want to verify Start/Stop lifecycle without a flaky race.
	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery("FROM memory_embed_dlq").
		WillReturnRows(sqlmock.NewRows([]string{
			"chunk_id", "project_id", "reason", "last_error",
			"retry_count", "retry_after", "first_failed_at", "last_failed_at",
		}))
	mock.ExpectQuery("UPDATE memory_embed_queue SET claimed_at").
		WillReturnRows(sqlmock.NewRows([]string{"chunk_id"}))

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	// Immediately stop — workerPollInterval=5s, no batch should run.
	cancel()
	w.Stop()
}

func TestStart_DefaultConcurrency(t *testing.T) {
	r, _, cleanup := newRepo(t)
	defer cleanup()
	w := NewWorker(Config{}, r, NewEmbedder(Config{}), zerolog.Nop())
	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	cancel()
	w.Stop()
}
