package service

// Per-candidate promotion-gate threshold resolution for the Self-Healing
// Workflow Genome. The workflowhealing.GateThresholdResolver is keyed by
// (project, workflow, trigger-CLASS), but a candidate only carries its
// trigger ID — so this adapter looks up the trigger to recover its class
// before delegating. Implements workflowhealing.GateResolver; the runner
// holds it via WithGateResolver and consults it per replay trial.

import (
	"context"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/workflowhealing"
)

type healingGateResolverAdapter struct {
	resolver *workflowhealing.GateThresholdResolver
	triggers persistence.WorkflowHealingTriggerRepository
}

// newHealingGateResolverAdapter returns nil when the underlying resolver
// is absent (the runner then uses its static gate). A nil triggers repo
// is tolerated — resolution falls back to the empty trigger class, which
// the overrides repo treats as "no override" → DefaultGateThresholds.
func newHealingGateResolverAdapter(resolver *workflowhealing.GateThresholdResolver, triggers persistence.WorkflowHealingTriggerRepository) workflowhealing.GateResolver {
	if resolver == nil {
		return nil
	}
	return &healingGateResolverAdapter{resolver: resolver, triggers: triggers}
}

func (a *healingGateResolverAdapter) ResolveForCandidate(ctx context.Context, cand *persistence.HealingCandidate) workflowhealing.GateThresholds {
	if a == nil || a.resolver == nil || cand == nil {
		return workflowhealing.DefaultGateThresholds()
	}
	// Recover the trigger class the override repo is keyed by. A missing
	// trigger (dismissed, pruned) just leaves the class empty → defaults.
	var class persistence.HealingTriggerClass
	if a.triggers != nil && cand.TriggerID != "" {
		if t, err := a.triggers.Get(ctx, cand.TriggerID); err == nil && t != nil {
			class = t.TriggerClass
		}
	}
	return a.resolver.Resolve(ctx, cand.ProjectID, cand.WorkflowID, class)
}
