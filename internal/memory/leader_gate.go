package memory

// LeaderGate is the narrow contract the singleton-style memory
// workers (title backfill, classify backfill, consolidate,
// LLM consolidate) consult before each tick. IsLeader()=false
// → skip the tick so two daemons in a multi-replica deployment
// don't race on the same chunks / project_gists rows.
//
// Defined locally so the memory package doesn't pull
// internal/leaderelection into its dependency set —
// *leaderelection.Elector satisfies this interface structurally.
//
// nil-tolerant at the consumer site: a worker with no gate
// runs every tick (single-process default).
type LeaderGate interface {
	IsLeader() bool
}
