package service

// Quality read-model wiring (cost/quality auto-tuning loop, Phase 1 — observe
// only). Publishes the two-tier quality gauges on the observability registry,
// refreshed on the same 30s cadence as the other state collectors
// (collectStateMetrics). No proposals, no auto-apply — that is Phase 2+.

import (
	"context"
	"time"

	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/quality"
)

const (
	// qualityWindow is the rolling window the quality tiers aggregate over.
	qualityWindow = 7 * 24 * time.Hour
	// Per-tier min-sample floors (design §A). A rate needs comparable samples
	// regardless of unit, so A2's floor is NOT lower than A1's — tasks simply
	// arrive slower than steps, so this floor gates minor/low-volume workflows
	// out of the (Phase-2) proposal set, which is intended. Phase 2 will
	// calibrate both against live task_llm_usage percentiles.
	qualityStepMinSample          = 20
	qualityTaskMinSample          = 20
	qualityScoreReconcileInterval = 30 * time.Second
)

// initQualityCollector builds the observe-only quality Service against the
// audit spine + the observability registry. No-op when observability, the
// registry, or the DB is absent (e.g. degraded boot).
func (c *Container) initQualityCollector() {
	reg := c.observabilityRegistry()
	if c.repos != nil && c.repos.ExecutionQualityScores != nil {
		var scoreMetrics *quality.ExecutionScoreMetrics
		if reg != nil {
			scoreMetrics = quality.NewExecutionScoreMetrics(reg)
		}
		c.executionScorePublisher = quality.NewExecutionScorePublisher(c.repos.ExecutionQualityScores, time.Now, scoreMetrics)
		go c.collectExecutionQualityScores()
		c.Logger.Info().Msg("execution quality score publisher started")
	}
	if reg == nil || c.Registry == nil || c.DB == nil {
		return
	}
	// Read-only by query (two SELECTs); the guarantee is by-query, not a
	// read-only connection — instrumentedDB is the shared handle writers use.
	repo := postgres.NewQualityRepository(c.instrumentedDB())
	// swarmOf runs on the state-collector goroutine (refreshQualityMetrics).
	// Registry.GetProject is RLock-guarded and returns a pointer to a Project
	// that config hot-reload replaces (new map) rather than mutating in place
	// (verified: registry.go Reload does `r.projects = cfg.projects` under
	// r.mu.Lock), so reading p.SwarmID here is a safe concurrent read.
	swarmOf := func(projectID string) string {
		p := c.Registry.GetProject(projectID)
		if p == nil {
			return ""
		}
		return p.SwarmID
	}
	c.qualityService = quality.NewService(repo, swarmOf, quality.NewMetrics(reg), quality.Config{
		StepMinSample: qualityStepMinSample,
		TaskMinSample: qualityTaskMinSample,
	})
	c.Logger.Info().Msg("quality metrics collector started")
}

// refreshQualityMetrics recomputes both quality tiers over the rolling window
// and republishes the gauges. Called from the state-collector tick; best-effort.
func (c *Container) refreshQualityMetrics() {
	if c.qualityService == nil {
		return
	}
	ctx := context.Background()
	if cc := c.collectorsCtx; cc != nil {
		ctx = cc
	}
	if _, err := c.qualityService.Refresh(ctx, time.Now().Add(-qualityWindow)); err != nil {
		c.Logger.Warn().Err(err).Msg("quality metrics refresh failed")
	}
}

// collectExecutionQualityScores is independent of Prometheus collection. The
// durable score row is part of the execution read model, so disabling metrics
// must not disable publication. It reconciles immediately at boot and then on
// a bounded cadence; the terminal execution ledger remains authoritative.
func (c *Container) collectExecutionQualityScores() {
	ticker := time.NewTicker(qualityScoreReconcileInterval)
	defer ticker.Stop()

	for {
		c.reconcileExecutionQualityScores()

		var done <-chan struct{}
		if ctx := c.collectorsCtx; ctx != nil {
			done = ctx.Done()
		}
		select {
		case <-ticker.C:
		case <-done:
			return
		}
	}
}

func (c *Container) reconcileExecutionQualityScores() {
	if c.executionScorePublisher == nil {
		return
	}
	ctx := context.Background()
	if cc := c.collectorsCtx; cc != nil {
		ctx = cc
	}
	result, err := c.executionScorePublisher.Reconcile(ctx, 100)
	if err != nil {
		c.Logger.Warn().Err(err).
			Int("selected", result.Selected).
			Int("published", result.Published).
			Int("failed", result.Failed).
			Msg("execution quality score reconciliation incomplete")
	}
}
