package registry

import "testing"

// The apply journal records generation_before and generation_after around a
// reload and calls the pair evidence that the running registry re-parsed the
// bytes the apply wrote. Before this counter existed it could not: the marker
// was a digest over the resolved project set, which says WHAT is resolved and
// not that anything was re-read. Two applies that produce the same resolved
// set are indistinguishable from an apply that never reached the registry —
// and "indistinguishable from never examined" is the failure this repository
// keeps recording (config-apply-journal design §9.2, CA-19).

func TestGeneration_AdvancesOnActivateStaged(t *testing.T) {
	r := New()
	before := r.Generation()

	r.staged = &ConfigSet{projects: map[string]*Project{}, swarms: map[string]*Swarm{}, workflows: map[string]*Workflow{}}
	if err := r.ActivateStaged(); err != nil {
		t.Fatalf("activate: %v", err)
	}

	if got := r.Generation(); got != before+1 {
		t.Fatalf("generation did not advance: before=%d after=%d", before, got)
	}
}

// Every activation counts, including one that resolves to the SAME content.
// A counter that only moved on a content change would be the digest again,
// and the whole point is to distinguish "re-parsed" from "unchanged".
func TestGeneration_AdvancesEvenWhenContentIsIdentical(t *testing.T) {
	r := New()
	empty := func() *ConfigSet {
		return &ConfigSet{projects: map[string]*Project{}, swarms: map[string]*Swarm{}, workflows: map[string]*Workflow{}}
	}

	r.staged = empty()
	_ = r.ActivateStaged()
	first := r.Generation()

	r.staged = empty()
	_ = r.ActivateStaged()

	if got := r.Generation(); got != first+1 {
		t.Fatalf("identical content did not advance the generation: %d then %d", first, got)
	}
}

// A refused activation must not advance it. Otherwise the journal would record
// a re-parse that did not happen, which is worse than recording nothing.
func TestGeneration_DoesNotAdvanceOnRefusedActivation(t *testing.T) {
	r := New()
	r.staged = &ConfigSet{projects: map[string]*Project{}, swarms: map[string]*Swarm{}, workflows: map[string]*Workflow{}}
	_ = r.ActivateStaged()
	after := r.Generation()

	if err := r.ActivateStaged(); err == nil {
		t.Fatal("activating with nothing staged should refuse")
	}
	if got := r.Generation(); got != after {
		t.Fatalf("a refused activation advanced the generation: %d -> %d", after, got)
	}
}

// A nil registry is safe: the container reads this on paths that run before
// the registry exists.
func TestGeneration_NilRegistryIsZero(t *testing.T) {
	var r *Registry
	if got := r.Generation(); got != 0 {
		t.Fatalf("want 0 from a nil registry, got %d", got)
	}
}
