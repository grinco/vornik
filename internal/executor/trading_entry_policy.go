package executor

import (
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/verifier"
)

// filterTradingEntryPolicy runs after the scorecard floor so ineligible
// scorecards do not consume the entry quota. Research/chat tasks are unaffected.
func (e *Executor) filterTradingEntryPolicy(task *persistence.Task, raw []byte) ([]byte, error) {
	if extractTaskType(task) != "trading" {
		return raw, nil
	}
	resolver, ok := e.workflows.(interface {
		GetProject(string) *registry.Project
	})
	if !ok {
		return raw, nil
	}
	p := resolver.GetProject(task.ProjectID)
	if p == nil {
		return raw, nil
	}
	return verifier.FilterTradingEntryPolicy(raw, p.Trading.EntryPolicy)
}
