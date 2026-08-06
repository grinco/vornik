package contractreg

import (
	forgeh "vornik.io/vornik/internal/executor/handlers/forge"
)

// AddSystemHandlers records the workflow system-step handler names.
//
// These are Go implementations selected BY NAME from workflow YAML
// (`type: system`, `handler: forge.post_review`), so they are exactly the
// live-by-contract case: a call graph cannot see the link, and today they are
// only reachable because the scheduler's wiring constructs them.
//
// Enumerated by calling Name() on zero-value handlers rather than by hardcoding
// strings. That reads the vocabulary out of the code — rename a handler and this
// stops compiling, instead of silently drifting the way the four agent-tool
// registries did (see CheckAgentToolAgreement).
//
// Safe because every Name() returns a literal and touches no field of its
// receiver. If one ever needs its dependencies to answer, this must switch to
// constructing them properly — a Name() that dereferences a nil field would
// panic here, loudly, which is the failure mode we want rather than a silent
// empty set.
//
// The registry itself cannot be enumerated statically: the scheduler builds it
// with live dependencies (a forge resolver, the artifact store, the AI-disclosure
// service), so there is no package-level instance to ask.
func (t *Table) AddSystemHandlers() {
	const src = "internal/service/container_scheduler.go (sysHandlers.Register)"
	for _, name := range []string{
		(&forgeh.OpenChangeRequestHandler{}).Name(),
		(&forgeh.PostReviewHandler{}).Name(),
		(&forgeh.FetchDiffHandler{}).Name(),
	} {
		t.Add(KindSystemHandler, name, src)
	}
}
