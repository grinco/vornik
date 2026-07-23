package registry

import "reflect"

// workflowChangeIsStructural reports whether the change between two versions of
// the same workflow goes BEYOND runtime-tuning scalars. A change limited to the
// tuning whitelist (per-step Timeout & RetryPolicy; workflow MaxWallClock,
// MaxStepVisits, MaxIterations) is execution-safe and may apply live even while
// in-flight work references the workflow (design 2026-07-23 §A). Anything else
// — topology, roles, prompts, gates, terminals, flags — is structural.
// Fail-closed: only the whitelisted fields are neutralised before comparison;
// every other field (incl. any newly added struct field) reads as structural
// until explicitly whitelisted here.
func workflowChangeIsStructural(active, staged *Workflow) bool {
	return !reflect.DeepEqual(neutralizeTuning(active), neutralizeTuning(staged))
}

// neutralizeTuning returns a copy of w with the tuning-only fields zeroed, so a
// DeepEqual of two neutralised copies is true iff only tuning differs. The
// original is never mutated: the Steps map is rebuilt; other shared
// slices/maps are read-only here.
func neutralizeTuning(w *Workflow) *Workflow {
	if w == nil {
		return nil
	}
	c := *w
	c.MaxWallClock = ""
	c.MaxStepVisits = 0
	c.MaxIterations = 0
	if w.Steps != nil {
		steps := make(map[string]WorkflowStep, len(w.Steps))
		for id, st := range w.Steps {
			st.Timeout = ""
			st.RetryPolicy = WorkflowRetryPolicy{}
			steps[id] = st
		}
		c.Steps = steps
	}
	return &c
}
