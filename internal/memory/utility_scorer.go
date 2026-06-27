package memory

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/rs/zerolog"
)

// UtilityScorer recomputes per-chunk utility scores from
// memory_retrieval_audit. The score lifts chunks repeatedly cited in
// retrievals (signal that the corpus actively uses them) and decays
// chunks that haven't been retrieved at all (candidates for prune).
//
// Score formula (per-project normalisation):
//
//	utility = ln(1 + hits) / ln(1 + max_hits_in_project)
//
// Normalised to [0, 1] per project so the search-side multiplier is
// bounded and comparable across projects of different sizes.
//
// Runs on a slow cadence (default hourly) — utility doesn't move
// fast and the COUNT() aggregation is expensive on large corpora. The
// 30-day window keeps the score responsive while smoothing over
// per-day noise.
type UtilityScorer struct {
	db       *sql.DB
	window   time.Duration
	maxBoost float64
	interval time.Duration
	logger   zerolog.Logger

	cancel context.CancelFunc
	done   chan struct{}
}

// NewUtilityScorer builds the scorer with safe defaults: 30-day
// retrieval window, hourly recompute cadence, 1.0 amplification cap.
// Zero/negative values fall back to defaults so config-omitted fields
// don't accidentally disable the loop.
func NewUtilityScorer(db *sql.DB, logger zerolog.Logger) *UtilityScorer {
	return &UtilityScorer{
		db:       db,
		window:   30 * 24 * time.Hour,
		maxBoost: 1.0,
		interval: time.Hour,
		logger:   logger,
		done:     make(chan struct{}),
	}
}

// WithWindow overrides the retrieval-audit lookback window.
func (s *UtilityScorer) WithWindow(d time.Duration) *UtilityScorer {
	if d > 0 {
		s.window = d
	}
	return s
}

// WithInterval overrides the recompute cadence.
func (s *UtilityScorer) WithInterval(d time.Duration) *UtilityScorer {
	if d > 0 {
		s.interval = d
	}
	return s
}

// WithMaxBoost overrides the per-row utility cap.
func (s *UtilityScorer) WithMaxBoost(b float64) *UtilityScorer {
	if b > 0 {
		s.maxBoost = b
	}
	return s
}

// RecomputeAll re-derives utility_score for every chunk in every
// project. Single round-trip — Postgres does the per-project
// normalisation in a CTE. Returns the number of rows updated.
//
// Skip-safe: when memory_retrieval_audit is empty (Phase 2 deployment
// without retrieval traffic yet) the UPDATE simply zeros everything,
// which is the right neutral state.
func (s *UtilityScorer) RecomputeAll(ctx context.Context) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("UtilityScorer: not configured")
	}
	// Outcome-aware feedback loop: retrievals that fed *successful*
	// steps contribute full positive weight; retrievals that fed
	// failed steps contribute negative weight; retrievals attached
	// to steps without a finalised outcome get a mild positive (they
	// were used, we just don't know whether they helped). Aggregated
	// per chunk, clamped to ≥0 before per-project normalisation so a
	// chunk that's purely retrieve-then-rejected doesn't sink below
	// 0 — utility_score is multiplicative in the search SQL; negative
	// values would invert relevance.
	//
	// Bug fix: column is `retrieved_at` per the 001_initial.sql
	// schema. The pre-2026.5.5 SQL referenced `recorded_at`, which
	// doesn't exist on this table — RecomputeAll errored on first
	// run against a real Postgres.
	const q = `
WITH retrievals AS (
    SELECT cid AS chunk_id, a.execution_id, a.step_id
    FROM memory_retrieval_audit a,
         LATERAL unnest(a.chunk_ids) AS cid
    WHERE a.retrieved_at > NOW() - $1::interval
),
scored_hits AS (
    SELECT r.chunk_id,
           SUM(CASE
               WHEN o.outcome = 'ok' THEN 1.0
               WHEN o.outcome IN ('parse_error','schema_violation','refused','downstream_rejected','failed','iteration_exhausted','degenerate_loop','timeout') THEN -0.5
               ELSE 0.3
           END) AS weighted_hits
    FROM retrievals r
    LEFT JOIN execution_step_outcomes o
        ON o.execution_id = r.execution_id
       AND o.step_id      = r.step_id
       AND o.outcome      NOT IN ('pending_validation','superseded','verifier_warn')
    GROUP BY r.chunk_id
),
joined AS (
    SELECT c.id AS chunk_id, c.project_id,
           GREATEST(COALESCE(s.weighted_hits, 0), 0) AS hits
    FROM project_memory_chunks c
    LEFT JOIN scored_hits s ON s.chunk_id = c.id
),
scored AS (
    SELECT
        chunk_id,
        CASE
            WHEN MAX(hits) OVER (PARTITION BY project_id) = 0 THEN 0
            ELSE LEAST($2::double precision,
                       LN(1 + hits) / NULLIF(LN(1 + MAX(hits) OVER (PARTITION BY project_id)), 0))
        END AS score
    FROM joined
)
UPDATE project_memory_chunks c
SET utility_score = COALESCE(s.score, 0)
FROM scored s
WHERE c.id = s.chunk_id
  AND c.utility_score IS DISTINCT FROM COALESCE(s.score, 0)`

	intervalLit := fmt.Sprintf("%d seconds", int64(s.window.Seconds()))
	res, err := s.db.ExecContext(ctx, q, intervalLit, s.maxBoost)
	if err != nil {
		return 0, fmt.Errorf("RecomputeAll: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// Run drives the periodic recompute loop. Blocks until ctx is cancelled
// or Stop is called. Fires immediately at start so a daemon restart
// picks up audit changes without waiting a full interval. Failures are
// logged at Warn and never abort the loop — utility lag is a quality
// regression, not a correctness one.
func (s *UtilityScorer) Run(ctx context.Context) {
	if s == nil || s.db == nil {
		close(s.done)
		return
	}
	loopCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	defer close(s.done)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	s.tick(loopCtx)
	for {
		select {
		case <-loopCtx.Done():
			return
		case <-ticker.C:
			s.tick(loopCtx)
		}
	}
}

// Stop signals Run to exit and waits for the loop to drain. Idempotent.
func (s *UtilityScorer) Stop() {
	if s == nil || s.cancel == nil {
		return
	}
	s.cancel()
	<-s.done
}

func (s *UtilityScorer) tick(ctx context.Context) {
	n, err := s.RecomputeAll(ctx)
	if err != nil {
		s.logger.Warn().Err(err).Msg("memory utility scorer: recompute failed")
		return
	}
	s.logger.Debug().Int("updated", n).Msg("memory utility scorer: recompute complete")
}
