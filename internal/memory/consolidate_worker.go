package memory

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/leaderelection"
)

// ProjectLister is the narrow surface the worker needs from the
// project registry. Kept as a single-method interface so tests
// can supply a slice without pulling registry.Registry into the
// memory package (would create a cycle).
type ProjectLister interface {
	ListProjectIDs() []string
}

// ConsolidateWorker drives the periodic LLM-free per-project gist
// pass. Mirrors the title/classify-backfill worker shape so the
// existing dashboards extend trivially:
//
//   - One-goroutine timer loop
//   - Immediate first tick at start (so a daemon restart picks up
//     state without waiting a full interval)
//   - Per-tick outcome label: idle / progressed / errored
//   - Per-project outcome label: ok / errored
//
// The library is sub-ms per kilobyte, so the loop is cheap to
// leave on; default cadence is 10 minutes (configurable).
type ConsolidateWorker struct {
	Consolid *Consolidator
	Repo     *Repository
	Projects ProjectLister
	Interval time.Duration
	// ScanLimit caps the chunks per project the consolidator
	// inspects each tick. 0 falls back to the library default.
	ScanLimit int
	Logger    zerolog.Logger
	Metrics   *Metrics
	// LeaderGate gates the tick on the elected leader so two
	// daemons don't both upsert project_gists rows. Nil-safe.
	LeaderGate LeaderGate
	// Hygiene is Consumer C of the instinct layer: it supplies
	// retrieval-domain boost/prune candidate HINTS per project. Nil or
	// disabled = no hints fetched and the worker behaves exactly as
	// today. The hints are ADVISORY — surfaced (logged + counted) for
	// the operator / retention sweeper; this worker NEVER deletes a
	// chunk on their basis. See instinct_hygiene.go.
	Hygiene *RetrievalHygiene

	stopped chan struct{}
}

// Run drives the periodic loop. Blocks until ctx is cancelled.
// Interval <= 0 returns immediately so the caller can disable the
// loop via config without conditionally spawning the goroutine.
func (w *ConsolidateWorker) Run(ctx context.Context) {
	if w == nil || w.Consolid == nil || w.Repo == nil || w.Projects == nil {
		return
	}
	if w.Interval <= 0 {
		w.Logger.Debug().Dur("interval", w.Interval).
			Msg("memory consolidate worker disabled by config")
		return
	}
	if w.stopped == nil {
		w.stopped = make(chan struct{})
	}
	defer close(w.stopped)

	w.Logger.Info().Dur("interval", w.Interval).Int("scan_limit", w.ScanLimit).
		Msg("memory consolidate worker started")
	defer w.Logger.Info().Msg("memory consolidate worker stopped")

	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()

	if w.LeaderGate == nil || w.LeaderGate.IsLeader() {
		w.tick(ctx)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if w.LeaderGate != nil && !w.LeaderGate.IsLeader() {
				continue
			}
			w.tick(ctx)
		}
	}
}

// Stopped returns a channel closed when Run exits. Tests use it to
// synchronise without polling.
func (w *ConsolidateWorker) Stopped() <-chan struct{} {
	if w.stopped == nil {
		w.stopped = make(chan struct{})
	}
	return w.stopped
}

// tick runs one full pass: ConsolidateProject + UpsertGist for
// every project in the registry. One project's failure never
// halts the tick — the failure is logged + counted, and the loop
// continues. Tick outcome is `progressed` when at least one
// project succeeded, `idle` when there were no projects to scan,
// `errored` when every project failed.
func (w *ConsolidateWorker) tick(ctx context.Context) {
	projects := w.Projects.ListProjectIDs()
	if len(projects) == 0 {
		if w.Metrics != nil {
			w.Metrics.ConsolidateTicksTotal.WithLabelValues("idle").Inc()
		}
		return
	}
	progressed, failed := 0, 0
	for _, pid := range projects {
		if ctx.Err() != nil {
			return
		}
		if err := w.consolidateOne(ctx, pid); err != nil {
			failed++
			if w.Metrics != nil {
				w.Metrics.ConsolidateProjectsTotal.WithLabelValues(pid, "errored").Inc()
			}
			w.Logger.Warn().Err(err).Str("project", pid).
				Msg("consolidate worker: project failed")
			continue
		}
		progressed++
		if w.Metrics != nil {
			w.Metrics.ConsolidateProjectsTotal.WithLabelValues(pid, "ok").Inc()
		}
	}
	switch {
	case progressed > 0:
		if w.Metrics != nil {
			w.Metrics.ConsolidateTicksTotal.WithLabelValues("progressed").Inc()
		}
	case failed > 0 && progressed == 0:
		if w.Metrics != nil {
			w.Metrics.ConsolidateTicksTotal.WithLabelValues("errored").Inc()
		}
	default:
		if w.Metrics != nil {
			w.Metrics.ConsolidateTicksTotal.WithLabelValues("idle").Inc()
		}
	}
}

// consolidateOne runs the library against one project and
// upserts the resulting gist. The duration measurement spans
// both library + persistence; that's what operators see on the
// histogram and what they care about (E2E per-project cost).
func (w *ConsolidateWorker) consolidateOne(ctx context.Context, projectID string) error {
	// Leader-epoch fence (review B1): the tick already gates on IsLeader,
	// but a TTL-expired-then-resumed leader can pass that stale check while
	// a newer leader owns the lock. VerifyEpoch fails closed — a superseded
	// epoch (or unreadable lock row) skips the UpsertGist write so the two
	// leaders don't race on the same project_gists row. A plain IsLeader-only
	// gate or nil gate proceeds (pre-fence behaviour).
	if proceed, reason := leaderelection.DangerousWriteAllowed(ctx, w.LeaderGate); !proceed {
		w.Logger.Warn().Str("reason", reason).Str("project", projectID).
			Msg("memory_consolidate: leader epoch fence — skipping gist consolidation")
		leaderelection.LeaderFenceRejected("memory_consolidate")
		return nil
	}
	start := time.Now()
	gist, err := w.Consolid.ConsolidateProject(ctx, projectID, w.ScanLimit)
	if err != nil {
		return err
	}
	elapsed := time.Since(start)
	persisted := &PersistedGist{
		ProjectID:     projectID,
		Terms:         gist.Terms,
		ChunksScanned: gist.ChunksScanned,
		GeneratedAt:   time.Now().UTC(),
		DurationMs:    int(elapsed.Milliseconds()),
	}
	if err := w.Repo.UpsertGist(ctx, persisted); err != nil {
		return err
	}
	if w.Metrics != nil {
		w.Metrics.ConsolidateDurationSeconds.WithLabelValues(projectID).Observe(elapsed.Seconds())
	}
	w.surfaceHygieneHints(ctx, projectID)
	return nil
}

// surfaceHygieneHints fetches Consumer C retrieval hints for the project
// and surfaces them advisorily (log + metric). It NEVER deletes a chunk.
// A nil/disabled Hygiene is a no-op, so with the consumer gate off the
// worker behaves exactly as before. A hint-fetch error is logged at Warn
// and swallowed — hygiene lag is a quality concern, not a correctness
// one, and must never abort the gist pass.
func (w *ConsolidateWorker) surfaceHygieneHints(ctx context.Context, projectID string) {
	if w.Hygiene == nil || !w.Hygiene.Enabled {
		return
	}
	hints, err := w.Hygiene.Hints(ctx, projectID)
	if err != nil {
		w.Logger.Warn().Err(err).Str("project", projectID).
			Msg("consolidate worker: instinct hygiene hint fetch failed")
		return
	}
	if hints == nil || (len(hints.BoostScopes) == 0 && len(hints.PruneChunkIDs) == 0) {
		return
	}
	if w.Metrics != nil {
		w.Metrics.HygieneCandidates.WithLabelValues(projectID, "boost").Add(float64(len(hints.BoostScopes)))
		w.Metrics.HygieneCandidates.WithLabelValues(projectID, "prune").Add(float64(len(hints.PruneChunkIDs)))
	}
	w.Logger.Info().Str("project", projectID).
		Int("boost_scopes", len(hints.BoostScopes)).
		Int("prune_candidates", len(hints.PruneChunkIDs)).
		Msg("consolidate worker: instinct retrieval hints (advisory; no auto-delete)")
}
