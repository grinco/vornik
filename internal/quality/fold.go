package quality

import "sort"

// RoleAggregate is a raw per-(project, role) A1 aggregate over the rolling
// window, as read from the audit spine (execution_step_outcomes joined to
// task_llm_usage). Passing = steps that met the A1 quality bar; PromptTokens =
// summed prompt tokens over the Passing steps.
type RoleAggregate struct {
	ProjectID    string
	Role         string
	Total        int64
	Passing      int64
	PromptTokens int64
}

// SwarmRoleAggregate is RoleAggregates folded to the (swarm, role) grain the
// knob is actually applied at. Projects lists the projects that share the
// swarm-role (the proposal's blast radius), sorted for stable display.
type SwarmRoleAggregate struct {
	Swarm    string
	Role     string
	Projects []string
	TierInput
}

// TaskAggregate is a raw per-(project, workflow) A2 aggregate over the window.
// Passing = tasks that met the A2 quality bar (terminal COMPLETED with no
// hard-failure step); PromptTokens = summed prompt tokens over the Passing tasks.
type TaskAggregate struct {
	ProjectID    string
	WorkflowID   string
	Total        int64
	Passing      int64
	PromptTokens int64
}

// SwarmWorkflowAggregate is TaskAggregates folded to the (swarm, workflow)
// grain — the A2 analogue of SwarmRoleAggregate.
type SwarmWorkflowAggregate struct {
	Swarm    string
	Workflow string
	Projects []string
	TierInput
}

// foldedAgg is the common result of folding any per-(project, key) aggregate to
// (swarm, key) grain. Key is the role (A1) or workflow (A2).
type foldedAgg struct {
	Swarm    string
	Key      string
	Projects []string
	TierInput
}

// foldBySwarm sums per-project aggregates into per-(swarm, key) aggregates:
// counts are summed BEFORE scoring (never average pre-computed rates); a project
// whose swarm does not resolve (empty) is dropped rather than pooled under an
// empty key; results are stably sorted. Shared by both tiers so the fold logic
// lives once (the A1/A2 wrappers below just re-key the typed output).
//
// Attribution model is "current swarm wins": swarmOf is evaluated now, so a
// project that changed swarms mid-window has its whole window attributed to its
// current swarm (and Projects under-reports the prior swarm's membership). This
// is acceptable for a 7-day observe window; Phase 2 blast-radius consumers
// should be aware it is not as-of-window attribution.
func foldBySwarm[T any](
	in []T,
	project func(T) string,
	key func(T) string,
	counts func(T) (total, passing, promptTokens int64),
	swarmOf func(projectID string) string,
	minSample int64,
) []foldedAgg {
	type k struct{ swarm, key string }
	acc := map[k]*foldedAgg{}
	for _, it := range in {
		swarm := swarmOf(project(it))
		if swarm == "" {
			continue
		}
		kk := k{swarm, key(it)}
		a := acc[kk]
		if a == nil {
			a = &foldedAgg{Swarm: swarm, Key: key(it), TierInput: TierInput{MinSample: minSample}}
			acc[kk] = a
		}
		tot, pass, pt := counts(it)
		a.Total += tot
		a.Passing += pass
		a.PromptTokens += pt
		a.Projects = append(a.Projects, project(it))
	}
	out := make([]foldedAgg, 0, len(acc))
	for _, a := range acc {
		sort.Strings(a.Projects)
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Swarm != out[j].Swarm {
			return out[i].Swarm < out[j].Swarm
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// FoldRolesBySwarm folds per-project A1 aggregates to (swarm, role) grain.
func FoldRolesBySwarm(in []RoleAggregate, swarmOf func(projectID string) string, minSample int64) []SwarmRoleAggregate {
	folded := foldBySwarm(in,
		func(a RoleAggregate) string { return a.ProjectID },
		func(a RoleAggregate) string { return a.Role },
		func(a RoleAggregate) (int64, int64, int64) { return a.Total, a.Passing, a.PromptTokens },
		swarmOf, minSample)
	out := make([]SwarmRoleAggregate, len(folded))
	for i, f := range folded {
		out[i] = SwarmRoleAggregate{Swarm: f.Swarm, Role: f.Key, Projects: f.Projects, TierInput: f.TierInput}
	}
	return out
}

// FoldTasksBySwarm folds per-project A2 aggregates to (swarm, workflow) grain.
func FoldTasksBySwarm(in []TaskAggregate, swarmOf func(projectID string) string, minSample int64) []SwarmWorkflowAggregate {
	folded := foldBySwarm(in,
		func(a TaskAggregate) string { return a.ProjectID },
		func(a TaskAggregate) string { return a.WorkflowID },
		func(a TaskAggregate) (int64, int64, int64) { return a.Total, a.Passing, a.PromptTokens },
		swarmOf, minSample)
	out := make([]SwarmWorkflowAggregate, len(folded))
	for i, f := range folded {
		out[i] = SwarmWorkflowAggregate{Swarm: f.Swarm, Workflow: f.Key, Projects: f.Projects, TierInput: f.TierInput}
	}
	return out
}
