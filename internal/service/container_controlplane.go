package service

import (
	"context"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/controlplane"
	"vornik.io/vornik/internal/persistence"
)

// newProposalApplier builds the Phase-2 apply/rollback engine (LLD
// 2026-07-08-control-plane-phase2). Returns nil when the proposal ledger
// isn't wired. Deps are late-bound so the reloader can be set after this.
func (c *Container) newProposalApplier() *controlplane.ApplyEngine {
	if c == nil || c.repos == nil || c.repos.Proposals == nil {
		return nil
	}
	return &controlplane.ApplyEngine{
		Proposals: c.repos.Proposals,
		// apply_target is resolved under the config.yaml dir, so both
		// "config.yaml" and "configs/swarms/<x>.md" resolve correctly.
		ConfigDir: filepath.Dir(c.ConfigPath),
		Reload: func() error {
			if c.ConfigReloader != nil {
				return c.ConfigReloader.Reload()
			}
			return nil
		},
		// Cheap pre-write syntactic gate; the reload is the authoritative
		// validator (and auto-rolls-back on rejection).
		Validate: func(_, content string) error {
			var v any
			return yaml.Unmarshal([]byte(content), &v)
		},
		HasActiveTasks: func(ctx context.Context, projectID string) (bool, error) {
			if c.repos.Tasks == nil {
				return false, nil
			}
			counts, err := c.repos.Tasks.CountByStatus(ctx, projectID)
			if err != nil {
				return false, err
			}
			return counts[persistence.TaskStatusRunning]+counts[persistence.TaskStatusLeased] > 0, nil
		},
		Logger: c.Logger.With().Str("component", "control-plane").Str("engine", "apply").Logger(),
	}
}

// Control-plane server-side workers (LLD 2026-07-07-control-plane-design,
// Phase 1). Currently the Tune detector: a leader-gated scan that raises a
// DRAFT proposal when a project's failed-task rate stays high. It never
// mutates config — proposing is the only action.

// tuneScanInterval / tuneWindow are the MVP cadence + look-back. Hourly scan
// over the trailing 6h keeps the signal responsive without over-reacting to a
// single bad task.
const (
	tuneScanInterval = time.Hour
	tuneWindow       = 6 * time.Hour
)

// execMetricsSource adapts the ExecutionRepository's windowed failed-rate + latency
// query to the controlplane.MetricsSource the Tune worker consumes.
type execMetricsSource struct {
	execs  persistence.ExecutionRepository
	window time.Duration
}

func (s execMetricsSource) FailedTaskRates(ctx context.Context) (map[string]controlplane.RateSample, error) {
	stats, err := s.execs.FailedRateByProject(ctx, time.Now().Add(-s.window))
	if err != nil {
		return nil, err
	}
	out := make(map[string]controlplane.RateSample, len(stats))
	for project, st := range stats {
		rate := 0.0
		if st.Total > 0 {
			rate = float64(st.Failed) / float64(st.Total)
		}
		out[project] = controlplane.RateSample{Failed: int(st.Failed), Total: int(st.Total), Rate: rate}
	}
	return out, nil
}

func (s execMetricsSource) LatencyP95s(ctx context.Context) (map[string]controlplane.LatencySample, error) {
	stats, err := s.execs.LatencyP95ByProject(ctx, time.Now().Add(-s.window))
	if err != nil {
		return nil, err
	}
	out := make(map[string]controlplane.LatencySample, len(stats))
	for project, st := range stats {
		out[project] = controlplane.LatencySample{P95Seconds: st.P95Seconds, Count: int(st.Count)}
	}
	return out, nil
}

// startTuneWorker wires + starts the control-plane Tune detector, leader-gated
// so only one replica scans. Nil-safe: a no-op when the proposal ledger or
// execution repo isn't wired (minimal harnesses).
func (c *Container) startTuneWorker(ctx context.Context) {
	if c == nil || c.repos == nil || c.repos.Proposals == nil || c.repos.Executions == nil {
		return
	}
	w := &controlplane.TuneWorker{
		Proposals: c.repos.Proposals,
		Metrics:   execMetricsSource{execs: c.repos.Executions, window: tuneWindow},
		Interval:  tuneScanInterval,
		Logger:    c.Logger.With().Str("component", "control-plane").Str("worker", "tune").Logger(),
	}
	if elector := c.initWorkerElector("control_plane_tune"); elector != nil {
		w.LeaderGate = elector
		elector.BootstrapAcquire(ctx)
		go elector.Run(ctx)
	}
	go w.Run(collectorsCtxFrom(ctx, c))
}
