package quality

import (
	"context"
	"time"
)

// Repo reads the raw per-(project,·) aggregates from the audit spine. Backed by
// internal/persistence/postgres.QualityRepository in production; faked in tests.
type Repo interface {
	RoleQualityAggregates(ctx context.Context, since time.Time) ([]RoleAggregate, error)
	TaskQualityAggregates(ctx context.Context, since time.Time) ([]TaskAggregate, error)
}

// Config carries the per-tier min-sample floors (design §A — each tier has its
// own floor; A2 is coarser/slower than A1).
type Config struct {
	StepMinSample int64
	TaskMinSample int64
}

// ScoredSwarmRole is an A1 result: a folded (swarm, role) aggregate with its
// TierScore and the sharing-project blast radius.
type ScoredSwarmRole struct {
	Swarm    string
	Role     string
	Projects []string
	TierScore
}

// ScoredSwarmWorkflow is the A2 analogue at (swarm, workflow) grain.
type ScoredSwarmWorkflow struct {
	Swarm    string
	Workflow string
	Projects []string
	TierScore
}

// Report is the two-tier quality snapshot for a window.
type Report struct {
	Steps []ScoredSwarmRole
	Tasks []ScoredSwarmWorkflow
}

// Service composes the repo, the project→swarm resolver, and the gauges into
// the observe-only quality read-model (Phase 1).
type Service struct {
	repo    Repo
	swarmOf func(projectID string) string
	metrics *Metrics
	cfg     Config
}

// NewService builds a quality Service. swarmOf maps a project id to its swarm id
// (registry-backed in production); metrics may be nil to skip gauge publishing.
func NewService(repo Repo, swarmOf func(projectID string) string, metrics *Metrics, cfg Config) *Service {
	return &Service{repo: repo, swarmOf: swarmOf, metrics: metrics, cfg: cfg}
}

// Refresh reads aggregates since `since`, folds them to swarm grain, scores both
// tiers, publishes the gauges, and returns the snapshot.
//
// Consumer contract: for a series with Sufficient=false the QualityRate and
// EffectiveCostTokens values are UNDEFINED (published as 0.0 for gauge
// continuity, not a real measurement). A Phase-2 detector/guard MUST gate on
// Sufficient and never read the 0.0 as a real zero.
func (s *Service) Refresh(ctx context.Context, since time.Time) (Report, error) {
	// Reset the gauge vecs each tick so the published set is a COMPLETE
	// snapshot of the window: series that fell out (a workflow not run in 7d,
	// a deleted project) clear instead of retaining a stale Sufficient=1 that a
	// Phase-2 consumer would misread as a live, trustworthy locus
	// (review-20260721-78d1 #4). Reset runs before repopulation, so a scrape
	// landing mid-Refresh sees a fresh-but-momentarily-partial snapshot (not
	// atomic) — acceptable for a 7-day window on a timer; GaugeVec.Reset is
	// internally locked so concurrent Set is safe (review-20260721-0f0e).
	if s.metrics != nil {
		s.metrics.QualityScore.Reset()
		s.metrics.EffectiveCostTokens.Reset()
		s.metrics.Sufficient.Reset()
	}
	roles, err := s.repo.RoleQualityAggregates(ctx, since)
	if err != nil {
		return Report{}, err
	}
	tasks, err := s.repo.TaskQualityAggregates(ctx, since)
	if err != nil {
		return Report{}, err
	}

	var rep Report
	for _, a := range FoldRolesBySwarm(roles, s.swarmOf, s.cfg.StepMinSample) {
		sc := ScoreTier(a.TierInput)
		rep.Steps = append(rep.Steps, ScoredSwarmRole{Swarm: a.Swarm, Role: a.Role, Projects: a.Projects, TierScore: sc})
		s.publish("step", a.Swarm, a.Role, sc)
	}
	for _, a := range FoldTasksBySwarm(tasks, s.swarmOf, s.cfg.TaskMinSample) {
		sc := ScoreTier(a.TierInput)
		rep.Tasks = append(rep.Tasks, ScoredSwarmWorkflow{Swarm: a.Swarm, Workflow: a.Workflow, Projects: a.Projects, TierScore: sc})
		s.publish("task", a.Swarm, a.Workflow, sc)
	}
	return rep, nil
}

func (s *Service) publish(tier, swarm, key string, sc TierScore) {
	if s.metrics == nil {
		return
	}
	s.metrics.QualityScore.WithLabelValues(tier, swarm, key).Set(sc.QualityRate)
	s.metrics.EffectiveCostTokens.WithLabelValues(tier, swarm, key).Set(sc.EffectiveCostTokens)
	suff := 0.0
	if sc.Sufficient {
		suff = 1.0
	}
	s.metrics.Sufficient.WithLabelValues(tier, swarm, key).Set(suff)
}
