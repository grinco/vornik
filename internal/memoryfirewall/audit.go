package memoryfirewall

// Batched audit writer. The recall hot path produces one
// EvaluationRow per chunk decision; this writer queues them in
// a buffered channel and flushes to memory_policy_evaluations
// at 100ms intervals (or when the buffer reaches 50 rows).
//
// Non-blocking: a full channel drops the oldest row and bumps
// a Prometheus counter. We never want the firewall to back-
// pressure the recall hot path.
//
// Failure mode: a DB write failure logs but doesn't retry
// in-place — the next batch picks up. The compliance trail
// has a documented (small) lossy ceiling under DB outage; the
// alternative is unbounded memory growth.

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// EvaluationRow is one audit record. Fields mirror the
// memory_policy_evaluations table 1:1.
type EvaluationRow struct {
	ID              string
	ProjectID       string
	TenantID        string
	ChunkID         string
	RequestRole     string
	RequestPurpose  string
	RequestOperator string
	TraceID         string
	Decision        EvaluationDecision
	PolicyDigest    string
	ReasonDetail    string
	EvaluatedAt     time.Time
}

// AuditSink is the narrow surface the writer needs from the
// persistence layer. Production: a *postgres.MemoryPolicyEvaluationRepository
// (lands in Phase B alongside this); tests supply a slice-
// recording stub.
type AuditSink interface {
	BatchInsert(ctx context.Context, rows []EvaluationRow) error
}

// AuditWriter buffers + batches evaluation rows. Constructed
// once at daemon boot; Start launches the flusher goroutine.
// Stop drains the buffer + waits for the in-flight flush.
type AuditWriter struct {
	sink       AuditSink
	logger     zerolog.Logger
	queue      chan EvaluationRow
	flushEvery time.Duration
	batchSize  int

	// Metrics counters surfaced via Prometheus. Set via the
	// SetMetrics setter; nil-safe so test wiring can skip.
	metrics *AuditMetrics

	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// AuditMetrics is the Prometheus surface for the writer. Held
// by reference so the container can wire it once + the writer
// reads it on the hot path.
type AuditMetrics struct {
	BufferDepth  func(int)
	WritesTotal  func(result string)
	WriteLatency func(d time.Duration)
	DroppedTotal func()
}

// NewAuditWriter constructs a writer with the documented
// defaults (100ms flush, 50-row batch, 500-row buffer). The
// sink and logger are required; metrics is optional.
func NewAuditWriter(sink AuditSink, logger zerolog.Logger) *AuditWriter {
	return &AuditWriter{
		sink:       sink,
		logger:     logger,
		queue:      make(chan EvaluationRow, 500),
		flushEvery: 100 * time.Millisecond,
		batchSize:  50,
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
}

// SetMetrics wires Prometheus counters. Nil-safe.
func (w *AuditWriter) SetMetrics(m *AuditMetrics) { w.metrics = m }

// Enqueue is the non-blocking submit path. A full queue drops
// the row + bumps the dropped counter; we never block the
// recall hot path. Idempotent on a nil receiver (tests that
// don't wire the writer get clean no-ops).
func (w *AuditWriter) Enqueue(row EvaluationRow) {
	if w == nil {
		return
	}
	select {
	case w.queue <- row:
		// queued
	default:
		// queue full — drop. Operators see the bump in
		// memory_firewall_audit_dropped_total; the alert
		// catches sustained backpressure.
		if w.metrics != nil && w.metrics.DroppedTotal != nil {
			w.metrics.DroppedTotal()
		}
		w.logger.Warn().Msg("memoryfirewall: audit queue full; dropping evaluation row")
	}
}

// Start launches the flusher goroutine. Called once at daemon
// boot; subsequent calls are no-ops via sync.Once.
func (w *AuditWriter) Start(ctx context.Context) {
	if w == nil {
		return
	}
	w.once.Do(func() {
		go w.run(ctx)
	})
}

// Stop signals shutdown + drains the in-flight buffer once.
// Safe to call multiple times.
func (w *AuditWriter) Stop() {
	if w == nil {
		return
	}
	select {
	case <-w.stop:
		// already stopping
	default:
		close(w.stop)
	}
	<-w.done
}

func (w *AuditWriter) run(ctx context.Context) {
	defer close(w.done)
	ticker := time.NewTicker(w.flushEvery)
	defer ticker.Stop()

	buf := make([]EvaluationRow, 0, w.batchSize)
	flush := func(reason string) {
		if len(buf) == 0 {
			return
		}
		start := time.Now()
		err := w.sink.BatchInsert(ctx, buf)
		dur := time.Since(start)
		if w.metrics != nil {
			if w.metrics.WriteLatency != nil {
				w.metrics.WriteLatency(dur)
			}
			if w.metrics.WritesTotal != nil {
				result := "ok"
				if err != nil {
					result = "error"
				}
				w.metrics.WritesTotal(result)
			}
		}
		if err != nil {
			w.logger.Warn().
				Err(err).
				Int("batch_size", len(buf)).
				Str("reason", reason).
				Msg("memoryfirewall: audit batch insert failed; rows dropped")
		}
		buf = buf[:0]
	}

	for {
		select {
		case <-ctx.Done():
			flush("ctx_done")
			return
		case <-w.stop:
			// Drain remaining queue entries before exit.
			for {
				select {
				case row := <-w.queue:
					buf = append(buf, row)
					if len(buf) >= w.batchSize {
						flush("drain_full")
					}
				default:
					flush("drain_exit")
					return
				}
			}
		case row := <-w.queue:
			buf = append(buf, row)
			if w.metrics != nil && w.metrics.BufferDepth != nil {
				w.metrics.BufferDepth(len(buf))
			}
			if len(buf) >= w.batchSize {
				flush("batch_full")
			}
		case <-ticker.C:
			flush("interval")
		}
	}
}
